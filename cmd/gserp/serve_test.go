// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
func servedAt(t *testing.T, opts serveOptions) string {
	t.Helper()
	opts.Addr = "127.0.0.1:0"
	opts.DB = filepath.Join(t.TempDir(), "h.db")

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
		ControlURL: fake.URL(), APIKey: fake.Key(), Threads: 1, Ports: 1,
	})

	st, err := store.Open(opts.DB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var out bytes.Buffer
	sup := opts.jobs(t.Context(), &out, st)
	t.Cleanup(func() { _ = sup.Close() })

	if len(fake.OpenPorts()) == 0 {
		t.Errorf("nothing was opened for a connection that was saved in the browser:\n%s", out.String())
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

func TestServe_RunsOnWhatWasSetInTheBrowserWhenTheCommandLineSaysNothing(t *testing.T) {
	// Settings are set in a browser and read by a process that starts later.
	// Ones that did not outlive the restart would have to be set again every
	// time, and nothing on the screen would say so.
	opts := configured(t, settings.Settings{Threads: 7, Ports: 9})

	threads, ports := opts.runOn(opts.saved(io.Discard))
	if threads != 7 || ports != 9 {
		t.Errorf("threads=%d ports=%d, want the 7 and 9 that were saved", threads, ports)
	}
}

func TestServe_ANumberOnTheCommandLineWinsOverTheSavedOne(t *testing.T) {
	// Somebody who typed a number meant it for this run. A saved setting that
	// overrode it would make the flag do nothing on exactly the machine where
	// somebody had a reason to reach for it.
	opts := configured(t, settings.Settings{Threads: 7, Ports: 9})
	opts.Threads, opts.Ports = 1, 2

	threads, ports := opts.runOn(opts.saved(io.Discard))
	if threads != 1 || ports != 2 {
		t.Errorf("threads=%d ports=%d, want the 1 and 2 that were typed", threads, ports)
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
