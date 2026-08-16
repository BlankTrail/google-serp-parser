// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "gserp.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateJob_WritesThePlanBeforeAnyWorkStarts(t *testing.T) {
	// The plan on disk is what makes an interrupted run resumable: without it,
	// a crash leaves nothing saying what was meant to happen.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(),
		JobSpec{Name: "nightly", Pages: 2, Country: "us", Language: "en"},
		[]string{"iphone 13", "golang generics", "iphone 13"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if id <= 0 {
		t.Fatalf("CreateJob returned id %d", id)
	}

	pending, err := s.Pending(context.Background(), id)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("%d queries pending, want 3", len(pending))
	}
	for i, p := range pending {
		if p.Ordinal != i {
			t.Errorf("pending[%d].Ordinal=%d, want %d", i, p.Ordinal, i)
		}
	}
	if pending[0].Text != "iphone 13" || pending[1].Text != "golang generics" {
		t.Errorf("pending order is %q, %q — want the order they were given",
			pending[0].Text, pending[1].Text)
	}
}

func TestCreateJob_KeepsTheSettingsTheJobWasGiven(t *testing.T) {
	// A history is read long after the run, and two nights' numbers only mean
	// anything apart if the settings behind each are written down with them.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(),
		JobSpec{Name: "nightly", Pages: 3, SpecName: "desktop", Country: "de", Language: "de"},
		[]string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	var (
		name, specName, country, language string
		pages                             int
		created                           string
	)
	if err := s.db.QueryRow(
		`SELECT name, pages, spec_name, country, language, created_at FROM jobs WHERE id = ?`, id,
	).Scan(&name, &pages, &specName, &country, &language, &created); err != nil {
		t.Fatalf("query: %v", err)
	}
	if name != "nightly" || pages != 3 || specName != "desktop" || country != "de" || language != "de" {
		t.Errorf("the job reads back as %q pages=%d spec=%q country=%q language=%q",
			name, pages, specName, country, language)
	}
	if created == "" {
		t.Error("the job carries no start time")
	}
}

func TestCreateJob_TakesAJobWithNoPageCountToMeanOnePage(t *testing.T) {
	// A stored zero would resume as a job with nothing to fetch, and the count
	// a caller left unset means the first page, as it does everywhere else.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "j"}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	var pages int
	if err := s.db.QueryRow(`SELECT pages FROM jobs WHERE id = ?`, id).Scan(&pages); err != nil {
		t.Fatalf("query: %v", err)
	}
	if pages != 1 {
		t.Errorf("the job asks for %d pages, want 1", pages)
	}
}

func TestCreateJob_KeepsARepeatedQueryAsTwoPiecesOfWork(t *testing.T) {
	// A user who lists the same query twice wants it run twice, deliberately
	// or by a mistake they will want to see. Collapsing the copies would
	// silently drop work, and the ordinal is what keeps them apart.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "j", Pages: 1},
		[]string{"same", "same"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	pending, err := s.Pending(context.Background(), id)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("%d queries pending, want both copies", len(pending))
	}
}

func TestCreateJob_RefusesAJobWithNothingToDo(t *testing.T) {
	s := testStore(t)
	if _, err := s.CreateJob(context.Background(), JobSpec{Name: "j", Pages: 1}, nil); !errors.Is(err, ErrNoQueries) {
		t.Errorf("CreateJob returned %v, want ErrNoQueries", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Errorf("%d jobs written for a list with nothing in it, want none", n)
	}
}

func TestCreateJob_LeavesNothingBehindWhenTheListFailsPartWayThrough(t *testing.T) {
	// A half-written plan is worse than none: a resume would run part of the
	// list and call the job finished. The refusal is arranged to land on the
	// second query, so a job and a query already exist when it does.
	s := testStore(t)
	if _, err := s.db.Exec(
		`CREATE TRIGGER refuse_one AFTER INSERT ON queries WHEN NEW.text = 'refused'
		 BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatalf("arranging the refusal: %v", err)
	}

	if _, err := s.CreateJob(context.Background(), JobSpec{Name: "j", Pages: 1},
		[]string{"kept", "refused"}); err == nil {
		t.Fatal("CreateJob succeeded although a query was refused")
	}

	var jobs, queries int
	if err := s.db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&jobs); err != nil {
		t.Fatalf("query: %v", err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM queries`).Scan(&queries); err != nil {
		t.Fatalf("query: %v", err)
	}
	if jobs != 0 || queries != 0 {
		t.Errorf("a failed create left %d jobs and %d queries behind, want none", jobs, queries)
	}
}

func TestCreateJob_StopsWhenTheCallerHasAlreadyGivenUp(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.CreateJob(ctx, JobSpec{Name: "j", Pages: 1}, []string{"a", "b"}); err == nil {
		t.Fatal("CreateJob succeeded on a cancelled context")
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Errorf("%d jobs left behind by a failed create, want none", n)
	}
}

func TestPending_OrdersByTheStoredOrdinalAndNotTheRowOrder(t *testing.T) {
	// The day queries are appended to a job, the row they land in says nothing
	// about where they belong in the list. The ordinal written with them does.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "j", Pages: 1}, []string{"first"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for _, later := range []struct {
		ordinal int
		text    string
	}{{3, "fourth"}, {1, "second"}, {2, "third"}} {
		if _, err := s.db.Exec(
			`INSERT INTO queries(job_id, ordinal, text) VALUES(?, ?, ?)`,
			id, later.ordinal, later.text); err != nil {
			t.Fatalf("appending a query: %v", err)
		}
	}

	pending, err := s.Pending(context.Background(), id)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	want := []string{"first", "second", "third", "fourth"}
	if len(pending) != len(want) {
		t.Fatalf("%d queries pending, want %d", len(pending), len(want))
	}
	for i, w := range want {
		if pending[i].Text != w || pending[i].Ordinal != i {
			t.Errorf("pending[%d] is %q at ordinal %d, want %q at %d",
				i, pending[i].Text, pending[i].Ordinal, w, i)
		}
	}
}

func TestPending_LeavesOutAQueryThatIsAlreadyDone(t *testing.T) {
	// Resuming means doing what is left. A query handed out again after it
	// finished is work done a second time for the same answer.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "j", Pages: 1},
		[]string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := s.db.Exec(
		`UPDATE queries SET state = 'done' WHERE job_id = ? AND ordinal = 1`, id); err != nil {
		t.Fatalf("marking a query done: %v", err)
	}

	pending, err := s.Pending(context.Background(), id)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("%d queries pending, want 2", len(pending))
	}
	if pending[0].Text != "a" || pending[1].Text != "c" {
		t.Errorf("pending is %q, %q — want the two that are not done",
			pending[0].Text, pending[1].Text)
	}
}

func TestPending_SaysNothingForAJobThatIsNotThere(t *testing.T) {
	s := testStore(t)
	pending, err := s.Pending(context.Background(), 4242)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("%d queries pending for a job that does not exist", len(pending))
	}
}

func TestFinishJob_StampsTheJobAsDone(t *testing.T) {
	s := testStore(t)
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "j", Pages: 1}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.FinishJob(context.Background(), id); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}
	var finished string
	if err := s.db.QueryRow(`SELECT coalesce(finished_at,'') FROM jobs WHERE id = ?`, id).Scan(&finished); err != nil {
		t.Fatalf("query: %v", err)
	}
	if finished == "" {
		t.Error("the job carries no finish time")
	}
}
