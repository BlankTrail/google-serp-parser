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
// What the callers and the worker share is four fields — the queue, the id of
// the job running, the function that ends it, and whether the supervisor has
// been shut down — and every one of them is read and written under mu and
// nowhere else. The id and the cancel function are always written together in
// one critical section, so a reader sees either no job at all or a job together
// with the means of stopping exactly that job; there is no ordering in which a
// stop can reach the job that came after the one it was aimed at.
//
// Nothing that leaves this type points into any of it. The queue is copied out,
// and the running job is a number.
type Supervisor struct {
	// Set once, before the worker starts, and never written again.
	st  *store.Store
	eng engine
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

// Resume puts a job that was left part way back in the queue.
//
// What it will run is not decided here. The queries a job has left are read
// when its turn comes, so a job that was stopped and resumed twice does not
// carry a plan drawn up before the last of its results landed.
func (v *Supervisor) Resume(jobID int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), jobSettleGrace)
	defer cancel()
	sum, err := v.st.Progress(ctx, jobID)
	if err != nil {
		return err
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

// Running is the job in flight and whether there is one.
func (v *Supervisor) Running() (int64, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.running, v.running != 0
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
	v.mu.Unlock()

	close(v.stopping)
	<-v.done
	return v.eng.Close()
}

// work is the one goroutine that runs jobs.
func (v *Supervisor) work() {
	defer close(v.done)
	for {
		id, ctx, ok := v.next()
		if !ok {
			select {
			case <-v.wake:
				continue
			case <-v.stopping:
				return
			}
		}
		v.runJob(ctx, id)
		v.settle(id)
		v.finished()
	}
}

// next takes the job at the front of the queue and marks it as the one running.
//
// The context it returns is the one Stop ends. It is registered alongside the
// id in the same critical section, so a stop aimed at this job can never arrive
// early enough to find the id without it.
func (v *Supervisor) next() (int64, context.Context, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || len(v.queue) == 0 {
		return 0, nil, false
	}
	id := v.queue[0]
	v.queue = v.queue[1:]
	ctx, cancel := context.WithCancel(context.Background())
	v.running, v.cancel = id, cancel
	return id, ctx, true
}

// finished puts the supervisor back to having no job in flight.
func (v *Supervisor) finished() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.cancel()
	v.running, v.cancel = 0, nil
}

// runJob reads back what the job has left and takes it.
func (v *Supervisor) runJob(ctx context.Context, id int64) {
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
	if rep := v.eng.Run(ctx, j, jobSink{st: v.st, jobID: id}); rep.Err != nil {
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
	select {
	case v.wake <- struct{}{}:
	default:
	}
	return nil
}

func (v *Supervisor) isClosed() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.closed
}
