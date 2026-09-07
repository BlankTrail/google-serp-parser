// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
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
		JobSpec{Name: "nightly", Pages: 3, Device: "desktop", Country: "de", Language: "de"},
		[]string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	var (
		name, device, country, language string
		pages                           int
		created                         string
	)
	if err := s.db.QueryRow(
		`SELECT name, pages, device, country, language, created_at FROM jobs WHERE id = ?`, id,
	).Scan(&name, &pages, &device, &country, &language, &created); err != nil {
		t.Fatalf("query: %v", err)
	}
	if name != "nightly" || pages != 3 || device != "desktop" || country != "de" || language != "de" {
		t.Errorf("the job reads back as %q pages=%d device=%q country=%q language=%q",
			name, pages, device, country, language)
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

func TestLastUnfinished_TakesTheNewestJobOfThatNameThatStillHasWork(t *testing.T) {
	// A resume asks for a job by the name the user knows it by. Handing back the
	// oldest one would take up a run somebody abandoned days ago instead of the
	// one that just died.
	s := testStore(t)
	older, err := s.CreateJob(context.Background(), JobSpec{Name: "nightly", Pages: 1}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	newer, err := s.CreateJob(context.Background(), JobSpec{Name: "nightly", Pages: 1}, []string{"b"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	got, err := s.LastUnfinished(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("LastUnfinished: %v", err)
	}
	if got.ID != newer {
		t.Errorf("LastUnfinished chose job %d, want the newer %d rather than %d", got.ID, newer, older)
	}
}

func TestLastUnfinished_WalksPastAJobThatIsAlreadyDone(t *testing.T) {
	// The newest job of a name is usually the finished one from last night.
	// Picking it up would rerun a job that has nothing left and record the
	// results against a run that was already reported.
	s := testStore(t)
	unfinished, err := s.CreateJob(context.Background(), JobSpec{Name: "nightly", Pages: 1}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	done, err := s.CreateJob(context.Background(), JobSpec{Name: "nightly", Pages: 1}, []string{"b"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.FinishJob(context.Background(), done); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}

	got, err := s.LastUnfinished(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("LastUnfinished: %v", err)
	}
	if got.ID != unfinished {
		t.Errorf("LastUnfinished chose job %d, want %d — the finished one is not work", got.ID, unfinished)
	}
}

func TestLastUnfinished_CarriesTheSettingsTheJobWasCreatedWith(t *testing.T) {
	// A job picked up part way has to run as the job it is. Taking today's
	// depth or today's country would put results of one shape into a run made
	// of another, and nothing in the history would say which rows were which.
	// The kind comes back named even though it was created unnamed. What was
	// stored is a search — a job created before there was a choice is the thing
	// this program has always done — and a resume that read the blank back as a
	// blank would have to decide all over again what the run it is carrying on
	// was asking.
	//
	// The pool is part of that: the run being taken up is the one that raises it,
	// so a resume that came back without the numbers would raise whatever the
	// machine felt like instead of what the job asked for. Ports and threads are
	// deliberately different numbers, and different from the depth, so that two
	// of them swapped anywhere along the way is a failure rather than a pass.
	s := testStore(t)
	if _, err := s.CreateJob(context.Background(),
		JobSpec{Name: "nightly", Pages: 3, Device: "desktop", Country: "de", Language: "de",
			Ports: 4, Threads: 7},
		[]string{"a"}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	got, err := s.LastUnfinished(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("LastUnfinished: %v", err)
	}
	want := JobSpec{Name: "nightly", Kind: KindParse, Pages: 3,
		Device: "desktop", Country: "de", Language: "de", Ports: 4, Threads: 7}
	if got.Spec != want {
		t.Errorf("LastUnfinished returned %+v, want %+v", got.Spec, want)
	}
}

// poolOf reads the two pool columns of a job straight out of the database, so a
// test can tell what was written from what a reader would make of it.
func poolOf(t *testing.T, s *Store, jobID int64) (ports, threads int) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT ports, threads FROM jobs WHERE id = ?`, jobID).
		Scan(&ports, &threads); err != nil {
		t.Fatalf("reading the pool of job %d: %v", jobID, err)
	}
	return ports, threads
}

func TestCreateJob_KeepsThePoolTheJobAskedFor(t *testing.T) {
	// The size of the pool is a property of the job now, not of the machine, and
	// a job that reached the database without it would be run on whatever number
	// the program happened to be holding.
	//
	// Four ports and seven threads: two different numbers, neither of them the
	// page count, so a pair written into each other's column fails here instead
	// of much later, on a run nobody can explain.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(),
		JobSpec{Name: "nightly", Pages: 3, Ports: 4, Threads: 7}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if ports, threads := poolOf(t, s, id); ports != 4 || threads != 7 {
		t.Errorf("the job reads back as %d ports and %d threads, want 4 and 7", ports, threads)
	}
}

func TestCreateJob_LeavesAJobThatNamedNoPoolNamingNone(t *testing.T) {
	// Zero is stored as zero and comes back as zero. It says this job named no
	// size, which is the one thing every job written before these columns existed
	// says too, and whoever raises the pool is the only one placed to decide what
	// that becomes. A number put in here instead would be filed as a size the
	// operator asked for, and the page offering to change it could never again
	// tell the two apart.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "plain", Pages: 1}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if ports, threads := poolOf(t, s, id); ports != 0 || threads != 0 {
		t.Errorf("a job that named no pool was stored with %d ports and %d threads, want zero for both",
			ports, threads)
	}
	sum, err := s.Progress(context.Background(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.Ports != 0 || sum.Threads != 0 {
		t.Errorf("a job that named no pool reads back as %d ports and %d threads, want zero for both",
			sum.Ports, sum.Threads)
	}
}

func TestCreateJob_ReadsAPoolBelowNothingAsOneThatWasNeverNamed(t *testing.T) {
	// A negative is a fumbled field, not a run somebody meant, and it is the one
	// value the column itself refuses. Turning the whole job away over it costs
	// more than reading it as the nothing it is.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(),
		JobSpec{Name: "j", Pages: 1, Ports: -4, Threads: -7}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if ports, threads := poolOf(t, s, id); ports != 0 || threads != 0 {
		t.Errorf("a job asking for %d ports and %d threads was stored as %d and %d, want zero for both",
			-4, -7, ports, threads)
	}
}

func TestReshape_ChangesThePoolOfThatJobAndOfNoOther(t *testing.T) {
	// Two jobs, four different numbers between them. With one job in the fixture,
	// a statement that changed every row in the table would pass this, and the
	// first operator to reshape one run would quietly reshape the lot.
	s := testStore(t)
	mine, err := s.CreateJob(context.Background(),
		JobSpec{Name: "mine", Pages: 1, Ports: 4, Threads: 7}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	theirs, err := s.CreateJob(context.Background(),
		JobSpec{Name: "theirs", Pages: 1, Ports: 9, Threads: 2}, []string{"b"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	if err := s.Reshape(context.Background(), mine, 11, 3, 5, 0, false); err != nil {
		t.Fatalf("Reshape: %v", err)
	}
	if ports, threads := poolOf(t, s, mine); ports != 11 || threads != 3 {
		t.Errorf("the reshaped job runs on %d ports and %d threads, want 11 and 3", ports, threads)
	}
	if ports, threads := poolOf(t, s, theirs); ports != 9 || threads != 2 {
		t.Errorf("another job was reshaped to %d ports and %d threads, want the 9 and 2 it asked for",
			ports, threads)
	}
}

func TestCreateJobAndReshape_KeepThePauseTheJobNamed(t *testing.T) {
	// The gap between two requests on one identity is the job's own. It is
	// written in milliseconds and set in seconds, so a job that named forty-five
	// seconds and reads back as forty-five thousand of anything is a unit lost
	// between the form and the column.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(),
		JobSpec{Name: "careful", Pages: 1, Cooldown: 45 * time.Second}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	sum, err := s.Progress(context.Background(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.Cooldown != 45*time.Second {
		t.Errorf("the job rests %v between two requests on one identity, want 45s", sum.Cooldown)
	}

	if err := s.Reshape(context.Background(), id, 2, 1, 5, 90*time.Second, false); err != nil {
		t.Fatalf("Reshape: %v", err)
	}
	sum, err = s.Progress(context.Background(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.Cooldown != 90*time.Second {
		t.Errorf("after the reshape the job rests %v, want the 90s it was changed to", sum.Cooldown)
	}
}

func TestReshape_RefusesAJobThatHasAlreadyFinished(t *testing.T) {
	// A finished job will never raise a pool again, so there is nothing for the
	// new numbers to be read by. Taking them and saying nothing reads as having
	// applied them, and the operator walks away believing a run that is over will
	// come back different.
	//
	// The second job is unfinished and reshaped afterwards, because a refusal
	// that refused everything would pass a test that only ever asked about the
	// finished one. The finished job's pool is read after that second reshape
	// rather than before it: read first, it would say nothing about a statement
	// that reshapes every row it can reach on somebody else's behalf.
	s := testStore(t)
	done, err := s.CreateJob(context.Background(),
		JobSpec{Name: "done", Pages: 1, Ports: 4, Threads: 7}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	running, err := s.CreateJob(context.Background(),
		JobSpec{Name: "running", Pages: 1, Ports: 9, Threads: 2}, []string{"b"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.FinishJob(context.Background(), done); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}

	if err := s.Reshape(context.Background(), done, 11, 3, 5, 0, false); !errors.Is(err, ErrJobFinished) {
		t.Errorf("Reshape returned %v, want ErrJobFinished", err)
	}
	if err := s.Reshape(context.Background(), running, 11, 3, 5, 0, false); err != nil {
		t.Errorf("a job with work left could not be reshaped: %v", err)
	}
	if ports, threads := poolOf(t, s, done); ports != 4 || threads != 7 {
		t.Errorf("the finished job ended up on %d ports and %d threads, want the 4 and 7 it ran on",
			ports, threads)
	}
}

func TestReshape_RefusesAJobThatIsNotThere(t *testing.T) {
	// Writing nothing and reporting nothing wrong would tell somebody who typed
	// the wrong id that their change landed.
	s := testStore(t)
	jobWith(t, s, "a")
	if err := s.Reshape(context.Background(), 4242, 11, 3, 5, 0, false); !errors.Is(err, ErrNoJob) {
		t.Errorf("Reshape returned %v, want ErrNoJob", err)
	}
}

func TestReshape_TakesTheNumbersAJobIsAlreadyOn(t *testing.T) {
	// A form is submitted with the fields it was shown, so the commonest reshape
	// of all changes nothing. Reading that as a job that could not be found would
	// make the ordinary case look like a failure.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(),
		JobSpec{Name: "j", Pages: 1, Ports: 4, Threads: 7}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.Reshape(context.Background(), id, 4, 7, 5, 0, false); err != nil {
		t.Errorf("Reshape of a job onto the pool it already has: %v", err)
	}
	if ports, threads := poolOf(t, s, id); ports != 4 || threads != 7 {
		t.Errorf("the job runs on %d ports and %d threads, want 4 and 7", ports, threads)
	}
}

func TestReshape_ReadsAPoolBelowNothingAsOneThatWasNeverNamed(t *testing.T) {
	// The same reading the job was created under. A reshape that stored what
	// creation refuses would leave the column holding a number no pool can be
	// built from.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(),
		JobSpec{Name: "j", Pages: 1, Ports: 4, Threads: 7}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.Reshape(context.Background(), id, -4, -7, 5, 0, false); err != nil {
		t.Fatalf("Reshape: %v", err)
	}
	if ports, threads := poolOf(t, s, id); ports != 0 || threads != 0 {
		t.Errorf("a reshape to %d ports and %d threads stored %d and %d, want zero for both",
			-4, -7, ports, threads)
	}
}

func TestLastUnfinished_SaysSoWhenThereIsNothingToTakeUp(t *testing.T) {
	s := testStore(t)
	if _, err := s.LastUnfinished(context.Background(), "never-ran"); !errors.Is(err, ErrNoUnfinishedJob) {
		t.Errorf("LastUnfinished returned %v, want ErrNoUnfinishedJob", err)
	}
}

func TestCreateJob_FilesTheJobUnderTheKindItWasAskedFor(t *testing.T) {
	// The kind decides what the run asks Google and what a captured row means
	// afterwards, so it is stored with the job rather than supplied again by
	// whoever happens to pick the job up.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(),
		JobSpec{Name: "addresses", Kind: KindIndex, Pages: 1}, []string{"example.com/a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	sum, err := s.Progress(context.Background(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.Kind != KindIndex {
		t.Errorf("the job reads as kind %q, want %q", sum.Kind, KindIndex)
	}
}

func TestCreateJob_NamesTheKindOfAJobThatNamedNone(t *testing.T) {
	// A caller who says nothing means a parse: that is what this program did
	// before there was a choice. It is written into the column rather than left
	// to the column's own default, so a job read straight back says what it is
	// instead of leaving every reader to work it out again.
	//
	// The word in the column is "search" and stays "search". Every job this
	// program ever wrote down was this kind, and renaming the value would file
	// that whole history under a question none of those runs asked.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(),
		JobSpec{Name: "plain", Pages: 1}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT kind FROM jobs WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("reading the kind back: %v", err)
	}
	if stored != "search" {
		t.Errorf("stored kind %q, want %q", stored, "search")
	}
	if KindParse != "search" {
		t.Errorf("the ordinary kind is written down as %q, and every job already in "+
			"the history says \"search\"", KindParse)
	}
}

func TestCreateJob_RefusesAPositionCheckWithNothingToLookFor(t *testing.T) {
	// The refusal is the whole of what makes "not found" mean anything. A check
	// with no site recognises nothing and settles every phrase in the list as one
	// the site does not rank for, and that answer is indistinguishable from a
	// real one once it is in the history.
	//
	// A site of nothing but spaces is nothing: a browser sends what was typed,
	// and a box somebody tabbed through is not a site.
	s := testStore(t)
	for _, target := range []string{"", "   "} {
		if _, err := s.CreateJob(context.Background(),
			JobSpec{Name: "places", Kind: KindPosition, Target: target, Pages: 1},
			[]string{"a"}); !errors.Is(err, ErrNoTarget) {
			t.Errorf("CreateJob with %q to look for gave %v, want ErrNoTarget", target, err)
		}
	}
	var jobs int
	if err := s.db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&jobs); err != nil {
		t.Fatalf("counting jobs: %v", err)
	}
	if jobs != 0 {
		t.Errorf("%d jobs were written down although they were refused", jobs)
	}
}

func TestCreateJob_TakesTheOtherKindsWithNoSiteNamed(t *testing.T) {
	// The site belongs to one kind. A parse is about every site the page carried
	// and an index check is about the address on the line, so demanding one of
	// them would refuse the two jobs this program spent a year doing.
	s := testStore(t)
	for _, kind := range []string{KindParse, KindIndex, ""} {
		if _, err := s.CreateJob(context.Background(),
			JobSpec{Name: "j", Kind: kind, Pages: 1}, []string{"a"}); err != nil {
			t.Errorf("CreateJob of kind %q with no site named: %v", kind, err)
		}
	}
}

func TestCreateJob_FilesTheSiteAPositionCheckIsAboutAndHandsItToAResume(t *testing.T) {
	// A check taken up part way has to go on being about the same site. Carried
	// on against another one, the column of positions it leaves stands for two
	// questions and nothing in the history says where the change fell.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(),
		JobSpec{Name: "places", Kind: KindPosition, Target: "  example.com/wanted  ", Pages: 1},
		[]string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	var stored string
	if err := s.db.QueryRow(`SELECT target FROM jobs WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("reading the site back: %v", err)
	}
	if stored != "example.com/wanted" {
		t.Errorf("stored %q, want the site with the spaces a browser sent around it taken off", stored)
	}

	sum, err := s.Progress(context.Background(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.Target != "example.com/wanted" {
		t.Errorf("the job reads back as being about %q, want example.com/wanted", sum.Target)
	}
	taken, err := s.LastUnfinished(context.Background(), "places")
	if err != nil {
		t.Fatalf("LastUnfinished: %v", err)
	}
	if taken.Spec.Target != "example.com/wanted" {
		t.Errorf("the resumed check is about %q, want example.com/wanted", taken.Spec.Target)
	}
}

func TestCreateJob_KeepsTheRetryLimitTheJobAskedFor(t *testing.T) {
	// The limit belongs to the job because the answer depends on the list. A
	// store that dropped it would leave every job on the built-in number, which
	// is exactly the state this column exists to end.
	s := testStore(t)
	id, err := s.CreateJob(t.Context(),
		JobSpec{Name: "on a tired list", Pages: 1, Ports: 9, Threads: 4, Tries: 17},
		[]string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	got, err := s.Jobs(t.Context(), 10)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("the job was not read back: %+v", got)
	}
	// The three numbers are all different on purpose: a store that read one
	// column into another would answer this test with the wrong one and pass.
	if got[0].Tries != 17 || got[0].Ports != 9 || got[0].Threads != 4 {
		t.Errorf("tries=%d ports=%d threads=%d, want 17, 9 and 4",
			got[0].Tries, got[0].Ports, got[0].Threads)
	}
}

func TestCreateJob_LeavesAJobThatNamedNoRetryLimitNamingNone(t *testing.T) {
	// Nought is a job that said nothing, and the store must not invent a number
	// for it: what an unspoken limit becomes is decided where the run is set up,
	// and a store with a second opinion would hide which jobs are in that state.
	s := testStore(t)
	if _, err := s.CreateJob(t.Context(), JobSpec{Name: "said nothing", Pages: 1}, []string{"a"}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	got, err := s.Jobs(t.Context(), 10)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if got[0].Tries != 0 {
		t.Errorf("tries=%d, want the nought that says the job named none", got[0].Tries)
	}
}

func TestTryFailedAgain_PutsTheFailuresBackInTheQueueAndLeavesTheRest(t *testing.T) {
	// A pool that goes away fails every query it is asked for, as fast as the
	// list can be walked. What that leaves is a job with nothing pending — and
	// so nothing to resume — while thousands of phrases were never really asked.
	// Measured on a live run: 24 787 of them, every one recorded as "every
	// candidate port is quarantined".
	s := testStore(t)
	ctx := context.Background()
	id, err := s.CreateJob(ctx, JobSpec{Name: "starved"}, []string{"one", "two", "three"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.Record(ctx, id, QueryOutcome{Ordinal: 0}); err != nil {
		t.Fatalf("recording the one that answered: %v", err)
	}
	if err := s.Record(ctx, id, QueryOutcome{Ordinal: 1, Err: errors.New("blanktrail: every candidate port is quarantined")}); err != nil {
		t.Fatalf("recording the one that failed: %v", err)
	}

	moved, err := s.TryFailedAgain(ctx, id)
	if err != nil {
		t.Fatalf("TryFailedAgain: %v", err)
	}
	if moved != 1 {
		t.Errorf("took back %d queries, want the one that failed", moved)
	}

	sum, err := s.Progress(ctx, id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.Failed != 0 {
		t.Errorf("%d queries are still failed after being taken back", sum.Failed)
	}
	if sum.Pending != 2 {
		t.Errorf("%d queries are pending, want the untried one and the one taken back", sum.Pending)
	}
	// What answered stays answered: this is not a way to ask a whole job again.
	if sum.Done != 1 {
		t.Errorf("%d queries are done, want the one that answered left alone", sum.Done)
	}
	// And the job is no longer done. The stamp is read by more than the word on
	// the screen: it is what tells the page whether to ask again while the job
	// runs, so a job left stamped ran on behind a screen that sat still.
	if sum.Finished {
		t.Error("the job is still stamped finished with work back in front of it")
	}
}

func TestReshape_CarriesWhetherTheJobSpendsTheWholeList(t *testing.T) {
	// The four numbers beside it can be changed on a job's own page, and so can
	// this: a run that has not started yet is a run whose pool is still an open
	// question. A reshape that dropped it would tick the box on the screen and
	// start the job on the pool it always had.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "whole", Pages: 1}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if wholePoolOf(t, s, id) {
		t.Fatal("a job that asked for nothing was written spending the whole list")
	}

	if err := s.Reshape(context.Background(), id, 2, 1, 5, 0, true); err != nil {
		t.Fatalf("Reshape: %v", err)
	}
	if !wholePoolOf(t, s, id) {
		t.Error("the reshape did not carry the whole list the reader asked for")
	}

	// And back off again, which is the half a flag written only when true loses.
	if err := s.Reshape(context.Background(), id, 2, 1, 5, 0, false); err != nil {
		t.Fatalf("Reshape back: %v", err)
	}
	if wholePoolOf(t, s, id) {
		t.Error("the tick could not be taken off again")
	}
}

// wholePoolOf reads back whether a job spends the whole list.
func wholePoolOf(t *testing.T, s *Store, id int64) bool {
	t.Helper()
	sum, err := s.Progress(context.Background(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	return sum.WholePool
}
