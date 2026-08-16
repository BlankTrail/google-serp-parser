// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
	"github.com/blanktrail/google-serp-parser/run"
	"github.com/blanktrail/google-serp-parser/store"
)

// pollGap is how often a test looks again at something a worker goroutine is
// changing. A supervisor at rest looks the same however it is built, so every
// question worth asking has to be asked while it is moving.
const pollGap = 2 * time.Millisecond

// patience bounds every wait in this file. It is long enough that a loaded
// machine does not fail the run and short enough that a supervisor which never
// takes the next job is reported as a failure rather than as a hung suite.
const patience = 10 * time.Second

// heldEngine stands in for whatever a job's queries are taken through. It can
// be stopped between two queries, which is the only place the questions about a
// queue can be asked: what happens to the job behind the one in flight, and
// what a stop leaves behind.
//
// It records each query through the sink it is handed, so the history a real
// job leaves is the history these tests read.
type heldEngine struct {
	// hold is received from before each query. Nil lets every query through, so
	// a job runs to the end without being driven.
	hold chan struct{}

	mu     sync.Mutex
	jobs   []run.Job
	live   int
	most   int
	closes int
}

func (e *heldEngine) Run(ctx context.Context, j run.Job, sink run.Sink) run.Report {
	e.enter(j)
	defer e.leave()

	rep := run.Report{Results: make([]run.QueryResult, len(j.Queries))}
	for i := range j.Queries {
		res := run.QueryResult{Query: j.Queries[i], Ordinal: i}
		if len(j.Ordinals) != 0 {
			res.Ordinal = j.Ordinals[i]
		}
		if !e.step(ctx) {
			rep.Untried = len(j.Queries) - i
			break
		}
		res.Attempted = true
		if err := sink.Record(ctx, res); err != nil {
			res.Err = err
		}
		rep.Results[i] = res
		rep.Done++
	}
	return rep
}

func (e *heldEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closes++
	return nil
}

func (e *heldEngine) step(ctx context.Context) bool {
	if e.hold == nil {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-e.hold:
		return true
	}
}

func (e *heldEngine) enter(j run.Job) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.jobs = append(e.jobs, j)
	e.live++
	if e.live > e.most {
		e.most = e.live
	}
}

func (e *heldEngine) leave() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.live--
}

// atMostAtOnce is the largest number of jobs that were ever inside the engine
// together.
func (e *heldEngine) atMostAtOnce() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.most
}

// ran returns the nth job the engine was handed, and whether it saw that many.
func (e *heldEngine) ran(n int) (run.Job, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n >= len(e.jobs) {
		return run.Job{}, false
	}
	return e.jobs[n], true
}

func (e *heldEngine) timesClosed() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closes
}

// let releases n queries, waiting rather than failing if the supervisor has not
// reached them yet.
func (e *heldEngine) let(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case e.hold <- struct{}{}:
		case <-time.After(patience):
			t.Fatalf("the supervisor did not reach query %d", i+1)
		}
	}
}

// heldSupervisor is a supervisor whose engine a test drives query by query.
func heldSupervisor(t *testing.T) (*Supervisor, *store.Store, *heldEngine) {
	t.Helper()
	st := testStore(t)
	eng := &heldEngine{hold: make(chan struct{})}
	v := newSupervisor(st, eng)
	t.Cleanup(func() { _ = v.Close() })
	return v, st, eng
}

// enqueue starts a job of the given queries and returns its id.
func enqueue(t *testing.T, v *Supervisor, name string, queries ...string) int64 {
	t.Helper()
	id, err := v.Enqueue(store.JobSpec{Name: name, Pages: 1, Country: "us", Language: "en"}, queries)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return id
}

// waitUntil polls until the condition holds, and says what it was waiting for
// when it never does.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(patience)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(pollGap)
	}
	t.Fatalf("timed out waiting until %s", what)
}

// progress reads how far a job has got.
func progress(t *testing.T, st *store.Store, id int64) store.JobSummary {
	t.Helper()
	sum, err := st.Progress(context.Background(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	return sum
}

func TestSupervisor_RunsOneJobAtATimeAndQueuesTheNext(t *testing.T) {
	// A second job running alongside the first would need a second set of
	// identities, and those are the expensive part. The question can only be
	// asked while the first job is in the middle: two jobs that have both
	// finished look the same whether they ran together or in turn.
	v, st, eng := heldSupervisor(t)
	first := enqueue(t, v, "first", "a")
	second := enqueue(t, v, "second", "b")

	// The wait asks only that a job has reached the engine, so that a
	// supervisor which went on to the second one is reported by the assertions
	// below rather than by a timeout that says nothing about why.
	waitUntil(t, "a job has reached the engine", func() bool {
		_, taken := eng.ran(0)
		return taken
	})
	for i := 0; i < 40; i++ {
		if got := eng.atMostAtOnce(); got != 1 {
			t.Fatalf("%d jobs were inside the engine at once, want 1", got)
		}
		if id, ok := v.Running(); !ok || id != first {
			t.Fatalf("running job is %d (%v), want the first one", id, ok)
		}
		if q := v.Queued(); len(q) != 1 || q[0] != second {
			t.Fatalf("queue is %v, want the second job alone", q)
		}
		time.Sleep(pollGap)
	}

	eng.let(t, 1)
	waitUntil(t, "the second job is running", func() bool {
		id, ok := v.Running()
		return ok && id == second
	})
	eng.let(t, 1)
	waitUntil(t, "both jobs are done", func() bool {
		return progress(t, st, first).Finished && progress(t, st, second).Finished
	})
	if got := eng.atMostAtOnce(); got != 1 {
		t.Errorf("%d jobs were inside the engine at once, want 1", got)
	}
}

// standInPool opens a pool against a stand-in for the service behind it. What
// it hands out reaches nothing on this machine, so every query fails and no
// request leaves — which is beside the point here: what is asked is whether the
// pool still holds what it held between two jobs, and a pool that was given up
// is one the stand-in no longer lists anything for.
func standInPool(t *testing.T, ports int) (*blanktrail.Pool, *fakebt.Server) {
	t.Helper()
	fake := fakebt.New(t)
	cl, err := blanktrail.NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	p, err := blanktrail.NewPool(context.Background(), blanktrail.PoolConfig{
		Client:           cl,
		Threads:          ports,
		PortsPerThread:   1,
		Spec:             blanktrail.DefaultPortSpec(),
		Channels:         []blanktrail.Channel{blanktrail.NewDirectChannel("direct")},
		Insecure:         true,
		Cooldown:         time.Nanosecond,
		MaxRetriesPerReq: 1,
		Sleep:            func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, fake
}

func TestSupervisor_KeepsThePoolBetweenJobs(t *testing.T) {
	// A pool of identities answers better the longer it has been used, and
	// getting there costs minutes of work. A supervisor that opened one per job
	// would pay that again for every job and hold twice as much while it did.
	//
	// The closing assertion is what gives the other two teeth: what the pool
	// holds does disappear when the pool is given up, so finding it still there
	// after a job means the pool outlived the job.
	pool, fake := standInPool(t, 2)
	st := testStore(t)
	v := NewSupervisor(st, pool, 1)
	t.Cleanup(func() { _ = v.Close() })

	first := enqueue(t, v, "first", "a")
	waitUntil(t, "the first job is done", func() bool { return progress(t, st, first).Finished })
	if pool.Size() == 0 || len(fake.OpenPorts()) == 0 {
		t.Fatalf("the identities were given up when the first job ended: %d held, %d open",
			pool.Size(), len(fake.OpenPorts()))
	}

	second := enqueue(t, v, "second", "b")
	waitUntil(t, "the second job is done", func() bool { return progress(t, st, second).Finished })
	if pool.Size() == 0 || len(fake.OpenPorts()) == 0 {
		t.Fatalf("the identities were given up between the two jobs: %d held, %d open",
			pool.Size(), len(fake.OpenPorts()))
	}

	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(fake.OpenPorts()); got != 0 {
		t.Errorf("%d identities still open after the supervisor closed, want none", got)
	}
}

func TestSupervisor_StopLeavesWhatWasRecordedAndTheRestPending(t *testing.T) {
	// Stopping is not cancelling. What was reached is kept, what was not stays
	// waiting, and the job is not stamped done — which is the whole of what a
	// resume takes up.
	v, st, eng := heldSupervisor(t)
	id := enqueue(t, v, "nightly", "a", "b", "c", "d")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})
	eng.let(t, 2)
	waitUntil(t, "two queries are recorded", func() bool { return progress(t, st, id).Done == 2 })

	if err := v.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitUntil(t, "the job has stopped", func() bool { _, ok := v.Running(); return !ok })

	sum := progress(t, st, id)
	if sum.Done != 2 || sum.Pending != 2 || sum.Failed != 0 {
		t.Errorf("done=%d failed=%d pending=%d, want 2/0/2", sum.Done, sum.Failed, sum.Pending)
	}
	if sum.Finished {
		t.Error("the stopped job was stamped done, and nothing is left for a resume to take up")
	}
}

func TestSupervisor_ResumeTakesOnlyWhatWasLeft(t *testing.T) {
	// A job taken up part way holds what is left, under the numbers those
	// queries were given. Numbering them afresh files every result against the
	// wrong query.
	v, st, eng := heldSupervisor(t)
	id := enqueue(t, v, "nightly", "a", "b", "c", "d")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})
	eng.let(t, 2)
	waitUntil(t, "two queries are recorded", func() bool { return progress(t, st, id).Done == 2 })
	if err := v.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitUntil(t, "the job has stopped", func() bool { _, ok := v.Running(); return !ok })

	if err := v.Resume(id); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	// What the resumed job carries is read before it is let through, so a job
	// carrying the wrong queries is reported as that rather than as whatever it
	// goes on to do with them.
	waitUntil(t, "the resumed job has reached the engine", func() bool {
		_, ok := eng.ran(1)
		return ok
	})
	taken, ok := eng.ran(1)
	if !ok {
		t.Fatal("the resumed job never reached the engine")
	}
	if len(taken.Queries) != 2 {
		t.Fatalf("the resumed job carried %d queries, want the 2 that were left", len(taken.Queries))
	}
	if taken.Queries[0].Text != "c" || taken.Queries[1].Text != "d" {
		t.Errorf("the resumed job carried %q and %q, want c and d",
			taken.Queries[0].Text, taken.Queries[1].Text)
	}
	if len(taken.Ordinals) != 2 || taken.Ordinals[0] != 2 || taken.Ordinals[1] != 3 {
		t.Fatalf("the resumed queries were numbered %v, want 2 and 3", taken.Ordinals)
	}

	eng.let(t, 2)
	waitUntil(t, "the job is done", func() bool { return progress(t, st, id).Finished })
	if sum := progress(t, st, id); sum.Done != 4 || sum.Pending != 0 {
		t.Errorf("done=%d pending=%d, want 4/0", sum.Done, sum.Pending)
	}
}

func TestSupervisor_StopOfAJobThatIsNotRunningSaysSo(t *testing.T) {
	// A stop is aimed at one job. Aiming it at a job that is only waiting, or
	// at nothing at all, must not end whichever job happens to be in flight.
	v, _, eng := heldSupervisor(t)

	if err := v.Stop(1); !errors.Is(err, ErrNotRunning) {
		t.Errorf("stopping with nothing running: %v, want ErrNotRunning", err)
	}

	first := enqueue(t, v, "first", "a")
	second := enqueue(t, v, "second", "b")
	waitUntil(t, "the first job is running", func() bool {
		id, ok := v.Running()
		return ok && id == first
	})
	if err := v.Stop(second); !errors.Is(err, ErrNotRunning) {
		t.Errorf("stopping the waiting job: %v, want ErrNotRunning", err)
	}
	if id, ok := v.Running(); !ok || id != first {
		t.Fatalf("running job is %d (%v) — stopping the waiting job ended the running one", id, ok)
	}
	eng.let(t, 1)
	waitUntil(t, "the second job is running", func() bool {
		id, ok := v.Running()
		return ok && id == second
	})
}

func TestSupervisor_StopLeavesTheJobsBehindItInTheQueue(t *testing.T) {
	// Stopping ends the job in flight and nothing else. A stop that emptied the
	// queue would throw away work nobody asked to throw away, and the reader
	// who pressed it would have no way of knowing.
	v, st, eng := heldSupervisor(t)
	first := enqueue(t, v, "first", "a", "b")
	second := enqueue(t, v, "second", "c")
	waitUntil(t, "the first job is running", func() bool {
		id, ok := v.Running()
		return ok && id == first
	})
	eng.let(t, 1)
	waitUntil(t, "one query is recorded", func() bool { return progress(t, st, first).Done == 1 })

	if err := v.Stop(first); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitUntil(t, "the second job is running", func() bool {
		id, ok := v.Running()
		return ok && id == second
	})
	eng.let(t, 1)
	waitUntil(t, "the second job is done", func() bool { return progress(t, st, second).Finished })

	if sum := progress(t, st, first); sum.Finished || sum.Pending != 1 {
		t.Errorf("the stopped job: finished=%v pending=%d, want false/1", sum.Finished, sum.Pending)
	}
}

func TestSupervisor_QueuedHandsBackACopy(t *testing.T) {
	// The queue is what the worker takes its next job from. A caller handed the
	// list itself is reading something being rewritten under it, and writing to
	// it decides what runs next.
	v, _, _ := heldSupervisor(t)
	enqueue(t, v, "first", "a")
	second := enqueue(t, v, "second", "b")
	third := enqueue(t, v, "third", "c")

	waitUntil(t, "two jobs are waiting", func() bool { return len(v.Queued()) == 2 })
	got := v.Queued()
	got[0], got[1] = 0, 0
	after := v.Queued()
	if len(after) != 2 || after[0] != second || after[1] != third {
		t.Errorf("the queue is now %v, want %d and %d — the caller was handed the queue itself",
			after, second, third)
	}
}

func TestSupervisor_CloseEndsTheRunningJobAndReturns(t *testing.T) {
	// A supervisor that would not shut down while a job was in flight would
	// hold the whole program open for as long as the job had left to run.
	st := testStore(t)
	eng := &heldEngine{hold: make(chan struct{})}
	v := newSupervisor(st, eng)

	id := enqueue(t, v, "nightly", "a", "b")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})
	eng.let(t, 1)
	waitUntil(t, "one query is recorded", func() bool { return progress(t, st, id).Done == 1 })

	closed := make(chan error, 1)
	go func() { closed <- v.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(patience):
		t.Fatal("Close did not return while a job was running")
	}

	if got := eng.timesClosed(); got != 1 {
		t.Errorf("the engine was closed %d times, want 1", got)
	}
	if sum := progress(t, st, id); sum.Finished || sum.Pending != 1 {
		t.Errorf("the job left by the shutdown: finished=%v pending=%d, want false/1",
			sum.Finished, sum.Pending)
	}
	if _, err := v.Enqueue(store.JobSpec{Name: "late"}, []string{"x"}); !errors.Is(err, ErrClosed) {
		t.Errorf("enqueueing after the shutdown: %v, want ErrClosed", err)
	}
}

func TestSupervisor_RefusesToTakeUpAJobItIsAlreadyHolding(t *testing.T) {
	// A job queued twice would be run twice, and the second run would find
	// nothing left and stamp a job the first one was still working through.
	v, _, _ := heldSupervisor(t)
	first := enqueue(t, v, "first", "a")
	second := enqueue(t, v, "second", "b")

	waitUntil(t, "the first job is running", func() bool {
		id, ok := v.Running()
		return ok && id == first
	})
	if err := v.Resume(first); !errors.Is(err, ErrBusy) {
		t.Errorf("resuming the running job: %v, want ErrBusy", err)
	}
	if err := v.Resume(second); !errors.Is(err, ErrBusy) {
		t.Errorf("resuming the waiting job: %v, want ErrBusy", err)
	}
	if q := v.Queued(); len(q) != 1 {
		t.Errorf("the queue holds %v, want the second job alone", q)
	}
}

func TestSupervisor_RefusesToTakeUpAJobWithNothingLeft(t *testing.T) {
	// Told to carry on with a job that has nothing left, a supervisor that
	// queued it anyway would answer the reader with a run that does nothing.
	v, st, eng := heldSupervisor(t)
	id := enqueue(t, v, "nightly", "a")
	eng.let(t, 1)
	waitUntil(t, "the job is done", func() bool { return progress(t, st, id).Finished })

	if err := v.Resume(id); !errors.Is(err, ErrNothingLeft) {
		t.Errorf("resuming a finished job: %v, want ErrNothingLeft", err)
	}
	if err := v.Resume(id + 1000); !errors.Is(err, store.ErrNoJob) {
		t.Errorf("resuming a job nothing was stored under: %v, want ErrNoJob", err)
	}
}
