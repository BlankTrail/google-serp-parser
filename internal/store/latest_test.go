// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/google"
)

func TestLatestRows_HandsBackTheNewestFirstAndOnlyAsManyAsAsked(t *testing.T) {
	// The page draws these while a job runs, and what somebody watching is asking
	// is whether results are still arriving. Ordered from the start of the list
	// the block never changes at all — it shows the same first rows for the whole
	// run and, when the job ends, it shows the beginning of a million.
	s := testStore(t)
	ctx := context.Background()
	const queries = 6
	list := make([]string, queries)
	for i := range list {
		list[i] = "q" + strconv.Itoa(i)
	}
	id, err := s.CreateJob(ctx, JobSpec{Name: "nightly", Pages: 1}, list)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for i := range queries {
		err := s.Record(ctx, id, QueryOutcome{
			Ordinal: i,
			Pages: []google.SERP{{
				Query:   list[i],
				Results: []google.Result{{Position: 1, Title: list[i], URL: "https://example.com/" + list[i]}},
			}},
		})
		if err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}

	rows, err := s.LatestRows(ctx, id, 2)
	if err != nil {
		t.Fatalf("LatestRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("asked for 2 and got %d", len(rows))
	}
	// Newest first: the last query recorded stands at the top.
	if rows[0].Query != "q5" || rows[1].Query != "q4" {
		t.Errorf("the last two read %q then %q, want q5 then q4", rows[0].Query, rows[1].Query)
	}

	// And asking for more than there is gives everything, still newest first.
	all, err := s.LatestRows(ctx, id, 100)
	if err != nil {
		t.Fatalf("LatestRows: %v", err)
	}
	if len(all) != queries {
		t.Fatalf("the job has %d results and %d came back", queries, len(all))
	}
	if all[0].Query != "q5" || all[len(all)-1].Query != "q0" {
		t.Errorf("the whole lot reads from %q to %q, want q5 to q0",
			all[0].Query, all[len(all)-1].Query)
	}
}

func TestLatestStandings_HandsBackTheNewestSettledFirst(t *testing.T) {
	// The same rule for a position check, which draws the same block. The moment
	// each query settled is what orders them: two threads settle queries out of
	// the order they were listed in, and it is arrival the reader is asking about.
	s := testStore(t)
	ctx := context.Background()
	id, err := s.CreateJob(ctx, JobSpec{Name: "positions", Kind: KindPosition,
		Target: "example.com", Pages: 1}, []string{"first", "second", "third"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	// Settled out of order on purpose: listed first, second, third, and settled
	// third, first, second. Ordered by the list this reads the same either way.
	at := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	for i, ordinal := range []int{2, 0, 1} {
		settleAt(t, s, id, ordinal, at.Add(time.Duration(i)*time.Minute))
	}

	got, err := s.LatestStandings(ctx, id, 3)
	if err != nil {
		t.Fatalf("LatestStandings: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("three settled and %d came back", len(got))
	}
	if got[0].Query != "second" || got[2].Query != "third" {
		t.Errorf("the standings read %q first and %q last, want second first and third last",
			got[0].Query, got[2].Query)
	}
}

func TestLatestVerdicts_HandsBackTheNewestSettledFirst(t *testing.T) {
	// The same rule again for an index check. It is asked of this reader as well
	// as of the standings because the two are separate statements: a rule kept in
	// one of them and lost in the other is exactly the kind of thing a test of
	// only one lets through.
	s := testStore(t)
	ctx := context.Background()
	id, err := s.CreateJob(ctx, JobSpec{Name: "index", Kind: KindIndex, Pages: 1},
		[]string{"https://a.example/", "https://b.example/", "https://c.example/"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	at := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	for i, ordinal := range []int{2, 0, 1} {
		settleAt(t, s, id, ordinal, at.Add(time.Duration(i)*time.Minute))
	}

	got, err := s.LatestVerdicts(ctx, id, 3)
	if err != nil {
		t.Fatalf("LatestVerdicts: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("three settled and %d came back", len(got))
	}
	if got[0].Target != "https://b.example/" || got[2].Target != "https://c.example/" {
		t.Errorf("the verdicts read %q first and %q last, want b first and c last",
			got[0].Target, got[2].Target)
	}
}
