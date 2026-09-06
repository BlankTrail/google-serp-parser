// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/export"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/store"
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
	// A job that has started and settled nothing is reaching its identities, and
	// the page says so: the two states are different things to be told, and a
	// job that spent its first minute on a challenge would otherwise read as one
	// that is answering.
	if got := shown(t, running, "state"); got != LangEN.T("job.state.starting") {
		t.Errorf("the page says the job is %q, and it has started without settling anything", got)
	}

	eng.let(t, 1)
	waitUntil(t, "one query is recorded", func() bool {
		return progress(t, s.store, id).Done == 1
	})
	// Now it is answering, and the state has to say the other thing. The word is
	// read out of the cell that holds it rather than looked for anywhere on the
	// page: "running" is an ordinary word, and a page mentioning it in a note
	// under some field would satisfy a search of the whole page while the state
	// said something else entirely — which is exactly what this test used to do.
	if got := shown(t, get(t, s, jobPath(id)).Body.String(), "state"); got != LangEN.T("job.state.running") {
		t.Errorf("the page says the job is %q after a query settled, want it running", got)
	}
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

func TestJobPage_ShowsTheAddressTheJobIsFetchingInSomethingCopyable(t *testing.T) {
	// Between two settled queries the only honest thing the counts can say is
	// that nothing has moved, and on a job taken a hundred pages deep that is a
	// hundred requests of silence. This is the line that says what is happening
	// this second.
	//
	// It is a box and not a sentence for two reasons, and both are checked: the
	// whole address has to be selectable in one press, and an address as long as
	// a query must not push the page sideways.
	s, v, _ := heldServer(t)
	id := enqueue(t, v, "nightly", "a", "b", "c")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})
	at := "https://www.google.com/search?gl=us&hl=en&q=golang+channels"
	v.asking.note(id, at)

	body := get(t, s, jobPath(id)).Body.String()
	// Read back the way a browser reads it: the address carries the characters a
	// page has to escape, and comparing the escaped text to the address would
	// fail on a page that is perfectly correct.
	tag := openingTag(t, body, `input id="asking"`)
	if !strings.Contains(html.UnescapeString(tag), at) {
		t.Errorf("the page does not carry the address being fetched: <%s>", tag)
	}
	if !strings.Contains(tag, "readonly") {
		t.Errorf("the address can be typed over, so a reader can lose it: <%s>", tag)
	}
	if !strings.Contains(tag, `class="address"`) {
		t.Errorf("the address box is drawn as an ordinary one, and a long address in an "+
			"ordinary one widens the page: <%s>", tag)
	}
}

func TestJobPage_LeavesOutTheAddressWhenThisJobIsNotTheOneRunning(t *testing.T) {
	// An address left on the screen of a job that has stopped reads as a job
	// still working, which is the one thing this line must never say.
	s, v, eng := heldServer(t)
	running := enqueue(t, v, "nightly", "a", "b", "c")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == running
	})
	v.asking.note(running, "https://www.google.com/search?q=one")

	other, err := v.st.CreateJob(t.Context(),
		store.JobSpec{Name: "another", Pages: 1}, []string{"z"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if body := get(t, s, jobPath(other)).Body.String(); strings.Contains(body, `id="asking"`) {
		t.Errorf("a job that is not running carries an address being fetched:\n%s", body)
	}

	// And the job itself once it is over: the address goes with the job, so its
	// own page stops claiming to be fetching something.
	eng.let(t, 3)
	waitUntil(t, "the job is over", func() bool { _, ok := v.Running(); return !ok })
	if body := get(t, s, jobPath(running)).Body.String(); strings.Contains(body, `id="asking"`) {
		t.Errorf("a job that has finished still shows an address being fetched:\n%s", body)
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
	if body := get(t, s, jobPath(id)).Body.String(); !strings.Contains(body, "data-refresh") {
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
	if body := get(t, s, jobPath(id)).Body.String(); strings.Contains(body, "data-refresh") {
		t.Errorf("the page of an abandoned job keeps asking for more:\n%s", body)
	}

	if err := v.Resume(id); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	eng.let(t, 1)
	waitUntil(t, "the job is done", func() bool { return progress(t, s.store, id).Finished })
	if body := get(t, s, jobPath(id)).Body.String(); strings.Contains(body, "data-refresh") {
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
	if strings.Contains(body, LangEN.T("job.results.sample")) {
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
	if !strings.Contains(body, LangEN.T("job.results.sample")) {
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
	// The same page draws three jobs that ask Google different questions, and a
	// reader who cannot tell which they are looking at cannot read any of them.
	s := testServer(t)
	search := seedJob(t, s, "phrases", 2, 1, 0)
	index := seedIndexJob(t, s, "addresses", []string{"example.com/a"}, nil)

	position := seedPositionJob(t, s, "places", "example.com", map[string]int{"iphone 13": 4})

	for kind, id := range map[string]int64{
		"form.kind.parse":    search,
		"form.kind.position": position,
		"form.kind.index":    index,
	} {
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
	if !strings.Contains(body, LangEN.T("job.results.sample")) {
		t.Errorf("the page shows part of the addresses and does not say so:\n%s", body)
	}
}

// filteredJob is a job that drops repeats and has already dropped some.
//
// Every query brings the same two addresses of one site, so under either filter
// there is something to drop and something to keep, and the number the page has
// to show is not the number of anything else on the page.
func filteredJob(t *testing.T, s *Server, by store.UniqueBy, queries int) int64 {
	t.Helper()
	list := make([]string, queries)
	for i := range list {
		list[i] = fmt.Sprintf("query %d", i+1)
	}
	ctx := t.Context()
	id, err := s.store.CreateJob(ctx, store.JobSpec{Name: "nightly", Pages: 1, UniqueBy: by}, list)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for i := range queries {
		err := s.store.Record(ctx, id, store.QueryOutcome{
			Ordinal: i,
			Pages: []google.SERP{{Origin: "https://www.google.com", Results: []google.Result{
				{Title: "one", URL: "https://example.com/one", Host: "example.com"},
				{Title: "two", URL: "https://example.com/two", Host: "example.com"},
			}}},
		})
		if err != nil {
			t.Fatalf("recording query %d: %v", i, err)
		}
	}
	return id
}

func TestJobPage_SaysHowManyRepeatsWereDroppedAndWhatCountedAsOne(t *testing.T) {
	// A reader looking at three results where they expected ten has to be told
	// that seven were repeats. The two go together: a count with no filter named
	// beside it reads as results lost to something nobody chose.
	s := testServer(t)
	id := filteredJob(t, s, store.UniqueHost, 3)

	body := get(t, s, jobPath(id)).Body.String()
	if got := shown(t, body, "count-dropped"); got != "5" {
		t.Errorf("the page shows %q dropped, want 5 — one of two results per query, and both of the last two queries", got)
	}
	if want := LangEN.T("form.unique.host"); !strings.Contains(body, want) {
		t.Errorf("the page does not say the job was set up as %q:\n%s", want, body)
	}
}

func TestJobPage_ShowsNoCountOfDroppedRepeatsForAJobThatKeepsEverything(t *testing.T) {
	// A figure reading nought is a number asking to be explained, and on a job
	// with no filter there is nothing to explain.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 3, 2, 0)

	body := get(t, s, jobPath(id)).Body.String()
	if strings.Contains(body, `id="count-dropped"`) {
		t.Errorf("a job that keeps everything is given a count of what it dropped:\n%s", body)
	}
	if want := LangEN.T("form.unique.off"); !strings.Contains(body, want) {
		t.Errorf("the page does not say the job was set up as %q:\n%s", want, body)
	}
}

func TestProgress_CarriesWhatWasDroppedToTheScriptThatFollowsTheJob(t *testing.T) {
	// The count climbs while the job runs, so it has to arrive with the counts
	// the script already replaces. Left out, it would stand at what it was when
	// the page was drawn until the reader loaded the page again.
	s := testServer(t)
	id := filteredJob(t, s, store.UniqueURL, 4)

	if got := askProgress(t, s, id).Dropped; got != 6 {
		t.Errorf("the poll answers %d dropped, want 6 — both results of every query after the first", got)
	}
}

// seedPositionJob writes a position check and what it settled: for each phrase,
// the rank the site stood at, or nought for a phrase the site was not found for.
//
// A phrase left out of the map entirely is one nobody has reached, which is the
// state the page has to keep apart from the other two.
func seedPositionJob(t *testing.T, s *Server, name, target string, at map[string]int) int64 {
	t.Helper()
	ctx := t.Context()
	phrases := make([]string, 0, len(at))
	for phrase := range at {
		phrases = append(phrases, phrase)
	}
	sort.Strings(phrases)
	id, err := s.store.CreateJob(ctx,
		store.JobSpec{Name: name, Kind: store.KindPosition, Target: target, Pages: 3}, phrases)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for i, phrase := range phrases {
		var found []google.Result
		if rank := at[phrase]; rank > 0 {
			found = []google.Result{{
				Position: rank, Title: target, URL: "https://" + target + "/page", Host: target}}
		}
		if err := s.store.Record(ctx, id, store.QueryOutcome{
			Ordinal: i,
			Pages:   []google.SERP{{Origin: "https://www.google.com", Results: found}},
		}); err != nil {
			t.Fatalf("recording the check of %q: %v", phrase, err)
		}
	}
	return id
}

func TestJobPage_AnswersForEveryPhraseAPositionCheckSettled(t *testing.T) {
	// The whole point of the job is one answer per phrase: the place, or the
	// words for not there. A page that drew the captured results instead would
	// show the phrases the site ranked for and simply leave out the ones it did
	// not, which is half the answer the check was run for.
	//
	// No rank here is one, so a page that drew the row it found rather than the
	// rank on it would be wrong on every line.
	s := testServer(t)
	id := seedPositionJob(t, s, "places", "example.com", map[string]int{
		"iphone 13":     4,
		"iphone 13 pro": 0,
		"pixel 8":       17,
	})

	body := get(t, s, jobPath(id)).Body.String()
	for _, want := range []string{"iphone 13", "iphone 13 pro", "pixel 8",
		">4<", ">17<", LangEN.T("job.rank.none")} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q:\n%s", want, body)
		}
	}
	// The site the check is about is named once, among the settings: a column of
	// numbers with no question above it is not an answer.
	if !strings.Contains(body, "example.com") {
		t.Errorf("the page does not say which site the places are of:\n%s", body)
	}
	// It is not drawn as what a parse captures: there is one row per phrase, and
	// the address and the title of the one result are not the answer.
	if strings.Contains(body, LangEN.T("job.result.title")) {
		t.Errorf("a position check was drawn as a table of captured results:\n%s", body)
	}
}

func TestJobPage_KeepsAPhraseNobodyReachedOffAPositionCheck(t *testing.T) {
	// Not found is an answer and not checked is the absence of one. A phrase
	// waiting its turn, drawn as a phrase the site does not rank for, tells the
	// reader something this program never established, and the two look identical
	// on the page.
	s := testServer(t)
	id, err := s.store.CreateJob(t.Context(),
		store.JobSpec{Name: "places", Kind: store.KindPosition, Target: "example.com", Pages: 1},
		[]string{"checked", "waiting"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.store.Record(t.Context(), id, store.QueryOutcome{
		Ordinal: 0,
		Pages:   []google.SERP{{Origin: "https://www.google.com"}},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rows := rowsUnder(get(t, s, jobPath(id)).Body.String())
	if !strings.Contains(rows, "checked") {
		t.Errorf("the page leaves out the phrase that was checked:\n%s", rows)
	}
	if strings.Contains(rows, "waiting") {
		t.Errorf("a phrase nobody has reached is drawn as one the site does not rank for:\n%s", rows)
	}
}

func TestJobPage_SaysSoWhenAPositionCheckHasCheckedNothingYet(t *testing.T) {
	// A check with nothing settled has no places, which is not the same as a
	// parse with nothing captured: the sentence has to name phrases, or the
	// reader is told their phrases produced no results.
	s := testServer(t)
	id, err := s.store.CreateJob(t.Context(),
		store.JobSpec{Name: "places", Kind: store.KindPosition, Target: "example.com", Pages: 1},
		[]string{"iphone 13"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	body := get(t, s, jobPath(id)).Body.String()
	if strings.Contains(body, "<table") {
		t.Error("a check that has settled nothing was shown an empty table")
	}
	if !strings.Contains(body, LangEN.T("job.standings.none")) {
		t.Errorf("the page does not say that nothing has been checked:\n%s", body)
	}
	if strings.Contains(body, LangEN.T("job.results.none")) {
		t.Errorf("a position check was told its results are empty:\n%s", body)
	}
}

func TestJobPage_DrawsAParseJobAsEverythingItCaptured(t *testing.T) {
	// A parse is the whole of what came back, and its page says so: every result
	// of every phrase, with the title and the address on it. Narrowed to a column
	// of places it would be a position check nobody asked for, and the reader
	// would have no way to see what was thrown away.
	s := testServer(t)
	id := seedJob(t, s, "phrases", 1, 1, 0)

	body := get(t, s, jobPath(id)).Body.String()
	for _, want := range []string{
		LangEN.T("job.result.title"), LangEN.T("history.address"), LangEN.T("history.rank"),
		"first.test", "second.test",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page of a parse does not show %q:\n%s", want, body)
		}
	}
	// Both results of the one phrase are drawn, so a page narrowed to one row per
	// phrase fails here rather than looking merely shorter.
	if got := strings.Count(rowsUnder(body), "<tr"); got != 2 {
		t.Errorf("the page drew %d rows for a phrase that captured two results", got)
	}
	if strings.Contains(body, LangEN.T("job.rank.none")) {
		t.Errorf("a parse was drawn as a check that found nothing:\n%s", body)
	}
}

// rowsUnder is the body of the one table on the page, so a test about the rows
// is not answered by a phrase that appears among the settings above them.
func rowsUnder(body string) string {
	from := strings.Index(body, "<tbody>")
	to := strings.Index(body, "</tbody>")
	if from < 0 || to < from {
		return ""
	}
	return body[from:to]
}

func TestReshape_ChangesThePoolTheJobWillComeUpOnAndNothingElse(t *testing.T) {
	// Only three things about a job can still be changed. What it is — its depth,
	// its country, its filter — is settled by the work already done under it, and
	// a page that offered to change one of those would be offering to file
	// results gathered under two rules as though they were one.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 2, 0, 0)
	before := theJob(t, s, id)

	rec := postForm(t, s, "/api/reshape", url.Values{
		"job": {strconv.FormatInt(id, 10)}, "ports": {"11"}, "threads": {"3"}, "tries": {"17"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reshaping came back %d, want a redirect: %s", rec.Code, rec.Body.String())
	}

	after := theJob(t, s, id)
	// The three are all different from each other and from what was there, so a
	// handler that read one box into another cannot pass this.
	if after.Ports != 11 || after.Threads != 3 || after.Tries != 17 {
		t.Errorf("the job now runs on ports=%d threads=%d tries=%d, want 11, 3 and 17",
			after.Ports, after.Threads, after.Tries)
	}
	if after.Pages != before.Pages || after.Country != before.Country ||
		after.UniqueBy != before.UniqueBy || after.Kind != before.Kind {
		t.Errorf("reshaping changed what the job is: %+v against %+v", after, before)
	}
	if !strings.Contains(get(t, s, jobPath(id)+"?reshaped=done").Body.String(),
		LangEN.T("job.reshape.done")) {
		t.Error("the page says nothing about the change that was just made")
	}
}

func TestReshape_SaysSoWhenTheJobHasAlreadyRunRatherThanAcceptingItQuietly(t *testing.T) {
	// Accepting it reads as applied, and the reader would go on waiting for a
	// change that has nothing left to change.
	s := testServer(t)
	id := seedJob(t, s, "done", 1, 1, 0)
	if err := s.store.FinishJob(t.Context(), id); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}

	rec := postForm(t, s, "/api/reshape", url.Values{
		"job": {strconv.FormatInt(id, 10)}, "ports": {"11"}, "threads": {"3"}, "tries": {"17"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reshaping a finished job came back %d", rec.Code)
	}
	if got := theJob(t, s, id); got.Ports == 11 {
		t.Error("a finished job was reshaped anyway")
	}
	if !strings.Contains(get(t, s, jobPath(id)+"?reshaped=finished").Body.String(),
		LangEN.T("job.reshape.finished")) {
		t.Error("the page does not say why nothing was changed")
	}
}

// theJob reads one job's summary back out of the history.
func theJob(t *testing.T, s *Server, id int64) store.JobSummary {
	t.Helper()
	sum, err := s.store.Progress(t.Context(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	return sum
}

func TestJobState_SaysItIsReachingTheIdentitiesBeforeTheFirstAnswer(t *testing.T) {
	// Measured on a real machine: the first query goes out three and a half
	// seconds after the press, and the answer arrives a minute or more later
	// because a cold identity meets a challenge first. A screen of noughts
	// through all of that reads as a job that never started, and the operator
	// reaches for the stop button.
	starting := progressJSON{Running: true, Total: 10, Pending: 10}
	if got := stateOf(starting, true); got != "job.state.starting" {
		t.Errorf("a running job with nothing settled reads as %q, want the one that says why", got)
	}
	// And the moment anything settles, it is an ordinary running job: the wait
	// this describes is over, and a screen that went on saying it would be
	// describing something that had stopped being true.
	settled := starting
	settled.Done = 1
	if got := stateOf(settled, true); got != "job.state.running" {
		t.Errorf("a job that has settled one query reads as %q, want running", got)
	}
	failed := starting
	failed.Failed = 1
	if got := stateOf(failed, true); got != "job.state.running" {
		t.Errorf("a job whose first query was refused reads as %q, want running", got)
	}

	// A job nothing is running says nothing about reaching anybody.
	waiting := progressJSON{Queued: true, Total: 10, Pending: 10}
	if got := stateOf(waiting, true); got != "job.state.waiting" {
		t.Errorf("a queued job reads as %q, want waiting", got)
	}
}

func TestNewJobForm_KeepsTheSwitchWithTheBoxesItChoosesBetweenAndAheadOfThem(t *testing.T) {
	// Where the phrases come from stands with the two boxes it chooses between,
	// because a switch a screen away from what it switches is one the reader has
	// to go and find.
	//
	// What it may not do is move below them. The whole form is sent as parts, a
	// browser sends them in the order they stand in the markup, and the server
	// has to know which box to read before the first line of a file arrives —
	// that order is what lets a list of a million lines be read as it arrives
	// rather than held whole. Moved under the file box, uploads break in a way
	// no small test file would show.
	page := mustAsset(t, "new.html")

	from := strings.Index(page, `name="from"`)
	queries := strings.Index(page, `name="queries"`)
	list := strings.Index(page, `name="list"`)
	for name, at := range map[string]int{"from": from, "queries": queries, "list": list} {
		if at < 0 {
			t.Fatalf("the form has no %s box", name)
		}
	}

	if from > queries || from > list {
		t.Error("the switch saying where the phrases come from stands after the box " +
			"it chooses; a file would then arrive before the server knew to read it")
	}

	// And it is beside them rather than up among the settings: nothing but the
	// two boxes and their own note comes between.
	if between := page[from:queries]; strings.Count(between, `<div class="field">`) > 2 {
		t.Errorf("%d fields stand between the switch and the queries box, want it "+
			"next to what it switches", strings.Count(between, `<div class="field">`))
	}
}

func TestJobPage_ShowsASampleShortEnoughToRead(t *testing.T) {
	// The page answers "is this still working, and what is it bringing back".
	// The export beside it hands over every row, so the table is a sample and
	// not a listing: long enough to see what is coming back, short enough that
	// the job itself is still on the screen.
	if rowsShown > 20 {
		t.Errorf("the page draws %d rows, which pushes the job off the screen it "+
			"is being watched on", rowsShown)
	}
	if rowsShown < 5 {
		t.Errorf("the page draws %d rows, too few to see what is coming back", rowsShown)
	}
}

func TestJobPage_OffersTheFailedQueriesBackWhenNothingIsPending(t *testing.T) {
	// A job that failed every query has nothing pending, so "carry on" is not
	// offered — and without a button of its own the screen offers nothing at
	// all, which is what a pool that went away leaves behind.
	s := testServerWithSupervisor(t)
	ctx := context.Background()
	id, err := s.store.CreateJob(ctx, store.JobSpec{Name: "starved"}, []string{"one", "two"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for _, ord := range []int{0, 1} {
		if err := s.store.Record(ctx, id, store.QueryOutcome{Ordinal: ord, Err: errors.New("blanktrail: every candidate port is quarantined")}); err != nil {
			t.Fatalf("recording a failure: %v", err)
		}
	}

	page := get(t, s, fmt.Sprintf("/job/%d", id)).Body.String()
	if !strings.Contains(page, `action="/api/retry"`) {
		t.Error("a job whose every query failed offers no way to ask them again")
	}

	// And pressing it puts them back, so the job has something to run.
	if rec := postForm(t, s, "/api/retry", url.Values{"job": {strconv.FormatInt(id, 10)}}); rec.Code >= 400 {
		t.Fatalf("pressing it answered %d: %s", rec.Code, rec.Body.String())
	}
	sum, err := s.store.Progress(ctx, id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.Pending != 2 {
		t.Errorf("%d queries are pending after the press, want both back", sum.Pending)
	}
}

func TestJobPage_KeepsAskingAfterTheFailedQueriesAreTakenBack(t *testing.T) {
	// The page asks itself again only while the job can answer differently, and
	// a job stamped finished never can. Taking its failures back leaves work in
	// front of it, so the stamp has to come off with them — left standing, the
	// job ran on behind a screen that sat still until the tab was reloaded.
	s := testServerWithSupervisor(t)
	ctx := context.Background()
	id, err := s.store.CreateJob(ctx, store.JobSpec{Name: "starved"}, []string{"one", "two"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for _, ord := range []int{0, 1} {
		if err := s.store.Record(ctx, id, store.QueryOutcome{Ordinal: ord, Err: errors.New("blanktrail: every candidate port is quarantined")}); err != nil {
			t.Fatalf("recording a failure: %v", err)
		}
	}
	if err := s.store.FinishJob(ctx, id); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}

	if _, err := s.store.TryFailedAgain(ctx, id); err != nil {
		t.Fatalf("TryFailedAgain: %v", err)
	}
	sum, err := s.store.Progress(ctx, id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.Finished {
		t.Fatal("the job is still stamped finished after its failures were taken back")
	}
	// Which is what the page reads to decide whether to keep asking.
	if !s.progress(sum).Watch && (s.progress(sum).Running || s.progress(sum).Queued) {
		t.Error("a job that is running again is not being watched")
	}
	// And it offers the way to carry on rather than nothing at all.
	page := get(t, s, fmt.Sprintf("/job/%d", id)).Body.String()
	if !strings.Contains(page, `action="/api/resume"`) {
		t.Error("a job with work back in front of it offers no way to carry on")
	}
}

func TestJobPage_CountsWhatTheJobHasCollected(t *testing.T) {
	// The counts beside this one count queries, and a query may bring back a
	// hundred results or none. The figure somebody watching a run is waiting on
	// is the second one: a job is run to collect something, and "four queries
	// done" says nothing about how much of it there is.
	//
	// It is compared against the store rather than against the number the
	// fixture wrote, so a page reading one count and working the rest out from
	// it is caught here.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 9, 4, 2)

	collected, err := s.store.ResultCount(t.Context(), id)
	if err != nil {
		t.Fatalf("ResultCount: %v", err)
	}
	if collected == 0 {
		t.Fatal("the seeded job collected nothing, so this test would pass over an empty page")
	}
	if got := shown(t, get(t, s, jobPath(id)).Body.String(), "count-collected"); got != strconv.Itoa(collected) {
		t.Errorf("the page shows %q collected, and the store holds %d", got, collected)
	}
}
