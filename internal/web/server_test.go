// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/store"
)

// quiet is the logger a test hands the server. The server writes a line when a
// page cannot be built, and a test that provokes that on purpose should not
// print it beside the result.
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gserp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func testServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Config{Store: testStore(t), Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// get drives the routes without a socket and returns what came back.
func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestServer_ShowsAPageWhereThereAreNoJobsYet(t *testing.T) {
	// The first thing anyone sees is an empty install. A blank page reads as a
	// broken one; a page that says there is nothing yet reads as working.
	rec := get(t, testServer(t), jobsAt)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s gave %d, want 200", jobsAt, rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<html") {
		t.Error("the response is not a page")
	}
	if !strings.Contains(body, "No jobs yet") {
		t.Errorf("the page does not say that nothing has been run yet:\n%s", body)
	}
	if strings.Contains(body, "%!") {
		t.Errorf("the template rendered a formatting error:\n%s", body)
	}
}

func TestServer_ListsEveryJobTheHistoryHolds(t *testing.T) {
	// A list that leaves a run out is worse than no list: the run is there, it
	// is just invisible, and nobody goes looking for what they were shown.
	st := testStore(t)
	for _, name := range []string{"morning list", "evening list"} {
		if _, err := st.CreateJob(context.Background(),
			store.JobSpec{Name: name, Pages: 1}, []string{"a", "b"}); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
	}
	s, err := New(Config{Store: st, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := get(t, s, jobsAt).Body.String()
	for _, name := range []string{"morning list", "evening list"} {
		if !strings.Contains(body, name) {
			t.Errorf("the page does not list %q:\n%s", name, body)
		}
	}
	if strings.Contains(body, "No jobs yet") {
		t.Error("the page says there is nothing yet while listing two jobs")
	}
}

func TestServer_RendersThePageInTheLanguageAsked(t *testing.T) {
	// jobs.none is the phrase under test because its two translations share no
	// letter: one is Latin and the other Cyrillic, so a page carrying both is
	// unmistakable, and the test cannot pass on a server that renders one
	// language and calls it the other.
	rec := get(t, testServer(t), jobsAt+"?lang=ru")

	body := rec.Body.String()
	if !strings.Contains(body, LangRU.T("jobs.none")) {
		t.Errorf("the page did not come back in Russian:\n%s", body)
	}
	if strings.Contains(body, LangEN.T("jobs.none")) {
		t.Errorf("the page carries both languages at once:\n%s", body)
	}
}

func TestServer_ShowsNoBareKeyWhereAPhraseBelongs(t *testing.T) {
	// A key on the page is a phrase that was never looked up: the template
	// printed the name of the text instead of the text. That mistake survives
	// every test written about a particular phrase, because it lands on the
	// phrases nobody thought to check, so this one is over all of them at once.
	//
	// Both an empty history and one with a job in it are drawn, since the two
	// between them are what puts every phrase this program has on a page.
	st := testStore(t)
	if _, err := st.CreateJob(context.Background(),
		store.JobSpec{Name: "morning list", Pages: 1}, []string{"a"}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	listing, err := New(Config{Store: st, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, s := range []*Server{testServer(t), listing} {
		for _, l := range Languages() {
			body := get(t, s, jobsAt+"?lang="+string(l)).Body.String()
			for key := range catalogue[l] {
				if strings.Contains(body, key) {
					t.Errorf("the %s page shows the key %q where its text belongs", l, key)
				}
			}
		}
	}
}

func TestServer_SaysWhichLanguageThePageIsWrittenIn(t *testing.T) {
	// A reader who needs the page read aloud, or translated by the browser, is
	// told which language it is in by the markup or not at all.
	body := get(t, testServer(t), "/?lang=ru").Body.String()
	if !strings.Contains(body, `<html lang="ru"`) {
		t.Errorf("the page does not declare the language it is written in:\n%s", body)
	}
}

func TestServer_RemembersTheLanguageAskedForSoTheNextPageNeedsNoAsking(t *testing.T) {
	// Without this the switch lasts exactly one page: every link on it drops the
	// query, and the reader lands back in the language the browser prefers.
	rec := get(t, testServer(t), "/?lang=ru")

	var stored string
	for _, c := range rec.Result().Cookies() {
		if c.Name == langCookie {
			stored = c.Value
		}
	}
	if stored != string(LangRU) {
		t.Errorf("the answer remembered %q, want ru", stored)
	}
}

func TestServer_RemembersNothingWhenNobodyHasChosen(t *testing.T) {
	// What Accept-Language says is a guess about the reader. Writing it down
	// turns that guess into a decision they never made, and a browser whose
	// preferences change afterwards is then ignored forever.
	rec := get(t, testServer(t), "/")

	for _, c := range rec.Result().Cookies() {
		if c.Name == langCookie {
			t.Errorf("the answer wrote down %q without being asked", c.Value)
		}
	}
}

func TestServer_ChoosesTheLanguageInTheSettingsAndNotAlongTheTop(t *testing.T) {
	// The strip along the top carried the choice on every screen, for a press
	// most readers make once and never again. It is made where the rest of the
	// machine is set up now.
	s := testServer(t)
	body := get(t, s, "/").Body.String()
	if strings.Contains(body, `class="langs"`) {
		t.Errorf("the language is still offered along the top:\n%s", body)
	}

	// Asking for a language in the address still works — it is how a reader who
	// cannot read this page reaches one they can, and how the choice they saved
	// is carried.
	if body := get(t, s, "/?lang=ru").Body.String(); !strings.Contains(body, `<html lang="ru"`) {
		t.Error("a page asked for in another language came back in English")
	}

	// And every language is offered where the choice is made. It takes a server
	// that has somewhere to write settings: one that has not offers no settings
	// page at all, which is right and is not what this is about.
	kept, err := New(Config{
		Store: testStore(t), Logger: quiet(),
		SettingsPath: filepath.Join(t.TempDir(), "settings.json"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	page := get(t, kept, settingsAt).Body.String()
	for _, want := range []string{`name="language"`, "Русский", "English"} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings do not offer %q", want)
		}
	}
}

func TestServer_OffersTheFormThatStartsAJobWhereJobsAreRead(t *testing.T) {
	// Starting a job is what this program is for, and the press that does it
	// stands where jobs are read: on the list of them, and on the screen the
	// work is watched from while nothing is running. A page nothing links to is
	// a page nobody finds.
	s := testServer(t)
	for _, at := range []string{jobsAt, stateAt} {
		if body := get(t, s, at).Body.String(); !strings.Contains(body, `href="/new"`) {
			t.Errorf("%s does not offer the form that starts a job:\n%s", at, body)
		}
	}

	// And it is not one of the screens. The strip of tabs is what a reader
	// watches the work from; a form standing among four screens that report is
	// one press in five being a different kind of thing.
	for _, t2 := range tabs {
		if t2.At == newAt {
			t.Errorf("the form that starts a job is a tab again")
		}
	}
	for _, shown := range tabsOffered(t, get(t, s, jobsAt).Body.String()) {
		if shown.At == newAt {
			t.Errorf("the strip of tabs carries the form that starts a job")
		}
	}
}

func TestServer_ServesItsOwnStylesheet(t *testing.T) {
	// Everything ships inside the binary; a missing asset means the embed
	// pattern stopped matching and nobody would notice from the Go side.
	rec := get(t, testServer(t), "/assets/app.css")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.css gave %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type=%q, want a stylesheet", ct)
	}
	if rec.Body.Len() == 0 {
		t.Error("the stylesheet came back empty")
	}
}

func TestServer_KeepsTheTemplatesItRendersFromOffTheWire(t *testing.T) {
	// The templates sit in the same embedded directory as the stylesheet. Handed
	// out as files they are a second, unrendered copy of every page, and each
	// later page adds to what that copy gives away.
	for _, path := range []string{"/assets/jobs.html", "/assets/layout.html"} {
		if code := get(t, testServer(t), path).Code; code != http.StatusNotFound {
			t.Errorf("GET %s gave %d, want 404", path, code)
		}
	}
}

func TestServer_AnswersAnUnknownPathWithNotFound(t *testing.T) {
	if code := get(t, testServer(t), "/nope").Code; code != http.StatusNotFound {
		t.Errorf("GET /nope gave %d, want 404", code)
	}
}

func TestServer_TellsTheReaderNothingAboutTheDatabaseWhenItCannotRead(t *testing.T) {
	// An error page quoting the query or the file behind it hands a reader
	// facts about the machine, and helps them not at all.
	st := testStore(t)
	s, err := New(Config{Store: st, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec := get(t, s, jobsAt)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET %s on an unreadable history gave %d, want 500", jobsAt, rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"sql", "SELECT", "sqlite", ".db"} {
		if strings.Contains(body, leak) {
			t.Errorf("the error page mentions %q:\n%s", leak, body)
		}
	}
	if strings.TrimSpace(body) == "" {
		t.Error("the reader was given a blank page instead of a sentence")
	}
}

func TestNew_RefusesToStartWithoutAStore(t *testing.T) {
	// Starting and failing on the first request would put the fault a page
	// away from its cause.
	if _, err := New(Config{Logger: quiet()}); err == nil {
		t.Error("New accepted a configuration with no store")
	}
}

func TestNew_RefusesToStartWhenALanguageIsShortOfText(t *testing.T) {
	// A check that is written but never called is worth nothing: the phrase
	// still goes missing, and the bare key still reaches the one reader of that
	// language. Nothing here runs in parallel, so the catalogue can be swapped
	// for a broken one and put back.
	whole := catalogue
	t.Cleanup(func() { catalogue = whole })
	catalogue = map[Lang]map[string]string{
		LangEN: {"jobs.title": "Jobs", "jobs.none": "No jobs yet."},
		LangRU: {"jobs.title": "Задания"},
	}

	_, err := New(Config{Store: testStore(t), Logger: quiet()})
	if !errors.Is(err, ErrMissingText) {
		t.Errorf("New gave %v, want a refusal naming the missing text", err)
	}
}

func TestNew_ParsesEveryPageBeforeTheFirstRequestArrives(t *testing.T) {
	// A broken template is a mistake in this repository, not in anybody's data.
	// Parsed at startup it stops the program; parsed per request it waits for
	// the one reader who happens to open that page.
	s := testServer(t)
	if len(s.pages) == 0 {
		t.Fatal("no page was parsed when the server was built")
	}
	if s.pages["jobs.html"] == nil {
		t.Error("the job list was not parsed when the server was built")
	}
}

func TestServe_StopsWhenTheCallerDoes(t *testing.T) {
	// A server that outlives its context holds the port and the database, and
	// the process never exits.
	s := testServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	// Reach it once so the test is about a running server, not a racing one.
	res, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within five seconds of its context being cancelled")
	}
}

// heldListener hands out connections that stop before the first byte of an
// answer reaches the client and wait for the test to let it through. A request
// whose answer is half written is the one state a graceful shutdown exists for,
// and it is reachable only from the listener side.
type heldListener struct {
	net.Listener
	answering chan struct{} // closed once an answer is ready to go out
	release   chan struct{} // closed by the test to let that answer through
	once      sync.Once
}

func held(ln net.Listener) *heldListener {
	return &heldListener{
		Listener:  ln,
		answering: make(chan struct{}),
		release:   make(chan struct{}),
	}
}

func (l *heldListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &heldConn{Conn: c, held: l}, nil
}

type heldConn struct {
	net.Conn
	held *heldListener
}

func (c *heldConn) Write(p []byte) (int, error) {
	c.held.once.Do(func() { close(c.held.answering) })
	<-c.held.release
	return c.Conn.Write(p)
}

func TestServe_FinishesTheAnswerAlreadyGoingOutBeforeItReturns(t *testing.T) {
	// Serve returning is the signal every caller acts on: it closes the history
	// and exits. Returning while a page is half written closes the database
	// under a request still reading it, and truncates the page.
	s := testServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	stalled := held(ln)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, stalled) }()

	type fetched struct {
		code int
		body string
		err  error
	}
	answered := make(chan fetched, 1)
	go func() {
		res, err := http.Get("http://" + addr + "/")
		if err != nil {
			answered <- fetched{err: err}
			return
		}
		defer func() { _ = res.Body.Close() }()
		body, err := io.ReadAll(res.Body)
		answered <- fetched{code: res.StatusCode, body: string(body), err: err}
	}()

	select {
	case <-stalled.answering:
	case got := <-answered:
		t.Fatalf("the answer went out before the test could hold it: %+v", got)
	case <-time.After(5 * time.Second):
		t.Fatal("the server never began answering")
	}

	cancel()
	select {
	case err := <-done:
		t.Fatalf("Serve returned %v with a page still half written", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(stalled.release)
	select {
	case got := <-answered:
		if got.err != nil {
			t.Fatalf("the answer in flight was cut off: %v", got.err)
		}
		if got.code != http.StatusOK {
			t.Errorf("the answer in flight came back %d, want 200", got.code)
		}
		if !strings.Contains(got.body, "</html>") {
			t.Errorf("the answer in flight was truncated:\n%s", got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the answer never arrived")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return once the answer had gone out")
	}
}

func TestBrowserPolls_NameEveryAddressThePagesThemselvesPostTo(t *testing.T) {
	// Whoever mounts a programmable interface under /api/ takes whatever is not
	// on this list, and the page then goes on posting to an address that refuses
	// it. The list is read against the markup rather than against itself, so a
	// page that grows an address without it being named here fails now instead
	// of on somebody's screen.
	s := testServer(t)
	id := seedJob(t, s, "for its own page", 1, 0, 0)

	named := map[string]bool{}
	for _, at := range BrowserPolls() {
		named[at] = true
	}
	for _, page := range []string{jobPath(id), "/", "/jobs", "/new", "/history"} {
		body := get(t, s, page).Body.String()
		for _, after := range strings.Split(body, `action="`)[1:] {
			at, _, _ := strings.Cut(after, `"`)
			if !strings.HasPrefix(at, "/api/") {
				continue
			}
			if !named[at] {
				t.Errorf("%s posts to %s, which is not named among the pages' own addresses",
					page, at)
			}
		}
	}
}
