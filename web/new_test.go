// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
	"github.com/blanktrail/google-serp-parser/run"
)

// testServerWithSupervisor is a server that can start a job.
//
// The engine the supervisor's own tests use stands where the identities go,
// with nothing holding it back, so a job started here runs to the end without
// a pool, without a network and without a second stand-in to keep in step.
func testServerWithSupervisor(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	v := newSupervisor(st, &heldEngine{})
	t.Cleanup(func() { _ = v.Close() })

	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func postForm(t *testing.T, s *Server, path string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// tenQueries is a list long enough that the estimate's arithmetic depends on
// every number it is given. A list shorter than the threads makes the thread
// count stop mattering, and a test written on one cannot tell the threads from
// the ports.
func tenQueries() []string {
	list := make([]string, 10)
	for i := range list {
		list[i] = "query " + strconv.Itoa(i)
	}
	return list
}

func TestNewJob_ShowsAFormWithNothingFilledIn(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/new", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /new gave %d, want 200", rec.Code)
	}
	for _, field := range []string{`name="name"`, `name="queries"`, `name="pages"`} {
		if !strings.Contains(rec.Body.String(), field) {
			t.Errorf("the form has no %s", field)
		}
	}
}

func TestNewJob_BothButtonsAreSubmitsOfTheOneForm(t *testing.T) {
	// Estimating and starting have to work with scripting switched off, so they
	// are two submit buttons of one form told apart by what they carry. A page
	// whose buttons need a script is a page half the readers cannot use at all.
	body := get(t, testServer(t), "/new").Body.String()

	for _, part := range []string{`method="post"`, `name="do"`} {
		if !strings.Contains(body, part) {
			t.Errorf("the form carries no %s:\n%s", part, body)
		}
	}
	// Each button is read as a whole tag rather than as the value it carries: a
	// button carrying the right value and doing nothing at all until a script
	// picks it up would satisfy a search for the value alone.
	for _, want := range []string{`value="estimate"`, `value="start"`} {
		var found bool
		for _, tag := range strings.Split(body, "<button")[1:] {
			tag, _, _ = strings.Cut(tag, ">")
			if !strings.Contains(tag, want) {
				continue
			}
			found = true
			if !strings.Contains(tag, `type="submit"`) {
				t.Errorf("the button carrying %s sends nothing on its own: <button%s>", want, tag)
			}
		}
		if !found {
			t.Errorf("the form carries no button with %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<script") {
		t.Error("the page reaches for a script to do what a form already does")
	}
}

func TestEstimate_AnswersWithoutStartingAnything(t *testing.T) {
	// The first thing anyone does with ten thousand queries is ask what it will
	// cost. Answering must not need a pool, a network, or a job in the history.
	//
	// What the page says is checked against the same arithmetic named in the
	// order it takes its arguments, so a page working the cost out on the ports
	// where the threads belong is caught here rather than believed.
	s := testServer(t)
	list := tenQueries()
	rec := postForm(t, s, "/new?do=estimate", url.Values{
		"name":    {"nightly"},
		"queries": {strings.Join(list, "\n")},
		"pages":   {"2"},
		"ports":   {"8"},
		"threads": {"4"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("estimating gave %d, want 200", rec.Code)
	}
	want := run.EstimateWith(run.Job{Queries: make([]google.Query, len(list)), Pages: 2},
		8, 4, blanktrail.DefaultCooldown, run.MeasuredPace)
	body := rec.Body.String()
	for what, part := range map[string]string{
		"the queries it counted":   strconv.Itoa(want.Queries),
		"the searches they make":   strconv.Itoa(want.Searches),
		"what will leave at all":   strconv.Itoa(want.Requests),
		"the worst it can cost":    strconv.Itoa(want.MaxRequests),
		"how long it will take":    want.Expected.Round(time.Second).String(),
		"the pacing floor beneath": want.Floor.Round(time.Second).String(),
	} {
		if !strings.Contains(body, part) {
			t.Errorf("the estimate does not say %s (%q):\n%s", what, part, body)
		}
	}

	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("estimating created %d jobs", len(jobs))
	}
}

func TestEstimate_SaysWhereItsNumbersCameFrom(t *testing.T) {
	// The costs behind the time were measured on one list, on one day, against
	// one target, and the range the dominant one was taken from spans a factor
	// of twenty. A bare number gets believed, so the number never travels alone.
	s := testServer(t)
	rec := postForm(t, s, "/new?do=estimate", url.Values{
		"queries": {"iphone 13"},
		"pages":   {"1"},
	})

	body := rec.Body.String()
	if !strings.Contains(body, LangEN.T("estimate.caveat")) {
		t.Errorf("the time is quoted with nothing about where it came from:\n%s", body)
	}
}

func TestEstimate_AnswersBeforeTheJobIsNamed(t *testing.T) {
	// Naming a job is a decision; learning what it costs is not. Refusing the
	// cost until the job has a name puts a form in front of the one question the
	// reader came with.
	s := testServer(t)
	rec := postForm(t, s, "/new?do=estimate", url.Values{
		"name":    {""},
		"queries": {strings.Join(tenQueries(), "\n")},
		"pages":   {"2"},
		"ports":   {"8"},
		"threads": {"4"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("estimating gave %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	want := run.EstimateWith(run.Job{Queries: make([]google.Query, 10), Pages: 2},
		8, 4, blanktrail.DefaultCooldown, run.MeasuredPace)
	if !strings.Contains(body, strconv.Itoa(want.Requests)) {
		t.Errorf("an unnamed job was told nothing about its cost:\n%s", body)
	}
	if !strings.Contains(body, LangEN.T("form.name.required")) {
		t.Error("the missing name went unmentioned")
	}
}

func TestCreateJob_KeepsWhatWasTypedWhenItRefuses(t *testing.T) {
	// A form that loses ten thousand lines over a missing name is a form
	// somebody fills in once.
	s := testServer(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":     {""},
		"queries":  {"iphone 13\ngolang generics\n"},
		"pages":    {"7"},
		"country":  {"de"},
		"language": {"de"},
	})

	if rec.Code == http.StatusSeeOther {
		t.Fatal("a job with no name was accepted")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "iphone 13") || !strings.Contains(body, "golang generics") {
		t.Error("the queries were not given back with the complaint")
	}
	// The settings are looked for as the value of the field they belong to: a
	// page that merely mentions 7 somewhere has not refilled the depth box.
	for _, filled := range []string{`value="7"`, `value="de"`} {
		if !strings.Contains(body, filled) {
			t.Errorf("the form came back without %s:\n%s", filled, body)
		}
	}
}

func TestCreateJob_ShowsEveryFaultOnTheOnePage(t *testing.T) {
	// One fault per submission means one submission per mistake. Somebody with
	// three mistakes should be able to fix all three and send the form once.
	s := testServer(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":    {"   "},
		"queries": {"# nothing but a note\n"},
		"pages":   {"0"},
	})

	body := rec.Body.String()
	for _, key := range []string{"form.name.required", "form.queries.required", "form.pages.positive"} {
		if !strings.Contains(body, LangEN.T(key)) {
			t.Errorf("the page does not report %s:\n%s", key, body)
		}
	}
}

func TestCreateJob_RefusesAListWithNoQueriesInIt(t *testing.T) {
	s := testServer(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":    {"nightly"},
		"queries": {"  \n# a comment\n\n"},
		"pages":   {"1"},
	})
	if rec.Code == http.StatusSeeOther {
		t.Error("a list holding no queries was accepted")
	}
}

func TestJobForm_ReadsOneQueryPerLineAndSkipsTheRest(t *testing.T) {
	// The same rule the command already applies to a file, so a list pasted
	// into the box and the same list on disk mean the same thing.
	// Named and given a depth, so the only thing left to complain about is the
	// list itself.
	f := jobForm{Name: "nightly", Pages: 1, Queries: "  \n# note\niphone 13\n\n golang \n"}
	queries, complaints := f.parse()
	if len(complaints) != 0 {
		t.Fatalf("complaints=%v, want none", complaints)
	}
	if len(queries) != 2 || queries[0] != "iphone 13" || queries[1] != "golang" {
		t.Errorf("queries=%q, want the two real ones, trimmed", queries)
	}
}

func TestJobForm_ReadsALineEndedTheWayABrowserEndsIt(t *testing.T) {
	// A textarea arrives with its lines ended the way the web ends them, and a
	// query carrying a stray carriage return is a query searched for with one.
	f := jobForm{Queries: "iphone 13\r\ngolang generics\r\n"}
	queries, _ := f.parse()
	if len(queries) != 2 || queries[0] != "iphone 13" || queries[1] != "golang generics" {
		t.Errorf("queries=%q, want the two lines with nothing trailing", queries)
	}
}

func TestJobForm_ComplainsAboutEveryFaultAtOnce(t *testing.T) {
	// Returning the first fault only means the form is submitted once per
	// mistake. Somebody with three mistakes should learn all three at once.
	f := jobForm{Name: "", Queries: "", Pages: -1}
	_, complaints := f.parse()
	if len(complaints) < 3 {
		t.Errorf("complaints=%v, want one for the name, one for the queries and one for the depth", complaints)
	}
}

func TestCreateJob_StartsTheJobAndSendsTheBrowserToIt(t *testing.T) {
	// The answer to a start is a redirect and never the job's own page: a page
	// rendered into the answer is one the browser's reload button sends again,
	// and the second send starts a second job.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":     {"nightly"},
		"queries":  {"iphone 13\ngolang generics"},
		"pages":    {"2"},
		"country":  {"de"},
		"language": {"de"},
	})

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("starting gave %d, want a redirect", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/job/") {
		t.Errorf("Location=%q, want the job's own page", loc)
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("%d jobs, want the one just started", len(jobs))
	}
	// The job is filed under what was typed. A job that took the settings from
	// somewhere else runs something other than what the reader asked for, and
	// the history then says the wrong thing about what was searched.
	if got := jobs[0]; got.Name != "nightly" || got.Pages != 2 ||
		got.Country != "de" || got.Language != "de" || got.Total != 2 {
		t.Errorf("the job was filed as %+v, want the two queries as they were typed", got)
	}
}

func TestCreateJob_WritesNothingDownWhenItRefuses(t *testing.T) {
	// Checking the form after the job has been written leaves a job in the
	// history for every mistyped form, and every one of them looks like a run
	// somebody meant to make.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":    {""},
		"queries": {"iphone 13"},
		"pages":   {"1"},
	})

	if rec.Code == http.StatusSeeOther {
		t.Fatal("a job with no name was accepted")
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("a refused form left %d jobs in the history", len(jobs))
	}
}

func TestCreateJob_RefusesToStartWithoutASupervisor(t *testing.T) {
	// A server built for reading history alone must say so rather than accept a
	// job nothing will ever run.
	s := testServer(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name": {"n"}, "queries": {"a"}, "pages": {"1"},
	})
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a job was accepted by a server with nothing to run it")
	}
	if !strings.Contains(rec.Body.String(), LangEN.T("form.norunner")) {
		t.Errorf("the reader was not told why nothing happened:\n%s", rec.Body.String())
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("%d jobs were written down by a server that cannot run them", len(jobs))
	}
}

func TestNewJob_ShowsNoBareKeyWhereAPhraseBelongs(t *testing.T) {
	// A key on the page is a phrase that was never looked up. It survives every
	// test written about a particular phrase, because it lands on the phrases
	// nobody thought to check.
	//
	// The empty form, a refusal and an estimate are all drawn, since between
	// them they are what puts every phrase this page has on a page.
	s := testServer(t)
	for _, l := range Languages() {
		lang := "?lang=" + string(l)
		bodies := []string{
			get(t, s, "/new"+lang).Body.String(),
			postForm(t, s, "/new"+lang+"&do=start", url.Values{
				"name": {""}, "queries": {""}, "pages": {"0"},
			}).Body.String(),
			postForm(t, s, "/new"+lang+"&do=estimate", url.Values{
				"name": {"nightly"}, "queries": {"iphone 13"}, "pages": {"2"},
			}).Body.String(),
		}
		for _, body := range bodies {
			for key := range catalogue[l] {
				if strings.Contains(body, key) {
					t.Errorf("the %s page shows the key %q where its text belongs", l, key)
				}
			}
		}
	}
}
