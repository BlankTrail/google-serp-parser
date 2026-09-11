// SPDX-License-Identifier: MIT

package web

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/store"
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

func TestNewJob_IsOneFormThatSendsItselfWithoutAScript(t *testing.T) {
	// One form, and a switch inside it saying where the phrases come from. Two
	// forms doing one thing made the reader choose between them before they knew
	// what the difference was. It has to send itself with scripting switched
	// off: a page whose button needs a script is a page half the readers cannot
	// use at all.
	body := get(t, testServer(t), "/new").Body.String()

	if n := strings.Count(body, "<form"); n != 1 {
		t.Errorf("the page carries %d forms, want the one: %s", n, body)
	}
	for _, part := range []string{`method="post"`, `enctype="multipart/form-data"`,
		`name="from"`, `name="queries"`, `name="list"`} {
		if !strings.Contains(body, part) {
			t.Errorf("the form carries no %s: %s", part, body)
		}
	}
	// The button is read as a whole tag rather than by the words on it: one that
	// looks like a button and does nothing until a script picks it up would
	// satisfy a search for the words alone.
	var sends bool
	for _, tag := range strings.Split(body, "<button")[1:] {
		tag, _, _ = strings.Cut(tag, ">")
		if strings.Contains(tag, `type="submit"`) {
			sends = true
		}
	}
	if !sends {
		t.Errorf("no button on this form sends it on its own: %s", body)
	}
}

func TestNewJob_KeepsTheJobsOwnPoolInAGroupOfItsOwn(t *testing.T) {
	// These three used to change a figure and nothing else. They are the job's
	// now: a pool is raised for it at this size when it starts and taken down
	// when it lets go, and the sentence under them says where they can be
	// changed afterwards. A box that looks like a setting and is not is worse
	// than no box, and so is one that is a setting and reads as an estimate.
	body := get(t, testServer(t), "/new").Body.String()

	// The group is found by its heading rather than by being first on the page:
	// a page that grows another group would otherwise move this test onto it and
	// go on passing.
	group := groupUnder(t, body, LangEN.T("form.pool"))
	if !strings.Contains(html.UnescapeString(group), LangEN.T("form.tries.why")) {
		t.Errorf("the group does not say what its boxes are: %s", group)
	}
	for _, field := range []string{`name="threads"`, `name="ports"`, `name="tries"`} {
		if !strings.Contains(group, field) {
			t.Errorf("%s is not in the group that says these belong to the job", field)
		}
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

func TestCreateJob_FilesThePauseInTheUnitTheBoxIsFilledIn(t *testing.T) {
	// The box is in seconds because that is what a person setting a pause between
	// two requests thinks in; the column is in milliseconds because a duration in
	// a database has to be a number. A unit lost between the two is a job resting
	// a thousandth of what was asked, and nothing on any page would say so.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name": {"careful"}, "queries": {"a"}, "pages": {"1"}, "cooldown": {"5"},
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
	if jobs[0].Cooldown != 5*time.Second {
		t.Errorf("the job rests %v between two requests on one identity, want the 5s that were typed",
			jobs[0].Cooldown)
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

func TestNewJob_ComesWithANameAlreadyInIt(t *testing.T) {
	// A job has to be named to be found again, and the reader came to run a list
	// rather than to name one. The moment the form was opened tells two runs of
	// the same list apart, and it is a default rather than a stamp: what the
	// reader types over it is what the job is called.
	s := testServer(t)
	body := get(t, s, "/new").Body.String()

	stamp := time.Now().Format("2006-01-02 15:04")
	if !strings.Contains(body, `value="`+stamp+`"`) {
		t.Errorf("the name box does not open with %q in it:\n%s", stamp, body)
	}
}

func TestNewJob_OffersParsingAsTheJobToStartFrom(t *testing.T) {
	// Parsing is what this program does and what somebody arriving at the form
	// came for. A page opening on one of the checks would put the narrow question
	// in front of a reader who came for the whole page of results, and the box
	// below it would then mean something they did not intend.
	s := testServer(t)
	body := get(t, s, newAt).Body.String()

	chosen := `<option value="` + store.KindParse + `" selected>`
	if !strings.Contains(body, chosen) {
		t.Errorf("the form does not open on a parse:\n%s", body)
	}
	for _, kind := range []string{store.KindPosition, store.KindIndex} {
		if strings.Contains(body, `<option value="`+kind+`" selected>`) {
			t.Errorf("the form opens on %q, and a parse is the ordinary job:\n%s", kind, body)
		}
	}
	// Every kind is on offer, or the choice is not one.
	for _, kind := range []string{store.KindParse, store.KindPosition, store.KindIndex} {
		if !strings.Contains(body, `<option value="`+kind+`"`) {
			t.Errorf("the form does not offer %q:\n%s", kind, body)
		}
	}
}

func TestCreateJob_FilesAJobThatNamedNoKindAsAParse(t *testing.T) {
	// A form posted without the field is the job this program did before there
	// was a choice, and that is the parse. Reading it as either check would run
	// somebody's phrases as a question about one site.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":    {"phrases"},
		"queries": {"iphone 13"},
		"pages":   {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("starting gave %d, want a redirect: %s", rec.Code, rec.Body)
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if jobs[0].Kind != store.KindParse {
		t.Errorf("the job was filed as kind %q, want %q", jobs[0].Kind, store.KindParse)
	}
}

func TestCreateJob_RefusesAPositionCheckWithNoSiteToLookFor(t *testing.T) {
	// Started, it would answer "not found" about every phrase in the list, and
	// that reads exactly like a site nobody ranks for. The complaint names the
	// box that is empty and judges nothing about it.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":    {"places"},
		"kind":    {store.KindPosition},
		"queries": {"iphone 13"},
		"pages":   {"1"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("a check with no site gave %d, want the form back", rec.Code)
	}
	if want := LangEN.T("form.target.required"); !strings.Contains(rec.Body.String(), want) {
		t.Errorf("the page does not say %q:\n%s", want, rec.Body)
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("%d jobs were written down although the form was refused", len(jobs))
	}
}

func TestCreateJob_FilesTheSiteAPositionCheckWasGiven(t *testing.T) {
	// The site is what the whole job is about. A box that stopped at the handler
	// would leave the history holding a check about nothing, and the run would
	// have nothing to recognise.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":    {"places"},
		"kind":    {store.KindPosition},
		"target":  {"example.com/wanted"},
		"unique":  {string(store.UniqueHost)},
		"queries": {"iphone 13"},
		"pages":   {"3"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("starting gave %d, want a redirect: %s", rec.Code, rec.Body)
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if jobs[0].Kind != store.KindPosition || jobs[0].Target != "example.com/wanted" {
		t.Errorf("the job was filed as kind %q about %q, want %q about example.com/wanted",
			jobs[0].Kind, jobs[0].Target, store.KindPosition)
	}
	// The depth is the reader's, unlike an index check: how deep to look before
	// calling a site absent is the question being asked.
	if jobs[0].Pages != 3 {
		t.Errorf("the check takes %d pages, want the 3 that were asked for", jobs[0].Pages)
	}
	// A position check drops nothing, whatever the box says. The one result it
	// keeps per phrase is the site itself, and a filter on hosts would drop it
	// from the second phrase onwards — every one of which would read back as a
	// phrase the site does not rank for.
	if jobs[0].UniqueBy != store.UniqueOff {
		t.Errorf("a position check was filed as filtered by %q, want no filter", jobs[0].UniqueBy)
	}
}

// groupUnder is the one group on the page whose legend says this, as markup.
func groupUnder(t *testing.T, body, legend string) string {
	t.Helper()
	for _, after := range strings.Split(body, "<fieldset>")[1:] {
		group, _, ok := strings.Cut(after, "</fieldset>")
		if !ok {
			t.Fatalf("a group on this page is never closed: %s", body)
		}
		if strings.Contains(group, legend) {
			return group
		}
	}
	t.Fatalf("no group on this page is headed %q: %s", legend, body)
	return ""
}

// The boxes the script puts away, named once so that both sides can be checked
// against the same list: the form has to carry each of them, and the script has
// to name each of them.
var shaped = []string{"kind", "from", "target", "pages", "queries", "list"}

func TestNewForm_CarriesEveryBoxTheScriptPutsAwayAndTheHooksItPutsThemAwayBy(t *testing.T) {
	// Nothing here runs a line of the script — there is no runtime to run it
	// with. What is pinned is the seam between the two files, which is the only
	// part of this that rots without a word: renaming a box on the form leaves a
	// script quietly hiding nothing, and a reader sees a form asking for a site
	// that a parse has no use for.
	body := get(t, testServer(t), "/new").Body.String()
	script := mustAsset(t, "static/app.js")

	for _, name := range shaped {
		if !strings.Contains(body, `name="`+name+`"`) {
			t.Errorf("the form has no box named %q, and the script hides that box", name)
		}
		// Named either as a name it hides or as a box it reads a choice from.
		if !strings.Contains(script, `"`+name+`"`) &&
			!strings.Contains(script, "[name="+name+"]") {
			t.Errorf("the script never names %q, so that box is shown whatever is chosen", name)
		}
	}

	// The parts of a result are a group of boxes under one name, so the group is
	// what is put away and the group is what has to be findable.
	if !strings.Contains(body, `class="keep-group"`) {
		t.Error("the form marks no group of result parts, so the script cannot put it away")
	}
	if !strings.Contains(script, "keep-group") {
		t.Error("the script never looks for the group of result parts, so it stays up for an index check")
	}

	// Every box the script hides is hidden by the block it stands in, and a box
	// standing in no such block would take its label with it or leave it behind.
	if !strings.Contains(script, ".field") {
		t.Error("the script hides boxes rather than the blocks they stand in, which leaves their labels")
	}
	if strings.Count(body, `class="field`) < len(shaped)-2 {
		t.Errorf("the form has %d blocks for %d boxes the script hides",
			strings.Count(body, `class="field`), len(shaped))
	}
}

func TestNewForm_ShapesItselfAgainWhenAScreenArrivesWithoutAReload(t *testing.T) {
	// The form is reached by pressing a tab as often as by opening its address,
	// and a screen swapped in is markup the shaping never saw. Without this the
	// form is shaped on a reload and not otherwise, which is the hardest kind of
	// half-working to notice.
	script := mustAsset(t, "static/app.js")
	if strings.Count(script, "gserp:screen") < 2 {
		t.Error("nothing both announces a swapped screen and listens for one, " +
			"so a form reached by pressing a tab is never shaped")
	}
}

func TestStyle_LeavesHiddenMeaningHidden(t *testing.T) {
	// The browser's own rule for a hidden element is the weakest rule there is,
	// and this file hands nearly everything a display of its own. A box put away
	// by the script and shown anyway is the worst outcome available here: the
	// markup says it was put away, the script says it was put away, and the
	// reader is looking straight at it.
	style := mustAsset(t, "static/app.css")
	at := strings.Index(style, "[hidden]")
	if at < 0 {
		t.Fatal("nothing in the stylesheet keeps a hidden element hidden, " +
			"so every rule here that gives a display shows it again")
	}
	rule := style[at:min(at+120, len(style))]
	if !strings.Contains(rule, "display: none") {
		t.Errorf("the rule for a hidden element does not take its display away: %q", rule)
	}
	if !strings.Contains(rule, "!important") {
		t.Errorf("the rule for a hidden element loses to the rules above it: %q", rule)
	}
}

func TestNewForm_AsksGoogleForOneAnswerRatherThanWhicheverTheIdentitySuggests(t *testing.T) {
	// An unqualified search is answered from what Google works out about the
	// identity making it, and a pool holds identities in many places. Left blank,
	// the same list comes back as a different page each run and nothing on the
	// page says why.
	form := blankForm()
	if form.Country == "" || form.Language == "" {
		t.Fatalf("a new job asks for country %q and language %q, so its results are "+
			"whatever the identity happened to suggest", form.Country, form.Language)
	}

	// And they reach the boxes, since a default nothing carries onto the page is
	// a default nobody has.
	body := get(t, testServer(t), "/new").Body.String()
	for _, box := range []struct{ name, want string }{
		{"country", form.Country},
		{"language", form.Language},
	} {
		tag := openingTag(t, body, `input id="`+box.name+`"`)
		if !strings.Contains(tag, `value="`+box.want+`"`) {
			t.Errorf("the %s box does not hold %q: <%s>", box.name, box.want, tag)
		}
	}
}

func TestNewForm_OffersThePauseBetweenTwoRequestsOnOneIdentity(t *testing.T) {
	// The pause was a setting of the machine, where one number served every job
	// this machine would ever run. It is the job's: how hard a list may be pushed
	// depends on the list and on what is being asked of it.
	//
	// It arrives filled in, because a box a reader has to work out a number for
	// is a box most readers leave alone.
	if form := blankForm(); form.Cooldown != defaultCooldown {
		t.Errorf("a new job rests %d seconds between two requests on one identity, want %d",
			form.Cooldown, defaultCooldown)
	}
	body := get(t, testServer(t), "/new").Body.String()
	tag := openingTag(t, body, `input id="cooldown"`)
	if !strings.Contains(tag, `value="`+strconv.Itoa(defaultCooldown)+`"`) {
		t.Errorf("the pause box does not hold the default: <%s>", tag)
	}
}

func TestNewForm_AsksForTheDesktopPageUnlessSomebodySaysOtherwise(t *testing.T) {
	// The box that used to stand here asked for the name of a template on the
	// operator's own service, and nothing ever opened one: every name it could
	// usefully hold was a name no machine had, and a job carrying one failed on
	// every query it made. What replaces it is a question with two answers, and
	// the one it starts on is the page most people mean by "the results".
	if form := blankForm(); form.Device != blanktrail.DeviceDesktop {
		t.Errorf("a new job asks Google for %q, want the desktop page", form.Device)
	}
	body := get(t, testServer(t), "/new").Body.String()
	if strings.Contains(body, `name="spec"`) {
		t.Errorf("the form still asks for a template name:\n%s", body)
	}
	// Both kinds are offered, and the desktop is the one already chosen.
	for _, device := range blanktrail.Devices() {
		if !strings.Contains(body, `value="`+device+`"`) {
			t.Errorf("the form does not offer %q:\n%s", device, body)
		}
	}
	if !strings.Contains(body, `value="`+blanktrail.DeviceDesktop+`" selected`) {
		t.Errorf("the form starts on something other than the desktop page:\n%s", body)
	}
}

func TestNewJob_OffersTheNumbersThatWereMeasured(t *testing.T) {
	// One identity a thread and five seconds between two requests on it. Both
	// are measured and both are in the code's own comments: an identity asked
	// every two seconds answered twelve requests before it was challenged and
	// one asked every five answered around forty, and three ports a thread —
	// which is what this offered while a thread stood still through its own
	// pause — buy nothing now that a thread works its other walks through it,
	// while every identity in play is one more the licensed solver has to carry.
	//
	// A default nobody meets is a default that does not matter, so this checks
	// the page a reader actually opens rather than the struct behind it.
	page := get(t, testServer(t), "/new").Body.String()
	for _, want := range []string{
		`id="ports" name="ports" type="number" min="1" value="1"`,
		`id="cooldown" name="cooldown" type="number" min="0" value="5"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the new-job form does not offer %s", want)
		}
	}
}

func TestNewJob_FilesTheProfileTheFormChose(t *testing.T) {
	// Which exits a job goes out through is the job's own now, so the form that
	// sets one up has to ask. A form that dropped the answer would file every
	// job against the default and say nothing, which is what every job did
	// before profiles existed and is exactly what this replaces.
	s := testServerWithSupervisor(t)
	ctx := t.Context()
	if _, err := s.store.CreateProfile(ctx, store.Profile{Name: "the default one"}); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	other, err := s.store.CreateProfile(ctx, store.Profile{Name: "the other one"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	// It is offered on the form, marked where the job would go without a choice.
	page := get(t, s, newAt).Body.String()
	if want := LangEN.T("form.profile"); !strings.Contains(page, want) {
		t.Errorf("the form does not ask which profile: %q is not on it", want)
	}
	if !strings.Contains(page, "the other one") {
		t.Error("the form does not offer every profile there is")
	}

	rec := postForm(t, s, "/new?do=start", url.Values{
		"name": {"through the other one"}, "queries": {"a"}, "pages": {"1"},
		"profile": {strconv.FormatInt(other, 10)},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("starting gave %d, want a redirect: %s", rec.Code, rec.Body)
	}
	jobs, err := s.store.Jobs(ctx, 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("%d jobs, want the one just started", len(jobs))
	}
	if jobs[0].ProfileID != other {
		t.Errorf("the job runs through profile %d, want the %d that was chosen", jobs[0].ProfileID, other)
	}
}

func TestJobPage_PointsAJobAtAnotherProfile(t *testing.T) {
	// The answer changes after a job is written down: a list that was refused
	// all afternoon is a job to point somewhere else and carry on, not a job to
	// set up again. It reaches the job the way the numbers beside it do — what
	// is written is what the next raise reads, so a job in flight keeps the pool
	// it already has.
	s := testServerWithSupervisor(t)
	ctx := t.Context()
	first, err := s.store.CreateProfile(ctx, store.Profile{Name: "first"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	second, err := s.store.CreateProfile(ctx, store.Profile{Name: "second"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	id, err := s.store.CreateJob(ctx, store.JobSpec{
		Name: "nightly", Pages: 1, ProfileID: first,
	}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	if page := get(t, s, jobPath(id)).Body.String(); !strings.Contains(page, LangEN.T("form.profile")) {
		t.Error("the job's own page does not offer to change the profile")
	}

	rec := postForm(t, s, "/api/reshape", url.Values{
		"job":     {strconv.FormatInt(id, 10)},
		"profile": {strconv.FormatInt(second, 10)},
		"threads": {"2"}, "ports": {"3"}, "tries": {"4"}, "cooldown": {"5"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("saving gave %d, want a redirect: %s", rec.Code, rec.Body)
	}
	sum, err := s.store.Progress(ctx, id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.ProfileID != second {
		t.Errorf("the job runs through profile %d, want the %d it was pointed at", sum.ProfileID, second)
	}
	// And the numbers beside it went through the same press, because a save that
	// wrote one of the two would be a save the reader cannot tell apart.
	if sum.Threads != 2 || sum.Ports != 3 || sum.Tries != 4 || sum.Cooldown != 5*time.Second {
		t.Errorf("the pool reads %d threads, %d ports, %d tries, %v pause",
			sum.Threads, sum.Ports, sum.Tries, sum.Cooldown)
	}
}

func TestCreateJob_TakesTheWholeListTickAndLeavesThePortsBoxUnread(t *testing.T) {
	// The tick is what is sent and the number beside it is not read. A form
	// that took both would let a job say two things about its pool — spend the
	// whole list, and open seven ports a thread — and the pool would have to
	// pick one without saying which.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":      {"whole"},
		"queries":   {"кондиционер"},
		"pages":     {"1"},
		"threads":   {"4"},
		"ports":     {"7"},
		"wholepool": {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the job was refused with %d: %s", rec.Code, rec.Body.String())
	}

	sums, err := s.store.Jobs(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(sums) != 1 {
		t.Fatalf("the history holds %d jobs, want the one", len(sums))
	}
	if !sums[0].WholePool {
		t.Error("the job was written without the tick that was sent")
	}
	if sums[0].Threads != 4 {
		t.Errorf("threads=%d, want the four that were asked for", sums[0].Threads)
	}
}

func TestJobPage_RefusesThePortsBoxWhileTheWholeListIsTicked(t *testing.T) {
	// Said on the page as well as in the handler. A box a job's settings ignore
	// must not be fillable: a reader who types into it and sees the number kept
	// has been told the run will use it.
	s := testServerWithSupervisor(t)
	if rec := postForm(t, s, "/new?do=start", url.Values{
		"name": {"whole"}, "queries": {"кондиционер"}, "pages": {"1"},
		"threads": {"4"}, "ports": {"7"}, "wholepool": {"1"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("the job was refused with %d", rec.Code)
	}
	sums, err := s.store.Jobs(t.Context(), 10)
	if err != nil || len(sums) != 1 {
		t.Fatalf("ListJobs: %v, %d jobs", err, len(sums))
	}

	body := get(t, s, jobPath(sums[0].ID)).Body.String()
	at := strings.Index(body, `name="ports"`)
	if at < 0 {
		t.Fatal("the job page does not show the ports box at all")
	}
	box := body[at : at+strings.Index(body[at:], ">")]
	if !strings.Contains(box, "disabled") {
		t.Errorf("the ports box is offered as %q, want it refused while the whole list is ticked", box)
	}
}
