// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
	"github.com/blanktrail/google-serp-parser/settings"
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
	// A machine with no key is a machine somebody is reading a history on, or
	// one nobody has set up yet. Refusing to start there takes away the half
	// that needs nothing opened, and a browser pointed at a dead port explains
	// none of it.
	//
	// What the page says is the sentence about a connection that has not been
	// set up rather than the one about a server built to read a history, because
	// this server can be set up from the page it is refusing on.
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
	if !strings.Contains(string(body), web.LangEN.T("form.notsetup")) {
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

	for _, path := range web.BrowserPolls() {
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

func TestServe_HandsTheSearchInsideARequestAPoolOfThisServersOwn(t *testing.T) {
	// The address somebody else's program already calls is the whole reason this
	// interface exists, and what it needs is identities rather than the queue: a
	// search waiting behind a job of ten thousand queries is a socket held open
	// for an hour. An interface built without them serves that address, refuses
	// every request on it with one sentence, and from the outside looks exactly
	// like one that works.
	//
	// The identities are the ones this machine keeps warm. A job raises its own
	// and takes them down when it ends, so an address borrowing those would
	// answer only while somebody happened to be running something — which is why
	// this address answers through the standing set and is off when none is
	// kept.
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
	// One identity kept warm, which is what this address answers through.
	saveSettingsBeside(t, db, settings.Settings{HotPorts: 1})
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
		t.Errorf("the search was never taken to the identities this server keeps: %d\n%s",
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

func TestServe_OpensThePortsAConnectionSavedInTheBrowserDescribes(t *testing.T) {
	// A connection is set up in a browser and read by a process started later.
	// One that had to be set up again after every restart is one nobody would
	// trust, and the interface would come up dead with nothing saying why.
	//
	// The environment is emptied so that what is opened here can have come from
	// the settings file alone.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	t.Setenv(envControlURL, "")
	t.Setenv(envAPIKey, "")
	t.Setenv(envProxyList, "")
	opts := configured(t, settings.Settings{
		ControlURL: fake.URL(), APIKey: fake.Key(), HotPorts: 1,
	})

	st, err := store.Open(opts.DB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var out bytes.Buffer
	sup, _ := opts.jobs(t.Context(), &out, st)
	t.Cleanup(func() { _ = sup.Close() })

	if len(fake.OpenPorts()) == 0 {
		t.Errorf("nothing was opened for a connection that was saved in the browser:\n%s", out.String())
	}
}

func TestServe_OpensItsPortsThroughTheAddressesTheSavedListNames(t *testing.T) {
	// A list named in the settings and read nowhere is the worst of the two
	// possible faults: the page says the work goes out through those addresses,
	// the work goes out through this machine's own, and nothing anywhere says
	// which. What proves the list was used is where the opened ports send their
	// traffic.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	t.Setenv(envControlURL, "")
	t.Setenv(envAPIKey, "")
	t.Setenv(envProxyList, "")

	list := filepath.Join(t.TempDir(), "list.txt")
	if err := os.WriteFile(list, []byte("5.5.5.5:1080\n"), 0o600); err != nil {
		t.Fatalf("writing the list: %v", err)
	}
	opts := configured(t, settings.Settings{
		ControlURL: fake.URL(), APIKey: fake.Key(), HotPorts: 1,
		Proxy: settings.ProxySource{Kind: "file", Location: list},
	})

	st, err := store.Open(opts.DB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var out bytes.Buffer
	sup, _ := opts.jobs(t.Context(), &out, st)
	t.Cleanup(func() { _ = sup.Close() })

	ports := fake.OpenPorts()
	if len(ports) == 0 {
		t.Fatalf("nothing was opened:\n%s", out.String())
	}
	if got, want := fake.UpstreamOf(ports[0]), "socks5://5.5.5.5:1080"; got != want {
		t.Errorf("the port sends through %q, want %q from the list the settings name", got, want)
	}
}

func TestServe_CarriesTheSavedSourceWholeToTheListItReads(t *testing.T) {
	// Every field here is a box somebody filled in on the settings page. One of
	// them read as something else — a file opened as an address, an interval
	// replaced by whatever this command would have chosen — is a box that looks
	// as though it works and does nothing, and the only symptom is a list that
	// never changes or never loads.
	for _, saved := range []settings.ProxySource{
		{Kind: "file", Location: "list.txt", Refresh: 15 * time.Minute},
		{Kind: "url", Location: "https://example.test/list", Refresh: time.Hour},
	} {
		src := listFrom(saved)
		if src.Kind != saved.Kind {
			t.Errorf("the list is read as %q, want %q", src.Kind, saved.Kind)
		}
		if src.Location != saved.Location {
			t.Errorf("the list is read from %q, want %q", src.Location, saved.Location)
		}
		if src.Refresh != saved.Refresh {
			t.Errorf("the list is read again every %v, want the %v that was saved", src.Refresh, saved.Refresh)
		}
	}
}

func TestServe_RaisesAJobsPoolAtTheSizeItIsAskedForRatherThanAtOneOfItsOwn(t *testing.T) {
	// This is the far end of a job naming its pool: where two numbers become
	// ports opened against the service. The queue asks for the job's own sizes,
	// or for this server's where the job named none, and what is asked for is
	// what has to be opened — a raiser with a size of its own would make every
	// number on the job page a number that changes nothing.
	//
	// Two pools are opened, of sizes that are not each other's, because a raiser
	// that ignored what it was asked for would open the same one twice.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	t.Setenv(envControlURL, "")
	t.Setenv(envAPIKey, "")
	t.Setenv(envProxyList, "")

	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	opts.Threads, opts.Ports = 3, 5
	saved, _ := opts.saved(io.Discard)
	raise := opts.raise(saved, false, &warmSet{})

	// A job that named no size reaches here already stood in for, so what this end
	// is asked for is the pair this server was started with.
	own, err := raise(t.Context(), opts.Ports, opts.Threads, blanktrail.DeviceDesktop, 0)
	if err != nil {
		t.Fatalf("raising the pool of a job that named no size: %v", err)
	}
	if got, want := len(fake.OpenPorts()), opts.Threads*opts.Ports; got != want {
		t.Errorf("%d ports were opened, want the %d this server was started with", got, want)
	}
	if err := own.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(fake.OpenPorts()); got != 0 {
		t.Fatalf("%d ports are still open after that pool was given up", got)
	}

	named, err := raise(t.Context(), 2, 1, blanktrail.DeviceDesktop, 0)
	if err != nil {
		t.Fatalf("raising the pool a job named: %v", err)
	}
	t.Cleanup(func() { _ = named.Close() })
	if got := len(fake.OpenPorts()); got != 2 {
		t.Errorf("%d ports were opened, want the 2 the job asked for", got)
	}
}

func TestServe_KeepsThePoolTheSearchUsesWhileEveryJobRaisesItsOwn(t *testing.T) {
	// The queue is handed a way of opening pools rather than a pool. The search
	// answered inside a request is handed one that is already open, and it has to
	// go on being open between jobs: an interface where half the addresses work
	// only while a job runs is one nobody can build against.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	t.Setenv(envControlURL, "")
	t.Setenv(envAPIKey, "")
	t.Setenv(envProxyList, "")
	opts := configured(t, settings.Settings{
		ControlURL: fake.URL(), APIKey: fake.Key(), HotPorts: 1,
	})

	st, err := store.Open(opts.DB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var out bytes.Buffer
	sup, pool := opts.jobs(t.Context(), &out, st)
	t.Cleanup(func() { _ = sup.Close() })

	if pool == nil {
		t.Fatalf("nothing was left for the search inside a request to go to:\n%s", out.String())
	}
	if !sup.CanRun() {
		t.Errorf("the queue was given no way of raising a pool for a job:\n%s", out.String())
	}
	if got := len(fake.OpenPorts()); got == 0 {
		t.Errorf("no identities are held with no job running, so the search has none:\n%s", out.String())
	}
}

// configured writes a settings file beside a history in a directory of this
// test's own, and returns the options that would find it. The history itself is
// never opened: what is asked here is which numbers a pool would be opened
// with, and opening one reaches for something this test must not reach for.
func configured(t *testing.T, s settings.Settings) serveOptions {
	t.Helper()
	dir := t.TempDir()
	if err := settings.Save(filepath.Join(dir, settingsName), s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return serveOptions{
		DB: filepath.Join(dir, "h.db"), Threads: defaultThreads, Ports: defaultPorts,
	}
}

func TestServe_TakesTheSizeOfAJobFromThisCommandAndNotFromTheSavedSettings(t *testing.T) {
	// How wide a job runs is the job's own now: it is saved with the job and
	// changed on the job's page. What this command was started with is the
	// answer for a job that named nothing, and the settings file — which decides
	// the connection every pool is opened through — has no say in it. A second
	// machine-wide answer standing behind the job's own is the kind nobody can
	// tell they are getting.
	opts := configured(t, settings.Settings{HotPorts: 3})
	opts.Threads, opts.Ports = 1, 2

	threads, ports := opts.runOn(opts.saved(io.Discard))
	if threads != 1 || ports != 2 {
		t.Errorf("threads=%d ports=%d, want the 1 and 2 this command was given", threads, ports)
	}
}

func TestServe_AMachineNobodyHasConfiguredRunsOnWhatItWasStartedWith(t *testing.T) {
	// There is no settings file until somebody opens the settings. Reading the
	// settings package's defaults there would open a pool of a size nobody on
	// this machine ever asked for.
	opts := serveOptions{
		DB: filepath.Join(t.TempDir(), "h.db"), Threads: defaultThreads, Ports: defaultPorts,
	}

	threads, ports := opts.runOn(opts.saved(io.Discard))
	if threads != defaultThreads || ports != defaultPorts {
		t.Errorf("threads=%d ports=%d, want this command's own %d and %d",
			threads, ports, defaultThreads, defaultPorts)
	}
}

func TestServe_SaysWhenTheSavedSettingsCannotBeRead(t *testing.T) {
	// It is the file somebody's connection is kept in. Running on the defaults
	// without a word looks like a program that has lost their settings.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, settingsName), []byte("{not settings"), 0o600); err != nil {
		t.Fatalf("writing the file: %v", err)
	}
	opts := serveOptions{
		DB: filepath.Join(dir, "h.db"), Threads: defaultThreads, Ports: defaultPorts,
	}

	var out bytes.Buffer
	if _, ok := opts.saved(&out); ok {
		t.Error("a file that cannot be read was taken as settings")
	}
	if !strings.Contains(out.String(), "could not be read") {
		t.Errorf("nothing says the settings could not be read:\n%s", out.String())
	}
	if strings.Contains(out.String(), dir) {
		t.Errorf("the complaint says where on this machine the file is:\n%s", out.String())
	}
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

func TestTranslate_SaysWhichLanguageArrivedAndWhatItIsShortOf(t *testing.T) {
	// A translation is read at startup and never mentioned again. Whoever put
	// the file there learns from these lines that it was read at all, and which
	// phrases are still going out in English — neither of which the browser can
	// show them, since a missing phrase renders as a finished one.
	dir := filepath.Join(t.TempDir(), "gserp-translations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("making the translations directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "de.json"),
		[]byte(`{"lang.name": "Deutsch", "jobs.title": "Aufträge"}`), 0o600); err != nil {
		t.Fatalf("writing the translation: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fr.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("writing the damaged translation: %v", err)
	}
	// Everything else in this binary answers in the languages the program was
	// built with, so the added one is put away again.
	t.Cleanup(func() { _, _, _ = web.LoadTranslations(t.TempDir()) })

	var out bytes.Buffer
	serveOptions{DB: "gserp.db"}.translate(&out, dir)

	said := out.String()
	for _, want := range []string{"answers in de", "fr.json", "the de translation is short of", "jobs.none"} {
		if !strings.Contains(said, want) {
			t.Errorf("the report does not say %q:\n%s", want, said)
		}
	}
	if strings.Contains(said, "jobs.title") {
		t.Errorf("a phrase the file does say is reported missing:\n%s", said)
	}
}

func TestServe_TellsTheCallerWhereItIsListeningOnceItIsBound(t *testing.T) {
	// A caller who asked for port zero has no other way to learn the port, and
	// the icon in the notification area sends a browser to exactly this address:
	// told too early it would send it to a socket nothing is on yet, and told
	// the address that was asked for rather than the one that was given, it
	// would send it somewhere else entirely.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	at := make(chan string, 1)
	opts := serveOptions{
		Addr: "127.0.0.1:0",
		DB:   filepath.Join(t.TempDir(), "h.db"),
		onListen: func(where string) {
			select {
			case at <- where:
			default:
			}
		},
	}
	done := make(chan error, 1)
	go func() { done <- serveInterface(ctx, io.Discard, opts) }()

	var where string
	select {
	case where = <-at:
	case err := <-done:
		t.Fatalf("the server ended before it said where it was listening: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the server never said where it was listening")
	}
	if _, port, err := net.SplitHostPort(where); err != nil || port == "0" {
		t.Fatalf("it says it is listening at %q, which is not a port the system gave", where)
	}

	// And it is listening there by the time it says so.
	res, err := http.Get("http://" + where + "/")
	if err != nil {
		t.Fatalf("nothing answers at the address it named: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("the address it named answered %d", res.StatusCode)
	}
	cancel()
	<-done
}

func TestRaise_GrowsTheStandingIdentitiesForAJobAndGivesBackOnlyTheGrowth(t *testing.T) {
	// The whole arrangement, end to end and in the one place it is assembled. Ten
	// identities are kept warm; a job of a hundred opens ninety more, runs on all
	// of them, and gives back the ninety — because giving back the ten would mean
	// the next job pays for warming them all over again, which is what keeping
	// them was for.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{
		ControlURL: fake.URL(), APIKey: fake.Key(), HotPorts: 10,
	})
	saved, _ := opts.saved(io.Discard)

	standing, err := opts.dial(t.Context(), saved, 1, 10, blanktrail.DeviceDesktop, 0)
	if err != nil {
		t.Fatalf("opening the standing identities: %v", err)
	}
	t.Cleanup(func() { _ = standing.Close() })
	if got := standing.KeepWarm(); got != 10 {
		t.Fatalf("%d identities were declared standing, want ten", got)
	}

	warm := &warmSet{pool: standing, device: blanktrail.DeviceDesktop}
	raise := opts.raise(saved, false, warm)
	pool, err := raise(t.Context(), 10, 10, blanktrail.DeviceDesktop, 0)
	if err != nil {
		t.Fatalf("raising a job of a hundred on ten standing: %v", err)
	}
	if pool != standing {
		t.Error("the job was handed a second pool rather than the standing one grown")
	}
	if got := len(fake.OpenPorts()); got != 100 {
		t.Errorf("%d ports are open, want the hundred the job asked for", got)
	}

	// What the job's end does. The queue reaches this through the engine it
	// wraps the pool in, and that engine's own rule — shrink a pool that has a
	// standing set, close one that has not — is checked where it lives.
	gone, err := pool.Shrink(t.Context())
	if err != nil {
		t.Fatalf("letting go of the job's identities: %v", err)
	}
	if gone != 90 {
		t.Errorf("the job gave back %d identities, want the ninety it opened", gone)
	}
	if got := len(fake.OpenPorts()); got != 10 {
		t.Errorf("%d ports are open after the job, want the ten that are kept warm", got)
	}
}

func TestRaise_LeavesTheStandingIdentitiesAloneForAJobOfTheOtherKind(t *testing.T) {
	// A phone's results are not a desktop's and an identity cannot be both, so a
	// job of the other kind opens its own from cold. Growing the standing set for
	// it would run the job on the wrong pages and file them under the right name.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{
		ControlURL: fake.URL(), APIKey: fake.Key(), HotPorts: 4,
	})
	saved, _ := opts.saved(io.Discard)

	standing, err := opts.dial(t.Context(), saved, 1, 4, blanktrail.DeviceDesktop, 0)
	if err != nil {
		t.Fatalf("opening the standing identities: %v", err)
	}
	t.Cleanup(func() { _ = standing.Close() })
	standing.KeepWarm()

	warm := &warmSet{pool: standing, device: blanktrail.DeviceDesktop}
	raise := opts.raise(saved, false, warm)
	own, err := raise(t.Context(), 2, 1, blanktrail.DeviceMobile, 0)
	if err != nil {
		t.Fatalf("raising a phone job beside the standing desktops: %v", err)
	}
	t.Cleanup(func() { _ = own.Close() })

	if own == standing {
		t.Fatal("a phone job was handed the desktop identities")
	}
	if got := standing.Stats().Ports; got != 4 {
		t.Errorf("the standing set is %d after a phone job was raised, want the four it was", got)
	}
	if got := own.Hot(); got != 0 {
		t.Errorf("the phone job's own pool holds %d standing ports, want none of its own", got)
	}
}

func TestRaise_TakesTheStandingIdentitiesAsTheyAreWhenAJobIsSmallerThanThey(t *testing.T) {
	// A job of two on a machine keeping ten warm opens nothing: it runs on what is
	// already there, which is the case this is all for.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{
		ControlURL: fake.URL(), APIKey: fake.Key(), HotPorts: 10,
	})
	saved, _ := opts.saved(io.Discard)

	standing, err := opts.dial(t.Context(), saved, 1, 10, blanktrail.DeviceDesktop, 0)
	if err != nil {
		t.Fatalf("opening the standing identities: %v", err)
	}
	t.Cleanup(func() { _ = standing.Close() })
	standing.KeepWarm()

	warm := &warmSet{pool: standing, device: blanktrail.DeviceDesktop}
	raise := opts.raise(saved, false, warm)
	if _, err := raise(t.Context(), 1, 2, blanktrail.DeviceDesktop, 0); err != nil {
		t.Fatalf("raising a job of two: %v", err)
	}
	if got := len(fake.OpenPorts()); got != 10 {
		t.Errorf("%d ports are open, want the ten that were already warm", got)
	}
}

func TestChecked_AsksOnceAMinuteRatherThanBeforeEveryPool(t *testing.T) {
	// The check is three seconds of asking the same four questions, and it ran
	// before every pool went up: a machine running ten jobs one after another
	// spent half a minute of somebody's waiting on answers that had not changed.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	client, err := blanktrail.NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	forgetTheCheck(t)

	var opts serveOptions
	before := len(fake.Requests())
	for range 5 {
		if _, err := opts.checked(t.Context(), client, 2); err != nil {
			t.Fatalf("checking the connection: %v", err)
		}
	}
	if got := len(fake.Requests()) - before; got == 0 {
		t.Fatal("the first check asked the service nothing at all")
	}
	first := len(fake.Requests()) - before

	// Four more checks over the same connection asked nothing more.
	if got := len(fake.Requests()) - before; got != first {
		t.Errorf("five checks asked %d questions, want the %d one check asks", got, first)
	}
}

func TestChecked_NeverRemembersARefusal(t *testing.T) {
	// A machine whose connection is broken has to be able to fix it and try
	// again at once. A refusal held for a minute is a minute of a page saying
	// the same wrong thing however many times it is pressed.
	fake := fakebt.New(t)
	// No CA: the check finds a real fault and refuses.
	client, err := blanktrail.NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	forgetTheCheck(t)

	var opts serveOptions
	if _, err := opts.checked(t.Context(), client, 2); err == nil {
		t.Fatal("a connection with no certificate was passed by the check")
	}
	// Put right, and asked again: the answer has to change now rather than in a
	// minute.
	fake.SetCA(testCAPEM)
	if _, err := opts.checked(t.Context(), client, 2); err != nil {
		t.Errorf("a connection that has been put right is still refused: %v", err)
	}
}

func TestChecked_AsksAgainForAPoolLargerThanTheOneItAnsweredAbout(t *testing.T) {
	// The check reports what a pool of that many ports would run into. A larger
	// pool answered from a smaller check would be told nothing about the ports it
	// added — which is the one thing the licence limits are about.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	client, err := blanktrail.NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	forgetTheCheck(t)

	var opts serveOptions
	if _, err := opts.checked(t.Context(), client, 2); err != nil {
		t.Fatalf("checking for two: %v", err)
	}
	asked := len(fake.Requests())
	if _, err := opts.checked(t.Context(), client, 200); err != nil {
		t.Fatalf("checking for two hundred: %v", err)
	}
	if len(fake.Requests()) == asked {
		t.Error("a pool a hundred times larger was passed on the smaller pool's check")
	}
}

// forgetTheCheck empties what the process remembers about the connection, so
// one test's check is never another's answer.
func forgetTheCheck(t *testing.T) {
	t.Helper()
	forget := func() {
		lastCheck.mu.Lock()
		lastCheck.at, lastCheck.ca, lastCheck.size = time.Time{}, nil, 0
		lastCheck.mu.Unlock()
	}
	forget()
	t.Cleanup(forget)
}

// saveSettingsBeside writes a settings file where a history's own server will
// read it, so a test can hand one to a server it starts by hand.
func saveSettingsBeside(t *testing.T, db string, s settings.Settings) {
	t.Helper()
	if err := settings.Save(filepath.Join(filepath.Dir(db), settingsName), s); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// benchKeeper stands where the history keeps the addresses found dead.
type benchKeeper struct {
	kept    map[string]time.Time
	written map[string]time.Time
	forgot  bool
}

func (b *benchKeeper) Rested(context.Context) (map[string]time.Time, error) {
	return b.kept, nil
}

func (b *benchKeeper) Rest(_ context.Context, key string, since time.Time) error {
	if b.written == nil {
		b.written = map[string]time.Time{}
	}
	b.written[key] = since
	return nil
}

func (b *benchKeeper) ForgetRestsBefore(context.Context, time.Time) error {
	b.forgot = true
	return nil
}

func TestRecall_TellsAFreshRotorWhatTheLastRunLearned(t *testing.T) {
	// A restart without this walks straight back into every address the run
	// before spent its time finding dead, and on a large cheap list that is most
	// of them.
	ups, bad := blanktrail.Parse("10.0.0.1:1080\n10.0.0.2:1080", "socks5")
	if len(bad) > 0 {
		t.Fatalf("Parse rejected %v", bad)
	}
	keeper := &benchKeeper{kept: map[string]time.Time{
		ups[0].Key(): time.Now().Add(-time.Minute),
	}}
	opts := serveOptions{Rests: keeper}
	rotor := blanktrail.NewStaticRotor(ups)

	opts.recall(t.Context(), rotor)

	if got := rotor.Benched(); got != 1 {
		t.Errorf("%d addresses rest after the record was read back, want the one in it", got)
	}
	if !keeper.forgot {
		t.Error("the stale records were not cleared, so the table grows for the life of the machine")
	}
}

func TestRemember_WritesDownAnAddressAsItIsFoundDead(t *testing.T) {
	// Written as it happens rather than gathered at shutdown: a program that is
	// killed never reaches a shutdown, and the run that has just spent an hour
	// learning which addresses are dead is the one worth not losing.
	keeper := &benchKeeper{}
	opts := serveOptions{Rests: keeper}
	at := time.Now()

	opts.remember("socks5|10.0.0.9:1080", at)

	if got, ok := keeper.written["socks5|10.0.0.9:1080"]; !ok || !got.Equal(at) {
		t.Errorf("the record holds %v, want the address at %v", keeper.written, at)
	}
}

func TestRemember_SaysNothingWhenThereIsNowhereToKeepIt(t *testing.T) {
	// A command exercised without a history has nowhere to write, and that is
	// not a fault worth a panic on every failed address.
	serveOptions{}.remember("socks5|10.0.0.9:1080", time.Now())
}

func TestWarmSet_LeavesAJobsIdentitiesAloneAndBringsTheNumberAboutAfterwards(t *testing.T) {
	// Lowering the number of identities this machine keeps used to close them
	// out from under a job that was running on them: ReduceTo leaves alone what
	// is leased at that instant and closes the rest, so a job raised on a
	// hundred went on with the thirty it happened to be holding — and nothing
	// grows a pool a second time, so it stayed there. Measured on a live run:
	// five and eight tenths queries a minute where a clean run of the same job
	// did seventy-seven.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{
		ControlURL: fake.URL(), APIKey: fake.Key(), HotPorts: 10,
	})
	saved, _ := opts.saved(io.Discard)

	standing, err := opts.dial(t.Context(), saved, 1, 10, blanktrail.DeviceDesktop, 0)
	if err != nil {
		t.Fatalf("opening the standing identities: %v", err)
	}
	t.Cleanup(func() { _ = standing.Close() })
	standing.KeepWarm()

	running := true
	warm := &warmSet{pool: standing, device: blanktrail.DeviceDesktop}
	warm.dial = func(ctx context.Context, want int, device string) (*blanktrail.Pool, error) {
		return opts.dial(ctx, saved, 1, want, device, 0)
	}
	warm.running = func() bool { return running }

	if err := warm.bring(t.Context(), 2, blanktrail.DeviceDesktop); err != nil {
		t.Fatalf("saving a smaller number while a job runs: %v", err)
	}
	if got := standing.Stats().Ports; got != 10 {
		t.Errorf("%d identities are open, want the ten the job is running on", got)
	}

	// The job lets go, and what was saved is brought about.
	running = false
	warm.mu.Lock()
	owed := warm.owed
	warm.owed = nil
	warm.mu.Unlock()
	if owed == nil {
		t.Fatal("the number saved while the job ran was not remembered")
	}
	if err := warm.bring(t.Context(), owed.want, owed.device); err != nil {
		t.Fatalf("bringing about what was owed: %v", err)
	}
	if got := standing.Stats().Ports; got != 2 {
		t.Errorf("%d identities are open after the job let go, want the two that were saved", got)
	}
}

func TestRaiseFor_PacesTheStandingIdentitiesByTheJobRatherThanByHowTheyWereOpened(t *testing.T) {
	// The standing set is opened once, as one thread's worth of however many
	// identities the machine keeps, and every job afterwards runs on that same
	// pool grown to its own size. The gap between two requests on one identity
	// was worked out at opening and never revisited, so a job inherited the
	// standing set's pace: ten identities opened for one thread came out at
	// thirty-five seconds, and a job of fifty threads on a hundred of them could
	// be handed a hundred divided by thirty-five identities a second — a hundred
	// and seventy-one requests a minute — however many identities it had and
	// whatever pause it asked for. Traced on a live run: the shortest gap
	// between two leases of one identity was 35.0 seconds across all hundred,
	// and threads spent sixty percent of their time queueing.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{
		ControlURL: fake.URL(), APIKey: fake.Key(), HotPorts: 10,
	})
	saved, _ := opts.saved(io.Discard)

	// Opened the way the standing set is opened: one thread, ten identities.
	standing, err := opts.dial(t.Context(), saved, 1, 10, blanktrail.DeviceDesktop, 0)
	if err != nil {
		t.Fatalf("opening the standing identities: %v", err)
	}
	t.Cleanup(func() { _ = standing.Close() })
	standing.KeepWarm()
	opened := standing.Cooldown()
	if opened <= 0 {
		t.Fatalf("the standing set was opened with no pace at all")
	}

	warm := &warmSet{pool: standing, device: blanktrail.DeviceDesktop}
	raise := opts.raise(saved, false, warm)

	// A job that names no pause wants none: nought is nought, and the identities
	// it was given are handed out as fast as they come free.
	if _, err := raise(t.Context(), 2, 50, blanktrail.DeviceDesktop, 0); err != nil {
		t.Fatalf("raising a job of fifty on two: %v", err)
	}
	if got := standing.Cooldown(); got != 0 {
		t.Errorf("the pool is paced at %s for a job that asked for no pause "+
			"(it was opened at %s)", got, opened)
	}
	// And no pause means no pause anywhere: a thread that sat out two to five
	// seconds after every query would be keeping one nobody asked for.
	if got := standing.NextDelay(); got != 0 {
		t.Errorf("a thread waits %s between queries on a job that asked for no pause", got)
	}

	// And a job that named a pause of its own is paced by that.
	if _, err := raise(t.Context(), 2, 50, blanktrail.DeviceDesktop, 3*time.Second); err != nil {
		t.Fatalf("raising a job that named its own pause: %v", err)
	}
	if got := standing.Cooldown(); got != 3*time.Second {
		t.Errorf("the pool is paced at %s, want the three seconds the job named", got)
	}
	if got := standing.NextDelay(); got != 3*time.Second {
		t.Errorf("a thread waits %s between queries, want the three seconds the job named", got)
	}
}

func TestListenOn_StaysOnThisMachineUnlessTheSettingsOpenItWithAPassword(t *testing.T) {
	// The switch alone does not open anything. An interface answering a network
	// with nothing to ask for is the settings, the queue and every result handed
	// to whoever is on it, so a switch turned on without a password leaves the
	// program where it was and says so.
	locked, err := settings.LockPassword("a good enough password")
	if err != nil {
		t.Fatalf("LockPassword: %v", err)
	}
	for _, tc := range []struct {
		name  string
		asked string
		saved settings.Settings
		want  string
		says  bool
	}{
		{"nothing asked, nothing set", defaultServeAddr, settings.Settings{}, defaultServeAddr, false},
		{"open, with a password", defaultServeAddr,
			settings.Settings{LANAccess: true, LANPassword: locked}, lanServeAddr, false},
		{"open, no password", defaultServeAddr,
			settings.Settings{LANAccess: true}, defaultServeAddr, true},
		{"a password, switch off", defaultServeAddr,
			settings.Settings{LANPassword: locked}, defaultServeAddr, false},
		{"the caller named one", "127.0.0.1:9999",
			settings.Settings{LANAccess: true, LANPassword: locked}, "127.0.0.1:9999", false},
		{"the caller named a network one", "0.0.0.0:1234", settings.Settings{}, "0.0.0.0:1234", false},
	} {
		var said strings.Builder
		if got := listenOn(tc.asked, tc.saved, &said); got != tc.want {
			t.Errorf("%s: listening on %q, want %q", tc.name, got, tc.want)
		}
		if spoke := said.Len() > 0; spoke != tc.says {
			t.Errorf("%s: said %q, want anything=%v", tc.name, said.String(), tc.says)
		}
	}
}

func TestLockFor_AsksForNothingWhileTheInterfaceIsOnThisMachine(t *testing.T) {
	// A password on loopback is a lock on a door the reader is already inside.
	locked, err := settings.LockPassword("a good enough password")
	if err != nil {
		t.Fatalf("LockPassword: %v", err)
	}
	if got := lockFor(settings.Settings{LANPassword: locked}); got != "" {
		t.Error("the pages ask for a password while answering this machine alone")
	}
	if got := lockFor(settings.Settings{LANAccess: true, LANPassword: locked}); got != locked {
		t.Error("the pages ask for nothing while answering the network")
	}
}

func TestDial_CarriesHowOftenAPortChangesIdentityIntoThePool(t *testing.T) {
	// The setting is worth nothing until it reaches the pool, and this is the
	// one place the two are joined. A pool built without it holds every identity
	// for as long as the job runs — which on a dozen gateways is a dozen
	// identities an origin comes to know.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{
		ControlURL: fake.URL(), APIKey: fake.Key(), RenewEvery: 10 * time.Minute,
	})
	saved, _ := opts.saved(io.Discard)

	pool, err := opts.dial(t.Context(), saved, 1, 2, blanktrail.DeviceDesktop, 0)
	if err != nil {
		t.Fatalf("opening the identities: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	if got := pool.RenewEvery(); got != 10*time.Minute {
		t.Errorf("the pool changes identity every %v, want the ten minutes that were saved", got)
	}
}
