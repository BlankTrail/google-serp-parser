// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/run"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

func TestCreateJob_FilesACompletionsJobAsTheGeneratorWouldRunIt(t *testing.T) {
	// One answer per question, no address to filter by, the completion's text
	// and nothing else kept — whatever the boxes for a search say — and the
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
	if got.Kind != store.KindSuggest || got.Pages != 1 || got.UniqueBy != store.UniqueOff {
		t.Errorf("filed as kind %q, %d pages, filter %q; want suggest, one page, no filter", got.Kind, got.Pages, got.UniqueBy)
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
