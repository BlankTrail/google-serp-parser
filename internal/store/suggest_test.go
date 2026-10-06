// SPDX-License-Identifier: MIT

package store

import "testing"

func TestJobs_KeepHowACompletionsJobTypesItsKeysWhicheverWayTheyAreWritten(t *testing.T) {
	// A completions job written whole or a list at a time, and taken up again
	// by name, carries Multiword and its cap through every one of them: lost
	// anywhere, a resumed run would type its keys differently from the run it
	// carries on.
	s := testStore(t)
	spec := JobSpec{Name: "whole", Kind: KindSuggest, Pages: 1, Multiword: true, SuggestLimit: 70}
	whole, err := s.CreateJob(t.Context(), spec, []string{"coffee maker"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	spec.Name = "listed"
	p, err := s.OpenPlan(t.Context(), spec)
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	if err := p.Add(t.Context(), "tea pot"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := p.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, id := range []int64{whole, p.JobID()} {
		sum, err := s.Progress(t.Context(), id)
		if err != nil {
			t.Fatalf("Progress: %v", err)
		}
		if sum.Kind != KindSuggest || !sum.Multiword || sum.SuggestLimit != 70 {
			t.Errorf("job %d reads back as %q, multiword %v, cap %d; want suggest, true, 70",
				id, sum.Kind, sum.Multiword, sum.SuggestLimit)
		}
	}
	back, err := s.LastUnfinished(t.Context(), "listed")
	if err != nil {
		t.Fatalf("LastUnfinished: %v", err)
	}
	if !back.Spec.Multiword || back.Spec.SuggestLimit != 70 {
		t.Errorf("taken up again with multiword %v and a cap of %d, want true and 70",
			back.Spec.Multiword, back.Spec.SuggestLimit)
	}
}

func TestJobs_RefuseANegativeCapOnAKeysQuestions(t *testing.T) {
	// The column is never written below nought, which is no cap.
	s := testStore(t)
	id, err := s.CreateJob(t.Context(), JobSpec{Name: "j", Kind: KindSuggest, Pages: 1, SuggestLimit: -5},
		[]string{"coffee"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	sum, err := s.Progress(t.Context(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.SuggestLimit != 0 {
		t.Errorf("a cap of -5 was kept as %d, want nought", sum.SuggestLimit)
	}
}
