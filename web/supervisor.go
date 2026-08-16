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

// ErrClosed is returned once the supervisor has been shut down. Work taken on
// after that would sit in a queue nobody is reading.
var ErrClosed = errors.New("web: the supervisor has been closed")

// ErrNoSwap is returned when a swap is asked for with nothing to swap in.
var ErrNoSwap = errors.New("web: nothing is waiting to be swapped in")

// SwapWhen is what a swap may do to the job that is running.
//
// It is a choice rather than a rule because the two answers cost different
// things and only the person asking knows which price they would rather pay:
// stopping the job costs a second warm-up, waiting costs however long the job
// has left. A program that picked one would be charging that price silently.
type SwapWhen int

const (
	// SwapNow stops the job in flight and takes the new engine into use at once.
	// The job keeps what it recorded and everything it did not reach stays
	// waiting, so it is a job that can be carried on rather than one thrown away.
	SwapNow SwapWhen = iota
	// SwapAfterThisJob leaves the job in flight to finish on the engine it
	// started on, and takes the new one into use as that job ends.
	SwapAfterThisJob
)

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

// poolEngine takes every job through the one pool the supervisor was handed.
//
// The pool is a field and never something this builds, and that is the whole
// shape of the decision behind this file. A pool answers better the longer it
// has been used, and getting there costs minutes. A pool opened per job pays
// that again every time and holds twice as much while it does: the measurement
// this was decided on got no answer at all in 393 seconds from a fresh pool,
// and an answer on the first attempt in 5 from one that had been used.
type poolEngine struct {
	pool    *blanktrail.Pool
	threads int
}

// Run builds a runner per job and a pool never. A runner is a few fields around
// the pool, and the sink is the one part of it that belongs to a single job.
func (e *poolEngine) Run(ctx context.Context, j run.Job, sink run.Sink) run.Report {
	return (&run.Runner{Pool: e.pool, Threads: e.threads, Sink: sink}).Run(ctx, j)
}

// Close gives up the identities. It runs when the supervisor shuts down and at
// no point between two jobs.
func (e *poolEngine) Close() error { return e.pool.Close() }

// Pool is the one pool, as it stands right now.
func (e *poolEngine) Pool() poolFacts {
	return poolFacts{Stats: e.pool.Stats(), Threads: e.threads, Cooldown: e.pool.Cooldown()}
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

// Supervisor runs jobs one at a time, in the order they were asked for, on one
// set of identities that outlives every job it carries.
//
// One goroutine runs the jobs and nothing else does. It owns everything to do
// with the job in flight: the plan read back from the history, the runner, the
// results going into the history, and the stamp at the end. No caller reaches
// any of that.
//
// What the callers and the worker share is the queue, the id of the job
// running, the function that ends it, whether the supervisor has been shut
// down, and the three engine fields below — and every one of them is read and
// written under mu and nowhere else. The id and the cancel function are always
// written together in one critical section, so a reader sees either no job at
// all or a job together with the means of stopping exactly that job; there is
// no ordering in which a stop can reach the job that came after the one it was
// aimed at. The same holds of a swap: the engine to come, the engine in use and
// the engine waiting move together, so no reader can find two of them
// disagreeing.
//
// Nothing that leaves this type points into any of it. The queue is copied out,
// and the running job is a number.
type Supervisor struct {
	// Set once, before the worker starts, and never written again.
	st  *store.Store
	log *slog.Logger

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
	// eng is what the next job will be taken through. It is nil on a machine
	// whose connection has not been set up yet, and it is replaced while the
	// server runs, which is why it is here rather than among the fields set once.
	eng engine
	// inUse is the engine the job in flight was handed. It is what keeps an
	// engine that has been replaced alive until the job inside it has let go.
	inUse engine
	// pending is an engine waiting for the job in flight to end.
	pending engine
}

// NewSupervisor starts the worker and hands back the queue in front of it.
//
// The pool comes from the caller and is closed when the supervisor is. Nothing
// here can open one, which is what makes a job unable to cost a fresh set of
// identities however this file is later rewritten.
func NewSupervisor(st *store.Store, pool *blanktrail.Pool, threads int) *Supervisor {
	return newSupervisor(st, &poolEngine{pool: pool, threads: threads})
}

func newSupervisor(st *store.Store, eng engine) *Supervisor {
	v := &Supervisor{
		st:  st,
		eng: eng,
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

// Swap changes what the jobs after this one are taken through, without the
// server stopping.
//
// The engine handed in becomes the supervisor's, and the one it replaces is
// given up as soon as no job is inside it: it holds ports nobody will use
// again. Which of the two the job in flight finishes on is the caller's choice.
//
// The context is the caller's, and a swap asked for by a request that has
// already been abandoned is refused: nobody is left to be told which of the two
// prices they have just paid.
func (v *Supervisor) Swap(ctx context.Context, eng engine, when SwapWhen) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if eng == nil {
		return ErrNoSwap
	}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return ErrClosed
	}
	var spent []engine
	if v.pending != nil {
		// Somebody who saved twice before a job ended meant the second save. The
		// engine built for the first is never going to be used, and left open it
		// holds its ports for as long as the process runs.
		spent = append(spent, v.pending)
		v.pending = nil
	}
	if when == SwapAfterThisJob && v.running != 0 {
		v.pending = eng
	} else {
		spent = append(spent, v.install(eng)...)
	}
	v.mu.Unlock()
	v.giveUp(spent)
	return nil
}

// PendingSwap says whether an engine is waiting for the job in flight to end.
//
// It reads under the same lock the swap is recorded under, so it answers with a
// swap that has been recorded whole or with none at all.
func (v *Supervisor) PendingSwap() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.pending != nil
}

// canRun says whether there is anything for a job to be taken through.
//
// A supervisor with no engine is what a machine whose connection has not been
// set up yet has. It writes down what it is given and holds it, because a list
// somebody has just typed in is not something to throw away over a setting they
// are about to change.
func (v *Supervisor) canRun() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.eng != nil
}

// install takes an engine into use and hands back what that leaves to be given
// up. It is called with mu held.
//
// The engine the job in flight is inside is never among them. Closing it under
// a running job would take away the ports that job is in the middle of using,
// so it is left to the worker to give up as it lets go.
func (v *Supervisor) install(eng engine) []engine {
	var spent []engine
	if v.eng != nil && v.eng != v.inUse {
		spent = append(spent, v.eng)
	}
	v.eng = eng
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

// pool is what the identities behind every job report.
//
// The engine is read under the lock because a swap replaces it, and asked under
// none, because what it answers with is the pool's own snapshot taken under the
// pool's own lock and holding two locks to get one number invites waiting on
// them in two orders.
func (v *Supervisor) pool() poolFacts {
	v.mu.Lock()
	eng := v.eng
	v.mu.Unlock()
	if eng == nil {
		// A supervisor with nothing to run on holds no identities. The numbers of
		// an engine that has been given up would describe something that is gone.
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
	v.pending = nil
	v.mu.Unlock()

	close(v.stopping)
	<-v.done

	// The worker has returned, so no job is inside an engine any more and what
	// is left is the one the next job would have run on.
	v.mu.Lock()
	eng := v.eng
	v.eng = nil
	v.mu.Unlock()

	if waiting != nil {
		v.giveUp([]engine{waiting})
	}
	if eng == nil {
		return nil
	}
	return eng.Close()
}

// work is the one goroutine that runs jobs.
func (v *Supervisor) work() {
	defer close(v.done)
	for {
		id, ctx, eng, ok := v.next()
		if !ok {
			select {
			case <-v.wake:
				continue
			case <-v.stopping:
				return
			}
		}
		v.runJob(ctx, eng, id)
		// Before the job is settled, because an engine the job has left holds
		// ports for nothing and settling takes a write to a database.
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
// The engine goes with them. The worker takes it here and uses that one for the
// whole job, so a swap part way through cannot leave the job running on one
// engine and reporting on another.
//
// A supervisor with no engine takes nothing. The job stays at the front of the
// queue and starts when there is something to take it through.
func (v *Supervisor) next() (int64, context.Context, engine, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || v.eng == nil || len(v.queue) == 0 {
		return 0, nil, nil, false
	}
	id := v.queue[0]
	v.queue = v.queue[1:]
	ctx, cancel := context.WithCancel(context.Background())
	v.running, v.cancel = id, cancel
	v.inUse = v.eng
	return id, ctx, v.eng, true
}

// release gives back the engine the job was taken through.
//
// It closes that engine when a swap replaced it while the job was inside it:
// the swap could not close it then, because the job was still sending through
// its ports. This is the moment that stops being true, and this is the only
// place that gives such an engine up.
//
// What it does not do is take the engine waiting for this job. That happens
// where the job stops counting as running, because anything between the two
// would be a stretch in which a swap is told the job is still running, records
// itself as waiting for its end, and is never come back for.
func (v *Supervisor) release() {
	v.mu.Lock()
	var spent []engine
	if v.inUse != nil && v.inUse != v.eng {
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
	if v.pending != nil {
		waiting := v.pending
		v.pending = nil
		// The job is already no longer running, so the stop install would send to
		// it goes nowhere, and the engine it hands back to be given up is the one
		// the next job would have used rather than the one this job was inside.
		spent = v.install(waiting)
	}
	v.mu.Unlock()
	v.giveUp(spent)
}

// runJob reads back what the job has left and takes it through the engine the
// job was given.
func (v *Supervisor) runJob(ctx context.Context, eng engine, id int64) {
	j, err := v.plan(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			v.log.Error("a job could not be read back before it ran", "job", id, "error", err)
		}
		return
	}
	if len(j.Queries) == 0 {
		return
	}
	if rep := eng.Run(ctx, j, jobSink{st: v.st, jobID: id}); rep.Err != nil {
		v.log.Error("a job was refused before anything was sent", "job", id, "error", rep.Err)
	}
}

// plan is the work a job has left, dressed in the settings it was created
// under.
//
// The settings come from the job and not from anything current: a job picked up
// part way has to run as the job it is, or the history holds two shapes of
// result under one name.
func (v *Supervisor) plan(ctx context.Context, id int64) (run.Job, error) {
	sum, err := v.st.Progress(ctx, id)
	if err != nil {
		return run.Job{}, err
	}
	left, err := v.st.Pending(ctx, id)
	if err != nil {
		return run.Job{}, err
	}
	j := run.Job{Pages: sum.Pages, SpecName: sum.SpecName}
	for _, q := range left {
		j.Queries = append(j.Queries,
			google.Query{Text: q.Text, Country: sum.Country, Language: sum.Language})
		// The query keeps the number it was given. A job taken up part way holds
		// only what is left, and numbering that from zero files every result
		// against the wrong query.
		j.Ordinals = append(j.Ordinals, q.Ordinal)
	}
	return j, nil
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
