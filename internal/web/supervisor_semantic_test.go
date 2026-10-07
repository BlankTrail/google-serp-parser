// SPDX-License-Identifier: MIT

package web

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/semantic"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// plannedAs plans a job of the kind given, on a supervisor that keeps the
// holder given, and hands back what the run would be told.
func plannedAs(t *testing.T, kind string, h *semantic.Holder, logged *bytes.Buffer) func(key string, completions []string) []float32 {
	t.Helper()
	v, st := supervisorOn(t, &heldEngine{hold: make(chan struct{})})
	if logged != nil {
		v.log = slog.New(slog.NewTextHandler(logged, nil))
	}
	if h != nil {
		v.SetSemantic(h)
	}
	id, err := st.CreateJob(t.Context(), store.JobSpec{Name: "p", Kind: kind, Target: targetFor(kind), Pages: 1}, []string{"coffee"})
	if err != nil {
		t.Fatal(err)
	}
	j, _, err := v.plan(t.Context(), id)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return j.Similar
}

// targetFor is the site a position job must carry; no other kind takes one.
func targetFor(kind string) string {
	if kind == store.KindPosition {
		return "example.com"
	}
	return ""
}

func holderWith(t *testing.T, contents []byte) *semantic.Holder {
	t.Helper()
	path := filepath.Join(t.TempDir(), semantic.FileName)
	if contents != nil {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return semantic.NewHolder(path)
}

func TestPlan_ScoresASuggestionsJobWhenAModelIsDownloaded(t *testing.T) {
	h := holderWith(t, tinyModelBytes(t))
	if plannedAs(t, store.KindSuggest, h, nil) == nil {
		t.Error("a suggestions job planned with a model downloaded has nothing to score with")
	}
}

func TestPlan_LeavesAJobWithoutScoresWhereItHasNoModelOrIsNotOfSuggestions(t *testing.T) {
	h := holderWith(t, tinyModelBytes(t))
	for _, kind := range []string{store.KindParse, store.KindPosition, store.KindIndex} {
		if plannedAs(t, kind, h, nil) != nil {
			t.Errorf("a %s job was given a way to score suggestions", kind)
		}
	}
	var logged bytes.Buffer
	if plannedAs(t, store.KindSuggest, holderWith(t, nil), &logged) != nil {
		t.Error("a job planned with no model file was given a way to score")
	}
	if logged.Len() != 0 {
		t.Errorf("a missing model is no event, yet the log says %q", logged.String())
	}
	if plannedAs(t, store.KindSuggest, nil, nil) != nil {
		t.Error("a supervisor handed no holder scored a job")
	}
}

func TestPlan_RunsWithoutScoresAndSaysSoOnceWhenTheModelFileIsDamaged(t *testing.T) {
	var logged bytes.Buffer
	if plannedAs(t, store.KindSuggest, holderWith(t, []byte("not a model")), &logged) != nil {
		t.Error("a damaged model scored a job")
	}
	if n := strings.Count(logged.String(), "could not be read"); n != 1 {
		t.Errorf("the damaged model was reported %d times in %q, want once", n, logged.String())
	}
}

func TestNew_HandsTheSupervisorTheHolderOfTheModel(t *testing.T) {
	v, _ := supervisorOn(t, &heldEngine{hold: make(chan struct{})})
	s, err := New(Config{Store: testStore(t), Logger: quiet(), Supervisor: v,
		ModelPath: filepath.Join(t.TempDir(), semantic.FileName)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if v.semantic.Load() == nil || v.semantic.Load() != s.semantic {
		t.Error("the supervisor was not given the holder the settings page downloads into")
	}
}
