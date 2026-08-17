// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
	"github.com/blanktrail/google-serp-parser/run"
	"github.com/blanktrail/google-serp-parser/store"
)

// ErrBusy is returned when a job is asked for again while it is already running
// or already waiting its turn.
var ErrBusy = errors.New("web: this job is already running or waiting")

// ErrNotRunning is returned when a job asked to stop is not the one running.
var ErrNotRunning = errors.New("web: this job is not the one running")

// ErrNothingLeft is returned when a job asked to carry on has no query left.
var ErrNothingLeft = errors.New("web: this job has nothing left to do")

// errNoPoolCameBack is what a raiser that answers with neither a pool nor a
// fault is turned into. It is not exported: nobody outside can be handed it,
// because nobody outside writes the raiser this happens in.
var errNoPoolCameBack = errors.New("web: opening a pool gave back nothing, and no reason either")

// ErrClosed is returned once the supervisor has been shut down. Work taken on
// after that would sit in a queue nobody is reading.
var ErrClosed = errors.New("web: the supervisor has been closed")

// jobSettleGrace bounds the writes around a job: creating one, reading back
// what it has left, stamping it done. All of it is a database on this machine,
// so the grace is generous for the work and short enough that a job somebody
// stopped stops.
const jobSettleGrace = 30 * time.Second

// engine takes one job's queries and says what came of them.
//
// It is an interface because every question worth asking a queue — whether the
// job behind the one in flight waited its turn, what a stop leaves behind, what
// a shutdown does to a job in the middle — has to be asked while a job is half
// done, and the only way to hold a job half done is to stand something
// controllable where the identities go.
type engine interface {
	Run(ctx context.Context, j run.Job, sink run.Sink) run.Report
	Close() error
	// Pool is what the identities behind this engine report about themselves.
	Pool() poolFacts
}

// poolFacts is what a screen can say about the identities every job runs on:
// the counts the pool keeps of itself, and the two settings an estimate for a
// job on it turns on.
//
// It is one value rather than three accessors because all of it is read at the
// same instant for the same screen, and three reads of a pool that is being used
// while they happen would put three moments beside each other and call them one.
type poolFacts struct {
	Stats    blanktrail.Stats
	Threads  int
	Cooldown time.Duration
}

// poolEngine takes one job through one pool of identities.
//
// The pool is raised for that job out of the sizes that job named, and given up
// when the job lets go of it. The price is known and is paid on purpose:
// reaching a port that answers was measured at between 27 seconds and 9 minutes
// 10, and a pool per job pays that again every time instead of once. What it
// buys is that no two jobs ever share identities — one job's list can no longer
// spoil the next one's — and that the size a job runs at is changed by editing
// that job rather than by starting the server again.
type poolEngine struct {
	pool    *blanktrail.Pool
	threads int
}

// Run builds a runner around the pool this job was raised. A runner is a few
// fields, and the sink is the one part of it that belongs to a single job.
func (e *poolEngine) Run(ctx context.Context, j run.Job, sink run.Sink) run.Report {
	return (&run.Runner{Pool: e.pool, Threads: e.threads, Sink: sink}).Run(ctx, j)
}

// Close gives up the identities. It runs when the job this pool was raised for
// has let go of it.
func (e *poolEngine) Close() error { return e.pool.Close() }

// Pool is this job's pool, as it stands right now.
func (e *poolEngine) Pool() poolFacts {
	return poolFacts{Stats: e.pool.Stats(), Threads: e.threads, Cooldown: e.pool.Cooldown()}
}

// Dial raises the identities one job asked to run on.
//
// The two numbers are that job's own, already stood in for where the job named
// nothing, so whatever is behind this is told a size it can act on rather than a
// zero it has to interpret a second time.
//
// The context is the job's: a job stopped while its pool is still going up stops
// there, rather than after ports it will never use have been opened.
type Dial func(ctx context.Context, ports, threads int, device string) (engine, error)

// OpenPool opens the identities one job asked to run on. It is Dial as a caller
// outside this package can write it: what a pool is opened as, how long its
// ports rest and what is checked before they are opened is decided by the
// command that starts this server, and an interface with a second opinion about
// that would give a job set up here a different cost from the same job set up
// there.
type OpenPool func(ctx context.Context, ports, threads int, device string) (*blanktrail.Pool, error)

// source is where the pool for the next job comes from.
//
// raise is the whole of it for a job that has its own pool put up. held is the
// bridge that is left: the settings page opens a pool itself and hands the
// finished thing over, so a source made that way has one already-open set of
// identities that every job it starts is taken through, and that it has to give
// back when it stops being what jobs are started from. It goes when the settings
// page hands over a connection instead of a pool.
type source struct {
	raise Dial
	held  engine
}

// standing makes a source out of a set of identities somebody else opened.
//
// Every job it starts is handed that same one, which is exactly what a pool per
// job is not; it is here because the page that opens pools has not been changed
// over yet, and it is the one path in this file where two jobs share identities.
func standing(eng engine) source {
	if eng == nil {
		return source{}
	}
	return source{
		raise: func(context.Context, int, int, string) (engine, error) { return eng, nil },
		held:  eng,
	}
}

// dialing makes a source that puts up a pool for each job.
func dialing(open OpenPool) source {
	if open == nil {
		return source{}
	}
	return source{raise: func(ctx context.Context, ports, threads int, device string) (engine, error) {
		pool, err := open(ctx, ports, threads, device)
		if err != nil {
			return nil, err
		}
		if pool == nil {
			// Neither identities nor a reason. Taken as it comes, the job would run
			// on nothing and fall over at the first screen that asks the pool how it
			// is doing — which is a crash a long way from the mistake that caused it.
			return nil, errNoPoolCameBack
		}
		// The engine is told the same number of threads the pool was opened for,
		// because that number paces the job and is what the screen puts into its
		// estimate: two answers to how wide this job runs would put a figure on the
		// screen that no run ever matched.
		return &poolEngine{pool: pool, threads: threads}, nil
	}}
}

// jobSink files a finished query in the history under the job it belongs to.
//
// It is the same bridge cmd/gserp keeps, for the same reason: it is the only
// thing that has to know both the shape a run produces and the shape the
// history stores, and a Record method on either package would make that package
// depend on the other.
//
// It holds nothing that changes and the history behind it takes one writer at a
// time, so the several threads of a job may call it at once, which the runner
// requires.
type jobSink struct {
	st    *store.Store
	jobID int64
}

func (s jobSink) Record(ctx context.Context, res run.QueryResult) error {
	return s.st.Record(ctx, s.jobID, store.QueryOutcome{
		Ordinal: res.Ordinal,
		Pages:   res.Pages,
		Err:     res.Err,
	})
}

// Supervisor runs jobs one at a time, in the order they were asked for, each on
// a pool of identities raised for it and given up when it lets go.
//
// One goroutine runs the jobs and nothing else does. It owns everything to do
// with the job in flight: the plan read back from the history, the pool that
// plan is taken through, the results going into the history, and the stamp at
// the end. No caller reaches any of that.
//
// What the callers and the worker share is the queue, the id of the job
// running, the function that ends it, whether the supervisor has been shut
// down, and the three fields below that say where pools come from — and every
// one of them is read and written under mu and nowhere else. The id and the
// cancel function are always written together in one critical section, so a
// reader sees either no job at all or a job together with the means of stopping
// exactly that job; there is no ordering in which a stop can reach the job that
// came after the one it was aimed at. The source to come, the pool in use and
// the source waiting move together for the same reason, so no reader can find
// two of them disagreeing.
//
// Nothing that leaves this type points into any of it. The queue is copied out,
// and the running job is a number.
type Supervisor struct {
	// Set once, before the worker starts, and never written again.
	st  *store.Store
	log *slog.Logger
	// ports and threads are what a job that named no size runs at. They are the
	// sizes this server was started with, handed in by whoever started it,
	// because a job that named nothing has to run at some size and this package
	// has no honest number of its own to put there.
	ports   int
	threads int

	// wake carries one signal, which is all the worker needs: it empties the
	// queue before it waits again, so a signal it missed is one it had already
	// acted on.
	wake chan struct{}
	// stopping is closed by Close, and done by the worker as it returns. Between
	// them they are how Close knows no job is still being written down.
	stopping chan struct{}
	done     chan struct{}

	mu      sync.Mutex
	queue   []int64
	running int64
	cancel  context.CancelFunc
	closed  bool
	// src is where the pool for the next job comes from. It is empty on a machine
	// whose connection has not been set up yet, and it is replaced while the
	// server runs, which is why it is here rather than among the fields set once.
	src source
	// inUse is the pool the job in flight is being taken through. It is what the
	// screens read the identities off, and what release gives back.
	inUse engine
	// pending is a source waiting for the job in flight to end.
	pending source
}

// NewSupervisor starts the worker and hands back the queue in front of it.
//
// Every job it takes has a pool opened for it by open and given up when that job
// lets go. The two numbers are what a job that named no size runs at: they are
// the sizes this server was started with, which on the command that starts it
// are the -threads and -ports flags, so a job that named nothing costs what the
// same job costs from the command line.
func NewSupervisor(st *store.Store, open OpenPool, ports, threads int) *Supervisor {
	return start(st, dialing(open), ports, threads)
}

// newSupervisor starts a worker that takes every job through the one set of
// identities handed in, and nil for a machine that has none.
//
// It is the settings page's way in and the last of the shape this file had
// before a pool belonged to a job. It goes when that page hands over a
// connection rather than a finished pool.
func newSupervisor(st *store.Store, eng engine) *Supervisor {
	return start(st, standing(eng), 0, 0)
}

func start(st *store.Store, src source, ports, threads int) *Supervisor {
	v := &Supervisor{
		st:      st,
		src:     src,
		ports:   ports,
		threads: threads,
		// The process logger, which stamps its lines and can be pointed
		// elsewhere. A worker whose only account of itself is unstamped text is
		// a worker nobody can operate.
		log:      slog.Default(),
		wake:     make(chan struct{}, 1),
		stopping: make(chan struct{}),
		done:     make(chan struct{}),
	}
	go v.work()
	return v
}

// Enqueue writes a job down and puts it at the back of the queue.
//
// The whole plan is written before anything runs, which is what makes a job
// that was interrupted resumable rather than lost.
//
// The write is on a context of its own rather than on a caller's request: the
// browser that asked for the job has no further part in it, and a job that
// vanished because a reader closed a tab would be one nobody could account for.
//
// The id comes back even when the queueing is refused, because by then the job
// is in the history and its number is how anyone finds it again.
func (v *Supervisor) Enqueue(spec store.JobSpec, queries []string) (int64, error) {
	if v.isClosed() {
		return 0, ErrClosed
	}
	ctx, cancel := context.WithTimeout(context.Background(), jobSettleGrace)
	defer cancel()
	id, err := v.st.CreateJob(ctx, spec, queries)
	if err != nil {
		return 0, err
	}
	return id, v.queueUp(id)
}

// Start puts a job that has already been written down at the back of the queue.
//
// It is the half of Enqueue that does not write the job. A list too large to
// hold is streamed into the history by whoever read the file, so by the time it
// reaches here the job is there with its whole plan and the mark that says so,
// and there is nothing left to write.
func (v *Supervisor) Start(jobID int64) error {
	if v.isClosed() {
		return ErrClosed
	}
	return v.queueUp(jobID)
}

// Resume puts a job that was left part way back in the queue.
//
// What it will run is not decided here. The queries a job has left are read
// when its turn comes, so a job that was stopped and resumed twice does not
// carry a plan drawn up before the last of its results landed.
//
// A job whose list never finished arriving is refused. Its queries look exactly
// like work waiting to be done, and they are a fraction of a list nobody knows
// the length of: running them would end with the job stamped complete on a
// plan that was never whole.
func (v *Supervisor) Resume(jobID int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), jobSettleGrace)
	defer cancel()
	sum, err := v.st.Progress(ctx, jobID)
	if err != nil {
		return err
	}
	if !sum.PlanReady {
		return fmt.Errorf("%w: %d", store.ErrPlanUnfinished, jobID)
	}
	if sum.Pending == 0 {
		return fmt.Errorf("%w: %d", ErrNothingLeft, jobID)
	}
	return v.queueUp(jobID)
}

// Stop ends the job that is running and leaves the queue exactly as it was.
//
// It ends one job, not the run. The queries nobody reached stay unfinished and
// the job is not stamped done, which is the whole of what a resume takes up,
// and it is the difference between enough of this for now and throw it away.
// The jobs waiting their turn were not part of what was stopped and keep it.
//
// It returns as soon as the job has been told. What the job was holding is
// written down as it lets go of it, and Running says when it has.
func (v *Supervisor) Stop(jobID int64) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.running != jobID || v.cancel == nil {
		return fmt.Errorf("%w: %d", ErrNotRunning, jobID)
	}
	v.cancel()
	return nil
}

// Reconnect changes where the pools for the jobs after this one come from.
//
// There is no question to ask and no price to name, and that is the whole of
// what a pool per job bought here. A job holds the pool it raised for itself
// until it ends, so a connection saved while one is running cannot disturb it,
// and the next job raises through what was just saved. The old swap had to ask
// "now, or after this job?" because one set of identities was shared by
// everything; nothing is shared any more, so nobody has to choose.
//
// A source that was holding a standing set of identities gives them back, as
// long as no job is inside them: they hold ports nobody will ask for again. The
// job in flight keeps what it is inside either way.
func (v *Supervisor) Reconnect(open OpenPool) error {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return ErrClosed
	}
	var spent []engine
	if v.src.held != nil && v.src.held != v.inUse {
		spent = append(spent, v.src.held)
	}
	v.src = dialing(open)
	// A job written down while there was nothing to run it on has something to
	// run on now.
	v.wakeUp()
	v.mu.Unlock()
	v.giveUp(spent)
	return nil
}

// CanRun reports whether there is anything for a job to be taken through.
//
// It is exported because the programmable interface has to ask it too: a job
// set up by a program on a machine whose connection has not been set up would
// otherwise be taken and never started, while the same job set up in the
// browser is refused with a sentence saying what to do about it. Two ways in
// that answer differently about what one machine can do is the defect, not the
// duplication.
func (v *Supervisor) CanRun() bool { return v.canRun() }

// canRun says whether there is anything for a job to be taken through.
//
// A supervisor with nowhere to raise a pool from is what a machine whose
// connection has not been set up yet has. It writes down what it is given and
// holds it, because a list somebody has just typed in is not something to throw
// away over a setting they are about to change.
func (v *Supervisor) canRun() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.src.raise != nil
}

// install takes a source into use and hands back what that leaves to be given
// up. It is called with mu held.
//
// The pool the job in flight is inside is never among them. Closing it under a
// running job would take away the ports that job is in the middle of using, so
// it is left to the worker to give up as it lets go.
func (v *Supervisor) install(next source) []engine {
	var spent []engine
	if v.src.held != nil && v.src.held != v.inUse {
		spent = append(spent, v.src.held)
	}
	v.src = next
	// The job in flight is stopped, not cancelled: what it recorded stays
	// recorded and what it did not reach stays waiting, which is the whole of
	// what a resume takes up.
	if v.cancel != nil {
		v.cancel()
	}
	// A job that was written down while there was nothing to run it on has
	// something to run on now.
	v.wakeUp()
	return spent
}

// giveUp closes engines nothing points at any more.
//
// It is called with the lock let go, because a pool takes a while to hand back
// what it holds and every reader of the queue would be waiting behind it. Each
// engine here was taken out of the fields under the lock before it got this
// far, so no two callers can arrive holding the same one.
func (v *Supervisor) giveUp(spent []engine) {
	for _, e := range spent {
		if err := e.Close(); err != nil {
			v.log.Error("an engine that was replaced could not be given up", "error", err)
		}
	}
}

// Running is the job in flight and whether there is one.
func (v *Supervisor) Running() (int64, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.running, v.running != 0
}

// pool is what the identities the job in flight is running on report.
//
// The pool of the job in flight is what a screen is asking about, so it is
// preferred to anything else; a source that stands on one already-open set of
// identities answers between jobs as well, because those identities are there
// between jobs and a screen saying nothing about them would be describing a
// server that had given them up.
//
// The engine is read under the lock because the worker replaces it as jobs come
// and go, and asked under none, because what it answers with is the pool's own
// snapshot taken under the pool's own lock and holding two locks to get one
// number invites waiting on them in two orders.
func (v *Supervisor) pool() poolFacts {
	v.mu.Lock()
	eng := v.inUse
	if eng == nil {
		eng = v.src.held
	}
	v.mu.Unlock()
	if eng == nil {
		// No job is running, and nothing is held between jobs. The numbers of a
		// pool that has been given up would describe something that is gone.
		return poolFacts{}
	}
	return eng.Pool()
}

// Queued is the jobs waiting their turn, in the order they will be taken.
//
// It is a copy. The queue is what the worker takes its next job from, and a
// caller handed the list itself would be reading something rewritten under it
// and could decide by writing to it what runs next.
func (v *Supervisor) Queued() []int64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.queue)
}

// Close ends the job in flight, stops taking further ones and gives up the
// identities.
//
// It waits for the worker, so a caller that has returned from Close is one
// behind which nothing is still writing to the history. The job that was
// running is left as a stop leaves it: what was reached is recorded and the
// rest is there to be taken up.
func (v *Supervisor) Close() error {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		<-v.done
		return nil
	}
	v.closed = true
	if v.cancel != nil {
		v.cancel()
	}
	// An engine waiting for the job in flight is waiting for something that will
	// not happen now, and its ports are held as surely as the ones in use.
	waiting := v.pending
	v.pending = source{}
	v.mu.Unlock()

	close(v.stopping)
	<-v.done

	// The worker has returned, so the pool of the job that was in flight has been
	// given back already and what is left is whatever a source holds open between
	// jobs — nothing at all for a source that raises a pool per job.
	v.mu.Lock()
	src := v.src
	v.src = source{}
	v.mu.Unlock()

	if waiting.held != nil {
		v.giveUp([]engine{waiting.held})
	}
	if src.held == nil {
		return nil
	}
	return src.held.Close()
}

// work is the one goroutine that runs jobs.
func (v *Supervisor) work() {
	defer close(v.done)
	for {
		id, ctx, src, ok := v.next()
		if !ok {
			select {
			case <-v.wake:
				continue
			case <-v.stopping:
				return
			}
		}
		v.runJob(ctx, src, id)
		// Before the job is settled, because a pool the job has left holds ports
		// for nothing and settling takes a write to a database.
		v.release()
		v.settle(id)
		// The job stops being the one running and the swap that was waiting for
		// it is taken, both here and both under one lock, so that a caller who
		// finds no job in flight finds the swap already made.
		v.finished()
	}
}

// next takes the job at the front of the queue and marks it as the one running.
//
// The context it returns is the one Stop ends. It is registered alongside the
// id in the same critical section, so a stop aimed at this job can never arrive
// early enough to find the id without it.
//
// The source goes with them. The worker takes it here and raises this job's
// pool from that one, so a source replaced part way through cannot leave the
// job running on one pool and reporting on another.
//
// A supervisor with nowhere to raise a pool from takes nothing. The job stays at
// the front of the queue and starts when there is something to take it through.
//
// The pool a source holds open between jobs is recorded as in use here, in the
// same critical section that takes the job, so that a source replaced while this
// job is being set up cannot have those identities closed out from under it. A
// source that raises a pool per job has nothing to record yet: what this job will
// run on does not exist until raise has put it up, and nothing else can reach it
// until raise has written it down.
func (v *Supervisor) next() (int64, context.Context, source, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || v.src.raise == nil || len(v.queue) == 0 {
		return 0, nil, source{}, false
	}
	id := v.queue[0]
	v.queue = v.queue[1:]
	ctx, cancel := context.WithCancel(context.Background())
	v.running, v.cancel = id, cancel
	v.inUse = v.src.held
	return id, ctx, v.src, true
}

// raise puts up the pool this job asked for and records it as the one in use.
//
// It runs after the job has been taken and with the lock let go, because raising
// a pool takes minutes: under the lock every screen reading the queue would wait
// out the whole warm-up, and before the job was taken it would be a pool raised
// for a job nobody had chosen yet.
//
// A source standing on identities somebody else opened answers here with those,
// having raised nothing. It is the one path where two jobs are handed the same
// pool, and keeping it inside the source rather than as a case here is what stops
// the rest of this file from having to know which kind of source it is holding.
func (v *Supervisor) raise(ctx context.Context, src source, sum store.JobSummary) (engine, error) {
	eng, err := src.raise(ctx, asked(sum.Ports, v.ports), asked(sum.Threads, v.threads), sum.Device)
	if err != nil {
		return nil, err
	}
	// Written down before the job goes into it, so that a stop or a shutdown
	// arriving now finds a pool to give back rather than one nothing points at.
	v.mu.Lock()
	v.inUse = eng
	v.mu.Unlock()
	return eng, nil
}

// asked is the size a job named, or the size this server was started with where
// the job named none.
//
// Nought is a job that said nothing about its pool — every job written down
// before jobs carried sizes reads back that way — and the history hands it on as
// the nought it is rather than filling it in, because filling it in there would
// make a job that named nothing impossible to tell from one that named exactly
// that number. This is where it is filled in, because this is where a pool goes
// up.
func asked(named, started int) int {
	if named < 1 {
		return started
	}
	return named
}

// release gives back the pool the job was taken through.
//
// A pool raised for this job is closed here, and this is the only place that
// closes it: the job was sending through its ports until the moment it let go,
// and until then there is nothing to give back. Identities a source holds open
// between jobs are left alone unless that source has since been replaced, in
// which case they are what the swap could not close at the time.
//
// What it does not do is take the source waiting for this job. That happens
// where the job stops counting as running, because anything between the two
// would be a stretch in which a swap is told the job is still running, records
// itself as waiting for its end, and is never come back for.
func (v *Supervisor) release() {
	v.mu.Lock()
	var spent []engine
	if v.inUse != nil && v.inUse != v.src.held {
		spent = append(spent, v.inUse)
	}
	v.inUse = nil
	v.mu.Unlock()
	v.giveUp(spent)
}

// finished puts the supervisor back to having no job in flight and takes into
// use whatever was waiting for that job to end.
//
// The two are one critical section, and that is what leaves no window rather
// than merely a narrow one. A swap told to wait either arrives before this and
// leaves an engine that this takes at once, or arrives after it and finds no
// job running and installs its own. There is no third state a caller can be in:
// none in which a swap is recorded against a job that has already had its end
// dealt with, and none in which the engine is changed under a job still
// counting as running.
//
// giveUp is called with the lock let go, as everywhere else: closing a pool
// takes a while, and every reader of the queue would be waiting behind it.
func (v *Supervisor) finished() {
	v.mu.Lock()
	v.cancel()
	v.running, v.cancel = 0, nil
	var spent []engine
	if v.pending.raise != nil {
		waiting := v.pending
		v.pending = source{}
		// The job is already no longer running, so the stop install would send to
		// it goes nowhere, and what it hands back to be given up is what the next
		// job would have used rather than what this job was inside.
		spent = v.install(waiting)
	}
	v.mu.Unlock()
	v.giveUp(spent)
}

// runJob reads back what the job has left, puts up the pool it asked for and
// takes the one through the other.
//
// The plan is read first and the pool raised second, so a job with nothing left
// costs no warm-up at all. A pool that would not go up leaves the job exactly as
// it was found: nothing has been recorded against it and nothing stamps it, so
// it is still there to be carried on, and the reason is in the log because it is
// a refusal from something on this machine rather than anything the job did.
func (v *Supervisor) runJob(ctx context.Context, src source, id int64) {
	j, sum, err := v.plan(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			v.log.Error("a job could not be read back before it ran", "job", id, "error", err)
		}
		return
	}
	if len(j.Queries) == 0 {
		return
	}
	eng, err := v.raise(ctx, src, sum)
	if err != nil {
		if ctx.Err() == nil {
			v.log.Error("the identities a job asked for could not be raised, and the job is still there to be carried on",
				"job", id, "ports", asked(sum.Ports, v.ports), "threads", asked(sum.Threads, v.threads),
				"error", err)
		}
		return
	}
	if rep := eng.Run(ctx, j, jobSink{st: v.st, jobID: id}); rep.Err != nil {
		v.log.Error("a job was refused before anything was sent", "job", id, "error", rep.Err)
	}
}

// plan is the work a job has left, dressed in the settings it was created
// under, beside the job as the history holds it.
//
// The settings come from the job and not from anything current: a job picked up
// part way has to run as the job it is, or the history holds two shapes of
// result under one name. The summary goes back with it because the sizes the
// pool is raised at are read from the same row, and reading that row twice would
// let a job be raised at one shape and run as another.
func (v *Supervisor) plan(ctx context.Context, id int64) (run.Job, store.JobSummary, error) {
	sum, err := v.st.Progress(ctx, id)
	if err != nil {
		return run.Job{}, store.JobSummary{}, err
	}
	left, err := v.st.Pending(ctx, id)
	if err != nil {
		return run.Job{}, store.JobSummary{}, err
	}
	// The target travels with the kind that needs it. A position check handed on
	// without the site it is about recognises nothing and reports every phrase as
	// one the site does not rank for.
	// The retry limit comes from the job for the same reason the pool does: how
	// many identities a query is worth depends on the list, and the list is the
	// operator's. A job that named none is run at whatever the run layer takes
	// as its own default, which is the one place that number is written down.
	// Mobile reaches the run because one header depends on it: what a browser
	// will accept differs between a phone and a desktop, and the ports were
	// already opened as one or the other.
	j := run.Job{Kind: runKind(sum.Kind), Target: sum.Target,
		Pages: sum.Pages, Tries: sum.Tries,
		Mobile: runsOnPhones(sum.Device)}
	for _, q := range left {
		j.Queries = append(j.Queries,
			google.Query{Text: q.Text, Country: sum.Country, Language: sum.Language})
		// The query keeps the number it was given. A job taken up part way holds
		// only what is left, and numbering that from zero files every result
		// against the wrong query.
		j.Ordinals = append(j.Ordinals, q.Ordinal)
	}
	return j, sum, nil
}

// settle stamps a job that has nothing left and leaves alone one that has.
//
// What is left decides it, not how the run ended. A job somebody stopped keeps
// its unfinished queries unfinished and no stamp, which is exactly what there
// is for a resume to take up; a job whose last query landed the instant before
// the stop has nothing left and is done.
//
// The context is its own, because the one the job ran on is the one the stop
// ended and every read here takes a context.
func (v *Supervisor) settle(id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), jobSettleGrace)
	defer cancel()

	left, err := v.st.Pending(ctx, id)
	if err != nil {
		v.log.Error("what a job had left could not be read", "job", id, "error", err)
		return
	}
	if len(left) != 0 {
		return
	}
	if err := v.st.FinishJob(ctx, id); err != nil {
		v.log.Error("a finished job could not be stamped", "job", id, "error", err)
	}
}

// queueUp puts a job at the back of the queue and wakes the worker.
func (v *Supervisor) queueUp(id int64) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return ErrClosed
	}
	if v.running == id || slices.Contains(v.queue, id) {
		// A job queued twice runs twice, and the second run finds nothing left
		// and stamps a job the first one is still working through.
		return fmt.Errorf("%w: %d", ErrBusy, id)
	}
	v.queue = append(v.queue, id)
	v.wakeUp()
	return nil
}

// wakeUp tells the worker there may be something to do. It is called with mu
// held and never waits: the channel carries one signal, and a signal already
// there is one the worker has not acted on yet.
func (v *Supervisor) wakeUp() {
	select {
	case v.wake <- struct{}{}:
	default:
	}
}

func (v *Supervisor) isClosed() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.closed
}

// runsOnPhones says whether a job's ports are phones.
//
// It is a function rather than a comparison written where it is needed, because
// it decides a header: what a browser will accept differs between the two, and
// it is the one header the proxy leaves alone for a Safari identity. Written
// twice it would one day be written differently in one of the two places, and
// what would come of that is a phone sending no Accept at all — which is a
// request no browser has ever made.
func runsOnPhones(device string) bool { return device == blanktrail.DeviceMobile }
