// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/google"
)

// jobWithEverything writes a job that has one of each thing a delete has to
// take: results, paid placements, suggested searches, and a row in what the
// filter has seen.
//
// All of them are needed. A job with results alone cannot tell a delete that
// takes everything from one that takes the table somebody remembered.
func jobWithEverything(t *testing.T, s *Store) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := s.CreateJob(ctx, JobSpec{Name: "nightly", Pages: 1, UniqueBy: UniqueURL},
		[]string{"iphone 13", "golang generics"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for i := range 2 {
		err := s.Record(ctx, id, QueryOutcome{
			Ordinal: i,
			Pages: []google.SERP{{
				Query: "iphone 13",
				Results: []google.Result{
					{Position: 1, Title: "one", URL: "https://example.com/" + string(rune('a'+i))},
				},
				Ads: []google.Ad{
					{Position: 1, Placement: google.PlacementTop, Title: "an ad", URL: "https://ads.example/"},
				},
				Related: []string{"another search"},
			}},
		})
		if err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	return id
}

func TestDeleteJob_TakesEveryRowGatheredUnderIt(t *testing.T) {
	// The whole point is the room. A delete that left the results behind would
	// take a job off the list and leave ten million rows in the file with nothing
	// pointing at them — invisible, unreachable, and exactly as large.
	s := testStore(t)
	id := jobWithEverything(t, s)
	other := jobWithEverything(t, s)

	before, err := s.CountsUnder(context.Background(), id)
	if err != nil {
		t.Fatalf("CountsUnder: %v", err)
	}
	for table, n := range before {
		if n == 0 {
			t.Fatalf("the fixture wrote no %s, so this test proves nothing about them", table)
		}
	}

	if err := s.DeleteJob(context.Background(), id); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}

	after, err := s.CountsUnder(context.Background(), id)
	if err != nil {
		t.Fatalf("CountsUnder: %v", err)
	}
	for table, n := range after {
		if n != 0 {
			t.Errorf("%d rows of %s survived the job they belonged to", n, table)
		}
	}
	if _, err := s.Progress(context.Background(), id); !errors.Is(err, ErrNoJob) {
		t.Errorf("the job itself reads back as %v, want ErrNoJob", err)
	}

	// And the job beside it is untouched, which is the half a cascade gets wrong.
	kept, err := s.CountsUnder(context.Background(), other)
	if err != nil {
		t.Fatalf("CountsUnder: %v", err)
	}
	for table, n := range kept {
		if n == 0 {
			t.Errorf("deleting one job emptied the %s of another", table)
		}
	}
}

func TestDeleteJob_SaysSoWhenThereIsNoSuchJob(t *testing.T) {
	// Pressed twice, or pressed from a second tab. It is not a fault, and the
	// caller has to be able to tell it from one: the answer to both is that the
	// job is not there, which is what the reader wanted.
	s := testStore(t)
	if err := s.DeleteJob(context.Background(), 404); !errors.Is(err, ErrNoJob) {
		t.Errorf("DeleteJob on a job that is not there returned %v, want ErrNoJob", err)
	}
}

func TestDeleteJob_RefusesToRunWithTheCascadeOff(t *testing.T) {
	// Every connection this package opens has foreign keys on, and this is the
	// one place where losing that is silent: the job goes, the results stay, and
	// the file is exactly as large as it was with nothing on any screen to say
	// so.
	s := testStore(t)
	id := jobWithEverything(t, s)
	if _, err := s.db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("turning the cascade off: %v", err)
	}
	t.Cleanup(func() { _, _ = s.db.Exec(`PRAGMA foreign_keys = ON`) })

	if err := s.DeleteJob(context.Background(), id); err == nil {
		t.Error("a delete went ahead with the cascade off, so its results are still in the file")
	}
	if _, err := s.Progress(context.Background(), id); err != nil {
		t.Errorf("the refused delete took the job anyway: %v", err)
	}
}
