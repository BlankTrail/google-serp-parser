// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"
	"time"
)

// settleAt writes a query down as settled at a given moment, the way a run
// does, so a test can lay out a pace without waiting for one.
func settleAt(t *testing.T, s *Store, jobID int64, ordinal int, at time.Time) {
	t.Helper()
	_, err := s.db.Exec(
		`UPDATE queries SET state = 'done', settled_at = ?
		  WHERE job_id = ? AND ordinal = ?`,
		at.UTC().Format(time.RFC3339Nano), jobID, ordinal)
	if err != nil {
		t.Fatalf("settling query %d: %v", ordinal, err)
	}
}

func TestPace_MeasuresTheSpeedFromWhenTheQueriesActuallySettled(t *testing.T) {
	// A speed taken by counting twice and subtracting depends on how often
	// somebody reloaded, so it is a different number for every reader. This one
	// is read from the history and is the same for all of them.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "nightly", Pages: 1},
		[]string{"a", "b", "c", "d", "e"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// Four gaps of fifteen seconds: a minute, four queries settled in it.
	start := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	for i := range 5 {
		settleAt(t, s, id, i, start.Add(time.Duration(i)*15*time.Second))
	}

	pace, err := s.Pace(context.Background(), id)
	if err != nil {
		t.Fatalf("Pace: %v", err)
	}
	if !pace.Known {
		t.Fatal("a job that settled five queries reports no speed at all")
	}
	if pace.Over != time.Minute {
		t.Errorf("the speed is measured over %v, and the five settled over a minute", pace.Over)
	}
	// Four gaps, not five queries: counting the queries over the span between
	// them reports a job running a quarter faster than it ever has.
	if pace.Settled != 4 {
		t.Errorf("the span holds %d settled queries, and there are four gaps in it", pace.Settled)
	}
	if got := pace.PerMinute(); got != 4 {
		t.Errorf("PerMinute() = %v, want the four a minute this job kept", got)
	}
}

func TestPace_SaysNothingRatherThanNoughtWhenThereIsNothingToMeasure(t *testing.T) {
	// Nought is a speed, and it is the speed of a job that has stopped. A job
	// nobody has measured yet has not stopped, and the two must not read alike.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "nightly", Pages: 1},
		[]string{"a", "b"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	pace, err := s.Pace(context.Background(), id)
	if err != nil {
		t.Fatalf("Pace: %v", err)
	}
	if pace.Known {
		t.Errorf("a job that has settled nothing reports a speed of %v", pace.PerMinute())
	}

	// One settled query is a moment, not a span, and still nothing to divide by.
	settleAt(t, s, id, 0, time.Now())
	pace, err = s.Pace(context.Background(), id)
	if err != nil {
		t.Fatalf("Pace: %v", err)
	}
	if pace.Known {
		t.Errorf("a job that settled one query reports a speed of %v", pace.PerMinute())
	}
}

func TestPace_FollowsTheJobNowRatherThanAveragingTheWholeRun(t *testing.T) {
	// The figure is for somebody watching a screen and asking whether the job is
	// moving. An average over the whole run answers a different question, and on
	// a job that spent its first hour crawling it answers it wrongly for the rest
	// of the day.
	s := testStore(t)
	queries := make([]string, PaceSample+10)
	for i := range queries {
		queries[i] = "q" + string(rune('a'+i%26))
	}
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "nightly", Pages: 1}, queries)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// Ten settled slowly, then the sample's worth settled quickly.
	at := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	for i := range 10 {
		settleAt(t, s, id, i, at)
		at = at.Add(10 * time.Minute)
	}
	for i := range PaceSample {
		settleAt(t, s, id, 10+i, at)
		at = at.Add(time.Second)
	}

	pace, err := s.Pace(context.Background(), id)
	if err != nil {
		t.Fatalf("Pace: %v", err)
	}
	if !pace.Known {
		t.Fatal("a job that settled thirty queries reports no speed")
	}
	if pace.PerMinute() < 30 {
		t.Errorf("the speed reads %.1f a minute, and the last twenty settled a second apart",
			pace.PerMinute())
	}
}

func TestPace_PassesOverTheQueriesWrittenBeforeTheMomentWasKept(t *testing.T) {
	// A history from an older build has settled queries with no moment beside
	// them. They are not nought o'clock, and a speed measured from the beginning
	// of time is a speed of nothing at all.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "nightly", Pages: 1},
		[]string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := s.db.Exec(
		`UPDATE queries SET state = 'done' WHERE job_id = ? AND ordinal = 0`, id); err != nil {
		t.Fatalf("settling the old way: %v", err)
	}
	at := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	settleAt(t, s, id, 1, at)
	settleAt(t, s, id, 2, at.Add(30*time.Second))

	pace, err := s.Pace(context.Background(), id)
	if err != nil {
		t.Fatalf("Pace: %v", err)
	}
	if !pace.Known || pace.Over != 30*time.Second {
		t.Errorf("the speed is measured over %v (known=%v), and the two with moments settled "+
			"thirty seconds apart", pace.Over, pace.Known)
	}
}

func TestRecord_WritesDownWhenAQuerySettled(t *testing.T) {
	// The moment is written by the run itself, in the transaction that settles
	// the query. Written anywhere else it would be a moment that survived a write
	// that failed, or one lost by a write that did not.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "nightly", Pages: 1},
		[]string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	before := time.Now().UTC().Add(-time.Second)
	if err := s.Record(context.Background(), id, QueryOutcome{Ordinal: 0}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var text string
	if err := s.db.QueryRow(
		`SELECT settled_at FROM queries WHERE job_id = ? AND ordinal = 0`, id).Scan(&text); err != nil {
		t.Fatalf("reading the moment: %v", err)
	}
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t.Fatalf("the moment written down is not one: %q", text)
	}
	if at.Before(before) || at.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("the query says it settled at %v, which is not now", at)
	}
}

// tookPages writes down that a settled query captured a number of pages, the
// way a run does when it records what it took.
func tookPages(t *testing.T, s *Store, jobID int64, ordinal, pages int) {
	t.Helper()
	var queryID int64
	if err := s.db.QueryRow(`SELECT id FROM queries WHERE job_id = ? AND ordinal = ?`,
		jobID, ordinal).Scan(&queryID); err != nil {
		t.Fatalf("finding query %d: %v", ordinal, err)
	}
	for n := 1; n <= pages; n++ {
		if _, err := s.db.Exec(`INSERT INTO pages(query_id, number) VALUES(?, ?)`,
			queryID, n); err != nil {
			t.Fatalf("recording page %d of query %d: %v", n, ordinal, err)
		}
	}
}

func TestPace_CountsThePagesThoseQueriesTookAsWellAsTheQueries(t *testing.T) {
	// A job taken ten pages deep settles one query for every ten requests it
	// makes. Told only in queries, it reads as a job that has nearly stopped
	// while it is working as hard as it ever does — so the pages are counted
	// beside them, and the two are shown side by side.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "deep", Pages: 10},
		[]string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	start := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		settleAt(t, s, id, i, start.Add(time.Duration(i)*30*time.Second))
		tookPages(t, s, id, i, 10)
	}

	pace, err := s.Pace(context.Background(), id)
	if err != nil {
		t.Fatalf("Pace: %v", err)
	}
	// Two gaps of thirty seconds, and the two newest queries' pages in them. The
	// oldest query's pages are left out for the reason its settling is: they were
	// taken before the span this speed is measured over.
	if pace.Pages != 20 {
		t.Errorf("the span holds %d pages, and the two queries in it took ten each", pace.Pages)
	}
	if got := pace.PagesPerMinute(); got != 20 {
		t.Errorf("PagesPerMinute() = %v, want the twenty a minute this job kept", got)
	}
	if got := pace.PerMinute(); got != 2 {
		t.Errorf("PerMinute() = %v, want the two queries a minute beside them", got)
	}
}
