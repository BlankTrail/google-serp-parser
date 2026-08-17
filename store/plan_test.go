// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"
)

// rowsOf is how many queries a job holds right now, read without going through
// Pending: Pending refuses a plan that is still being written, and half of what
// is proved here is what is in the database while it still refuses.
func rowsOf(t *testing.T, s *Store, jobID int64) int {
	t.Helper()
	// A deadline rather than a wait without end. A plan that never lets go of the
	// store's one connection would otherwise hang this test instead of failing
	// it, and a test that hangs reports nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM queries WHERE job_id = ?`, jobID).Scan(&n); err != nil {
		t.Fatalf("counting the queries of job %d: %v — the upload is holding the database", jobID, err)
	}
	return n
}

// readyOf is the flag that says a job's list finished arriving.
func readyOf(t *testing.T, s *Store, jobID int64) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var ready bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT plan_ready FROM jobs WHERE id = ?`, jobID).Scan(&ready); err != nil {
		t.Fatalf("reading the plan flag of job %d: %v", jobID, err)
	}
	return ready
}

func TestPlan_WritesTheQueriesInPiecesAndOnlyThenSaysItIsReady(t *testing.T) {
	s := testStore(t)
	p, err := s.OpenPlan(t.Context(), JobSpec{Name: "big", Pages: 1})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	for i := 0; i < 2500; i++ {
		if err := p.Add(t.Context(), fmt.Sprintf("q%05d", i)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	// Before Ready, the job is not work anybody may pick up. Both ways in are
	// asked, because they are two doors to the same room: Pending is what a run
	// reads its work from and LastUnfinished is what a resume finds a job by, and
	// a job shut out of one and offered by the other is a job that runs half a
	// list.
	if _, err := s.Pending(t.Context(), p.JobID()); !errors.Is(err, ErrPlanUnfinished) {
		t.Errorf("Pending returned %v, want ErrPlanUnfinished", err)
	}
	if _, err := s.LastUnfinished(t.Context(), "big"); !errors.Is(err, ErrNoUnfinishedJob) {
		t.Errorf("LastUnfinished offered a job whose list was still arriving: %v", err)
	}
	if err := p.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	left, err := s.Pending(t.Context(), p.JobID())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(left) != 2500 {
		t.Errorf("%d queries in the plan, want 2500", len(left))
	}
}

func TestPlan_KeepsTheOrderTheLinesArrivedIn(t *testing.T) {
	// A batch boundary is where an order gets shuffled, so the fixture is
	// deliberately larger than one batch.
	s := testStore(t)
	p, err := s.OpenPlan(t.Context(), JobSpec{Name: "ordered", Pages: 1})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	for i := 0; i < 2500; i++ {
		if err := p.Add(t.Context(), fmt.Sprintf("q%05d", i)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if err := p.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	left, err := s.Pending(t.Context(), p.JobID())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(left) != 2500 {
		t.Fatalf("%d queries came back, want 2500", len(left))
	}
	for i, q := range left {
		if q.Ordinal != i || q.Text != fmt.Sprintf("q%05d", i) {
			t.Fatalf("query %d is %+v — the order did not survive the batches", i, q)
		}
	}
}

func TestPlan_PutsEachBatchInTheDatabaseAndLetsGoOfItBeforeTheNext(t *testing.T) {
	// This is the whole mechanism in one test. A plan that gathered the lines and
	// wrote them at the end would show nothing here; a plan whose batch is the
	// whole file would still be holding the store's one connection, and the read
	// below is what finds that out.
	s := testStore(t)
	p, err := s.OpenPlan(t.Context(), JobSpec{Name: "batched", Pages: 1})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	// The job is there before a single line is, so a very large file is a job the
	// operator can already see rather than nothing at all for twenty minutes.
	if rowsOf(t, s, p.JobID()) != 0 {
		t.Error("a plan nothing has been added to already holds queries")
	}

	// Deliberately stopped part way through a batch. The database has to be
	// readable at that moment and not only on the boundaries, because that is
	// where an upload spends nearly all of its time: the slow part is the file
	// arriving, and a plan holding the store's one connection across that is an
	// interface frozen for the length of the upload.
	for i := 0; i < 2*planBatch+137; i++ {
		if err := p.Add(t.Context(), fmt.Sprintf("q%05d", i)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if got := rowsOf(t, s, p.JobID()); got != 2*planBatch {
		t.Errorf("%d lines reached the database before the upload ended, want the %d whole batches", got, 2*planBatch)
	}
	if readyOf(t, s, p.JobID()) {
		t.Error("the job says its list is complete while the upload is still running")
	}
	if err := p.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !readyOf(t, s, p.JobID()) {
		t.Error("Ready left the job saying its list never finished arriving")
	}
	if got := rowsOf(t, s, p.JobID()); got != 2*planBatch+137 {
		t.Errorf("%d lines are in the database after Ready, want %d", got, 2*planBatch+137)
	}
}

func TestPlan_CountsWhatItWroteRatherThanWhatItWasHanded(t *testing.T) {
	// The count is what the operator is told they uploaded, so it is checked
	// against the rows that are actually there and never against the number the
	// test happened to loop to. A figure that is only ever compared with itself
	// is a figure that can be anything.
	//
	// The number is deliberately not a multiple of the batch, so a count kept in
	// whole batches is short by the remainder rather than right by accident.
	const lines = 2*planBatch + 137
	s := testStore(t)
	p, err := s.OpenPlan(t.Context(), JobSpec{Name: "counted", Pages: 1})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	for i := 0; i < lines; i++ {
		if err := p.Add(t.Context(), fmt.Sprintf("q%05d", i)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	// Asked here first, where the two answers differ. After Ready every line is
	// in the database and a count of the committed batches alone reads exactly
	// like a count of everything taken; here, with a batch in hand, it is short
	// by the remainder. This is the number the operator is shown while a very
	// large file is still arriving, so it is the number that has to be right.
	if p.Count() != lines {
		t.Errorf("part way through the upload the plan says it has taken %d of %d lines", p.Count(), lines)
	}
	if waiting := rowsOf(t, s, p.JobID()); waiting != 2*planBatch {
		t.Fatalf("%d lines are in the database part way through, want the %d whole batches", waiting, 2*planBatch)
	}
	if err := p.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	written := rowsOf(t, s, p.JobID())
	if p.Count() != written {
		t.Errorf("the plan says it took %d lines and the database holds %d", p.Count(), written)
	}
	if written != lines {
		t.Errorf("%d lines were written, want the %d that were handed over", written, lines)
	}
}

func TestPlan_AnAbandonedUploadLeavesAJobNobodyCanRun(t *testing.T) {
	// This is the whole point. A cut-off upload must not leave a job that looks
	// complete, because a resume would run part of the list and stamp it done.
	s := testStore(t)
	p, err := s.OpenPlan(t.Context(), JobSpec{Name: "cut off", Pages: 1})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	for i := 0; i < 1500; i++ {
		if err := p.Add(t.Context(), fmt.Sprintf("q%05d", i)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if err := p.Abandon(); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	if _, err := s.Pending(t.Context(), p.JobID()); !errors.Is(err, ErrPlanUnfinished) {
		t.Errorf("Pending returned %v, want ErrPlanUnfinished", err)
	}
	if _, err := s.LastUnfinished(t.Context(), "cut off"); !errors.Is(err, ErrNoUnfinishedJob) {
		t.Errorf("LastUnfinished offered the abandoned job to be carried on: %v", err)
	}
}

func TestPlan_AnAbandonedUploadStaysWhereItCanBeSeen(t *testing.T) {
	// The choice this pins: a browser that went away halfway through a file
	// leaves the job in the history, marked as a list that never finished
	// arriving, rather than taking it out of the world. Somebody uploaded a file
	// for twenty minutes, and a job that vanished is twenty minutes with nothing
	// to show and nothing to ask about. What was already written is kept with it,
	// because it costs nothing and says how far the file got.
	s := testStore(t)
	p, err := s.OpenPlan(t.Context(), JobSpec{Name: "cut off", Pages: 1})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	for i := 0; i < planBatch+500; i++ {
		if err := p.Add(t.Context(), fmt.Sprintf("q%05d", i)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if err := p.Abandon(); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	jobs, err := s.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("%d jobs in the history, want the abandoned one", len(jobs))
	}
	if jobs[0].PlanReady {
		t.Error("the abandoned upload is listed as a job with a complete list")
	}
	// The batches that were committed are still there, and the one that was open
	// is not: that is what a rollback of the last batch alone means.
	if jobs[0].Total != planBatch {
		t.Errorf("the abandoned job holds %d queries, want the %d that were committed", jobs[0].Total, planBatch)
	}
}

func TestPlan_RefusesToCallAnEmptyListReady(t *testing.T) {
	// A file with nothing in it is not a job. Marked ready it would be picked up,
	// found to have nothing left, and stamped done — a run that never happened
	// filed as one that did.
	s := testStore(t)
	p, err := s.OpenPlan(t.Context(), JobSpec{Name: "empty", Pages: 1})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	if err := p.Ready(t.Context()); !errors.Is(err, ErrNoQueries) {
		t.Errorf("Ready of an empty list returned %v, want ErrNoQueries", err)
	}
	if readyOf(t, s, p.JobID()) {
		t.Error("an empty list was marked complete")
	}
}

func TestOpenPlan_TakesAJobOfNoPagesToBeAJobOfOne(t *testing.T) {
	// A job stored as fetching no pages would be resumed with nothing to do. The
	// guard is the one CreateJob has, because the two write the same job and a
	// job set up through a file must not differ from the same job set up through
	// the box.
	s := testStore(t)
	p, err := s.OpenPlan(t.Context(), JobSpec{Name: "shallow"})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	if err := p.Add(t.Context(), "iphone 13"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := p.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	sum, err := s.Progress(t.Context(), p.JobID())
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.Pages != 1 {
		t.Errorf("the job is filed as taking %d pages a query, want one", sum.Pages)
	}
}

func TestPlan_TakesNoMoreLinesOnceItIsFinished(t *testing.T) {
	// A line added after the flag is set is a line no run will ever read: the
	// plan it belongs to was already handed out as complete.
	s := testStore(t)
	p, err := s.OpenPlan(t.Context(), JobSpec{Name: "closed", Pages: 1})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	if err := p.Add(t.Context(), "iphone 13"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := p.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if err := p.Add(t.Context(), "golang generics"); !errors.Is(err, ErrPlanDone) {
		t.Errorf("Add after Ready returned %v, want ErrPlanDone", err)
	}
	if err := p.Ready(t.Context()); !errors.Is(err, ErrPlanDone) {
		t.Errorf("Ready a second time returned %v, want ErrPlanDone", err)
	}
	if got := rowsOf(t, s, p.JobID()); got != 1 {
		t.Errorf("the finished plan holds %d queries, want the one it was given", got)
	}
}

func TestPlan_AbandoningAFinishedUploadTakesNothingAway(t *testing.T) {
	// The handler that reads a file abandons the plan on its way out whatever
	// happened, because that is the only way a return down any path leaves
	// nothing half written. An abandon after a successful upload therefore has to
	// be nothing at all.
	s := testStore(t)
	p, err := s.OpenPlan(t.Context(), JobSpec{Name: "done", Pages: 1})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	if err := p.Add(t.Context(), "iphone 13"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := p.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if err := p.Abandon(); err != nil {
		t.Fatalf("Abandon after Ready: %v", err)
	}
	left, err := s.Pending(t.Context(), p.JobID())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(left) != 1 {
		t.Errorf("%d queries left after the plan was abandoned, want the one that was uploaded", len(left))
	}
}

func TestPlan_DoesNotHoldAMillionLinesInMemory(t *testing.T) {
	// The reason this exists at all. Measured rather than asserted: the figure
	// is logged, and the test fails only on a growth that no batching could
	// explain.
	//
	// The bound is far below what a million lines cost to hold. Seven-figure
	// strings are about thirty-two bytes each once the header and the allocation
	// class are counted, and the slice of headers is sixteen more, so a plan that
	// gathered them would land near fifty megabytes — under a bound of sixty-four
	// and nowhere near this one. A bound a holding implementation would pass is a
	// test that says nothing.
	if testing.Short() {
		t.Skip("a million rows through a database is not a short test")
	}
	const grewAtMost = 8 << 20

	s := testStore(t)
	p, err := s.OpenPlan(t.Context(), JobSpec{Name: "million", Pages: 1})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < 1_000_000; i++ {
		if err := p.Add(t.Context(), fmt.Sprintf("q%07d", i)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if err := p.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	grew := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("a million lines grew the heap by %d bytes", grew)
	if grew > grewAtMost {
		t.Errorf("the heap grew by %d bytes over a million lines — the plan is being held rather than written", grew)
	}
	// The million lines are in the database and not merely absent from memory. A
	// plan that dropped every line would pass the measurement above outright.
	if got := rowsOf(t, s, p.JobID()); got != 1_000_000 {
		t.Errorf("%d of a million lines were written", got)
	}
}

func TestOpenPlan_FilesTheJobUnderTheKindItWasAskedFor(t *testing.T) {
	// A list of addresses too long to hold arrives by this door and no other, so
	// a kind this door dropped would make the index job unreachable for exactly
	// the lists it was built for.
	s := testStore(t)
	p, err := s.OpenPlan(t.Context(), JobSpec{Name: "addresses", Kind: KindIndex, Pages: 1})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	if err := p.Add(t.Context(), "example.com/a"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := p.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	sum, err := s.Progress(t.Context(), p.JobID())
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.Kind != KindIndex {
		t.Errorf("an uploaded job reads as kind %q, want %q", sum.Kind, KindIndex)
	}
}
