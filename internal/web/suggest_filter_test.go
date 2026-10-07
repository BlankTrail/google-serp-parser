// SPDX-License-Identifier: MIT

package web

import (
	"strconv"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// filterJob makes a suggestions job of four answers to one key, each told by
// its last word: A is related and measured at 0.7, B related and measured at
// 0.4, C marked as having nothing of its key and measured at 0.9, and D related
// and never measured. With scored false none is measured, as a job collected
// before the model was downloaded.
func filterJob(t *testing.T, s *Server, scored bool) string {
	t.Helper()
	id, err := s.store.CreateJob(t.Context(), store.JobSpec{Name: "f", Kind: store.KindSuggest, Pages: 1,
		Fields: store.FieldsOf([]string{store.FieldTitle})}, []string{"coffee maker"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.store.Record(t.Context(), id, store.QueryOutcome{Ordinal: 0,
		Pages: []google.SERP{{Results: []google.Result{
			{Position: 1, Title: "coffee maker alpha"},
			{Position: 2, Title: "coffee maker bravo"},
			{Position: 3, Title: "charlie", Offtopic: true},
			{Position: 4, Title: "coffee maker delta"},
		}}}}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if scored {
		rows, err := s.store.Unscored(t.Context(), id, 0, 10)
		if err != nil || len(rows) != 4 {
			t.Fatalf("Unscored = %d rows (%v), want 4", len(rows), err)
		}
		if err := s.store.SetSimilarity(t.Context(), map[int64]float64{rows[0].ID: 0.7, rows[1].ID: 0.4, rows[2].ID: 0.9}); err != nil {
			t.Fatalf("SetSimilarity: %v", err)
		}
	}
	return strconv.FormatInt(id, 10)
}

// leftIn is which of the four answers a download of the job holds, by their
// last words, in order.
func leftIn(t *testing.T, s *Server, query, job string) string {
	t.Helper()
	body := get(t, s, "/export?format=txt&header=0&cols=suggestion&job="+job+query).Body.String()
	var words []string
	for line := range strings.SplitSeq(strings.TrimSpace(body), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			words = append(words, f[len(f)-1])
		}
	}
	return strings.Join(words, " ")
}

func TestExport_FiltersSuggestionsByWordsOrByMeaning(t *testing.T) {
	s := testServerWithSupervisor(t)
	job := filterJob(t, s, true)
	for _, c := range []struct{ query, want, why string }{
		{"&filter=none", "alpha bravo charlie delta", "no filter keeps all"},
		{"&filter=words", "alpha bravo delta", "by words leaves out the marked one only"},
		{"&filter=meaning&min=0.5", "alpha delta", "by meaning leaves out the marked and the measured below the threshold, never the unmeasured"},
		{"", "alpha delta", "nothing said on a job with scores is by meaning at the default threshold"},
		{"&offtopic=1", "alpha bravo charlie delta", "the old box keeps everything, as links made before it do"},
		{"&filter=meaning&min=0.35", "alpha bravo delta", "a lower threshold leaves less out"},
		{"&filter=meaning&min=junk", "alpha delta", "a threshold that is no number is the default"},
		{"&filter=meaning&min=NaN", "alpha delta", "NaN is no number either and is the default"},
		{"&filter=meaning&min=0.1", "alpha bravo delta", "a threshold below the slider's range is its low end, 0.30, not the default"},
		{"&filter=meaning&min=-1", "alpha bravo delta", "a negative threshold is the slider's low end"},
		{"&filter=meaning&min=0.85", "delta", "a threshold above the slider's range is its high end, 0.80"},
		{"&filter=meaning&min=7", "delta", "a threshold far above the range is its high end too"},
	} {
		if got := leftIn(t, s, c.query, job); got != c.want {
			t.Errorf("%s: %q holds %q, want %q", c.why, c.query, got, c.want)
		}
	}
	// A preview is the file's first lines and is filtered the same way.
	if p := get(t, s, "/export/preview?format=txt&header=0&cols=suggestion&filter=meaning&min=0.5&job="+job).Body.String(); strings.Contains(p, "bravo") || !strings.Contains(p, "delta") {
		t.Errorf("the preview of a filter by meaning is %q, want alpha and delta", p)
	}

	// A job nobody measured is filtered by its words, and cannot be asked to be
	// filtered by meaning: there is nothing to measure by.
	bare := filterJob(t, s, false)
	for _, query := range []string{"", "&filter=words", "&filter=meaning&min=0.9"} {
		if got := leftIn(t, s, query, bare); got != "alpha bravo delta" {
			t.Errorf("%q on a job with no scores holds %q, want it filtered by words", query, got)
		}
	}
	if got := leftIn(t, s, "&filter=none", bare); got != "alpha bravo charlie delta" {
		t.Errorf("no filter on a job with no scores holds %q, want all", got)
	}
}

func TestExport_TheClosenessColumnIsEmptyWhereNothingWasMeasured(t *testing.T) {
	s := testServerWithSupervisor(t)
	job := filterJob(t, s, true)
	file := get(t, s, "/export?format=csv&filter=none&cols=suggestion,similarity&job="+job).Body.String()
	for _, want := range []string{"coffee maker alpha,0.700\n", "coffee maker bravo,0.400\n", "charlie,0.900\n", "coffee maker delta,\n"} {
		if !strings.Contains(file, want) {
			t.Errorf("the file is %q, want the line %q", file, want)
		}
	}
	if def := get(t, s, "/export?format=csv&job="+job).Body.String(); !strings.HasPrefix(def, "ordinal,key,suggestion\n") {
		t.Errorf("the file with nothing said begins %q, want the closeness column left out", def[:min(len(def), 40)])
	}
	if tab := get(t, s, exportsAt+"?job="+job).Body.String(); strings.Contains(tab, `name="cols" value="similarity" checked`) ||
		!strings.Contains(tab, `name="cols" value="similarity"`) {
		t.Error("the tab does not offer the closeness column unticked")
	}
}

func TestExportsTab_OffersTheFilterAndCountsWhatItLeavesOut(t *testing.T) {
	s := testServerWithSupervisor(t)
	job := filterJob(t, s, true)

	tab := get(t, s, exportsAt+"?filter=meaning&min=0.5&job="+job).Body.String()
	for _, want := range []string{`<select name="filter">`, `value="none"`, `value="words"`, `value="meaning" selected`,
		`<input type="range" name="min"`, `value="0.50"`, "Left out: 2 / 4 (50.0%)", "filter=meaning", "min=0.50"} {
		if !strings.Contains(tab, want) {
			t.Errorf("the tab with a filter by meaning lacks %q", want)
		}
	}
	if strings.Contains(tab, "bravo") || !strings.Contains(tab, "alpha") {
		t.Error("the tab's own preview is not filtered like the file")
	}
	if strings.Contains(tab, `name="offtopic"`) {
		t.Error("the tab still offers the box that came before the filter")
	}
	// Nothing said on a job with scores opens on the meaning at the default
	// threshold, shown as two decimals; a threshold that is no number is the
	// default, and one out of the slider's range is the slider's nearer end.
	for query, want := range map[string]string{
		"": "0.54", "&filter=meaning&min=junk": "0.54",
		"&filter=meaning&min=9": "0.80", "&filter=meaning&min=0.05": "0.30",
	} {
		page := get(t, s, exportsAt+"?job="+job+query).Body.String()
		if !strings.Contains(page, `value="meaning" selected`) || !strings.Contains(page, `value="`+want+`"`) ||
			!strings.Contains(page, `min="0.30" max="0.80"`) {
			t.Errorf("%q: the tab does not open on the meaning at %s within 0.30 to 0.80", query, want)
		}
	}
	// By words the slider is gone and the count is of the marked.
	words := get(t, s, exportsAt+"?filter=words&job="+job).Body.String()
	if strings.Contains(words, `type="range"`) || !strings.Contains(words, "Left out: 1 / 4 (25.0%)") {
		t.Error("the tab by words shows a slider, or does not count the one marked")
	}
	// With no filter there is nothing to count.
	none := get(t, s, exportsAt+"?filter=none&job="+job).Body.String()
	if strings.Contains(none, "Left out:") || strings.Contains(none, `type="range"`) || !strings.Contains(none, "bravo") {
		t.Error("the tab with no filter counts something left out, shows a slider, or hides a suggestion")
	}

	// A job nobody measured offers the words and no meaning.
	bare := get(t, s, exportsAt+"?filter=meaning&job="+filterJob(t, s, false)).Body.String()
	if !strings.Contains(bare, `<select name="filter">`) || strings.Contains(bare, `value="meaning"`) ||
		!strings.Contains(bare, `value="words" selected`) || strings.Contains(bare, `type="range"`) {
		t.Error("the tab of a job with no scores offers the meaning, or does not open on the words")
	}

	search, err := s.store.CreateJob(t.Context(), store.JobSpec{Name: "s", Pages: 1}, []string{"coffee"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if strings.Contains(get(t, s, exportsAt+"?job="+strconv.FormatInt(search, 10)).Body.String(), `name="filter"`) {
		t.Error("the tab of a search offers a filter of suggestions")
	}
}
