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
	"github.com/blanktrail/google-serp-parser/store"
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
	// The form itself reaches for nothing. Every screen loads the one script that
	// puts a fetched screen in place of the one on show, and that script does no
	// part of this form's work, so the form is read on its own here.
	if form := oneTag(t, body, "form"); strings.Contains(form, "<script") {
		t.Error("the form reaches for a script to do what it already does itself")
	}
}

func TestNewJob_SaysThatTheThreadAndPortBoxesOnlyChangeTheEstimate(t *testing.T) {
	// The two feed the estimate and nothing else. What runs a job is the one
	// warm pool the server was started with, and the thread count it was built
	// with: neither is a property of a job, and the history has nowhere to keep
	// them. A box that looks like a setting and is not is worse than no box,
	// because somebody will set it to sixteen, watch the job run at four and
	// conclude the program ignores what it is told.
	//
	// The two are read out of the group they stand in rather than looked for
	// anywhere on the page, because a sentence at the bottom of a form explains
	// nothing about the box at the top of it.
	body := get(t, testServer(t), "/new").Body.String()

	_, opened, ok := strings.Cut(body, "<fieldset")
	if !ok {
		t.Fatalf("the estimate's own boxes stand in no group of their own:\n%s", body)
	}
	group, _, ok := strings.Cut(opened, "</fieldset>")
	if !ok {
		t.Fatalf("the group the estimate's boxes stand in is never closed:\n%s", body)
	}
	if !strings.Contains(group, LangEN.T("form.estimateonly")) {
		t.Errorf("the group does not say what its boxes are for:\n%s", group)
	}
	for _, field := range []string{`name="threads"`, `name="ports"`} {
		if !strings.Contains(group, field) {
			t.Errorf("%s is not in the group that says these change only the estimate", field)
		}
	}
	// The depth is a setting of the job itself: it is written down with the job
	// and it decides what is searched for. Standing it beside the two that are
	// not would say the opposite of what this group is for.
	for _, field := range []string{`name="pages"`, `name="queries"`, `name="name"`} {
		if strings.Contains(group, field) {
			t.Errorf("%s was grouped with the boxes that change nothing but the estimate", field)
		}
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

func TestCreateJob_FilesTheJobUnderTheKindTheFormChose(t *testing.T) {
	// The choice on the form is what the run asks Google. A kind that stopped at
	// the handler would file every job as a search, and a list of addresses
	// would be searched for as phrases.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":    {"addresses"},
		"kind":    {store.KindIndex},
		"queries": {"example.com/a\nexample.com/b"},
		"pages":   {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("starting gave %d, want a redirect: %s", rec.Code, rec.Body)
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("%d jobs, want the one just started", len(jobs))
	}
	if jobs[0].Kind != store.KindIndex {
		t.Errorf("the job was filed as kind %q, want %q", jobs[0].Kind, store.KindIndex)
	}
}

func TestCreateJob_TakesAnIndexJobToOnePageWhateverTheDepthSays(t *testing.T) {
	// Presence is settled by the first page, so a depth of five is four pages of
	// work this job will not do. It is settled where the job is written down, so
	// the number in the history, the number in the estimate and the number of
	// requests that leave the machine are one number.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":    {"addresses"},
		"kind":    {store.KindIndex},
		"queries": {"example.com/a"},
		"pages":   {"5"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("starting gave %d, want a redirect: %s", rec.Code, rec.Body)
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if jobs[0].Pages != 1 {
		t.Errorf("the job asks for %d pages, want the 1 an index check takes", jobs[0].Pages)
	}
}

func TestCreateJob_RefusesAKindNothingAnswersTo(t *testing.T) {
	// The two kinds ask different questions and their answers mean different
	// things. Running an unknown one as a search would file a job under a
	// question nobody asked, and nothing on the page would say so.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":    {"odd"},
		"kind":    {"images"},
		"queries": {"a"},
		"pages":   {"1"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("an unknown kind gave %d, want the form back", rec.Code)
	}
	if want := LangEN.T("form.kind.unknown"); !strings.Contains(rec.Body.String(), want) {
		t.Errorf("the page does not say %q:\n%s", want, rec.Body)
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("%d jobs written for a kind nothing answers to, want none", len(jobs))
	}
}

func TestNewJob_OffersEveryKindAJobCanBe(t *testing.T) {
	body := get(t, testServer(t), "/new").Body.String()
	if !strings.Contains(body, `name="kind"`) {
		t.Fatalf("the form has no choice of kind:\n%s", body)
	}
	for _, k := range kinds() {
		if !strings.Contains(body, `value="`+k.Value+`"`) {
			t.Errorf("the form does not offer %q", k.Value)
		}
		if want := LangEN.T(k.Label); !strings.Contains(body, want) {
			t.Errorf("the form does not name %q as %q", k.Value, want)
		}
	}
}

func TestCreateJob_FilesTheJobUnderTheFilterTheFormChose(t *testing.T) {
	// The choice on the form is what the run throws away. A filter that stopped
	// at the handler would keep every repeat while the page said otherwise, and
	// nothing about the finished job would say which of the two happened.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":    {"nightly"},
		"unique":  {string(store.UniqueHost)},
		"queries": {"a\nb"},
		"pages":   {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("starting gave %d, want a redirect: %s", rec.Code, rec.Body)
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("%d jobs, want the one just started", len(jobs))
	}
	if jobs[0].UniqueBy != store.UniqueHost {
		t.Errorf("the job was filed as filtered by %q, want %q", jobs[0].UniqueBy, store.UniqueHost)
	}
}

func TestCreateJob_LeavesAnIndexJobUnfilteredHoweverTheBoxIsSet(t *testing.T) {
	// An index job answers one verdict per address, worked out from the results
	// filed against that address. A result dropped for sharing a site with an
	// earlier one would read back as an address Google does not hold — a wrong
	// answer the reader has no way to disbelieve. It is settled where the job is
	// written down, so the job in the history and the job that runs agree.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":    {"addresses"},
		"kind":    {store.KindIndex},
		"unique":  {string(store.UniqueHost)},
		"queries": {"example.com/a\nexample.com/b"},
		"pages":   {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("starting gave %d, want a redirect: %s", rec.Code, rec.Body)
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if jobs[0].UniqueBy != store.UniqueOff {
		t.Errorf("an index job was filed as filtered by %q, want no filter", jobs[0].UniqueBy)
	}
}

func TestCreateJob_RefusesAWayOfDroppingRepeatsThatIsNotOne(t *testing.T) {
	// Dropping cannot be undone. A word nobody here knows, read as keeping
	// everything, would run the job under a rule the reader did not choose, and
	// nothing on the page would say so.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":    {"odd"},
		"unique":  {"domain"},
		"queries": {"a"},
		"pages":   {"1"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("an unknown filter gave %d, want the form back", rec.Code)
	}
	if want := LangEN.T("form.unique.unknown"); !strings.Contains(rec.Body.String(), want) {
		t.Errorf("the page does not say %q:\n%s", want, rec.Body)
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("%d jobs written under a filter nobody reads, want none", len(jobs))
	}
}

func TestNewJob_OffersEveryWayOfDroppingRepeats(t *testing.T) {
	body := get(t, testServer(t), "/new").Body.String()
	if !strings.Contains(body, `name="unique"`) {
		t.Fatalf("the form has no choice of what to do with repeats:\n%s", body)
	}
	for _, f := range filters() {
		if !strings.Contains(body, `value="`+f.Value+`"`) {
			t.Errorf("the form does not offer %q", f.Value)
		}
		if want := LangEN.T(f.Label); !strings.Contains(body, want) {
			t.Errorf("the form does not name %q as %q", f.Value, want)
		}
	}
}
