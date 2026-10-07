// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/semantic"
	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// scoreServer is a server that keeps a model and has a supervisor whose one job
// can be held, with the model in place when withModel says so.
func scoreServer(t *testing.T, withModel bool) (*Server, *Supervisor) {
	t.Helper()
	st := testStore(t)
	v := newSupervisor(st, &heldEngine{hold: make(chan struct{})})
	t.Cleanup(func() { _ = v.Close() })
	model := filepath.Join(t.TempDir(), semantic.FileName)
	body := tinyModelBytes(t)
	if withModel {
		if err := os.WriteFile(model, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(Config{
		Store: st, Supervisor: v, Logger: quiet(),
		SettingsPath: settingsFile(t, settings.Settings{ControlURL: "http://127.0.0.1:1", APIKey: "k"}),
		ModelPath:    model,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.modelSize = int64(len(body))
	return s, v
}

// collectedJob is a job of the kind given with two suggestions of one key,
// neither measured, as one collected before the model was downloaded is.
func collectedJob(t *testing.T, s *Server, kind string) int64 {
	t.Helper()
	id, err := s.store.CreateJob(t.Context(), store.JobSpec{Name: "old", Kind: kind, Pages: 1}, []string{"кофе"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	p := google.SERP{Results: []google.Result{{Position: 1, Title: "кофе"}, {Position: 2, Title: "tea"}}}
	if err := s.store.Record(t.Context(), id, store.QueryOutcome{Ordinal: 0, Pages: []google.SERP{p}}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	return id
}

func pressScore(t *testing.T, s *Server, id int64) *httptest.ResponseRecorder {
	t.Helper()
	return postForm(t, s, "/api/score", url.Values{"job": {strconv.FormatInt(id, 10)}})
}

func scoreIdle(t *testing.T, s *Server) {
	t.Helper()
	waitUntil(t, "the scoring is over", func() bool {
		s.scoring.mu.Lock()
		defer s.scoring.mu.Unlock()
		return !s.scoring.running
	})
}

const scoreAction = `action="/api/score"`

func TestScore_JobPageOffersTheButtonOnlyWhereItCanDoSomething(t *testing.T) {
	s, v := scoreServer(t, true)
	id := collectedJob(t, s, store.KindSuggest)
	if page := get(t, s, jobPath(id)).Body.String(); !strings.Contains(page, scoreAction) ||
		!strings.Contains(page, LangEN.T("job.score")) {
		t.Fatal("a collected suggestions job with a model is not offered the scoring")
	}
	// A search job has no suggestions to measure.
	if page := get(t, s, jobPath(collectedJob(t, s, store.KindParse))).Body.String(); strings.Contains(page, scoreAction) {
		t.Error("a search job is offered the scoring")
	}
	// A job whose suggestions are all measured has nothing left to do.
	done := collectedJob(t, s, store.KindSuggest)
	batch, _ := s.store.Unscored(t.Context(), done, 0, 10)
	if err := s.store.SetSimilarity(t.Context(), map[int64]float64{batch[0].ID: 0.1, batch[1].ID: 0.2}); err != nil {
		t.Fatal(err)
	}
	if page := get(t, s, jobPath(done)).Body.String(); strings.Contains(page, scoreAction) {
		t.Error("a job with every suggestion measured is offered the scoring")
	}
	// The job in flight is not offered it, whatever it has so far.
	running, err := v.Enqueue(store.JobSpec{Name: "live", Kind: store.KindSuggest, Pages: 1}, []string{"tea"})
	if err != nil {
		t.Fatal(err)
	}
	waitUntilRunning(t, v, running)
	p := google.SERP{Results: []google.Result{{Position: 1, Title: "tea pot"}}}
	if err := s.store.Record(t.Context(), running, store.QueryOutcome{Ordinal: 0, Pages: []google.SERP{p}}); err != nil {
		t.Fatal(err)
	}
	if page := get(t, s, jobPath(running)).Body.String(); strings.Contains(page, scoreAction) {
		t.Error("the job in flight is offered the scoring")
	}
}

func TestScore_JobPageHasNoButtonWithoutAModel(t *testing.T) {
	s, _ := scoreServer(t, false)
	id := collectedJob(t, s, store.KindSuggest)
	if page := get(t, s, jobPath(id)).Body.String(); strings.Contains(page, scoreAction) {
		t.Error("a server with no model file offers the scoring")
	}
	// A model of another length than the release says is damaged, though it loads.
	if err := os.WriteFile(s.semantic.Path(), tinyModelBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}
	s.semantic.Reset()
	s.modelSize++
	if page := get(t, s, jobPath(id)).Body.String(); strings.Contains(page, scoreAction) {
		t.Error("a server with a model of the wrong size offers the scoring")
	}
	pressScore(t, s, id)
	scoreIdle(t, s)
	if n, _ := s.store.UnscoredCount(t.Context(), id); n != 2 {
		t.Errorf("a model of the wrong size scored the job: %d left unmeasured, want 2", n)
	}
	s.modelSize--
	// A file that is not the model is not one either.
	if err := os.WriteFile(s.semantic.Path(), []byte("not a model"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.semantic.Reset()
	if page := get(t, s, jobPath(id)).Body.String(); strings.Contains(page, scoreAction) {
		t.Error("a server with a damaged model offers the scoring")
	}
}

func TestScore_APressMeasuresEveryRowAndTheButtonGoes(t *testing.T) {
	s, _ := scoreServer(t, true)
	id := collectedJob(t, s, store.KindSuggest)
	rec := pressScore(t, s, id)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != jobPath(id) {
		t.Fatalf("POST = %d to %q, want 303 to the job", rec.Code, rec.Header().Get("Location"))
	}
	scoreIdle(t, s)
	if _, done, total, fault := s.scoringOf(id); done != 2 || total != 2 || fault != "" {
		t.Errorf("the scoring reports %d of %d with fault %q, want 2 of 2 and none", done, total, fault)
	}
	if n, err := s.store.UnscoredCount(t.Context(), id); err != nil || n != 0 {
		t.Fatalf("%d suggestions still unmeasured (%v)", n, err)
	}
	m, err := s.semantic.Get()
	if err != nil {
		t.Fatal(err)
	}
	seen, nonzero := 0, false
	err = s.store.Rows(t.Context(), id, func(r store.Row) error {
		seen++
		if !r.Scored {
			t.Errorf("%q came back unscored", r.Title)
		}
		// What is kept is what the model says of the pair, not just a mark.
		if want := float64(m.Score(r.Query, r.Title)); r.Similarity != want {
			t.Errorf("%q kept %v, the model says %v", r.Title, r.Similarity, want)
		}
		nonzero = nonzero || r.Similarity != 0
		return nil
	})
	if err != nil || seen != 2 {
		t.Fatalf("rows = %d (%v)", seen, err)
	}
	if !nonzero {
		t.Error("every score is nought, so the test cannot tell a score from none")
	}
	if page := get(t, s, jobPath(id)).Body.String(); strings.Contains(page, scoreAction) {
		t.Error("the button stays after the job has been scored")
	}
}

func TestScore_PageFollowsARunningScoringOfItsOwnJobOnly(t *testing.T) {
	s, _ := scoreServer(t, true)
	id := collectedJob(t, s, store.KindSuggest)
	other := collectedJob(t, s, store.KindSuggest)
	s.scoring.mu.Lock()
	s.scoring.job, s.scoring.running, s.scoring.done, s.scoring.total = id, true, 1, 2
	s.scoring.mu.Unlock()
	page := get(t, s, jobPath(id)).Body.String()
	if !strings.Contains(page, LangEN.T("job.score.running")+" 1 / 2") {
		t.Error("the page does not say how far the scoring is")
	}
	if !strings.Contains(page, "data-refresh") {
		t.Error("the page does not redraw itself while it is scored")
	}
	if strings.Contains(page, scoreAction) {
		t.Error("the page offers a second scoring of the job being scored")
	}
	if page := get(t, s, jobPath(other)).Body.String(); strings.Contains(page, LangEN.T("job.score.running")) {
		t.Error("another job's page shows this job's scoring")
	}
}

func TestScore_PageTellsOfAScoringThatStopped(t *testing.T) {
	s, _ := scoreServer(t, true)
	id := collectedJob(t, s, store.KindSuggest)
	s.scoring.mu.Lock()
	s.scoring.job, s.scoring.fault = id, "job.score.failed"
	s.scoring.mu.Unlock()
	if page := get(t, s, jobPath(id)).Body.String(); !strings.Contains(page, LangEN.T("job.score.failed")) {
		t.Error("the page does not say the scoring stopped")
	}
}

func TestScore_RefusesWhatThePageDoesNotOffer(t *testing.T) {
	s, v := scoreServer(t, true)
	search := collectedJob(t, s, store.KindParse)
	pressScore(t, s, search)
	scoreIdle(t, s)
	if n, _ := s.store.UnscoredCount(t.Context(), search); n != 2 {
		t.Errorf("a search job was scored: %d left unmeasured, want 2", n)
	}

	running, err := v.Enqueue(store.JobSpec{Name: "live", Kind: store.KindSuggest, Pages: 1}, []string{"tea"})
	if err != nil {
		t.Fatal(err)
	}
	waitUntilRunning(t, v, running)
	p := google.SERP{Results: []google.Result{{Position: 1, Title: "tea pot"}}}
	if err := s.store.Record(t.Context(), running, store.QueryOutcome{Ordinal: 0, Pages: []google.SERP{p}}); err != nil {
		t.Fatal(err)
	}
	pressScore(t, s, running)
	scoreIdle(t, s)
	if n, _ := s.store.UnscoredCount(t.Context(), running); n != 1 {
		t.Errorf("the job in flight was scored: %d left unmeasured, want 1", n)
	}

	bare, _ := scoreServer(t, false)
	id := collectedJob(t, bare, store.KindSuggest)
	pressScore(t, bare, id)
	scoreIdle(t, bare)
	if n, _ := bare.store.UnscoredCount(t.Context(), id); n != 2 {
		t.Errorf("a job was scored with no model: %d left unmeasured, want 2", n)
	}
	// And a server that keeps no model at all answers 404.
	if rec := postForm(t, testServer(t), "/api/score", url.Values{"job": {"1"}}); rec.Code != http.StatusNotFound {
		t.Errorf("a server with no model answered %d, want 404", rec.Code)
	}
}

func TestScore_ASecondPressWhileOneRunsStartsNothing(t *testing.T) {
	s, _ := scoreServer(t, true)
	id := collectedJob(t, s, store.KindSuggest)
	// A scoring that is under way and, as far as anything can tell, stays so.
	s.scoring.mu.Lock()
	s.scoring.job, s.scoring.running, s.scoring.done, s.scoring.total = id, true, 0, 99
	s.scoring.mu.Unlock()
	pressScore(t, s, id)
	time.Sleep(100 * time.Millisecond)
	if n, _ := s.store.UnscoredCount(t.Context(), id); n != 2 {
		t.Errorf("a second scoring ran: %d left unmeasured, want 2", n)
	}
	s.scoring.mu.Lock()
	total := s.scoring.total
	s.scoring.mu.Unlock()
	if total != 99 {
		t.Errorf("the second press reset the first one's figures (total %d)", total)
	}
}
