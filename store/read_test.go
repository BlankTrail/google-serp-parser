// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/google"
)

// mustRecord writes one query's worth of results: a single page carrying the
// named hosts, ranked in the order they are given.
func mustRecord(t *testing.T, s *Store, jobID int64, ordinal int, hosts ...string) {
	t.Helper()
	rs := make([]google.Result, 0, len(hosts))
	for i, h := range hosts {
		rs = append(rs, google.Result{
			Title:   fmt.Sprintf("%s at %d", h, i+1),
			URL:     fmt.Sprintf("https://%s/%d", h, i+1),
			Host:    h,
			Snippet: "what the page said about " + h,
		})
	}
	if err := s.Record(context.Background(), jobID, QueryOutcome{
		Ordinal: ordinal,
		Pages:   []google.SERP{{Origin: "https://www.google.com", Results: rs}},
	}); err != nil {
		t.Fatalf("Record(ordinal %d): %v", ordinal, err)
	}
}

// stampJob rewrites when a job ran, so a test can arrange runs in an order the
// rows were not written in.
func stampJob(t *testing.T, s *Store, jobID int64, at string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE jobs SET created_at = ? WHERE id = ?`, at, jobID); err != nil {
		t.Fatalf("stamping the job: %v", err)
	}
}

// appendResult adds one more result to a query's first page, at a rank of the
// test's choosing. Everything Record writes lands in rank order, so a test that
// means to pin an ordering has to break that agreement itself.
func appendResult(t *testing.T, s *Store, jobID int64, ordinal, rank int, host string) {
	t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO results(page_id, rank, title, url, host, snippet)
		 SELECT p.id, ?, ?, ?, ?, ?
		   FROM pages p JOIN queries q ON q.id = p.query_id
		  WHERE q.job_id = ? AND q.ordinal = ? AND p.number = 1`,
		rank, fmt.Sprintf("%s at %d", host, rank), fmt.Sprintf("https://%s/%d", host, rank),
		host, "what the page said about "+host, jobID, ordinal)
	if err != nil {
		t.Fatalf("appending a result: %v", err)
	}
}

func TestRows_ComeBackInTheOrderTheJobHadAndNotTheOrderTheyWereWritten(t *testing.T) {
	// An export is read as the list the user handed in, whatever order the run
	// happened to finish in. Both orderings here are arranged to disagree with
	// the order the rows were written: on a fresh database the two agree, and a
	// walk that promised nothing would pass anyway.
	s := testStore(t)
	id := jobWith(t, s, "alpha", "beta")
	mustRecord(t, s, id, 1, "b1.test", "b2.test")
	mustRecord(t, s, id, 0, "a1.test")
	appendResult(t, s, id, 0, 3, "a3.test")
	appendResult(t, s, id, 0, 2, "a2.test")

	var got []string
	if err := s.Rows(context.Background(), id, func(r Row) error {
		got = append(got, fmt.Sprintf("%d/%d/%s", r.Ordinal, r.Rank, r.Host))
		return nil
	}); err != nil {
		t.Fatalf("Rows: %v", err)
	}
	want := []string{"0/1/a1.test", "0/2/a2.test", "0/3/a3.test", "1/1/b1.test", "1/2/b2.test"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows are %v, want %v — the order the job had, not the order they were written", got, want)
	}
}

func TestRows_CarryEnoughOfTheirQueryToStandAloneInAFile(t *testing.T) {
	// An export is read long after the run by someone who cannot go back and
	// look. A field dropped here is a field nobody can recover, and two fields
	// read in the wrong order are worse, because the file still looks whole.
	s := testStore(t)
	id := jobWith(t, s, "iphone 13", "untouched")
	err := s.Record(context.Background(), id, QueryOutcome{
		Ordinal: 0,
		Pages: []google.SERP{
			{Origin: "https://www.google.de", Results: []google.Result{{
				Title:   "The first title",
				URL:     "https://one.test/a",
				Link:    "/goto?url=opaque",
				Host:    "one.test",
				Snippet: "what the page said about the first",
			}}},
			{Origin: "https://www.google.de", Results: []google.Result{{
				Title:   "The second title",
				URL:     "https://two.test/b",
				Host:    "two.test",
				Snippet: "what the page said about the second",
			}}},
		},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	var got []Row
	if err := s.Rows(context.Background(), id, func(r Row) error {
		got = append(got, r)
		return nil
	}); err != nil {
		t.Fatalf("Rows: %v", err)
	}
	want := []Row{
		{Ordinal: 0, Query: "iphone 13", Page: 1, Rank: 1, Title: "The first title",
			URL: "https://one.test/a", Host: "one.test", Snippet: "what the page said about the first"},
		{Ordinal: 0, Query: "iphone 13", Page: 2, Rank: 2, Title: "The second title",
			URL: "https://two.test/b", Host: "two.test", Snippet: "what the page said about the second"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows read back as %+v, want %+v", got, want)
	}
}

func TestRows_LeaveOutWhatAnotherRunCaptured(t *testing.T) {
	// Every run shares the one database, and a file that quietly mixed two of
	// them would put last night's numbers in this morning's export.
	s := testStore(t)
	asked := jobWith(t, s, "alpha")
	other := jobWith(t, s, "alpha")
	mustRecord(t, s, asked, 0, "asked.test")
	mustRecord(t, s, other, 0, "other.test")

	var hosts []string
	if err := s.Rows(context.Background(), asked, func(r Row) error {
		hosts = append(hosts, r.Host)
		return nil
	}); err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if !reflect.DeepEqual(hosts, []string{"asked.test"}) {
		t.Errorf("the export holds %v, want only what the job it names captured", hosts)
	}
}

func TestRows_StopAtOnceWhenTheCallerHasSeenEnough(t *testing.T) {
	// An export that is cancelled or whose file fills up must not keep reading a
	// million rows to discover nobody wants them.
	s := testStore(t)
	id := jobWith(t, s, "alpha")
	mustRecord(t, s, id, 0, "a.test", "b.test", "c.test")

	stop := errors.New("enough")
	var seen int
	err := s.Rows(context.Background(), id, func(Row) error {
		seen++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("Rows returned %v, want the caller's own error", err)
	}
	if seen != 1 {
		t.Errorf("the callback saw %d rows after asking to stop, want 1", seen)
	}
}

func TestRows_EndLoudlyWhenTheWalkBreaksOffPartWayThrough(t *testing.T) {
	// A walk cut short must not read as a job that held nothing more, or the
	// export would be written short and called whole. Giving up on the read is
	// the readiest way to break a walk in two; the pause is what gives the halt
	// time to reach it.
	s := testStore(t)
	id := jobWith(t, s, "a")
	mustRecord(t, s, id, 0, "a.test", "b.test", "c.test", "d.test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen int
	err := s.Rows(ctx, id, func(Row) error {
		seen++
		cancel()
		time.Sleep(50 * time.Millisecond)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the walk ended with %v, want the reason it stopped", err)
	}
	if seen != 1 {
		t.Errorf("the callback saw %d rows, want only the one before the read was given up on", seen)
	}
}

func TestHistory_EndsLoudlyWhenTheWalkBreaksOffPartWayThrough(t *testing.T) {
	// Half a history read as a whole one is a site that looks like it was never
	// tracked before today.
	s := testStore(t)
	id := jobWith(t, s, "a")
	mustRecord(t, s, id, 0, "example.com", "b.test", "c.test")
	appendResult(t, s, id, 0, 4, "example.com")
	appendResult(t, s, id, 0, 5, "example.com")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen int
	err := s.History(ctx, "example.com", func(Position) error {
		seen++
		cancel()
		time.Sleep(50 * time.Millisecond)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the walk ended with %v, want the reason it stopped", err)
	}
	if seen != 1 {
		t.Errorf("the callback saw %d positions, want only the one before the read was given up on", seen)
	}
}

func TestHistory_FollowsOneSiteAcrossJobsInTheOrderThePositionsWereTaken(t *testing.T) {
	// This is what a rank tracker is for: the same site, and how its number
	// moved. Every ordering below is arranged to disagree with the order the
	// rows were written, because otherwise the two agree and the walk could
	// promise nothing and still pass.
	s := testStore(t)

	// Ran first, though its two queries were recorded back to front.
	early := jobWith(t, s, "iphone 13", "iphone 13 pro")
	stampJob(t, s, early, "2026-03-01T00:00:00Z")
	mustRecord(t, s, early, 1, "shop.test", "example.com")
	mustRecord(t, s, early, 0, "news.test", "wiki.test", "example.com")

	// Ran last, though it was written second.
	latest := jobWith(t, s, "iphone 13")
	stampJob(t, s, latest, "2026-03-03T00:00:00Z")
	mustRecord(t, s, latest, 0, "example.com")

	// Ran second, and holds the site twice, the higher position written last.
	middle := jobWith(t, s, "iphone 13")
	stampJob(t, s, middle, "2026-03-02T00:00:00Z")
	mustRecord(t, s, middle, 0, "a.test", "b.test", "c.test", "d.test", "e.test", "example.com")
	appendResult(t, s, middle, 0, 4, "example.com")

	var got []string
	if err := s.History(context.Background(), "example.com", func(p Position) error {
		got = append(got, fmt.Sprintf("%s/%d", p.TakenAt.Format(time.RFC3339), p.Rank))
		return nil
	}); err != nil {
		t.Fatalf("History: %v", err)
	}
	want := []string{
		"2026-03-01T00:00:00Z/3",
		"2026-03-01T00:00:00Z/2",
		"2026-03-02T00:00:00Z/4",
		"2026-03-02T00:00:00Z/6",
		"2026-03-03T00:00:00Z/1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("positions are %v, want %v — oldest run first, and within a run the job's own order", got, want)
	}
}

func TestHistory_SaysWhichRunAndWhichQueryEachPositionCameFrom(t *testing.T) {
	// A rank on its own means nothing. It is worth reading only against the run
	// it was taken in and the query it answers.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(),
		JobSpec{Name: "nightly", Pages: 1, Country: "de"}, []string{"iphone 13"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	stampJob(t, s, id, "2026-03-01T22:30:00Z")
	mustRecord(t, s, id, 0, "other.test", "example.com")

	var got []Position
	if err := s.History(context.Background(), "example.com", func(p Position) error {
		got = append(got, p)
		return nil
	}); err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d positions, want 1", len(got))
	}
	want := Position{
		JobID:   id,
		JobName: "nightly",
		TakenAt: time.Date(2026, 3, 1, 22, 30, 0, 0, time.UTC),
		Query:   "iphone 13",
		Rank:    2,
		URL:     "https://example.com/2",
	}
	if !got[0].TakenAt.Equal(want.TakenAt) {
		t.Errorf("the position was taken at %v, want %v", got[0].TakenAt, want.TakenAt)
	}
	got[0].TakenAt = want.TakenAt
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("the position reads back as %+v, want %+v", got[0], want)
	}
}

func TestHistory_KeepsAPositionWhoseStoredTimeCannotBeRead(t *testing.T) {
	// A damaged stamp costs the reader the date, not the number: the site still
	// stood where it stood. Giving up on the row would throw away every later
	// position with it, and a zero time says plainly what was lost.
	s := testStore(t)
	id := jobWith(t, s, "iphone 13")
	stampJob(t, s, id, "one evening in March")
	mustRecord(t, s, id, 0, "other.test", "example.com")

	var got []Position
	if err := s.History(context.Background(), "example.com", func(p Position) error {
		got = append(got, p)
		return nil
	}); err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d positions for a run with an unreadable stamp, want 1", len(got))
	}
	if got[0].Rank != 2 {
		t.Errorf("rank %d, want 2 — the stamp was unreadable, not the position", got[0].Rank)
	}
	if !got[0].TakenAt.IsZero() {
		t.Errorf("the unreadable stamp came back as %v, want the zero time", got[0].TakenAt)
	}
}

func TestHistory_StopsAtOnceWhenTheCallerHasSeenEnough(t *testing.T) {
	// A site that has been tracked nightly for a year has a long history, and a
	// caller that wants its last position should not have to read all of it.
	s := testStore(t)
	id := jobWith(t, s, "iphone 13")
	mustRecord(t, s, id, 0, "example.com", "other.test")
	appendResult(t, s, id, 0, 3, "example.com")

	stop := errors.New("enough")
	var seen int
	err := s.History(context.Background(), "example.com", func(Position) error {
		seen++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("History returned %v, want the caller's own error", err)
	}
	if seen != 1 {
		t.Errorf("the callback saw %d positions after asking to stop, want 1", seen)
	}
}

func TestHistory_SaysNothingForASiteThatNeverRanked(t *testing.T) {
	s := testStore(t)
	id := jobWith(t, s, "q")
	mustRecord(t, s, id, 0, "other.test")

	var n int
	if err := s.History(context.Background(), "example.com", func(Position) error { n++; return nil }); err != nil {
		t.Fatalf("History: %v", err)
	}
	if n != 0 {
		t.Errorf("%d positions for a site that never appeared", n)
	}
}
