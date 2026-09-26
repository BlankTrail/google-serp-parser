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
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/run"
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
		"job": {strconv.FormatInt(id, 10)}, "threads": {"3"}, "tries": {"17"},
		"cooldown": {"45"}, "restupto": {"90"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reshaping came back %d, want a redirect: %s", rec.Code, rec.Body.String())
	}

	after := theJob(t, s, id)
	// The four are all different from each other and from what was there, so a
	// handler that read one box into another cannot pass this.
	if after.Threads != 3 || after.Tries != 17 ||
		after.Cooldown != 45*time.Second || after.RestUpTo != 90*time.Second {
		t.Errorf("the job now runs threads=%d tries=%d resting %v to %v, want 3, 17, 45s and 90s",
			after.Threads, after.Tries, after.Cooldown, after.RestUpTo)
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
		"job": {strconv.FormatInt(id, 10)}, "threads": {"3"}, "tries": {"17"},
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

func TestJobPage_CountsWhatTheRunHasBroughtBackBeforeItIsWrittenDown(t *testing.T) {
	// A query taken to a hundred pages is written down once, at the end of all
	// hundred. Read from the history alone, a screen watching such a job shows
	// nothing collected and no speed for as long as that takes — which is most
	// of an afternoon on a wide job, and reads as a run that has stopped.
	//
	// So the screen shows both halves of the same count: what has settled, and
	// what this run has in flight.
	s, _, _ := serverWithRunningJob(t)
	id, running := s.sup.Running()
	if !running {
		t.Fatal("the fixture has no job in flight, so there is nothing in flight to count")
	}

	before := shown(t, get(t, s, jobPath(id)).Body.String(), "count-collected")
	if before != "0" {
		t.Fatalf("the job starts with %q collected, so this test cannot tell the halves apart", before)
	}

	// Three pages come back, none of them written yet.
	now := time.Now()
	for i := range 3 {
		s.sup.caught.took(id, 10, now.Add(time.Duration(i)*2*time.Second))
	}
	body := get(t, s, jobPath(id)).Body.String()
	if got := shown(t, body, "count-collected"); got != "30" {
		t.Errorf("the screen shows %q collected while the run holds 30 unwritten", got)
	}
	// And the pages a minute follow the same moments rather than waiting for a
	// query to settle: three pages four seconds apart is thirty a minute.
	if got := shown(t, body, "run-page-speed"); got != "30" {
		t.Errorf("the screen shows %q pages a minute, want the run's own 30", got)
	}

	// What reaches the history stops being in flight, so it is counted once.
	s.sup.caught.written(id, 30)
	if got := shown(t, get(t, s, jobPath(id)).Body.String(), "count-collected"); got != "0" {
		t.Errorf("the screen shows %q collected after the run handed those results over", got)
	}
}

func TestJobPage_SaysWhatIdentityTheJobsPortsWore(t *testing.T) {
	// A run pinned to one browser and a run spread over every browser produce
	// different results from the same list of phrases, and nothing in the rows
	// afterwards says which it was. The page is where that is answered — beside
	// the country and the kind of page, which are the other two settings that
	// are what the results are rather than how fast they were gathered.
	s := testServer(t)
	pinned, err := s.store.CreateJob(t.Context(),
		store.JobSpec{Name: "pinned", Pages: 1, Browser: "safari", OS: "macos", Release: 26},
		[]string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	body := get(t, s, jobPath(pinned)).Body.String()
	if !strings.Contains(body, "Safari 26 · macOS") {
		t.Errorf("the page of a pinned job does not say what its ports wore:\n%s", body)
	}

	// And a job that named none of it is drawn as a dash rather than as a list
	// of everything: the list is the same on every such job, and what is being
	// asked here is whether this one was pinned.
	spread, err := s.store.CreateJob(t.Context(),
		store.JobSpec{Name: "spread", Pages: 1}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if body := get(t, s, jobPath(spread)).Body.String(); strings.Contains(body, "Safari") {
		t.Errorf("the page of a job that named nothing names a browser:\n%s", body)
	}
}

// runningWith puts a job in flight on an engine whose pool reads as given, and
// answers the server, the supervisor and the job's id.
func runningWith(t *testing.T, facts poolFacts) (*Server, *Supervisor, int64) {
	t.Helper()
	s, v, eng := heldServer(t)
	eng.facts = &facts
	id := enqueue(t, v, "nightly", "a", "b", "c")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})
	return s, v, id
}

func TestJobPage_ShowsWhatTheIdentitiesUnderTheRunningJobAreDoing(t *testing.T) {
	// The reading used to be on the proxy screen, where it was the profile's.
	// It is the job's: a pool is raised for one job and taken down when that job
	// lets go, so how many ports are open and how many addresses are banned this
	// minute is a fact about the run somebody is watching.
	s, _, id := runningWith(t, poolFacts{
		Stats:     blanktrail.Stats{Ports: 50, Warm: 7, Quarantined: 2},
		Addresses: 15000, Banned: 40,
		Sessions: 37, SessionsResting: 20,
	})

	body := get(t, s, jobPath(id)).Body.String()
	for id, want := range map[string]string{
		"addresses": "15000", "banned": "40",
		"ports": "50", "quarantined": "2",
		"sessions": "37", "sessions-asleep": "20",
	} {
		if got := shown(t, body, id); got != want {
			t.Errorf("the job's page says %s is %q, want %q", id, got, want)
		}
	}
}

func TestProxiesScreen_NoLongerCarriesWhatThePoolIsDoingThisMinute(t *testing.T) {
	// The other half of moving it. Left on both screens, the same numbers would
	// be read under a profile — where they are the list's — and under a job,
	// where they are the run's, and a reader comparing the two would be
	// comparing one pool with itself.
	s := testServerWithSupervisor(t)
	prof := onlyProfile(t, s)
	s.sup.onProfile = prof
	body := get(t, s, readingOf(prof)).Body.String()
	for _, gone := range []string{`id="addresses"`, `id="banned"`, `id="ports"`, `id="warm"`, `id="quarantined"`} {
		if strings.Contains(body, gone) {
			t.Errorf("the proxy screen still carries %s, which is now the job's", gone)
		}
	}
	// What belongs to the profile stays: what its addresses have done since the
	// count was cleared.
	if !strings.Contains(body, `id="rotations"`) {
		t.Error("the proxy screen lost the count that is the profile's own")
	}
}

func TestJobPage_ShowsGooglesChecksAndWhatTheSolverHasInHand(t *testing.T) {
	// Three different things, and the page keeps them apart. What was passed is
	// this run's own count; what is being solved and what is waiting are the
	// service's, because the solver processes are licensed to the machine.
	s, _, id := runningWith(t, poolFacts{
		Queue:  blanktrail.SolverQueue{Running: 3, Queued: 4},
		Checks: run.Rhythm{Met: 6, Intervals: 6, Between: 13, Known: true, Answered: 400, PerThousand: 15},
	})

	body := get(t, s, jobPath(id)).Body.String()
	for id, want := range map[string]string{
		"checks-met": "6", "checks-solving": "3", "checks-queued": "4", "checks-perthousand": "15.0",
	} {
		if got := shown(t, body, id); got != want {
			t.Errorf("the job's page says %s is %q, want %q", id, got, want)
		}
	}
	// And a run that has had no answer yet is not given a figure worked out
	// from nothing.
	s2, _, other := runningWith(t, poolFacts{Checks: run.Rhythm{Met: 1}})
	if got := shown(t, get(t, s2, jobPath(other)).Body.String(), "checks-perthousand"); got != noFigure {
		t.Errorf("a run with no answer yet reports %q checks a thousand pages, want the mark", got)
	}
}

func TestJobPage_AdvisesALongerRestOnlyWhereTheRestIsWhatLimitsTheRun(t *testing.T) {
	// The advice is the point of the count: on a run whose sessions are all the
	// sessions it can use, checks coming every few requests are those sessions
	// asked again before they have rested, and only a longer rest will mend it.
	crowded := run.Rhythm{Met: 9, Intervals: 9, Between: 2, Known: true, Crowded: true}
	s, _, id := runningWith(t, poolFacts{Checks: crowded, Ramp: run.Ramping{AtSpeed: true}})
	body := get(t, s, jobPath(id)).Body.String()
	if !strings.Contains(body, `id="checks-crowded"`) {
		t.Error("the checks come every other request on a run at its speed and the page says nothing")
	}
	// Unescaped, because the advice carries an apostrophe and the template
	// writes it as an entity.
	if want := catalogue[LangEN]["job.checks.crowded"]; !strings.Contains(html.UnescapeString(body), want) {
		t.Errorf("the page does not carry the advice itself:\n%s", body)
	}

	// A run still taking sessions on is limited by how many it has rather than
	// by how long they rest, and the checks it is paying are what a session
	// arriving somewhere new pays. A longer rest makes more such sessions: the
	// advice would make the thing it was given about worse.
	widening, _, other := runningWith(t, poolFacts{Checks: crowded})
	if body := get(t, widening, jobPath(other)).Body.String(); strings.Contains(body, `id="checks-crowded"`) {
		t.Error("a run still taking sessions on is told to rest them longer")
	}

	// Nor is a run that cannot take on more because the list will not let it.
	short, _, third := runningWith(t, poolFacts{Checks: crowded, Ramp: run.Ramping{Short: true}})
	if body := get(t, short, jobPath(third)).Body.String(); strings.Contains(body, `id="checks-crowded"`) {
		t.Error("a run with no address left for another session is told to rest its sessions longer")
	}

	// And a run meeting them seldom is left alone whatever else is true of it.
	easy, _, fourth := runningWith(t, poolFacts{
		Checks: run.Rhythm{Met: 9, Intervals: 9, Between: 100, Known: true},
		Ramp:   run.Ramping{AtSpeed: true},
	})
	if body := get(t, easy, jobPath(fourth)).Body.String(); strings.Contains(body, `id="checks-crowded"`) {
		t.Error("a run meeting a check every hundred requests is told to rest its sessions longer")
	}
}

func TestJobPage_SaysWhereTheRunStandsAgainstItsOwnSpeed(t *testing.T) {
	// Three states, and the middle one is the question being asked: a run that
	// has stopped taking sessions on because it has enough is a run whose speed
	// is its own. One that has stopped because every address is full looks the
	// same in every other figure on the screen and wants the opposite answer.
	for _, state := range []struct {
		at   run.Ramping
		want string
	}{
		{run.Ramping{Made: 12}, "job.ramp.widening"},
		{run.Ramping{Made: 12, AtSpeed: true}, "job.ramp.atspeed"},
		{run.Ramping{Made: 12, Short: true}, "job.ramp.short"},
	} {
		s, _, id := runningWith(t, poolFacts{Ramp: state.at})
		body := get(t, s, jobPath(id)).Body.String()
		if got := shown(t, body, "ramp"); got != LangEN.T(state.want) {
			t.Errorf("a run reading %+v is drawn as %q, want %q", state.at, got, LangEN.T(state.want))
		}
		// And how many sessions it had made for it, which is what the word is
		// about: the others were made by an earlier run or by another job.
		if got := shown(t, body, "sessions-made"); got != "12" {
			t.Errorf("the page says %q sessions were made for this job, want 12", got)
		}
	}
}

func TestJobPage_DrawsNoIdentitiesForAJobThatIsNotTheOneRunning(t *testing.T) {
	// The pool belongs to whichever job has it now. Drawn on another job's page
	// it would be one run's numbers read as another's — and on a finished job's,
	// numbers from a pool that no longer exists.
	s, v, eng := heldServer(t)
	eng.facts = &poolFacts{Stats: blanktrail.Stats{Ports: 50}, Addresses: 15000}
	running := enqueue(t, v, "nightly", "a", "b", "c")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == running
	})

	other, err := v.st.CreateJob(t.Context(), store.JobSpec{Name: "another", Pages: 1}, []string{"z"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if body := get(t, s, jobPath(other)).Body.String(); strings.Contains(body, `id="pool"`) {
		t.Error("a job that is not running is drawn with the identities of the one that is")
	}

	eng.let(t, 3)
	waitUntil(t, "the job is over", func() bool { _, ok := v.Running(); return !ok })
	if body := get(t, s, jobPath(running)).Body.String(); strings.Contains(body, `id="pool"`) {
		t.Error("a job that has finished still shows a pool that was taken down with it")
	}
}

// between is the part of a page after the first from and before the next to
// after it, and says whether both were found.
func between(body, from, to string) (string, bool) {
	_, rest, ok := strings.Cut(body, from)
	if !ok {
		return "", false
	}
	part, _, ok := strings.Cut(rest, to)
	return part, ok
}

func TestJobPage_PutsItsControlsAndItsExportRightUnderTheSummary(t *testing.T) {
	// A running job draws its readings between the summary and whatever came
	// after them, and the stop used to come after them — off the bottom of the
	// screen while the run was the thing being watched. The presses that act on
	// the run stand right under its summary, on the left, and the way to take
	// its findings away stands on the same line, on the right.
	s, _, id := runningWith(t, poolFacts{Addresses: 15000})
	body := get(t, s, jobPath(id)).Body.String()
	row, ok := between(body, `id="progress"`, `id="pool"`)
	if !ok {
		t.Fatalf("the page has no summary followed by the pool's reading:\n%s", body)
	}
	if !strings.Contains(row, `action="/api/stop"`) {
		t.Error("the stop is not right under the summary of a running job")
	}
	if !strings.Contains(row, `href="`+exportsAt+`?job=`+strconv.FormatInt(id, 10)+`"`) {
		t.Error("the way to the export is not beside the controls under the summary")
	}

	// A job nothing can be pressed on still has its findings to take away.
	idle := testServer(t)
	done := seedJob(t, idle, "finished", 1, 1, 0)
	under, ok := between(get(t, idle, jobPath(done)).Body.String(), `id="progress"`, `<section`)
	if !ok || !strings.Contains(under, `href="`+exportsAt+`?job=`+strconv.FormatInt(done, 10)+`"`) {
		t.Errorf("a finished job does not offer its export right under the summary:\n%s", under)
	}
}

func TestJobPage_LeavesOutTheWarmPortsASessionRunNeverHas(t *testing.T) {
	// A port was warm when it had answered under the identity it kept. A run on
	// sessions puts a different session on a port for every page, so the count
	// read nought on every job and was a figure asking to be explained.
	s, _, id := runningWith(t, poolFacts{Stats: blanktrail.Stats{Ports: 50, Warm: 7}})
	if body := get(t, s, jobPath(id)).Body.String(); strings.Contains(body, `id="warm"`) {
		t.Error("the page still counts the warm ports")
	}
}

func TestJobPage_FoldsTheThreadsByStageAwayUntilAsked(t *testing.T) {
	// The table is for somebody looking into a slow run, and on every other visit
	// it is a table to scroll past. It is folded, and opened by a press.
	s, _, id := runningWith(t, poolFacts{Standing: run.Census{
		Standing: []run.Standing{{Doing: run.DoingAsk, Threads: 3}}, Threads: 3,
	}})
	body := get(t, s, jobPath(id)).Body.String()
	fold, ok := between(body, `<details id="standing-fold"`, `</details>`)
	if !ok {
		t.Fatalf("the threads by stage are not folded:\n%s", body)
	}
	if tag, _, _ := strings.Cut(fold, ">"); strings.Contains(tag, "open") {
		t.Errorf("the fold is drawn open: <details id=\"standing-fold\"%s>", tag)
	}
	if !strings.Contains(fold, `class="rows"`) {
		t.Error("the table of threads stands outside its fold")
	}
}

func TestScript_KeepsAFoldAsTheReaderLeftItWhenTheScreenIsDrawnAgain(t *testing.T) {
	// A running job's page is drawn again every few seconds. A fold the reader
	// opened would snap shut under them at the next drawing, so the drawing again
	// of the same screen keeps every fold with a name as it was — and only that:
	// a screen arrived at by a press is drawn as the server drew it.
	script := mustAsset(t, "static/app.js")
	for _, part := range []string{`swap(html, true)`, `details[id]`, `if (keepFolds)`} {
		if !strings.Contains(script, part) {
			t.Errorf("the script never names %q", part)
		}
	}
	if strings.Count(script, "swap(html, true)") != 1 {
		t.Error("folds are kept on more than the drawing again of the same screen")
	}
}

func TestJobPage_SaysAJobToldToStopIsStoppingAndOffersNoSecondStop(t *testing.T) {
	// The stop returns as soon as the job has been told, and the job counts as
	// running until it has written down what it collected and given its ports
	// back — on a busy service, minutes. The page the press landed on offered the
	// same stop again, and a reader who saw it read a press that had not worked
	// and pressed again, and again. It says the job is stopping instead, offers
	// nothing to press, and goes on watching until the job has let go.
	s, v, eng := heldServer(t)
	eng.linger = make(chan struct{})
	id := enqueue(t, v, "nightly", "a", "b", "c")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})
	// Inside its engine, and not merely taken: a job told to stop while its pool
	// is still going up lets go at once, and there is no stretch to look at.
	waitUntil(t, "the job is inside its engine", func() bool {
		_, in := eng.ran(0)
		return in
	})

	rec := postForm(t, s, "/api/stop", url.Values{"job": {strconv.FormatInt(id, 10)}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the stop gave %d, want 303", rec.Code)
	}
	body := get(t, s, jobPath(id)).Body.String()
	if strings.Contains(body, `action="/api/stop"`) {
		t.Error("the page offers the stop again to a job already told to stop")
	}
	if !strings.Contains(body, `id="stopping"`) {
		t.Errorf("the page does not say the job is stopping:\n%s", body)
	}
	if got, want := shown(t, body, "state"), LangEN.T("job.state.stopping"); got != want {
		t.Errorf("the job is said to be %q, want %q", got, want)
	}
	if !strings.Contains(body, `data-refresh=`) {
		t.Error("the page stops watching a job that has not let go yet")
	}

	close(eng.linger)
	waitUntil(t, "the job has let go", func() bool {
		_, ok := v.Running()
		return !ok
	})
	after := get(t, s, jobPath(id)).Body.String()
	if strings.Contains(after, `id="stopping"`) {
		t.Error("the page still says stopping about a job that has let go")
	}
	if !strings.Contains(after, `action="/api/resume"`) {
		t.Error("a stopped job with queries left is not offered a resume")
	}
}

func TestScript_HoldsTheDrawingAgainWhileAPressIsUnderWay(t *testing.T) {
	// A button is pressed in two halves, and a screen drawn again between them
	// puts a new button where the old one stood: the press is lost and nothing
	// is sent. A running job's page is drawn every three seconds, and its stop
	// was lost that way. The drawing again waits while a press is under way.
	script := mustAsset(t, "static/app.js")

	// A press begins on the screen and ends on the release, or when it is
	// abandoned.
	for event, want := range map[string]string{
		`"pointerdown"`:                "pressing = true",
		`"pointerup", "pointercancel"`: "pressing = false",
	} {
		_, handler, ok := strings.Cut(script, event)
		if !ok {
			t.Errorf("the script never listens for %s", event)
			continue
		}
		handler, _, _ = strings.Cut(handler, "}, true);")
		if !strings.Contains(handler, want) {
			t.Errorf("on %s the script does not say %q", event, want)
		}
	}

	// Asking again waits for the press to be over, as it waits for somebody
	// typing.
	if !strings.Contains(script, "editing() || pressing") {
		t.Error("the screen is asked for again in the middle of a press")
	}
	// And a screen that arrives in the middle of a press is not put in: the
	// check has to stand between the answer and the swap, or a press begun while
	// the screen was on its way is lost as before.
	_, arrived, ok := strings.Cut(script, "fetched(window.location.href).then(")
	if !ok {
		t.Fatal("the script never asks for the screen again")
	}
	arrived, _, _ = strings.Cut(arrived, "swap(html, true)")
	if !strings.Contains(arrived, "if (pressing)") {
		t.Error("a screen that arrives in the middle of a press is drawn over it")
	}
}

func TestJobPage_CountsTheCaptchasTheJobCostOverAllItsRuns(t *testing.T) {
	// The count stands in the job's summary, where it outlives the run: the
	// checks earlier runs of the job paid for, and while a run is going the ones
	// it has paid for so far on top.
	s, v, eng := heldServer(t)
	eng.facts = &poolFacts{Checks: run.Rhythm{Met: 7}}
	id := enqueue(t, v, "nightly", "a", "b")
	waitUntil(t, "the job is inside its engine", func() bool {
		_, in := eng.ran(0)
		return in
	})
	if got := shown(t, get(t, s, jobPath(id)).Body.String(), "captchas"); got != "7" {
		t.Errorf("the first run reads %q captchas, want its own seven", got)
	}
	if err := v.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitUntil(t, "the job has let go", func() bool {
		_, ok := v.Running()
		return !ok
	})
	if got := shown(t, get(t, s, jobPath(id)).Body.String(), "captchas"); got != "7" {
		t.Errorf("a stopped job reads %q captchas, want the seven its run paid for", got)
	}

	if err := v.Resume(id); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitUntil(t, "the job is inside its engine again", func() bool {
		_, in := eng.ran(1)
		return in
	})
	body := get(t, s, jobPath(id)).Body.String()
	if got := shown(t, body, "captchas"); got != "14" {
		t.Errorf("the second run reads %q captchas, want the first run's seven and its own", got)
	}
	if got := shown(t, body, "checks-met"); got != "14" {
		t.Errorf("the checks passed by this job read %q, want the whole job's fourteen", got)
	}
	if err := v.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
