// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/google"
)

// suggestJob makes a completions job for key with one answer per text, none of
// them measured, and returns its id.
func suggestJob(t *testing.T, s *Store, name, key string, texts ...string) int64 {
	t.Helper()
	id, err := s.CreateJob(t.Context(), JobSpec{Name: name, Kind: KindSuggest, Pages: 1}, []string{key})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	p := google.SERP{}
	for i, x := range texts {
		p.Results = append(p.Results, google.Result{Position: i + 1, Title: x})
	}
	if err := s.Record(t.Context(), id, QueryOutcome{Ordinal: 0, Pages: []google.SERP{p}}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	return id
}

func TestUnscored_WalksAJobsUnmeasuredSuggestionsInBatches(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	id := suggestJob(t, s, "mine", "coffee", "coffee maker", "coffee shop", "coffee beans")
	// Another job's rows must never be handed over, nor counted.
	other := suggestJob(t, s, "other", "tea", "tea pot")

	first, err := s.Unscored(ctx, id, 0, 2)
	if err != nil {
		t.Fatalf("Unscored: %v", err)
	}
	if len(first) != 2 || first[0].Key != "coffee" || first[0].Text != "coffee maker" ||
		first[1].Text != "coffee shop" || first[0].ID >= first[1].ID {
		t.Fatalf("first batch = %+v, want the first two in id order with their key", first)
	}
	if err := s.SetSimilarity(ctx, map[int64]float64{first[0].ID: 0.5, first[1].ID: 0}); err != nil {
		t.Fatalf("SetSimilarity: %v", err)
	}
	if n, _ := s.UnscoredCount(ctx, id); n != 1 {
		t.Errorf("UnscoredCount = %d, want 1 (a score of nought is a measurement)", n)
	}
	if n, _ := s.UnscoredCount(ctx, other); n != 1 {
		t.Errorf("the other job counts %d, want its own 1", n)
	}
	// Walked from the start, the measured ones are not handed over again.
	again, err := s.Unscored(ctx, id, 0, 10)
	if err != nil || len(again) != 1 || again[0].Text != "coffee beans" {
		t.Errorf("a walk from the start = %+v (%v), want only the unmeasured one", again, err)
	}
	rest, err := s.Unscored(ctx, id, first[1].ID, 10)
	if err != nil {
		t.Fatalf("Unscored: %v", err)
	}
	if len(rest) != 1 || rest[0].Text != "coffee beans" {
		t.Errorf("the rest = %+v, want only the one not yet measured", rest)
	}
	// after is exclusive: the row it names is not handed over a second time.
	one, _ := s.Unscored(ctx, other, 0, 1)
	if next, _ := s.Unscored(ctx, other, one[0].ID, 1); len(next) != 0 {
		t.Errorf("after named a row and it came back: %+v", next)
	}
	// after skips ids at or below it even for rows still unmeasured.
	skipped, _ := s.Unscored(ctx, other, 1<<40, 10)
	if len(skipped) != 0 {
		t.Errorf("after the last id still handed over %+v", skipped)
	}
}

func TestSetSimilarity_WritesAllOrNone(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	id := suggestJob(t, s, "j", "coffee", "coffee maker", "coffee shop")
	batch, _ := s.Unscored(ctx, id, 0, 10)
	// A cancelled context fails the transaction; nothing may stick.
	dead, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.SetSimilarity(dead, map[int64]float64{batch[0].ID: 1, batch[1].ID: 1}); err == nil {
		t.Fatal("a cancelled write succeeded")
	}
	if n, _ := s.UnscoredCount(ctx, id); n != 2 {
		t.Errorf("%d unmeasured after a failed write, want both still unmeasured", n)
	}
	// A failing row in the middle (a statement error) must roll back the rest.
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER stop BEFORE UPDATE ON results
		WHEN NEW.id = `+strconv.FormatInt(batch[1].ID, 10)+` BEGIN SELECT RAISE(ABORT, 'no'); END`); err != nil {
		t.Fatal(err)
	}
	err := s.SetSimilarity(ctx, map[int64]float64{batch[0].ID: 1, batch[1].ID: 1})
	if err == nil {
		t.Fatal("a write with a failing row succeeded")
	}
	if n, _ := s.UnscoredCount(ctx, id); n != 2 {
		t.Errorf("%d unmeasured after a half-failed write, want both: the first row stuck", n)
	}
}

// planOf is the query plan SQLite chooses for a statement, as one string.
func planOf(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	return plan
}

func TestUnscored_ReadsThroughThePartialIndexAndNotTheWholeTable(t *testing.T) {
	// On a job of half a million rows the walk and the count must find the few
	// unmeasured ones by the index kept for them, not by reading every row.
	s := testStore(t)
	id := suggestJob(t, s, "j", "coffee", "coffee maker")
	walk := planOf(t, s, unscoredSQL, id, 0, 10)
	count := planOf(t, s, unscoredCountSQL, id)
	if !strings.Contains(walk, "results_unscored") {
		t.Errorf("the walk does not use the partial index:\n%s", walk)
	}
	if !strings.Contains(count, "results_unscored") {
		t.Errorf("the count does not use the partial index:\n%s", count)
	}
}

// filteredJob is a suggestions job of four answers to one key: A related and
// measured at 0.8, B related at 0.2, C marked as having nothing of its key at
// 0.9, and D related and never measured.
func filteredJob(t *testing.T, s *Store, name string, measured bool) int64 {
	t.Helper()
	id, err := s.CreateJob(t.Context(), JobSpec{Name: name, Kind: KindSuggest, Pages: 1}, []string{"coffee"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	p := google.SERP{Results: []google.Result{
		{Position: 1, Title: "coffee alpha"}, {Position: 2, Title: "coffee bravo"},
		{Position: 3, Title: "charlie", Offtopic: true}, {Position: 4, Title: "coffee delta"},
	}}
	if err := s.Record(t.Context(), id, QueryOutcome{Ordinal: 0, Pages: []google.SERP{p}}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if measured {
		rows, err := s.Unscored(t.Context(), id, 0, 10)
		if err != nil || len(rows) != 4 {
			t.Fatalf("Unscored = %d (%v), want 4", len(rows), err)
		}
		if err := s.SetSimilarity(t.Context(), map[int64]float64{rows[0].ID: 0.8, rows[1].ID: 0.2, rows[2].ID: 0.9}); err != nil {
			t.Fatalf("SetSimilarity: %v", err)
		}
	}
	return id
}

func TestFilterCount_CountsWhatTheExportLeavesOut(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	id := filteredJob(t, s, "mine", true)
	// Another job's rows are neither counted in nor out.
	filteredJob(t, s, "other", true)

	for _, c := range []struct {
		meaning   bool
		min       float64
		cut, tote int
		why       string
	}{
		{false, 0, 1, 4, "by words only the marked one is left out, whatever the threshold"},
		{false, 0.99, 1, 4, "the threshold means nothing by words"},
		{true, 0.5, 2, 4, "by meaning the marked and the measured below 0.5; the unmeasured stays"},
		{true, 0.1, 1, 4, "a lower threshold leaves out only the marked"},
		{true, 0.95, 3, 4, "a higher threshold leaves out all the measured but never the unmeasured one"},
	} {
		total, cut, err := s.FilterCount(ctx, id, c.meaning, c.min)
		if err != nil || total != c.tote || cut != c.cut {
			t.Errorf("%s: FilterCount(%v, %v) = (%d, %d, %v), want (%d, %d)", c.why, c.meaning, c.min, total, cut, err, c.tote, c.cut)
		}
	}
	if total, cut, err := s.FilterCount(ctx, 9999, true, 0.5); err != nil || total != 0 || cut != 0 {
		t.Errorf("a job with nothing counts (%d, %d, %v), want (0, 0)", total, cut, err)
	}
}

func TestHasScores_SaysWhetherAnythingWasMeasured(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	measured := filteredJob(t, s, "measured", true)
	bare := filteredJob(t, s, "bare", false)
	if ok, err := s.HasScores(ctx, measured); err != nil || !ok {
		t.Errorf("HasScores of a measured job = (%v, %v), want true", ok, err)
	}
	if ok, err := s.HasScores(ctx, bare); err != nil || ok {
		t.Errorf("HasScores of a job nobody measured = (%v, %v), want false (a measured job beside it must not count)", ok, err)
	}
}
