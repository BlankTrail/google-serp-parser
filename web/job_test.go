// SPDX-License-Identifier: MIT

package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/export"
	"github.com/blanktrail/google-serp-parser/google"
	"github.com/blanktrail/google-serp-parser/store"
)

// heldServer is a server whose one job can be held in the middle of itself.
//
// A job at rest looks the same however the page was built, so every question
// about a running job — which buttons it offers, whether the page keeps
// watching it — has to be asked while the job is moving.
func heldServer(t *testing.T) (*Server, *Supervisor, *heldEngine) {
	t.Helper()
	st := testStore(t)
	eng := &heldEngine{hold: make(chan struct{})}
	v := newSupervisor(st, eng)
	t.Cleanup(func() { _ = v.Close() })

	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, v, eng
}

// shown reads what the page put in one of the cells its script keeps up to
// date.
//
// It finds the cell and reads what is in it, rather than searching the page for
// the number: a page holding the digit 4 anywhere at all would satisfy a search
// for the number alone, and every page holds a handful of digits.
func shown(t *testing.T, body, id string) string {
	t.Helper()
	anchor := `id="` + id + `">`
	at := strings.Index(body, anchor)
	if at < 0 {
		t.Fatalf("the page has no cell %q:\n%s", id, body)
	}
	rest := body[at+len(anchor):]
	end := strings.IndexByte(rest, '<')
	if end < 0 {
		t.Fatalf("the cell %q is never closed:\n%s", id, body)
	}
	return strings.TrimSpace(rest[:end])
}

// tagsOf is every tag of one kind the page carries, each cut off at its own
// closing bracket, so a test can ask what a single tag says rather than whether
// the whole page mentions it somewhere.
func tagsOf(body, name string) []string {
	var tags []string
	for _, after := range strings.Split(body, "<"+name)[1:] {
		tag, _, _ := strings.Cut(after, ">")
		tags = append(tags, tag)
	}
	return tags
}

func TestJobPage_ShowsEveryCountBeforeALineOfScriptHasRun(t *testing.T) {
	// The page is drawn on the server, so the numbers are on it before anything
	// has run in the browser, and they stay there if nothing ever does. Nothing
	// in this test executes a script, which is the whole point of it.
	//
	// The counts are compared against the store rather than against the numbers
	// typed into the fixture, so a page reading one count and calculating the
	// rest is caught here.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 9, 4, 2)

	rec := get(t, s, jobPath(id))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET the job gave %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "nightly") {
		t.Errorf("the page does not name the job it is about:\n%s", body)
	}

	sum, err := s.store.Progress(t.Context(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	for id, want := range map[string]int{
		"count-total": sum.Total, "count-done": sum.Done,
		"count-failed": sum.Failed, "count-pending": sum.Pending,
	} {
		if got := shown(t, body, id); got != strconv.Itoa(want) {
			t.Errorf("the page shows %s = %q, and the store says %d", id, got, want)
		}
	}
}

func TestJobPage_AnswersNotFoundForAJobThatIsNotThere(t *testing.T) {
	// A page of zeroes for a job that does not exist reads as a job that found
	// nothing, which is a different and much worse answer.
	s := testServer(t)
	for _, at := range []string{"/job/4242", "/job/nonsense"} {
		if code := get(t, s, at).Code; code != http.StatusNotFound {
			t.Errorf("GET %s gave %d, want 404", at, code)
		}
	}
}

func TestJobPage_OffersToStopOnlyTheJobThatIsRunning(t *testing.T) {
	// The supervisor is what knows the difference between a job that is running
	// and one abandoned half way, and the two are offered opposite buttons. A
	// page that guessed from the counts alone would offer to carry on with a job
	// that is already carrying on.
	s, v, eng := heldServer(t)
	id := enqueue(t, v, "nightly", "a", "b", "c")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})

	running := get(t, s, jobPath(id)).Body.String()
	if !strings.Contains(running, LangEN.T("job.stop")) {
		t.Errorf("a running job is not offered a stop:\n%s", running)
	}
	if strings.Contains(running, LangEN.T("job.resume")) {
		t.Error("a running job is offered a resume, which would queue it a second time")
	}
	if !strings.Contains(running, LangEN.T("job.state.running")) {
		t.Error("the page does not say the job is running")
	}

	eng.let(t, 1)
	waitUntil(t, "one query is recorded", func() bool {
		return progress(t, s.store, id).Done == 1
	})
	if err := v.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitUntil(t, "the job has stopped", func() bool { _, ok := v.Running(); return !ok })

	stopped := get(t, s, jobPath(id)).Body.String()
	if !strings.Contains(stopped, LangEN.T("job.resume")) {
		t.Errorf("a job left part way is not offered a resume:\n%s", stopped)
	}
	if strings.Contains(stopped, LangEN.T("job.stop")) {
		t.Error("a job that is not running is offered a stop")
	}
}

func TestJobPage_OffersNeitherButtonOnAServerThatCannotRunAnything(t *testing.T) {
	// A server reading a history on another machine has nothing to press these
	// against, and a button that cannot work is a button somebody presses twice
	// before believing it.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 4, 1, 0)

	body := get(t, s, jobPath(id)).Body.String()
	for _, key := range []string{"job.stop", "job.resume"} {
		if strings.Contains(body, LangEN.T(key)) {
			t.Errorf("a server with nothing to run jobs still offers %s", key)
		}
	}
}

func TestJobPage_StopsItselfWithAFormAndNoScript(t *testing.T) {
	// The buttons have to work with scripting switched off, so each is a form
	// that sends itself. They are read as whole tags rather than as the address
	// they carry: a button pointing at the right place and doing nothing at all
	// until a script picks it up would satisfy a search for the address alone.
	s, v, _ := heldServer(t)
	id := enqueue(t, v, "nightly", "a", "b")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})
	body := get(t, s, jobPath(id)).Body.String()

	var found bool
	for _, form := range strings.Split(body, "<form")[1:] {
		form, _, _ = strings.Cut(form, "</form>")
		head, _, _ := strings.Cut(form, ">")
		if !strings.Contains(head, `action="/api/stop"`) {
			continue
		}
		found = true
		if !strings.Contains(head, `method="post"`) {
			t.Errorf("the stop is sent by something other than a post: <form%s>", head)
		}
		if !strings.Contains(form, `value="`+strconv.FormatInt(id, 10)+`"`) {
			t.Errorf("the stop does not carry the job it is aimed at:\n%s", form)
		}
		var sends bool
		for _, button := range tagsOf(form, "button") {
			if strings.Contains(button, `type="submit"`) {
				sends = true
			}
		}
		if !sends {
			t.Errorf("nothing in the stop form sends it:\n%s", form)
		}
	}
	if !found {
		t.Errorf("the page has no form that stops the job:\n%s", body)
	}
}

func TestJobPage_AsksTheBrowserToWatchOnlyWhileSomethingIsStillComing(t *testing.T) {
	// A page left open overnight on a job nothing is running would otherwise
	// knock every two seconds until morning, and every answer would say what the
	// last one said.
	s, v, eng := heldServer(t)
	id := enqueue(t, v, "nightly", "a", "b")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})
	if body := get(t, s, jobPath(id)).Body.String(); !strings.Contains(body, "data-poll") {
		t.Errorf("the page of a running job asks the browser for nothing more:\n%s", body)
	}

	eng.let(t, 1)
	waitUntil(t, "one query is recorded", func() bool {
		return progress(t, s.store, id).Done == 1
	})
	if err := v.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitUntil(t, "the job has stopped", func() bool { _, ok := v.Running(); return !ok })
	if body := get(t, s, jobPath(id)).Body.String(); strings.Contains(body, "data-poll") {
		t.Errorf("the page of an abandoned job keeps asking for more:\n%s", body)
	}

	if err := v.Resume(id); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	eng.let(t, 1)
	waitUntil(t, "the job is done", func() bool { return progress(t, s.store, id).Finished })
	if body := get(t, s, jobPath(id)).Body.String(); strings.Contains(body, "data-poll") {
		t.Errorf("the page of a finished job keeps asking for more:\n%s", body)
	}
}

func TestJobPage_ShowsTheResultsTheJobCaptured(t *testing.T) {
	// A job page that shows counts and nothing else says how much was done and
	// nothing about what came of it.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 3, 2, 0)

	body := get(t, s, jobPath(id)).Body.String()
	for _, want := range []string{"query 1", "https://first.test/1", "second.test at 2"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, LangEN.T("job.results.capped")) {
		t.Error("a job of six rows was reported as too long to show")
	}
}

func TestJobPage_SaysSoRatherThanDrawingAMillionRows(t *testing.T) {
	// A job of ten thousand queries holds a million rows. Drawing them costs the
	// server the whole job and hands the reader a page no browser will lay out,
	// and the export is where the whole of it already is.
	s := testServer(t)
	id := seedJob(t, s, "wide", 2, 1, 0)
	fill(t, s, id, 1, rowsShown+50, 10)

	body := get(t, s, jobPath(id)).Body.String()
	// One row per result and one for the head of the table.
	if got := strings.Count(body, "<tr"); got != rowsShown+1 {
		t.Errorf("the page drew %d rows, want the %d it shows and a heading", got, rowsShown)
	}
	if !strings.Contains(body, LangEN.T("job.results.capped")) {
		t.Errorf("the page shows part of the results and does not say so:\n%s", body)
	}
}

func TestJobPage_SaysSoWhenNothingHasBeenCapturedYet(t *testing.T) {
	// An empty table reads as a broken page; a sentence reads as an answer.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 3, 0, 0)

	body := get(t, s, jobPath(id)).Body.String()
	if strings.Contains(body, "<table") {
		t.Error("a job that has captured nothing was shown an empty table")
	}
	if !strings.Contains(body, LangEN.T("job.results.none")) {
		t.Errorf("the page does not say that nothing has been captured:\n%s", body)
	}
}

func TestJobPage_OffersTheJobInEveryFormatTheExportWrites(t *testing.T) {
	// The whole of a job leaves through these links, and a page that offers only
	// what somebody remembered to type is a format nobody can reach.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 2, 2, 0)

	body := get(t, s, jobPath(id)).Body.String()
	for _, format := range export.Formats() {
		// The two halves are looked for apart because the ampersand joining them
		// in a link is written as an entity, and a search for the raw query
		// string would fail on a link that is perfectly correct.
		if !strings.Contains(body, "/export?job="+strconv.FormatInt(id, 10)) || !strings.Contains(body, "format="+format) {
			t.Errorf("the page does not offer the job as %s:\n%s", format, body)
		}
	}
}

func TestJobPage_LoadsItsOneScriptFromThisServerAndAfterThePage(t *testing.T) {
	// The script improves a page that already works, so it waits for the page
	// rather than holding it up, and it comes out of this binary. A page that
	// fetched it from somewhere else would put a stranger between a reader and
	// their own records, and would show nothing at all on a machine with no way
	// out to the network.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 2, 1, 0)

	tags := tagsOf(get(t, s, jobPath(id)).Body.String(), "script")
	if len(tags) != 1 {
		t.Fatalf("the page carries %d scripts, want the one this program ships", len(tags))
	}
	if !strings.Contains(tags[0], `src="/assets/app.js"`) {
		t.Errorf("the script is not the one in this binary: <script%s>", tags[0])
	}
	if !strings.Contains(tags[0], "defer") {
		t.Errorf("the script is loaded ahead of the page it improves: <script%s>", tags[0])
	}
}

func TestScript_IsServedFromThisBinaryAndReachesNowhereElse(t *testing.T) {
	// The script ships inside the binary like every other asset. An address in
	// it would be a machine this program has no business talking to, and a page
	// that quietly failed without one.
	rec := get(t, testServer(t), "/assets/app.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.js gave %d, want 200", rec.Code)
	}
	if kind := rec.Header().Get("Content-Type"); !strings.HasPrefix(kind, "text/javascript") {
		t.Errorf("Content-Type=%q, want a script", kind)
	}
	script := rec.Body.String()
	if strings.TrimSpace(script) == "" {
		t.Fatal("the script came back empty")
	}
	for _, reach := range []string{"http://", "https://", "//cdn", "import "} {
		if strings.Contains(script, reach) {
			t.Errorf("the script carries %q, so the page depends on something else", reach)
		}
	}
}

func TestJobList_LinksEveryJobToItsOwnPage(t *testing.T) {
	// A job page nothing links to is a page reachable only by typing an address,
	// and the list is where anybody looking for a job starts.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 2, 1, 0)

	if body := get(t, s, jobsAt).Body.String(); !strings.Contains(body, `href="`+jobPath(id)+`"`) {
		t.Errorf("the list does not link to the job it names:\n%s", body)
	}
}

func TestJobPage_ShowsNoBareKeyWhereAPhraseBelongs(t *testing.T) {
	// A key on the page is a phrase that was never looked up. It lands on
	// whichever phrase nobody wrote a test about, so this one is over all of
	// them, in both languages, on a job with results, a job with none, a job
	// that has finished, and both states of the other kind of job.
	s := testServer(t)
	full := seedJob(t, s, "nightly", 3, 2, 1)
	empty := seedJob(t, s, "morning", 3, 0, 0)
	done := seedJob(t, s, "evening", 2, 2, 0)
	if err := s.store.FinishJob(t.Context(), done); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}
	// An index job draws the other half of this page, so the phrases on it are
	// reached by nothing above.
	checked := seedIndexJob(t, s, "addresses",
		[]string{"example.com/a", "example.com/gone"},
		map[string]bool{"example.com/a": true})
	unchecked, err := s.store.CreateJob(t.Context(),
		store.JobSpec{Name: "waiting", Kind: store.KindIndex, Pages: 1}, []string{"example.com/a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	for _, id := range []int64{full, empty, done, checked, unchecked} {
		for _, l := range Languages() {
			body := get(t, s, jobPath(id)+"?lang="+string(l)).Body.String()
			for key := range catalogue[l] {
				if strings.Contains(body, key) {
					t.Errorf("the %s page of job %d shows the key %q where its text belongs", l, id, key)
				}
			}
		}
	}
}

// seedIndexJob writes an index job whose addresses have been checked: the ones
// named in held came back, the rest were checked and did not.
//
// The page it produces is what an index run really leaves behind — a query
// carrying results, and a query carrying a page with none — rather than a job
// with rows in it that a test called an index job.
func seedIndexJob(t *testing.T, s *Server, name string, addresses []string, held map[string]bool) int64 {
	t.Helper()
	ctx := t.Context()
	id, err := s.store.CreateJob(ctx,
		store.JobSpec{Name: name, Kind: store.KindIndex, Pages: 1}, addresses)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for i, address := range addresses {
		var found []google.Result
		if held[address] {
			found = []google.Result{{
				Title: address, URL: "https://" + address, Host: "example.com", Snippet: "held"}}
		}
		if err := s.store.Record(ctx, id, store.QueryOutcome{
			Ordinal: i,
			Pages:   []google.SERP{{Origin: "https://www.google.com", Results: found}},
		}); err != nil {
			t.Fatalf("recording the check of %q: %v", address, err)
		}
	}
	return id
}

func TestJobPage_AnswersForEveryAddressAnIndexJobChecked(t *testing.T) {
	// The whole point of the job is the answer per address, and it is stated as
	// what was found. A page that showed the captured results instead would show
	// the addresses Google held and simply omit the ones it did not, which is
	// the half of the answer the reader ran the job for.
	s := testServer(t)
	id := seedIndexJob(t, s, "addresses",
		[]string{"example.com/a", "example.com/gone"},
		map[string]bool{"example.com/a": true})

	body := get(t, s, jobPath(id)).Body.String()
	for _, want := range []string{"example.com/a", "example.com/gone",
		LangEN.T("job.verdict.held"), LangEN.T("job.verdict.absent")} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q:\n%s", want, body)
		}
	}
	// The address that was not found is on the page as an answer and not as a
	// row of results: an index job has no positions to draw.
	if strings.Contains(body, LangEN.T("history.rank")) {
		t.Errorf("an index job was drawn as a table of positions:\n%s", body)
	}
}

func TestJobPage_SaysWhatKindOfJobItIs(t *testing.T) {
	// The same page draws two jobs that ask Google different questions, and a
	// reader who cannot tell which they are looking at cannot read either.
	s := testServer(t)
	search := seedJob(t, s, "phrases", 2, 1, 0)
	index := seedIndexJob(t, s, "addresses", []string{"example.com/a"}, nil)

	for kind, id := range map[string]int64{"form.kind.search": search, "form.kind.index": index} {
		body := get(t, s, jobPath(id)).Body.String()
		if want := LangEN.T(kind); !strings.Contains(body, want) {
			t.Errorf("the page of job %d does not say it is %q:\n%s", id, want, body)
		}
	}
}

func TestJobPage_SaysSoWhenAnIndexJobHasCheckedNothingYet(t *testing.T) {
	// An index job with nothing checked has no verdicts, which is not the same
	// as a search job with nothing captured: the sentence has to name addresses,
	// or the reader is told their addresses produced no results.
	s := testServer(t)
	id, err := s.store.CreateJob(t.Context(),
		store.JobSpec{Name: "addresses", Kind: store.KindIndex, Pages: 1},
		[]string{"example.com/a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	body := get(t, s, jobPath(id)).Body.String()
	if strings.Contains(body, "<table") {
		t.Error("a job that has checked nothing was shown an empty table")
	}
	if !strings.Contains(body, LangEN.T("job.verdicts.none")) {
		t.Errorf("the page does not say that nothing has been checked:\n%s", body)
	}
	if strings.Contains(body, LangEN.T("job.results.none")) {
		t.Errorf("an index job was told its results are empty:\n%s", body)
	}
}

func TestJobPage_SaysSoRatherThanDrawingAMillionAddresses(t *testing.T) {
	// A list of addresses is as long as somebody's file. The cap is the same one
	// the results table has, and it is on the read as well as on the drawing.
	s := testServer(t)
	addresses := make([]string, rowsShown+50)
	for i := range addresses {
		addresses[i] = fmt.Sprintf("example.com/%d", i)
	}
	id := seedIndexJob(t, s, "wide", addresses, nil)

	body := get(t, s, jobPath(id)).Body.String()
	if got := strings.Count(body, "<tr"); got != rowsShown+1 {
		t.Errorf("the page drew %d rows, want the %d it shows and a heading", got, rowsShown)
	}
	if !strings.Contains(body, LangEN.T("job.results.capped")) {
		t.Errorf("the page shows part of the addresses and does not say so:\n%s", body)
	}
}
