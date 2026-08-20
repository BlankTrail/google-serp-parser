// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
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
	// linger is received from after the last query and before the job leaves the
	// engine. It is how a test holds a job inside an engine it has been told to
	// stop, which is the one arrangement in which «given up too early» differs
	// from «given up». Nil lets a job leave the moment it is done.
	linger chan struct{}
	// gone, when set, is told the instant this engine is given up. It is how
	// whatever raised it counts how many are open at once, which is the only way
	// to tell a pool per job from one pool for all of them while jobs are running.
	gone func()

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
	if e.linger != nil {
		<-e.linger
	}
	return rep
}

// fakePool is what a held engine says about the identities behind it.
//
// Every number differs from every other, and none is zero: a fixture of zeroes
// would let a screen showing the wrong count, or no count at all, agree with a
// test that expected the right one.
var fakePool = poolFacts{
	Stats: blanktrail.Stats{
		Ports: 6, Available: 5, Quarantined: 1,
		EgressRotations: 4, Revivals: 2,
	},
	Threads:  2,
	Cooldown: 3 * time.Second,
}

func (e *heldEngine) Pool() poolFacts { return fakePool }

func (e *heldEngine) Close() error {
	e.mu.Lock()
	e.closes++
	gone := e.gone
	e.mu.Unlock()
	// Told with this engine's own lock let go, because what is told takes a lock
	// of its own and holding both would be two locks taken in two orders.
	if gone != nil {
		gone()
	}
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

// took says whether the engine has been handed a job carrying this query.
//
// A job reaches an engine as the work it has left, under no name and no number,
// so the text of a query is what tells one job from another here.
func (e *heldEngine) took(query string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, j := range e.jobs {
		for _, q := range j.Queries {
			if q.Text == query {
				return true
			}
		}
	}
	return false
}

// poolShape is the size one job asked the pool raised for it to be.
type poolShape struct{ Ports, Threads int }

// raisedPools stands where the pools go.
//
// It records the size every job asked for, hands each job an engine of its own,
// and counts how many are open at the same time — which is the only way to tell
// a pool per job from one pool handed to all of them while the jobs are still
// running: once they have all ended, the two look alike.
type raisedPools struct {
	// hold and linger are given to every engine it raises, so a test can drive a
	// job query by query and hold it inside the pool that was raised for it.
	hold   chan struct{}
	linger chan struct{}

	mu    sync.Mutex
	asked []poolShape
	open  int
	most  int
	shut  int
	// refuse is what raising comes back with while it is set: a machine where
	// nothing will answer.
	refuse error
}

// raise is the Dial a supervisor is built on.
func (r *raisedPools) raise(_ context.Context, ports, threads int, _ string,
	_ time.Duration) (engine, error) {
	r.mu.Lock()
	r.asked = append(r.asked, poolShape{Ports: ports, Threads: threads})
	if r.refuse != nil {
		err := r.refuse
		r.mu.Unlock()
		return nil, err
	}
	r.open++
	if r.open > r.most {
		r.most = r.open
	}
	r.mu.Unlock()
	return &heldEngine{hold: r.hold, linger: r.linger, gone: r.gaveUp}, nil
}

func (r *raisedPools) gaveUp() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.open--
	r.shut++
}

// tried is how many times a pool was asked for, whether or not one went up.
func (r *raisedPools) tried() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.asked)
}

// up is how many pools have been raised, and down how many have been given up.
func (r *raisedPools) up() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shut + r.open
}

func (r *raisedPools) down() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shut
}

// mostAtOnce is the largest number of pools that were ever open together.
func (r *raisedPools) mostAtOnce() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.most
}

// shapes are the sizes asked for, in the order they were asked for.
func (r *raisedPools) shapes() []poolShape {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.asked)
}

// answering stops the refusing, so a job that could not be run can be carried on.
func (r *raisedPools) answering() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refuse = nil
}

// let releases n queries of whichever job is inside a pool right now.
func (r *raisedPools) let(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case r.hold <- struct{}{}:
		case <-time.After(patience):
			t.Fatalf("the supervisor did not reach query %d", i+1)
		}
	}
}

// recorded is what the supervisor said, readable while it is still saying things.
type recorded struct {
	mu   sync.Mutex
	said strings.Builder
}

func (s *recorded) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.said.Write(p)
}

func (s *recorded) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.said.String()
}

// raisingSupervisor is a supervisor that raises a pool for each job, at the
// sizes that job named and at the two handed in where the job named none.
func raisingSupervisor(t *testing.T, pools *raisedPools, ports, threads int) (*Supervisor, *store.Store) {
	t.Helper()
	st := testStore(t)
	v := start(st, source{raise: pools.raise}, ports, threads)
	t.Cleanup(func() { _ = v.Close() })
	return v, st
}

// enqueueSized starts a job that names the pool it wants. Nought in either is a
// job that named none.
func enqueueSized(t *testing.T, v *Supervisor, name string, ports, threads int, queries ...string) int64 {
	t.Helper()
	id, err := v.Enqueue(store.JobSpec{
		Name: name, Pages: 1, Country: "us", Language: "en", Ports: ports, Threads: threads,
	}, queries)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return id
}

// supervisorOn is a supervisor built on the engine handed in, shut down when the
// test ends.
func supervisorOn(t *testing.T, eng engine) (*Supervisor, *store.Store) {
	t.Helper()
	st := testStore(t)
	v := newSupervisor(st, eng)
	t.Cleanup(func() { _ = v.Close() })
	return v, st
}

// heldSupervisor is a supervisor whose engine a test drives query by query.
func heldSupervisor(t *testing.T) (*Supervisor, *store.Store, *heldEngine) {
	t.Helper()
	eng := &heldEngine{hold: make(chan struct{})}
	v, st := supervisorOn(t, eng)
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

// waitUntilRunning waits for a job to be the one in flight.
func waitUntilRunning(t *testing.T, v *Supervisor, id int64) {
	t.Helper()
	waitUntil(t, "the job is the one running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})
}

// waitUntilIdle waits for the supervisor to have no job in flight.
func waitUntilIdle(t *testing.T, v *Supervisor) {
	t.Helper()
	waitUntil(t, "no job is running", func() bool { _, ok := v.Running(); return !ok })
}

// waitUntilTook waits for a job to reach the engine it was meant to run on.
func waitUntilTook(t *testing.T, e *heldEngine, query string) {
	t.Helper()
	waitUntil(t, "the engine was handed the query "+query, func() bool { return e.took(query) })
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

// standInPools opens real pools against a stand-in for the service behind them.
// What they hand out reaches nothing on this machine, so every query fails and
// no request leaves — which is beside the point: what is asked is whether the
// ports a job's pool was opened with are still listed by the service after that
// job has ended, and only a real pool can answer that.
func standInPools(t *testing.T) (OpenPool, *fakebt.Server) {
	t.Helper()
	fake := fakebt.New(t)
	cl, err := blanktrail.NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return func(ctx context.Context, ports, threads int, device string,
		_ time.Duration) (*blanktrail.Pool, error) {
		return blanktrail.NewPool(ctx, blanktrail.PoolConfig{
			Specs:            blanktrail.SpecsFor(device),
			Client:           cl,
			Threads:          threads,
			PortsPerThread:   ports,
			Spec:             blanktrail.DefaultPortSpec(),
			Channels:         []blanktrail.Channel{blanktrail.NewDirectChannel("direct")},
			Insecure:         true,
			Cooldown:         time.Nanosecond,
			MaxRetriesPerReq: 1,
			Sleep:            func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		})
	}, fake
}

func TestSupervisor_HandsTheIdentitiesBackToTheServiceWhenTheJobThatAskedForThemEnds(t *testing.T) {
	// Everything else in this file counts pools against a stand-in, and a
	// stand-in cannot say whether closing a real pool actually gives the ports
	// back. This is the one place a real pool goes up.
	//
	// The first half is what gives the second half teeth: opening one here and
	// closing it shows that what the service lists does move, so nought listed
	// after a job is a pool that was given up rather than one that was never
	// opened.
	open, fake := standInPools(t)
	ctx := t.Context()

	proof, err := open(ctx, 2, 1, blanktrail.DeviceDesktop, 0)
	if err != nil {
		t.Fatalf("opening a pool: %v", err)
	}
	if got := len(fake.OpenPorts()); got != 2 {
		t.Fatalf("the service lists %d ports for a pool of 2, so it cannot say what a job holds", got)
	}
	if err := proof.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(fake.OpenPorts()); got != 0 {
		t.Fatalf("the service still lists %d ports for a pool that was closed", got)
	}

	// Counted as well as listed, because nought ports listed is also what a
	// supervisor that never opened a pool at all would leave behind.
	var counting sync.Mutex
	opened := 0
	count := func(ctx context.Context, ports, threads int, device string,
		_ time.Duration) (*blanktrail.Pool, error) {
		counting.Lock()
		opened++
		counting.Unlock()
		return open(ctx, ports, threads, device, 0)
	}
	raised := func() int {
		counting.Lock()
		defer counting.Unlock()
		return opened
	}

	st := testStore(t)
	v := NewSupervisor(st, count, 2, 1)
	t.Cleanup(func() { _ = v.Close() })

	first := enqueue(t, v, "first", "a")
	waitUntil(t, "the first job is done", func() bool { return progress(t, st, first).Finished })
	waitUntil(t, "the first job's identities are handed back",
		func() bool { return len(fake.OpenPorts()) == 0 })

	second := enqueue(t, v, "second", "b")
	waitUntil(t, "the second job is done", func() bool { return progress(t, st, second).Finished })
	waitUntil(t, "the second job's identities are handed back",
		func() bool { return len(fake.OpenPorts()) == 0 })

	if got := raised(); got != 2 {
		t.Errorf("%d pools were opened for two jobs, want one each", got)
	}
}

func TestSupervisor_RaisesAPoolOfItsOwnForEveryJobAndGivesItBackAsThatJobEnds(t *testing.T) {
	// Three jobs, because a queue of one cannot tell a pool per job from one pool
	// for all of them: with a single job the two raise once, give up once and
	// leave the same history behind. Each names a different size, and none names
	// the size this server was started with, so a supervisor reusing one pool and
	// a supervisor raising them all from the default both fail here.
	//
	// The counts are read while the jobs are running, one job at a time. Read at
	// the end they would say only that three pools went up and three came down,
	// which is also what three pools opened at once and closed together says.
	pools := &raisedPools{hold: make(chan struct{})}
	v, st := raisingSupervisor(t, pools, 6, 5)

	ids := []int64{
		enqueueSized(t, v, "first", 3, 2, "a"),
		enqueueSized(t, v, "second", 9, 4, "b"),
		enqueueSized(t, v, "third", 11, 7, "c"),
	}
	for n, id := range ids {
		waitUntilRunning(t, v, id)
		waitUntil(t, "the job in flight has a pool", func() bool { return pools.up() == n+1 })
		if got := pools.down(); got != n {
			t.Fatalf("%d pools had been given up while job %d was still running, want %d", got, n+1, n)
		}
		pools.let(t, 1)
	}
	waitUntil(t, "every job is done", func() bool {
		for _, id := range ids {
			if !progress(t, st, id).Finished {
				return false
			}
		}
		return true
	})
	waitUntil(t, "the last job's pool has been given up", func() bool { return pools.down() == 3 })

	if got := pools.up(); got != 3 {
		t.Errorf("%d pools were raised for three jobs, want one each", got)
	}
	if got := pools.mostAtOnce(); got != 1 {
		t.Errorf("%d pools were open at once, want one at a time", got)
	}
	want := []poolShape{{Ports: 3, Threads: 2}, {Ports: 9, Threads: 4}, {Ports: 11, Threads: 7}}
	if got := pools.shapes(); !slices.Equal(got, want) {
		t.Errorf("the pools were raised at %v, want the sizes the three jobs named: %v", got, want)
	}
}

func TestSupervisor_GivesUpAJobsPoolOnlyOnceThatJobHasLetGoOfIt(t *testing.T) {
	// A job does not let go of its pool at the instant its last query lands, and
	// a pool given up in between has its ports taken back while the job is still
	// sending through them. The job is held inside the pool here so that the two
	// moments are far apart and the count below is read between them; a test that
	// let the job leave straight away would be reading whichever of the two
	// goroutines got there first.
	pools := &raisedPools{hold: make(chan struct{}), linger: make(chan struct{})}
	v, st := raisingSupervisor(t, pools, 6, 5)
	id := enqueueSized(t, v, "nightly", 3, 2, "a", "b")

	waitUntilRunning(t, v, id)
	pools.let(t, 2)
	waitUntil(t, "both queries are recorded", func() bool { return progress(t, st, id).Done == 2 })
	if got := pools.down(); got != 0 {
		t.Errorf("the pool was given up %d times while the job was still inside it", got)
	}

	close(pools.linger)
	waitUntilIdle(t, v)
	waitUntil(t, "the pool has been given up", func() bool { return pools.down() == 1 })
	if got := pools.up(); got != 1 {
		t.Errorf("%d pools were raised for one job", got)
	}
}

func TestSupervisor_RunsAJobThatNamedNoSizeOnWhatTheServerWasStartedWith(t *testing.T) {
	// Nought is a job that said nothing about its pool, and every job written down
	// before jobs carried sizes reads back that way. It has to run, and it runs at
	// the size this machine was started at.
	//
	// The second job names sizes of its own, and neither is the server's. Without
	// it a supervisor that ignored the job entirely and always used the default
	// would pass, and with only the second a supervisor that never used the
	// default would.
	pools := &raisedPools{hold: make(chan struct{})}
	v, st := raisingSupervisor(t, pools, 6, 5)
	silent := enqueueSized(t, v, "said nothing", 0, 0, "a")
	spoken := enqueueSized(t, v, "said so", 3, 2, "b")

	waitUntilRunning(t, v, silent)
	pools.let(t, 1)
	waitUntilRunning(t, v, spoken)
	pools.let(t, 1)
	waitUntil(t, "both jobs are done", func() bool {
		return progress(t, st, silent).Finished && progress(t, st, spoken).Finished
	})

	want := []poolShape{{Ports: 6, Threads: 5}, {Ports: 3, Threads: 2}}
	if got := pools.shapes(); !slices.Equal(got, want) {
		t.Errorf("the pools were raised at %v, want %v — this server's own sizes, then the job's", got, want)
	}
}

func TestSupervisor_LeavesAJobWhosePoolWouldNotGoUpToBeCarriedOn(t *testing.T) {
	// Reaching identities that answer is the part of this that fails, and it fails
	// on machines nobody can inspect afterwards. A job lost to it — stamped done
	// with nothing recorded, or dropped out of the queue with no trace — is a
	// list somebody typed in and will never get back.
	pools := &raisedPools{hold: make(chan struct{}), refuse: errors.New("no port answered")}
	v, st := raisingSupervisor(t, pools, 6, 5)
	// Written before anything is queued, and read after: the worker takes the
	// queue under the lock the enqueue below releases, so it sees this rather than
	// racing with it.
	said := &recorded{}
	v.log = slog.New(slog.NewTextHandler(said, nil))

	id := enqueueSized(t, v, "nightly", 3, 2, "a", "b")
	waitUntil(t, "the pool was asked for", func() bool { return pools.tried() == 1 })
	waitUntilIdle(t, v)

	sum := progress(t, st, id)
	if sum.Finished {
		t.Error("the job was stamped done, and nothing is left to carry it on")
	}
	if sum.Done != 0 || sum.Failed != 0 || sum.Pending != 2 {
		t.Errorf("done=%d failed=%d pending=%d, want 0/0/2 — the job kept everything it had",
			sum.Done, sum.Failed, sum.Pending)
	}
	if q := v.Queued(); len(q) != 0 {
		t.Errorf("the job is still queued as %v, so the queue is stuck on it", q)
	}
	if !strings.Contains(said.String(), "no port answered") {
		t.Errorf("nothing says why the job did not run:\n%s", said.String())
	}

	// And it is a job, not a wreck: with something answering it carries on from
	// where it never started.
	pools.answering()
	if err := v.Resume(id); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitUntilRunning(t, v, id)
	pools.let(t, 2)
	waitUntil(t, "the job is done", func() bool { return progress(t, st, id).Finished })
	if got := progress(t, st, id).Done; got != 2 {
		t.Errorf("the carried-on job recorded %d queries, want the 2 it had waiting", got)
	}
}

func TestSupervisor_GivesUpThePoolOfAJobThatWasStopped(t *testing.T) {
	// A job that was stopped has ended, and its pool holds ports for nothing. It
	// is the ending nobody arranges for, so it is the one where a pool is left
	// open until the process ends.
	pools := &raisedPools{hold: make(chan struct{})}
	v, st := raisingSupervisor(t, pools, 6, 5)
	id := enqueueSized(t, v, "nightly", 3, 2, "a", "b", "c")

	waitUntilRunning(t, v, id)
	pools.let(t, 1)
	waitUntil(t, "one query is recorded", func() bool { return progress(t, st, id).Done == 1 })
	if err := v.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitUntilIdle(t, v)

	waitUntil(t, "the stopped job's pool has been given up", func() bool { return pools.down() == 1 })
	if got := pools.up(); got != 1 {
		t.Errorf("%d pools were raised, want the one the stopped job was taken through", got)
	}
	if sum := progress(t, st, id); sum.Finished || sum.Pending != 2 {
		t.Errorf("the stopped job: finished=%v pending=%d, want false/2", sum.Finished, sum.Pending)
	}
}

func TestSupervisor_GivesUpThePoolOfTheJobInFlightWhenItShutsDown(t *testing.T) {
	// A shutdown that left the pool of the job it ended open would hold those
	// ports until the process died, and the process is what is dying.
	pools := &raisedPools{hold: make(chan struct{})}
	st := testStore(t)
	v := start(st, source{raise: pools.raise}, 6, 5)

	id := enqueueSized(t, v, "nightly", 3, 2, "a", "b")
	waitUntilRunning(t, v, id)
	waitUntil(t, "the job has a pool", func() bool { return pools.up() == 1 })

	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := pools.down(); got != 1 {
		t.Errorf("the pool of the job in flight was given up %d times, want 1", got)
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

func TestReconnect_ChangesWhereTheNextJobsPoolComesFrom(t *testing.T) {
	// A connection saved in the browser has to reach the jobs that follow, and
	// the only way it can is by changing what raises their pools.
	v, _, _ := heldSupervisor(t)
	next := &heldEngine{}
	if err := v.Reconnect(func(context.Context, int, int, string, time.Duration) (*blanktrail.Pool, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	_ = next
	if err := v.Reconnect(nil); err != nil {
		t.Fatalf("Reconnect(nil): %v", err)
	}
}

func TestReconnect_LeavesTheJobInFlightOnThePoolItRaised(t *testing.T) {
	// This is the whole of what a pool per job bought: there is no question to
	// ask and no price to name, because the running job is inside identities
	// nobody else can reach.
	v, _, first := heldSupervisor(t)
	id := enqueue(t, v, "one", "a")
	waitUntilRunning(t, v, id)

	if err := v.Reconnect(func(context.Context, int, int, string, time.Duration) (*blanktrail.Pool, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if got, ok := v.Running(); !ok || got != id {
		t.Errorf("running=%d,%v — reconnecting took the job down", got, ok)
	}
	if n := first.closes; n != 0 {
		t.Errorf("the pool the running job is inside was closed %d times", n)
	}
	close(first.hold)
	waitUntilIdle(t, v)
}

func TestReconnect_GivesUpAStandingSetOfIdentitiesNobodyIsInside(t *testing.T) {
	// A supervisor built around one standing set holds ports. Once the jobs that
	// follow raise their own, those ports are nobody's, and left open they are
	// held for as long as the process runs.
	v, _, standing := heldSupervisor(t)
	if err := v.Reconnect(func(context.Context, int, int, string, time.Duration) (*blanktrail.Pool, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if n := standing.closes; n != 1 {
		t.Errorf("the standing identities were given up %d times, want once", n)
	}
}

func TestReconnect_RefusesAfterTheSupervisorIsClosed(t *testing.T) {
	v, _, _ := heldSupervisor(t)
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := v.Reconnect(nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Reconnect after Close returned %v, want ErrClosed", err)
	}
}

func TestSupervisor_RefusesARaiseThatCameBackWithNeitherAPoolNorAReason(t *testing.T) {
	// Taken as it comes, the job runs on nothing and the first screen that asks
	// the pool how it is doing falls over — a crash a long way from the mistake
	// that caused it. It is refused where it happens instead, and the job stays
	// there to be carried on.
	st := testStore(t)
	v := start(st, dialing(func(context.Context, int, int, string, time.Duration) (*blanktrail.Pool, error) {
		return nil, nil
	}, nil), 1, 1)
	t.Cleanup(func() { _ = v.Close() })

	id := enqueue(t, v, "on nothing", "a")
	waitUntil(t, "the job to have been let go of", func() bool {
		_, running := v.Running()
		return !running && len(v.Queued()) == 0
	})
	sum := progress(t, st, id)
	if sum.Finished {
		t.Error("a job that never ran is stamped finished, so nothing can carry it on")
	}
	if sum.Done != 0 || sum.Pending == 0 {
		t.Errorf("done=%d pending=%d, want the work still there to be taken up", sum.Done, sum.Pending)
	}
}

func TestSupervisor_HoldsAJobUntilThereIsSomethingToRunItOn(t *testing.T) {
	// A machine whose connection has not been set up yet has a supervisor and no
	// engine. A job asked for there is written down and waits: throwing it away
	// would lose a list somebody has just typed in, and running it is impossible.
	v, st := supervisorOn(t, nil)
	if v.canRun() {
		t.Error("a supervisor with no engine says it can run a job")
	}
	id := enqueue(t, v, "before", "a")
	for i := 0; i < 40; i++ {
		if _, ok := v.Running(); ok {
			t.Fatal("a job started on a supervisor that has nothing to run it on")
		}
		time.Sleep(pollGap)
	}

	// What a connection being set up does, and nothing besides: from this moment
	// there is something to raise a pool from. It is put in place in one step
	// rather than by a raise that answers with nothing and a source swapped after
	// it — the worker wakes on the first of those two and would take the job
	// through the one this test is not about.
	eng := &heldEngine{}
	v.mu.Lock()
	v.src = standing(eng)
	v.wakeUp()
	v.mu.Unlock()
	if !v.canRun() {
		t.Error("a supervisor holding an engine says it cannot run a job")
	}
	waitUntilTook(t, eng, "a")
	waitUntil(t, "the waiting job is done", func() bool { return progress(t, st, id).Finished })
}

func TestSupervisor_TakesAJobThroughTheEngineAsTheKindItWasFiledUnder(t *testing.T) {
	// The joint between the two halves of the kind. A job filed as an index
	// check, drawn on its own page as an index check, and then handed to the
	// engine as an ordinary search would search every address as a phrase — and
	// the history, the page and the estimate would all go on saying the right
	// thing about a run that asked the wrong question.
	//
	// Every kind is asked for, because a version that always says index is wrong
	// in the other direction and looks identical from the index side.
	//
	// The site a position check is about travels with it. Handed on without one,
	// the check has nothing to recognise and reports every phrase in the list as
	// one the site does not rank for — and the page, the history and the estimate
	// would all go on saying the right thing about it.
	cases := []struct {
		kind, target string
		want         run.Kind
	}{
		{kind: store.KindIndex, want: run.Index},
		{kind: store.KindParse, want: run.Parse},
		{kind: store.KindPosition, target: "example.com", want: run.Position},
		{kind: "", want: run.Parse},
	}
	for _, tc := range cases {
		t.Run("filed as "+tc.kind, func(t *testing.T) {
			v, _, eng := heldSupervisor(t)
			if _, err := v.Enqueue(
				store.JobSpec{Name: "j", Kind: tc.kind, Target: tc.target, Pages: 1},
				[]string{"example.com/a"}); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			waitUntil(t, "a job has reached the engine", func() bool {
				_, taken := eng.ran(0)
				return taken
			})
			got, _ := eng.ran(0)
			if got.Kind != tc.want {
				t.Errorf("the engine was handed kind %v, want %v", got.Kind, tc.want)
			}
			if got.Target != tc.target {
				t.Errorf("the engine was handed %q to look for, want %q", got.Target, tc.target)
			}
		})
	}
}

func TestSupervisor_TakesAJobToAsManyIdentitiesAsTheJobAskedFor(t *testing.T) {
	// The machinery for this was written with the run layer and nothing ever set
	// it: every job ran on the built-in number, and no test noticed, because the
	// run layer's own tests set it directly. This is the seam between the two —
	// the store keeps it, the run layer honours it, and the supervisor is what
	// carries it across.
	v, st, eng := heldSupervisor(t)
	_ = st
	close(eng.hold)

	id, err := v.Enqueue(store.JobSpec{
		Name: "a tired list", Pages: 1, Country: "us", Language: "en", Tries: 17,
	}, []string{"a"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitUntil(t, "the job to have been taken", func() bool {
		_, seen := eng.ran(0)
		return seen
	})

	got, _ := eng.ran(0)
	if got.Tries != 17 {
		t.Errorf("the job ran at %d identities per query, want the 17 it asked for", got.Tries)
	}
	_ = id
}

func TestCreateJob_KeepsWhichKindOfResultPageWasAskedFor(t *testing.T) {
	// It cannot be worked out from the results afterwards and cannot be changed
	// part way through: Google simply answers a phone and a desktop with
	// different pages. A job that lost the answer would be a run nobody could
	// say what it measured.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{
		"name": {"phones"}, "queries": {"iphone 13"}, "pages": {"1"},
		"device": {blanktrail.DeviceMobile},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("starting a mobile job came back %d:\n%s", rec.Code, rec.Body.String())
	}

	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("%d jobs were written, want the one", len(jobs))
	}
	if jobs[0].Device != blanktrail.DeviceMobile {
		t.Errorf("the job was filed as asking for %q, want the phone", jobs[0].Device)
	}
	// And the job's own page says so, in words rather than in the word the
	// database files it under.
	body := get(t, s, jobPath(jobs[0].ID)).Body.String()
	if !strings.Contains(body, LangEN.T("form.device.mobile")) {
		t.Errorf("the job's page does not say which kind of page it asked for:\n%s", body)
	}
}

func TestSupervisor_RaisesThePoolAsTheKindOfPageTheJobAskedFor(t *testing.T) {
	// The choice reaches the one place that can act on it: the ports are opened
	// as phones or as desktops, and nothing afterwards can change what they are.
	st := testStore(t)
	var asked []string
	var mu sync.Mutex
	v := start(st, source{raise: func(_ context.Context, _, _ int, device string, _ time.Duration) (engine, error) {
		mu.Lock()
		asked = append(asked, device)
		mu.Unlock()
		return &heldEngine{}, nil
	}}, 1, 1)
	t.Cleanup(func() { _ = v.Close() })

	id, err := v.Enqueue(store.JobSpec{Name: "phones", Pages: 1,
		Device: blanktrail.DeviceMobile}, []string{"a"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitUntil(t, "the job to have been taken up", func() bool {
		sum, err := st.Progress(t.Context(), id)
		return err == nil && sum.Finished
	})

	mu.Lock()
	defer mu.Unlock()
	if len(asked) == 0 {
		t.Fatal("no pool was raised at all")
	}
	if asked[0] != blanktrail.DeviceMobile {
		t.Errorf("the pool was raised as %q, and the job asked for a phone", asked[0])
	}
}

func TestRunsOnPhones_IsWhatDecidesTheOneHeaderThatDiffers(t *testing.T) {
	// What a browser will accept differs between a phone and a desktop, and it is
	// the one header the proxy leaves alone for a Safari identity — measured
	// through a real port. Read wrongly here, a phone sends no Accept at all,
	// which is a request no browser has ever made.
	if !runsOnPhones(blanktrail.DeviceMobile) {
		t.Error("a job on phones is treated as a desktop")
	}
	for _, device := range []string{blanktrail.DeviceDesktop, "", "tractor"} {
		if runsOnPhones(device) {
			t.Errorf("a job asking for %q is treated as a phone", device)
		}
	}
}

func TestPoolEngine_ShrinksAPoolWithStandingIdentitiesAndClosesOneWithout(t *testing.T) {
	// The rule the whole arrangement rests on, in the one place a job's end
	// reaches it. Closed instead of shrunk, a machine that keeps identities warm
	// would lose them to the first job that ran; shrunk instead of closed, a job
	// that opened its own would hold them open for as long as the program runs.
	fake := fakebt.New(t)
	client, err := blanktrail.NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	open := func(ports int) *blanktrail.Pool {
		t.Helper()
		pool, err := blanktrail.NewPool(t.Context(), blanktrail.PoolConfig{
			Client: client, Threads: ports, PortsPerThread: 1,
			Spec: blanktrail.DefaultPortSpec(),
		})
		if err != nil {
			t.Fatalf("opening %d ports: %v", ports, err)
		}
		return pool
	}

	// A job's own pool: nothing standing, so its end closes the lot.
	own := open(2)
	if err := (&poolEngine{pool: own}).Close(); err != nil {
		t.Fatalf("closing a job's own identities: %v", err)
	}
	if got := len(fake.OpenPorts()); got != 0 {
		t.Errorf("%d ports survived a job that owned them", got)
	}

	// A standing pool a job grew: its end gives back the growth and no more.
	standing := open(3)
	t.Cleanup(func() { _ = standing.Close() })
	standing.KeepWarm()
	if err := standing.Grow(t.Context(), 5); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	if err := (&poolEngine{pool: standing}).Close(); err != nil {
		t.Fatalf("letting go of a grown standing pool: %v", err)
	}
	if got := len(fake.OpenPorts()); got != 3 {
		t.Errorf("%d ports are open after the job, want the three kept warm", got)
	}
}

func TestSupervisor_GoesOnReportingTheIdentitiesAfterTheJobHasLetGoOfThem(t *testing.T) {
	// A stop ends a job and not the pool. The counts on the identities are the
	// account of what the run met — how many requests failed, on what, how often
	// an address had to be changed — and that is exactly what somebody reads
	// once a run has stopped going well. Zeroing the screen at the moment the
	// job stops throws away the reading at the moment it is wanted.
	st := testStore(t)
	eng := &heldEngine{}
	v := start(st, dialing(nil, nil), 1, 1)
	t.Cleanup(func() { _ = v.Close() })

	// The engine a job ran on, given and then let go the way a stop lets go.
	v.mu.Lock()
	v.inUse, v.last = eng, eng
	v.mu.Unlock()
	if got := v.pool().Stats.EgressRotations; got != fakePool.Stats.EgressRotations {
		t.Fatalf("a running job reports %d address changes, want %d",
			got, fakePool.Stats.EgressRotations)
	}

	v.release()
	facts := v.pool()
	if facts.Stats.EgressRotations != fakePool.Stats.EgressRotations {
		t.Errorf("after the job let go the screen reports %d address changes, want the "+
			"%d the run actually made", facts.Stats.EgressRotations, fakePool.Stats.EgressRotations)
	}
	if facts.Stats.Ports != fakePool.Stats.Ports {
		t.Errorf("after the job let go the screen reports %d ports, want %d",
			facts.Stats.Ports, fakePool.Stats.Ports)
	}
}

func TestSupervisor_ReportsNothingBeforeAnythingHasRun(t *testing.T) {
	// Numbers about a pool that has never existed describe nothing, and a screen
	// inventing them is a screen nobody can act on.
	st := testStore(t)
	v := start(st, dialing(nil, nil), 1, 1)
	t.Cleanup(func() { _ = v.Close() })

	if got := v.pool(); got.Stats.Ports != 0 || got.Stats.EgressRotations != 0 {
		t.Errorf("a supervisor that has run nothing reports %+v", got.Stats)
	}
}
