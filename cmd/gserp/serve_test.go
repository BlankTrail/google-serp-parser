// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
	"github.com/blanktrail/google-serp-parser/store"
	"github.com/blanktrail/google-serp-parser/web"
)

// listeningAt reads back the address the command says to open.
func listeningAt(t *testing.T, printed string) string {
	t.Helper()
	_, rest, ok := strings.Cut(printed, "http://")
	if !ok {
		t.Fatalf("nothing printed says where to open a browser:\n%s", printed)
	}
	return strings.Fields(rest)[0]
}

func TestServeCommand_SaysWhichPortItWasGivenAndStopsWhenTold(t *testing.T) {
	// Port zero is the only way to ask for a free one, and whoever asked cannot
	// reach the server unless the command says which one it got. Stopping is the
	// other half: a server that outlives its context holds the port and the
	// history, and the process never exits.
	//
	// The key is emptied for the length of this test, so the command opens
	// nothing: what it would open is somewhere else on this machine or on
	// another, and a unit test that reached for it would be reporting on that.
	t.Setenv(envAPIKey, "")
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- serveCommand(ctx, []string{
			"-addr", "127.0.0.1:0", "-db", filepath.Join(t.TempDir(), "h.db"),
		}, out)
	}()
	waitFor(t, "the command to say where to open a browser", func() bool {
		return strings.Contains(out.String(), "http://")
	})

	res, err := http.Get("http://" + listeningAt(t, out.String()) + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("the job list came back %d, want 200", res.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveCommand returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command did not return within ten seconds of its context being cancelled")
	}
}

// servedAt starts the interface on a port of the system's choosing and returns
// where to reach it. The command stops when the test ends.
//
// A history named by the caller is left alone, so a test that had to write
// something into one before the interface opened it — a key, for instance — can
// hand over the one it wrote.
func servedAt(t *testing.T, opts serveOptions) string {
	t.Helper()
	opts.Addr = "127.0.0.1:0"
	if opts.DB == "" {
		opts.DB = filepath.Join(t.TempDir(), "h.db")
	}

	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveInterface(ctx, out, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serveInterface returned %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("the interface did not stop within thirty seconds of being told to")
		}
	})

	waitFor(t, "the interface to say where to open a browser", func() bool {
		return strings.Contains(out.String(), "http://")
	})
	return "http://" + listeningAt(t, out.String())
}

// startAJob fills the form in and presses start, without following the answer.
func startAJob(t *testing.T, at string) *http.Response {
	t.Helper()
	form := url.Values{
		"name": {"from the browser"}, "queries": {"golang channels"},
		"pages": {"1"}, "do": {"start"},
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.PostForm(at+"/new", form)
	if err != nil {
		t.Fatalf("pressing start: %v", err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func TestServe_TakesAJobFromTheFormWhenItHasSomethingToRunItOn(t *testing.T) {
	// The whole of this milestone is a job started in a browser. An interface
	// wired to a history and to nothing else serves every page and refuses the
	// one press the milestone is about, and every page still looks right.
	//
	// What stands where the identities go answers the control API and nothing
	// else, so the job started here reaches no network. That it fails is beside
	// the point: what is asked is whether the press was taken at all.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	t.Setenv(envControlURL, fake.URL())
	t.Setenv(envAPIKey, fake.Key())
	t.Setenv(envProxyList, "")

	res := startAJob(t, servedAt(t, serveOptions{Threads: 1, Ports: 1}))
	if res.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("pressing start came back %d, want 303:\n%s", res.StatusCode, body)
	}
	if got := res.Header.Get("Location"); !strings.HasPrefix(got, "/job/") {
		t.Errorf("start sent the browser to %q, want a job's own page", got)
	}
}

func TestServe_ShowsTheHistoryAndSaysSoWhenItCannotRunAnything(t *testing.T) {
	// A machine with no key is a machine somebody is reading a history on.
	// Refusing to start there takes away the half that needs nothing opened,
	// and a browser pointed at a dead port explains none of it.
	//
	// The control address is pointed at the stand-in as well as the key being
	// emptied. Whichever of the two the command notices first, what it notices
	// is on this machine and answers only the control API, so this test cannot
	// start reaching for something real if the order is ever changed.
	t.Setenv(envControlURL, fakebt.New(t).URL())
	t.Setenv(envAPIKey, "")
	t.Setenv(envProxyList, "")

	res := startAJob(t, servedAt(t, serveOptions{Threads: 1, Ports: 1}))
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the form came back %d, want the page again with its complaint", res.StatusCode)
	}
	if !strings.Contains(string(body), web.LangEN.T("form.norunner")) {
		t.Errorf("the page does not say why it cannot run the job:\n%s", body)
	}
}

// answered is one reply: what it came back as, and what it said.
type answered struct {
	code int
	kind string
	body string
}

// ask sends a plain GET, carrying a key when one is given.
func ask(t *testing.T, at, secret string) answered {
	t.Helper()
	return send(t, http.MethodGet, at, secret, "")
}

// send makes one request, with a key when one is given and a body when there is
// one, and hands back what came of it.
func send(t *testing.T, method, at, secret, body string) answered {
	t.Helper()
	var in io.Reader
	if body != "" {
		in = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, at, in)
	if err != nil {
		t.Fatalf("building a request for %s: %v", at, err)
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, at, err)
	}
	defer func() { _ = res.Body.Close() }()
	said, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the answer to %s: %v", at, err)
	}
	return answered{code: res.StatusCode, kind: res.Header.Get("Content-Type"), body: string(said)}
}

// json reports whether this is an answer the programmable interface wrote:
// JSON, said to be JSON, and carrying a sentence in the field every refusal
// there carries one in.
//
// The code alone says nothing. A page refusing in HTML carries the same number,
// and a program parsing that finds no field it knows.
func (a answered) json() bool {
	if !strings.HasPrefix(a.kind, "application/json") {
		return false
	}
	var said struct {
		Error string `json:"error"`
	}
	return json.Unmarshal([]byte(a.body), &said) == nil && said.Error != ""
}

// keyIn issues a key in a history of its own and hands back the secret and the
// file it was written into, so the interface can be started on a history a
// program is already entitled to read.
func keyIn(t *testing.T) (secret, db string) {
	t.Helper()
	db = filepath.Join(t.TempDir(), "h.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("opening a history to put a key in: %v", err)
	}
	secret, _, err = st.CreateKey(context.Background(), "a test")
	if err != nil {
		t.Fatalf("issuing a key: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("closing the history the key went into: %v", err)
	}
	return secret, db
}

func TestServe_RefusesAProgramWithNoKeyInTheShapeAProgramReads(t *testing.T) {
	// This is what says the programmable interface is mounted, and mounted where
	// it is documented. A 404 would mean it is not there at all, and a page would
	// mean the pages answered in its place. Both leave a server that looks
	// started to whoever started it and answers nothing a program can use.
	t.Setenv(envAPIKey, "")
	at := servedAt(t, serveOptions{})

	for _, path := range []string{"/api/v1/jobs", "/api/v1/history", "/search?q=anything"} {
		got := ask(t, at+path, "")
		if got.code != http.StatusUnauthorized {
			t.Errorf("GET %s came back %d, want 401:\n%s", path, got.code, got.body)
			continue
		}
		if !got.json() {
			t.Errorf("GET %s refused as %q rather than as this interface refuses:\n%s",
				path, got.kind, got.body)
		}
	}
}

func TestServe_AnswersAnAddressNobodyServesUnderTheProgramsPrefixAsAProgram(t *testing.T) {
	// Two shapes of refusal on one server is the point of the programmable
	// interface having a catch-all of its own. A program that mistyped an address
	// should read the one shape it parses every refusal in, rather than a page
	// written for somebody looking at a screen.
	t.Setenv(envAPIKey, "")
	at := servedAt(t, serveOptions{})

	for _, path := range []string{"/api/nothing-here", "/api/v1/nothing-here"} {
		got := ask(t, at+path, "")
		if got.code != http.StatusNotFound {
			t.Errorf("GET %s came back %d, want 404:\n%s", path, got.code, got.body)
		}
		if !got.json() {
			t.Errorf("GET %s was answered as %q rather than by the programmable interface:\n%s",
				path, got.kind, got.body)
		}
	}
}

func TestServe_LeavesTheJobPagesOwnAddressesWithThePages(t *testing.T) {
	// The poll behind the job page and the two buttons on it have sat under /api/
	// since before anything programmable did. Mounting the programmable interface
	// on that prefix without saying so takes them, and the page goes on asking an
	// address that now refuses it.
	t.Setenv(envAPIKey, "")
	at := servedAt(t, serveOptions{})

	for _, path := range browserPolls {
		got := ask(t, at+path+"?job=4242", "")
		if got.json() {
			t.Errorf("GET %s was answered by the programmable interface:\n%s", path, got.body)
		}
	}
}

func TestServe_TellsAProgramItHasNoQueueRatherThanFallingOverWithoutOne(t *testing.T) {
	// A machine with no key opens no ports and builds no queue, and it is still a
	// machine somebody reads a history on. The programmable interface asks
	// whether it has a queue at all, and a queue that does not exist, handed over
	// all the same, answers that question yes: the first job anybody sets up
	// takes the process down instead of being told plainly that nothing here can
	// run it.
	t.Setenv(envAPIKey, "")
	secret, db := keyIn(t)
	at := servedAt(t, serveOptions{DB: db})

	got := send(t, http.MethodPost, at+"/api/v1/jobs", secret,
		`{"name":"a job","queries":["one"],"pages":1}`)
	if got.code != http.StatusServiceUnavailable {
		t.Errorf("setting up a job on an interface with no queue came back %d, want 503:\n%s",
			got.code, got.body)
	}
	if !got.json() {
		t.Errorf("the refusal came back as %q rather than as this interface refuses:\n%s",
			got.kind, got.body)
	}
}

func TestServe_HandsTheSearchInsideARequestTheIdentitiesTheJobsRunOn(t *testing.T) {
	// The address somebody else's program already calls is the whole reason this
	// interface exists, and what it needs is identities rather than the queue: a
	// search waiting behind a job of ten thousand queries is a socket held open
	// for an hour. An interface built without them serves that address, refuses
	// every request on it with one sentence, and from the outside looks exactly
	// like one that works.
	//
	// What stands where the identities go answers the control API and nothing
	// else, so this search reaches no network and comes back with no results.
	// That it fails is beside the point: what is asked is whether it was tried.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	t.Setenv(envControlURL, fake.URL())
	t.Setenv(envAPIKey, fake.Key())
	t.Setenv(envProxyList, "")

	secret, db := keyIn(t)
	at := servedAt(t, serveOptions{DB: db, Threads: 1, Ports: 1})

	// The search is hung up on rather than waited out. Reaching an identity that
	// answers takes as long as it takes, and a test that sat through it would be
	// timing this machine's ports. A second is long past the instant a refusal
	// comes back in, so the only way to still be waiting is to be searching.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, at+"/search?q=anything", nil)
	if err != nil {
		t.Fatalf("building the search: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = res.Body.Close() }()
	said, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the answer to the search: %v", err)
	}
	if res.StatusCode == http.StatusNotFound {
		t.Fatalf("nothing serves the address somebody else's program calls:\n%s", said)
	}
	if strings.Contains(string(said), "without identities") {
		t.Errorf("the search was never taken to the identities the jobs run on: %d\n%s",
			res.StatusCode, said)
	}
}

func TestServe_SetsAJobRunningFromAProgramOnTheQueueTheBrowserWatches(t *testing.T) {
	// One history and one queue is the whole reason this command builds both
	// interfaces itself. A second history lets a program set up a job the browser
	// never lists; a second queue runs that job beside the one the operator is
	// watching, on the same identities, which is neither what was measured nor
	// what anybody asked for.
	//
	// What stands where the identities go answers the control API and nothing
	// else, so the job set going here reaches no network. That it will fail is
	// beside the point: what is asked is whose history it landed in and whose
	// queue took it.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	t.Setenv(envControlURL, fake.URL())
	t.Setenv(envAPIKey, fake.Key())
	t.Setenv(envProxyList, "")

	secret, db := keyIn(t)
	at := servedAt(t, serveOptions{DB: db, Threads: 1, Ports: 1})

	const name = "set up by a program"
	res := send(t, http.MethodPost, at+"/api/v1/jobs", secret,
		`{"name":"`+name+`","queries":["one","two","three"],"pages":1}`)
	if res.code != http.StatusCreated {
		t.Fatalf("setting up a job came back %d, want 201:\n%s", res.code, res.body)
	}
	var made struct {
		Job struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal([]byte(res.body), &made); err != nil {
		t.Fatalf("reading back the job that was set up: %v\n%s", err, res.body)
	}
	if made.Job.ID == 0 {
		t.Fatal("the job that was set up came back without a number to find it by")
	}

	if listed := ask(t, at+"/", ""); !strings.Contains(listed.body, name) {
		t.Errorf("the job list does not name the job a program set up:\n%s", listed.body)
	}

	id := strconv.FormatInt(made.Job.ID, 10)
	waitFor(t, "the job page's own poll to place the job in the queue it watches", func() bool {
		got := ask(t, at+"/api/progress?job="+id, "")
		if got.code != http.StatusOK {
			return false
		}
		var how struct {
			Running bool `json:"running"`
			Queued  bool `json:"queued"`
		}
		if err := json.Unmarshal([]byte(got.body), &how); err != nil {
			return false
		}
		return how.Running || how.Queued
	})
}

func TestServeLog_DoesNotSayWhereOnThisMachineTheHistoryIsKept(t *testing.T) {
	// A log line is printed once and read for years. Which file could not be
	// read is the answer; the directories above it say where somebody keeps
	// their things, and an error quotes the path in whatever shape it likes.
	//
	// The directory is looked for by name rather than as a whole path: every
	// layer between the error and the line — the quoting in the error, the
	// quoting in the handler — doubles the separators again, so a whole path is
	// a substring of nothing while every name in it is still there to read.
	const marker = "where-nobody-should-look"
	db := filepath.Join(t.TempDir(), marker, "h.db")

	var out bytes.Buffer
	serveOptions{DB: db}.logger(&out).Error("a page could not be built",
		"error", fmt.Errorf("store: open %q: locked", db))

	got := out.String()
	if strings.Contains(got, marker) {
		t.Errorf("the log says which directory the history is kept in:\n%s", got)
	}
	if !strings.Contains(got, "h.db") {
		t.Errorf("the log no longer says which file it could not read:\n%s", got)
	}
}

func TestServeLog_SaysWhenEachLineWasWritten(t *testing.T) {
	// A server's log is read long after the fact and beside other logs. A line
	// with no time on it cannot be placed against anything that happened.
	var out bytes.Buffer
	serveOptions{}.logger(&out).Error("a page could not be built")

	if !strings.Contains(out.String(), "time=") {
		t.Errorf("the line carries no time:\n%s", out.String())
	}
}

func TestUsage_ListsEveryFlagTheServeCommandTakes(t *testing.T) {
	// Help that has drifted from the flags is the same defect as documentation
	// that is wrong: it is read instead of the code, and it is believed.
	var opts serveOptions
	fs := serveFlags(&opts)

	if !strings.Contains(usageText, "gserp serve") {
		t.Error("the help does not list the serve command")
	}
	fs.VisitAll(func(f *flag.Flag) {
		if !strings.Contains(usageText, "--"+f.Name) {
			t.Errorf("the help does not mention --%s", f.Name)
		}
	})
}
