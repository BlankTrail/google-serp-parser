// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/run"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

func TestCreateJob_FilesACompletionsJobAsTheGeneratorWouldRunIt(t *testing.T) {
	// One answer per question, repeats told apart by the text — a filter by
	// address named is a filter by text, there being no address — the
	// completion's text and nothing else kept, whatever the boxes for a search
	// say, and the
	// generator's Multiword and the cap on one key's questions carried with it.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":         {"completions"},
		"kind":         {store.KindSuggest},
		"queries":      {"coffee maker"},
		"pages":        {"5"},
		"unique":       {string(store.UniqueHost)},
		"keep":         {store.FieldURL, store.FieldSnippet},
		"chose":        {"1"},
		"multiword":    {"on"},
		"suggestlimit": {"120"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("starting gave %d, want a redirect: %s", rec.Code, rec.Body)
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	got := jobs[0]
	if got.Kind != store.KindSuggest || got.Pages != 1 || got.UniqueBy != store.UniqueText {
		t.Errorf("filed as kind %q, %d pages, filter %q; want suggest, one page, by text", got.Kind, got.Pages, got.UniqueBy)
	}
	if got.Fields != store.FieldsOf([]string{store.FieldTitle}) {
		t.Errorf("keeps %q, want the completion's text alone", got.Fields)
	}
	if !got.Multiword || got.SuggestLimit != 120 {
		t.Errorf("multiword %v and a cap of %d, want both as the form gave them", got.Multiword, got.SuggestLimit)
	}
}

func TestCreateJob_RefusesANegativeCapOnAKeysQuestions(t *testing.T) {
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name":         {"completions"},
		"kind":         {store.KindSuggest},
		"queries":      {"coffee"},
		"suggestlimit": {"-1"},
	})
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a negative cap was taken")
	}
}

func TestRunKind_RunsACompletionsJobAsOne(t *testing.T) {
	if runKind(store.KindSuggest) != run.Suggest {
		t.Error("a completions job would be run as something else")
	}
}

func TestSupervisor_HandsACompletionsJobItsWayOfTyping(t *testing.T) {
	// Filed with Multiword and a cap, a completions job reaches the engine with
	// both: lost on the way, it would be run as the generator without its box
	// ticked, and with no bound on what one key may cost.
	v, _, eng := heldSupervisor(t)
	if _, err := v.Enqueue(store.JobSpec{Name: "j", Kind: store.KindSuggest, Pages: 1,
		Multiword: true, SuggestLimit: 40}, []string{"coffee maker"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitUntil(t, "the job has reached the engine", func() bool {
		_, taken := eng.ran(0)
		return taken
	})
	got, _ := eng.ran(0)
	if got.Kind != run.Suggest || !got.Multiword || got.SuggestLimit != 40 {
		t.Errorf("the engine was handed kind %v, multiword %v, cap %d; want suggest, true, 40",
			got.Kind, got.Multiword, got.SuggestLimit)
	}
}

func TestCreateJob_DropsACompletionsJobsRepeatsUnlessAskedToKeepThem(t *testing.T) {
	// The user's rule: a completions job carries each completion once by
	// default. A form that never offered the choice gets the filter by text;
	// one that chose to keep everything keeps everything.
	for _, c := range []struct {
		name string
		form url.Values
		want store.UniqueBy
	}{
		{"nothing said", url.Values{}, store.UniqueText},
		{"keep everything chosen", url.Values{"unique": {""}}, store.UniqueOff},
		{"by text chosen", url.Values{"unique": {string(store.UniqueText)}}, store.UniqueText},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServerWithSupervisor(t)
			form := url.Values{"name": {"c"}, "kind": {store.KindSuggest}, "queries": {"coffee"}}
			for k, v := range c.form {
				form[k] = v
			}
			if rec := postForm(t, s, "/new?do=start", form); rec.Code != http.StatusSeeOther {
				t.Fatalf("starting gave %d: %s", rec.Code, rec.Body)
			}
			jobs, err := s.store.Jobs(t.Context(), 0)
			if err != nil {
				t.Fatalf("Jobs: %v", err)
			}
			if jobs[0].UniqueBy != c.want {
				t.Errorf("filed with filter %q, want %q", jobs[0].UniqueBy, c.want)
			}
		})
	}
}

func TestCreateJob_RefusesTheFilterByTextForASearch(t *testing.T) {
	// A search filtered by its titles would drop different pages that share one.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{"name": {"s"}, "kind": {store.KindParse},
		"queries": {"coffee"}, "unique": {string(store.UniqueText)}})
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a search was filed with the filter by text")
	}
	if want := LangEN.T("form.unique.textonly"); !strings.Contains(rec.Body.String(), want) {
		t.Errorf("the form does not say %q: %s", want, rec.Body)
	}
}

func TestJobPage_DrawsACompletionsJobAsKeysAndSuggestions(t *testing.T) {
	// A completion has a key and a text: no place in a list, no address, no
	// pages, no sessions. The speeds read keys and requests a minute.
	s := testServerWithSupervisor(t)
	id, err := s.store.CreateJob(t.Context(), store.JobSpec{Name: "c", Kind: store.KindSuggest, Pages: 1,
		Fields: store.FieldsOf([]string{store.FieldTitle})}, []string{"coffee"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.store.Record(t.Context(), id, store.QueryOutcome{Ordinal: 0,
		Pages: []google.SERP{{Results: []google.Result{{Position: 1, Title: "coffee maker"}}}}}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	body := get(t, s, jobPath(id)+"?lang=en").Body.String()
	for _, want := range []string{LangEN.T("job.result.key"), LangEN.T("job.result.suggestion"), "coffee maker",
		LangEN.T("job.speed.keys"), LangEN.T("job.speed.requests")} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not say %q", want)
		}
	}
	for _, unwanted := range []string{LangEN.T("history.rank"), LangEN.T("history.address"),
		">" + LangEN.T("job.speed.pages") + "<", "<dt>" + LangEN.T("form.pages") + "</dt>"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the page still says %q", unwanted)
		}
	}
	sum, err := s.store.Progress(t.Context(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if got := strings.Join(chosenOf(sum, partResults), ","); got != "ordinal,key,suggestion" {
		t.Errorf("a completions job exports with %s when nobody chose, want the key and the text alone", got)
	}
	// Whether a suggestion is related to its key is offered, not given: a file
	// that leaves out the unrelated ones would say yes on every line.
	if got := strings.Join(fieldsOf(sum, partResults), ","); got != "ordinal,key,suggestion,related" {
		t.Errorf("a completions job offers %s, want the key, the text and whether it is related", got)
	}
}

func TestNewJob_WarnsThatASuggestionsJobWithNoLanguageTypesInLatinLetters(t *testing.T) {
	// Said, not refused: the form comes back for another reason, and the
	// warning stands shown for a suggestions job with no language and hidden
	// for one that named its language.
	warning := LangEN.T("form.suggest.nolang")
	for _, c := range []struct {
		language string
		shown    bool
	}{{"", true}, {"ru", false}} {
		s := testServerWithSupervisor(t)
		rec := postForm(t, s, "/new?do=start", url.Values{"kind": {store.KindSuggest},
			"queries": {"купить кофе"}, "language": {c.language}})
		body := rec.Body.String()
		if !strings.Contains(body, warning) {
			t.Fatalf("language %q: the form does not carry the warning at all", c.language)
		}
		shown := strings.Contains(body, `id="suggest-nolang">`)
		if shown != c.shown {
			t.Errorf("language %q: the warning is shown %v, want %v", c.language, shown, c.shown)
		}
	}
}

func TestNewJob_CallsASuggestionsJobsTriesPerRequestAndPutsAwayTheRest(t *testing.T) {
	// A suggestions job retries each substitution and has no sessions to rest:
	// the form, come back for another reason, says the one and hides the other.
	s := testServerWithSupervisor(t)
	body := postForm(t, s, "/new?do=start", url.Values{"kind": {store.KindSuggest}, "queries": {"coffee"},
		"language": {"en"}}).Body.String()
	if !strings.Contains(body, `<span data-kind="suggest">`+LangEN.T("form.tries.suggest")+`</span>`) {
		t.Error("the tries are not called tries per request")
	}
	if !strings.Contains(body, `<span data-kind="other" hidden>`+LangEN.T("form.tries")+`</span>`) {
		t.Error("tries per phrase still shows")
	}
	if !strings.Contains(body, `<div class="field" hidden>`+"\n"+`<label for="cooldown">`) {
		t.Error("the session rest still shows")
	}
	search := postForm(t, s, "/new?do=start", url.Values{"kind": {store.KindParse}, "queries": {"coffee"}}).Body.String()
	if strings.Contains(search, `<div class="field" hidden>`+"\n"+`<label for="cooldown">`) {
		t.Error("the session rest is hidden for a search")
	}
}

func TestJobPage_CallsASuggestionsJobsTriesPerRequestAndPutsAwayTheRest(t *testing.T) {
	s := testServerWithSupervisor(t)
	id, err := s.store.CreateJob(t.Context(), store.JobSpec{Name: "c", Kind: store.KindSuggest, Pages: 1}, []string{"coffee"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	body := get(t, s, jobPath(id)+"?lang=en").Body.String()
	if !strings.Contains(body, `<label for="tries">`+LangEN.T("form.tries.suggest")+`</label>`) {
		t.Error("the job page does not call the tries per request")
	}
	if strings.Contains(body, LangEN.T("form.rest.why")) {
		t.Error("the job page still explains a session's rest")
	}
}

func TestExports_NameASuggestionsJobsColumnsForWhatTheyAre(t *testing.T) {
	// On the export tab and in the file, a suggestions job's columns are the
	// key and the suggestion, its part its suggestions — not the phrase, the
	// title and the results a search's are.
	s := testServerWithSupervisor(t)
	id, err := s.store.CreateJob(t.Context(), store.JobSpec{Name: "c", Kind: store.KindSuggest, Pages: 1,
		Fields: store.FieldsOf([]string{store.FieldTitle})}, []string{"coffee"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.store.Record(t.Context(), id, store.QueryOutcome{Ordinal: 0,
		Pages: []google.SERP{{Results: []google.Result{{Position: 1, Title: "coffee maker"}}}}}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	tab := get(t, s, exportsAt+"?lang=ru&job="+strconv.FormatInt(id, 10)).Body.String()
	for _, want := range []string{LangRU.T("export.field.keyordinal"), LangRU.T("export.field.key"),
		LangRU.T("export.field.suggestion"), LangRU.T("exports.part.suggestions")} {
		if !strings.Contains(tab, want) {
			t.Errorf("the tab does not say %q", want)
		}
	}
	for _, unwanted := range []string{">" + LangRU.T("export.field.query") + "<", ">" + LangRU.T("field.title") + "<"} {
		if strings.Contains(tab, unwanted) {
			t.Errorf("the tab still says %q", unwanted)
		}
	}
	file := get(t, s, "/export?format=csv&cols=ordinal,key,suggestion&job="+strconv.FormatInt(id, 10)).Body.String()
	if !strings.HasPrefix(strings.TrimPrefix(file, "\ufeff"), "ordinal,key,suggestion\n") ||
		!strings.Contains(file, "0,coffee,coffee maker") {
		t.Errorf("the file begins %q, want a header of key and suggestion and the row", file[:min(len(file), 80)])
	}
	// Columns remembered from a search are no choice for this part.
	stale := get(t, s, exportsAt+"?lang=ru&cols=query,title&job="+strconv.FormatInt(id, 10)).Body.String()
	if strings.Count(stale, `name="cols" value="key" checked`)+strings.Count(stale, `name="cols" value="suggestion" checked`) != 2 {
		t.Error("columns of another kind left this part with nothing ticked")
	}
}

func TestSupervisor_AsksForKeptAliveConnectionsForASuggestionsJobAlone(t *testing.T) {
	// A suggestions job's ports carry no sessions and keep their connections;
	// a search's open one a request, as they were measured to want.
	pools := &raisedPools{}
	v, _ := raisingSupervisor(t, pools, 1, 1)
	for _, kind := range []string{store.KindParse, store.KindSuggest} {
		if eng, err := v.raise(t.Context(), source{raise: pools.raise}, store.JobSummary{
			Kind: kind, Ports: 1, Threads: 1, Device: "desktop"}); err == nil && eng != nil {
			_ = eng.Close()
		}
	}
	pools.mu.Lock()
	defer pools.mu.Unlock()
	if len(pools.asked) != 2 || pools.asked[0].KeepAlive || !pools.asked[1].KeepAlive {
		t.Errorf("asked %+v, want a search without and a suggestions job with kept-alive connections", pools.asked)
	}
}

func TestExport_LeavesOutTheSuggestionsUnrelatedToTheKeyUnlessAskedFor(t *testing.T) {
	// A suggestion marked as having nothing of its key in it is set aside by
	// the file and the preview alike unless the box keeps it; the column that
	// says which is which is offered and not ticked; and the box is a
	// suggestions job's alone.
	s := testServerWithSupervisor(t)
	id, err := s.store.CreateJob(t.Context(), store.JobSpec{Name: "c", Kind: store.KindSuggest, Pages: 1,
		Fields: store.FieldsOf([]string{store.FieldTitle})}, []string{"coffee maker"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.store.Record(t.Context(), id, store.QueryOutcome{Ordinal: 0,
		Pages: []google.SERP{{Results: []google.Result{
			{Position: 1, Title: "coffee maker app"},
			{Position: 2, Title: "kafka on the shore", Offtopic: true},
		}}}}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	job := strconv.FormatInt(id, 10)

	file := get(t, s, "/export?format=csv&job="+job).Body.String()
	if !strings.Contains(file, "coffee maker app") || strings.Contains(file, "kafka on the shore") {
		t.Errorf("the file with nothing said is %q, want the related suggestion alone", file)
	}
	if !strings.HasPrefix(file, "ordinal,key,suggestion\n") {
		t.Errorf("the file with nothing said begins %q, want the related column left out", file[:min(len(file), 40)])
	}
	kept := get(t, s, "/export?format=csv&offtopic=1&cols=key,suggestion,related&job="+job).Body.String()
	if !strings.Contains(kept, "coffee maker,coffee maker app,true") || !strings.Contains(kept, "coffee maker,kafka on the shore,false") {
		t.Errorf("the file keeping them is %q, want both, each saying whether it is related", kept)
	}

	preview := get(t, s, "/export/preview?format=csv&job="+job).Body.String()
	if strings.Contains(preview, "kafka on the shore") {
		t.Errorf("the preview showed the unrelated suggestion: %q", preview)
	}
	if !strings.Contains(get(t, s, "/export/preview?format=csv&offtopic=1&job="+job).Body.String(), "kafka on the shore") {
		t.Error("the preview asked to keep the unrelated suggestion left it out")
	}

	tab := get(t, s, exportsAt+"?job="+job).Body.String()
	if strings.Contains(tab, "kafka on the shore") {
		t.Error("the tab's own preview showed the unrelated suggestion")
	}
	if !strings.Contains(get(t, s, exportsAt+"?format=csv&offtopic=1&cols=key,suggestion&job="+job).Body.String(), "kafka on the shore") {
		t.Error("the tab's own preview asked to keep the unrelated suggestion left it out")
	}
	if !strings.Contains(tab, `name="offtopic"`) {
		t.Error("the tab of a suggestions job does not offer to keep the unrelated suggestions")
	}
	if strings.Contains(tab, `name="cols" value="related" checked`) {
		t.Error("the tab ticks the related column on a screen nobody chose")
	}
	search, err := s.store.CreateJob(t.Context(), store.JobSpec{Name: "s", Pages: 1}, []string{"coffee"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if strings.Contains(get(t, s, exportsAt+"?job="+strconv.FormatInt(search, 10)).Body.String(), `name="offtopic"`) {
		t.Error("the tab of a search offers a box about suggestions")
	}
}
