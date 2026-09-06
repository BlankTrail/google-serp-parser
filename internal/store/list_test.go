// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/google"
)

func TestJobs_ListsTheNewestFirst(t *testing.T) {
	// A list is read from the top, and the run someone wants is almost always
	// the one they just started. The three jobs are stamped so that neither the
	// order they were written in nor its reverse is the order they must come
	// back in, which is what makes the answer say something about the ordering.
	s := testStore(t)
	middle := jobWith(t, s, "a")
	stampJob(t, s, middle, "2026-08-02T10:00:00Z")
	newest := jobWith(t, s, "b")
	stampJob(t, s, newest, "2026-08-03T10:00:00Z")
	oldest := jobWith(t, s, "c")
	stampJob(t, s, oldest, "2026-08-01T10:00:00Z")

	got, err := s.Jobs(context.Background(), 10)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	want := []int64{newest, middle, oldest}
	if len(got) != len(want) {
		t.Fatalf("%d jobs, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("job %d of the list is %d, want %d — newest first", i, got[i].ID, w)
		}
	}
}

func TestJobs_PutsTheLaterOfTwoJobsStampedTheSameSecondFirst(t *testing.T) {
	// A stamp is written to the second, so two jobs started in the same minute
	// are ordered by which of them was written last. Without that the list
	// would put the older of the pair on top of the newer.
	s := testStore(t)
	earlier := jobWith(t, s, "a")
	later := jobWith(t, s, "b")
	stampJob(t, s, earlier, "2026-08-01T10:00:00Z")
	stampJob(t, s, later, "2026-08-01T10:00:00Z")

	got, err := s.Jobs(context.Background(), 10)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d jobs, want 2", len(got))
	}
	if got[0].ID != later {
		t.Errorf("the list starts with job %d, want %d — the later of the two", got[0].ID, later)
	}
}

func TestJobs_CountsEachStateAgainstTheSameMoment(t *testing.T) {
	// Counting done, failed and pending separately gives three snapshots of
	// three different moments, and on a running job they will not add up to
	// the total. One pass over the queries cannot disagree with itself.
	s := testStore(t)
	id := jobWith(t, s, "a", "b", "c", "d")
	mustRecord(t, s, id, 0, "one.test")
	mustRecord(t, s, id, 1, "two.test")
	if err := s.Record(context.Background(), id, QueryOutcome{Ordinal: 2, Err: errors.New("refused")}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := s.Progress(context.Background(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if got.Total != 4 || got.Done != 2 || got.Failed != 1 || got.Pending != 1 {
		t.Errorf("total=%d done=%d failed=%d pending=%d, want 4/2/1/1",
			got.Total, got.Done, got.Failed, got.Pending)
	}
	if got.Done+got.Failed+got.Pending != got.Total {
		t.Error("the counts do not add up to the total")
	}
}

func TestProgress_KeepsTheCountsAddingUpWhileTheJobIsStillRunning(t *testing.T) {
	// Counting each state on its own asks the database four separate questions,
	// and work that lands between two of them is counted twice or not at all.
	// Somebody watching a running job would see a bar jump backwards or past its
	// own end. Only a reading taken while the job is being written to tells one
	// pass from four: on a job standing still the two agree.
	s := testStore(t)
	const n = 40
	queries := make([]string, n)
	for i := range queries {
		queries[i] = fmt.Sprintf("q%d", i)
	}
	id := jobWith(t, s, queries...)

	recorded := make(chan struct{})
	go func() {
		defer close(recorded)
		for i := 0; i < n; i++ {
			err := s.Record(context.Background(), id, QueryOutcome{
				Ordinal: i,
				Pages: []google.SERP{{
					Origin:  "https://www.google.com",
					Results: []google.Result{{Title: "One", URL: "https://one.test/1", Host: "one.test"}},
				}},
			})
			if err != nil {
				t.Errorf("Record(ordinal %d): %v", i, err)
				return
			}
		}
	}()

	var wrong string
	for running := true; running && wrong == ""; {
		select {
		case <-recorded:
			running = false
		default:
		}
		got, err := s.Progress(context.Background(), id)
		switch {
		case err != nil:
			wrong = fmt.Sprintf("Progress: %v", err)
		case got.Done+got.Failed+got.Pending != got.Total:
			wrong = fmt.Sprintf("done=%d failed=%d pending=%d against a total of %d",
				got.Done, got.Failed, got.Pending, got.Total)
		}
	}
	<-recorded
	if wrong != "" {
		t.Error(wrong)
	}
}

func TestProgress_CountsOnlyTheQueriesOfTheJobItWasAsked(t *testing.T) {
	// Two jobs share a table, and a count that forgets to say which job it is
	// counting reports a night's work as this morning's.
	s := testStore(t)
	mine := jobWith(t, s, "a", "b")
	theirs := jobWith(t, s, "c", "d", "e")
	mustRecord(t, s, theirs, 0, "one.test")

	got, err := s.Progress(context.Background(), mine)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if got.Total != 2 || got.Pending != 2 || got.Done != 0 {
		t.Errorf("total=%d pending=%d done=%d, want 2/2/0 — the other job's work is not this one's",
			got.Total, got.Pending, got.Done)
	}
}

func TestProgress_CarriesTheSettingsAndTheTimes(t *testing.T) {
	// A list that shows only names cannot tell last night's mobile run from
	// this morning's desktop one.
	s := testStore(t)
	id, err := s.CreateJob(context.Background(),
		JobSpec{Name: "nightly", Pages: 3, Country: "us", Language: "en", Device: "desktop"},
		[]string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	got, err := s.Progress(context.Background(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if got.Name != "nightly" || got.Pages != 3 || got.Country != "us" ||
		got.Language != "en" || got.Device != "desktop" {
		t.Errorf("settings came back as %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("the job carries no creation time")
	}
	if got.Finished {
		t.Error("a job with work left reports itself finished")
	}
	if !got.FinishedAt.IsZero() {
		t.Errorf("a job with work left carries a finish time of %v", got.FinishedAt)
	}
}

func TestJobs_CarryThePoolEachOfThemRunsOn(t *testing.T) {
	// Two jobs no longer share a pool, so a listing that showed one job's numbers
	// against another's name would be describing runs that never happened.
	//
	// All four numbers differ, and neither job's pair matches the other's or its
	// own depth. That is what makes this say something: with two jobs asking for
	// the same pool, a listing reading one row's ports for every row would pass,
	// and with ports equal to threads a pair of columns read in the wrong order
	// would pass as well.
	s := testStore(t)
	small, err := s.CreateJob(context.Background(),
		JobSpec{Name: "small", Pages: 1, Ports: 4, Threads: 7}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	stampJob(t, s, small, "2026-08-01T10:00:00Z")
	large, err := s.CreateJob(context.Background(),
		JobSpec{Name: "large", Pages: 1, Ports: 9, Threads: 2}, []string{"b"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	stampJob(t, s, large, "2026-08-02T10:00:00Z")

	listed, err := s.Jobs(context.Background(), 10)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	want := map[int64][2]int{small: {4, 7}, large: {9, 2}}
	if len(listed) != len(want) {
		t.Fatalf("%d jobs listed, want %d", len(listed), len(want))
	}
	for _, got := range listed {
		if w := want[got.ID]; got.Ports != w[0] || got.Threads != w[1] {
			t.Errorf("job %q is listed on %d ports and %d threads, want %d and %d",
				got.Name, got.Ports, got.Threads, w[0], w[1])
		}
	}

	one, err := s.Progress(context.Background(), small)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if one.Ports != 4 || one.Threads != 7 {
		t.Errorf("job %q reads on its own as %d ports and %d threads, want 4 and 7",
			one.Name, one.Ports, one.Threads)
	}
}

func TestProgress_SaysAFinishedJobIsFinished(t *testing.T) {
	s := testStore(t)
	id := jobWith(t, s, "a")
	mustRecord(t, s, id, 0, "one.test")
	if err := s.FinishJob(context.Background(), id); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}

	got, err := s.Progress(context.Background(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if !got.Finished || got.FinishedAt.IsZero() {
		t.Errorf("finished=%v at=%v, want a finished job with a time", got.Finished, got.FinishedAt)
	}
}

func TestProgress_RefusesAJobThatIsNotThere(t *testing.T) {
	// Answering with an empty summary would show a page of zeroes for a job
	// that does not exist, which reads as a job that found nothing.
	s := testStore(t)
	if _, err := s.Progress(context.Background(), 4242); !errors.Is(err, ErrNoJob) {
		t.Errorf("Progress returned %v, want ErrNoJob", err)
	}
}

func TestProgress_KeepsAJobWithNoQueriesOfItsOwn(t *testing.T) {
	// A job nothing is recorded against is still a job somebody started. Dropped
	// from the list it reads as a run that vanished, and refused by name it
	// reads as a run that never existed.
	s := testStore(t)
	withWork := jobWith(t, s, "a")
	stampJob(t, s, withWork, "2026-08-01T10:00:00Z")
	res, err := s.db.Exec(
		`INSERT INTO jobs(name, created_at, pages) VALUES('bare', '2026-08-02T10:00:00Z', 1)`)
	if err != nil {
		t.Fatalf("writing a job with no queries: %v", err)
	}
	bare, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("reading the job id: %v", err)
	}

	listed, err := s.Jobs(context.Background(), 10)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("%d jobs listed, want both", len(listed))
	}
	if listed[0].ID != bare {
		t.Errorf("the list starts with job %d, want the newer %d", listed[0].ID, bare)
	}

	got, err := s.Progress(context.Background(), bare)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if got.Total != 0 || got.Done != 0 || got.Failed != 0 || got.Pending != 0 {
		t.Errorf("total=%d done=%d failed=%d pending=%d, want no work at all",
			got.Total, got.Done, got.Failed, got.Pending)
	}
	if got.Name != "bare" {
		t.Errorf("the job came back named %q, want %q", got.Name, "bare")
	}
}

func TestJobs_HonoursTheLimitAndTakesTheNewest(t *testing.T) {
	// A limit that keeps the first rows it meets rather than the newest turns a
	// short list into a list of the runs nobody is looking for.
	s := testStore(t)
	var ids []int64
	for _, day := range []int{3, 5, 1, 4, 2} {
		id := jobWith(t, s, "q")
		stampJob(t, s, id, fmt.Sprintf("2026-08-%02dT10:00:00Z", day))
		ids = append(ids, id)
	}

	got, err := s.Jobs(context.Background(), 2)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d jobs, want 2", len(got))
	}
	want := []int64{ids[1], ids[3]}
	if got[0].ID != want[0] || got[1].ID != want[1] {
		t.Errorf("the limit kept jobs %d, %d — want the two newest %d, %d, newest first",
			got[0].ID, got[1].ID, want[0], want[1])
	}
}

func TestJobs_ANonPositiveLimitStillReturnsSomething(t *testing.T) {
	// A page that asks for zero rows because a parameter was missing should
	// show a sensible page, not an empty one.
	s := testStore(t)
	jobWith(t, s, "a")
	got, err := s.Jobs(context.Background(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(got) == 0 {
		t.Error("a zero limit returned nothing at all")
	}
}

func TestResultCount_CountsWhatOneJobCollectedAndNothingElse(t *testing.T) {
	// The count is read off a walk that starts at the results and climbs to the
	// job, which is exactly the walk that has no index unless one is built for
	// it — and the failure of getting it wrong is silent: a number that is right
	// on a machine with one job and wrong on every machine with two.
	st := testStore(t)
	ctx := context.Background()

	mine := jobWithResults(t, st, "mine", 3)
	theirs := jobWithResults(t, st, "theirs", 5)

	for _, one := range []struct {
		job  int64
		want int
	}{{mine, 3}, {theirs, 5}} {
		got, err := st.ResultCount(ctx, one.job)
		if err != nil {
			t.Fatalf("ResultCount: %v", err)
		}
		if got != one.want {
			t.Errorf("job %d counted %d results, want %d", one.job, got, one.want)
		}
	}

	// And a job that has run nothing counts nothing rather than failing to read.
	bare, err := st.CreateJob(ctx, JobSpec{Name: "bare", Pages: 1}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if got, err := st.ResultCount(ctx, bare); err != nil || got != 0 {
		t.Errorf("a job that has collected nothing counted %d (%v), want 0", got, err)
	}
}

// jobWithResults writes one job whose single query came back with the given
// number of results.
func jobWithResults(t *testing.T, st *Store, name string, results int) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.CreateJob(ctx, JobSpec{Name: name, Pages: 1}, []string{"one"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	found := make([]google.Result, results)
	for i := range found {
		found[i] = google.Result{Position: i + 1, URL: fmt.Sprintf("https://%s.test/%d", name, i)}
	}
	err = st.Record(ctx, id, QueryOutcome{
		Ordinal: 0,
		Pages:   []google.SERP{{Origin: "https://www.google.com", Results: found}},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	return id
}
