// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"errors"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
)

// carry is how a thread works a job in a run that keeps its own sessions.
//
// The walk belongs to the session, so what a thread does next is decided by
// which session is due rather than by which query is next. It takes a port,
// asks the keeper for one of the sessions carrying a query, and takes the next
// page of whatever that one owes. The port goes back the moment the page is
// taken and the rest between two requests on one session is the keeper's to
// enforce, so the pause is never spent on the port and never spent by the
// thread.
//
// Only when none of the sessions in flight is due does the thread open another
// query — and that is the whole of how many queries a thread holds at once:
// another whenever it would otherwise stand still, and no more once the ones in
// hand come due fast enough to keep it busy. A thread that asked for any session
// every time it looked would be given a new one on every glance, because a new
// one is what the keeper makes when nothing has rested.
//
// One port a thread is therefore enough, which is what this is for. The old way
// held the port for the whole walk and slept the pause on it, and the only cure
// for a thread standing still was more ports.
func (c *crew) carry(ctx context.Context) {
	drained := false
	// A thread that has gone home is taken out of the census: left in it, it
	// would read as one still standing wherever it stopped.
	defer c.r.Where.Gone(c.thread)
	for {
		if ctx.Err() != nil {
			return
		}
		c.r.Where.At(c.thread, DoingPort)
		lease, err := c.a.lease(ctx)
		if err != nil {
			if errors.Is(err, blanktrail.ErrPoolExhausted) {
				// There is nothing to ask through. That is the pool's condition
				// and not this query's, so the run stops rather than spending
				// the rest of the list on an empty pool.
				c.starve()
			}
			return
		}

		// A session carrying a query comes first; a query nobody is carrying is
		// opened only where none of them is due.
		var one *walk
		c.r.Where.At(c.thread, DoingTake)
		held, err := c.a.Keeper.TakeOneOf(ctx, leasePort{lease}, c.a.Want, c.walks.carriers())
		if errors.Is(err, sessions.ErrNothingDue) {
			one = c.opening(ctx, &drained)
			if one == nil {
				lease.Release()
				if c.over(drained) {
					return
				}
				// Queries are still being carried by sessions that are resting.
				// Their pages are what is left of this job.
				c.r.Where.At(c.thread, DoingIdle)
				if err := c.r.Pool.Sleep(ctx, waitingForAnIdentity); err != nil {
					return
				}
				continue
			}
			held, err = c.a.Keeper.Take(ctx, leasePort{lease}, c.a.Want)
		}
		if err != nil {
			if one != nil {
				c.walks.waitFor(one)
			}
			if errors.Is(err, sessions.ErrNoAddress) {
				// A thread that wanted a session and could not have one: every
				// address is already carrying as many as it may. The run has
				// stopped widening because the list will not let it.
				c.r.Ramp.Short()
			}
			c.letGoUntaken(ctx, lease, err)
			if ctx.Err() != nil {
				return
			}
			continue
		}

		if held.Fresh {
			// None of the sessions there were was ready for this thread, so one
			// was made: the run is still widening into its speed.
			c.r.Ramp.Made()
		}
		if one == nil {
			one, _ = c.walks.of(held.ID)
		} else if carrying, took := c.walks.give(held.ID, one); !took {
			// The keeper handed back a session that is already carrying a
			// query. Its page is the work here, and the query just opened waits
			// for a session of its own rather than displacing that one.
			c.walks.waitFor(one)
			one = carrying
		}
		if one == nil {
			// Nothing to do with this one after all: another thread took its
			// walk on between the asking and the answer.
			held.PutBack()
			lease.Release()
			continue
		}
		c.page(ctx, lease, held, one)
	}
}

// opening is a query for a thread that has nothing due to carry on with: one
// that was refused and is waiting for another session, or the next off the
// queue. It never waits — a thread with walks in the register has pages to come
// back for, and one without is a thread whose job is ending.
func (c *crew) opening(ctx context.Context, drained *bool) *walk {
	// A query whose first page was refused waits for another session rather
	// than going back to the end of the queue: it has spent tries, and what
	// refused it refused the session it was on.
	if one, ok := c.walks.resume(); ok {
		return one
	}
	if *drained {
		return nil
	}
	select {
	case i, ok := <-c.queue:
		if !ok {
			*drained = true
			return nil
		}
		c.attempted(i)
		return c.walks.begin(i, c.j.Queries[i], time.Now())
	case <-c.starved:
		return nil
	case <-ctx.Done():
		return nil
	default:
		return nil
	}
}

// over says whether this thread has seen the last of the job: the queue drained
// and no query anywhere still in flight. A thread that stopped while another
// still carried one would leave its pages to whoever is left.
func (c *crew) over(drained bool) bool {
	if drained && c.walks.open() == 0 {
		return true
	}
	select {
	case <-c.starved:
		// The pool has nothing left to give. What is still being carried cannot
		// be carried further, and there is nothing to wait for.
		return true
	default:
		return false
	}
}

// letGoUntaken gives back a port no session went onto, and waits where waiting
// is what the refusal calls for.
func (c *crew) letGoUntaken(ctx context.Context, lease *blanktrail.Lease, err error) {
	c.r.Where.At(c.thread, DoingIdle)
	// A port the service lost to a restart is opened again before it is handed
	// out next; a service that is away is waited for, not asked in a loop.
	if blanktrail.PortLost(err) {
		lease.Reopen()
	}
	lease.Release()
	if blanktrail.Unreachable(err) {
		_ = c.r.Pool.Sleep(ctx, serviceAwayWait)
		return
	}
	// Every address already working as many sessions as it may, and whatever
	// else the keeper could not do this instant.
	_ = c.r.Pool.Sleep(ctx, waitingForAnIdentity)
}

// page asks for one page of one walk and does with the answer what the answer
// deserves.
func (c *crew) page(ctx context.Context, lease *blanktrail.Lease, held *sessions.Held, one *walk) {
	defer lease.Release()
	port := leasePort{lease}
	q := one.q
	q.Page = one.page + 1
	search := boundSearcher{attempt: c.a, lease: lease, held: held,
		where: c.r.Where, thread: c.thread}

	// What Google's check left on this session before the request, so an answer
	// that comes back with a new one can be read as a check just paid for.
	//
	// And whether this session is one Google already knows at the address it is
	// asking from. A session it has never answered is paying to be let in, and
	// one that has been moved since its last answer is a stranger where it has
	// landed: both pay a check whatever pace they are asked at, so neither says
	// anything about the pace.
	held0 := held.Clearance()
	known := held.Admitted() && !held.Moved()

	var serp google.SERP
	var err error
	moved := false
	shells := 0
	// again says the page has been asked once more through the address the
	// session was answered from, after the road there failed once.
	again := false
	c.r.Where.At(c.thread, DoingAsk)
	for {
		asked := time.Now()
		if one.next == "" {
			serp, err = search.Search(ctx, q)
		} else {
			// The address the page before it carried, with the tags Google
			// issued to this session on it.
			serp, err = search.SearchAt(ctx, q, one.next)
		}
		c.r.step(c.thread, StageAsk, asked, q.Text, err)
		if err == nil || ctx.Err() != nil {
			break
		}
		if class, judged := google.ClassOf(err); judged {
			// A page Google would not show is its check on the address, handed
			// back unsolved. Asked again through the same session and the same
			// address, it costs one more request; condemned at once, it costs
			// the session its address and a check to be let in at the next one.
			if class == google.ClassShell && shells < c.shellTriesAllowed() &&
				c.walks.spend(held.ID) < c.triesAllowed() {
				shells++
				continue
			}
			break
		}
		// Nothing reached Google, so nothing about this is the session's and
		// there is nothing to rest from. Another address, and asked again at
		// once — bounded by the job's tries per phrase, which is what bounds
		// the road to Google everywhere else too.
		if c.walks.spend(held.ID) >= c.triesAllowed() {
			break
		}
		// An address Google has answered this session from is asked once more
		// before the session is taken off it. One dropped connection is not an
		// address that has stopped answering — the pool gives an address one
		// miss for the same reason — and taking the session off costs it the
		// clearance it holds there: it is a stranger at the next address, and
		// pays a check to be let in. Measured on the wingate list through its
		// first hop: of the addresses put away in the two minutes before, 20 of
		// 22 answered again, and a check costs a thread over a minute where
		// asking again costs one request.
		//
		// A session that is a stranger where it stands has no clearance there
		// to keep. It pays a check wherever it goes, so it moves at once rather
		// than spending a try on an address that has just failed it.
		if known && !again {
			again = true
			continue
		}
		if err := held.MoveOn(ctx, port); err != nil {
			break
		}
		moved = true
	}

	c.r.Where.At(c.thread, DoingGiveBack)
	_, judged := google.ClassOf(err)
	switch {
	case ctx.Err() != nil:
		// The run is ending. A page that came back is kept all the same — it
		// was fetched and paid for, and the run writes down what its walks hold
		// when it stops — but nothing further is asked of the service or the
		// history under a context that has ended.
		if err == nil {
			c.keep(one.at, serp)
		}
		held.PutBack()
	case err == nil:
		// Written down and given back: its rest starts here, and the keeper
		// will not hand it out again before the rest is over.
		// The move may also have happened inside this request: the address it
		// set out through carried nothing and it was sent to another. What
		// answered is a session Google has not seen at that address either.
		c.r.Challenges.Answer(known && !moved, held0, held.Clearance())
		_ = held.Answered(ctx, port)
		c.took(ctx, one, held.ID, serp)
	case judged && moved:
		// It did not revive. Google answered from the new address with a
		// refusal, and the pages this session was keeping are addressed to an
		// exit it no longer has: there is nothing left for it to carry.
		//
		// What becomes of the query is the same question as after any refusal:
		// with pages in hand it is a finished collection, and with none it has
		// not started and waits for another session. A query settled as
		// collected with nothing collected would be written down as done and
		// never asked again.
		_ = held.GiveUp(ctx)
		c.stopped(ctx, one, held.ID, err, true)
	case judged:
		letGoAfter(ctx, held, lease, err)
		c.stopped(ctx, one, held.ID, err, true)
	default:
		// The road, and the tries for it are spent or there was nowhere to move
		// to. Nothing is held against the session.
		held.PutBack()
		c.stopped(ctx, one, held.ID, err, false)
	}
}

// took is what a page that came back does to the walk carrying it.
func (c *crew) took(ctx context.Context, one *walk, session int64, serp google.SERP) {
	// A page that carried nothing is how the walk learned there is no more of
	// this query, not a page of it. Google answers a page past the end of the
	// results as an ordinary page with nothing on it, and kept, it would put a
	// row in the history for a page holding no result.
	if len(serp.Results) == 0 {
		c.done(ctx, one, session, nil)
		return
	}
	c.keep(one.at, serp)
	// The page says where the next one is, and says nothing when there is none:
	// the last page of a query links back and not on. That, and the depth the
	// job asked for, are the two ends of a walk.
	if serp.NextPage == "" || one.page+1 >= c.pages {
		c.done(ctx, one, session, nil)
		return
	}
	c.walks.carry(session, serp.NextPage)
}

// stopped is a walk that cannot go on through the session it was on.
//
// With a page in hand it is a finished collection: the query keeps what it
// took, and asking for the rest through another session would ask Google for a
// page addressed to a search it never showed that one. With nothing in hand
// the query has not started, so it waits for another session — unless the tries
// its phrase is allowed are spent, and then the refusal stands as its answer.
//
// Unless nothing ever reached Google. A phrase that spent every try on
// addresses the service could not reach was never asked, and writing it down as
// failed says the opposite of what happened: measured on a live job the hour
// its list went down, 141 phrases were recorded as failures in twenty minutes
// without one of them being put to Google. So it is left as the run found it,
// the way a query is left when the pool has nothing to give — the next run
// takes it up again, and the history is not filled with failures nobody can
// act on.
func (c *crew) stopped(ctx context.Context, one *walk, session int64, err error, spend bool) {
	if one.page > 0 {
		c.done(ctx, one, session, nil)
		return
	}
	spent := c.walks.spent(session)
	if spend {
		spent = c.walks.spend(session)
	}
	if spent < c.triesAllowed() {
		c.walks.park(session)
		return
	}
	if _, judged := google.ClassOf(err); !judged && err != nil {
		c.never(one, session)
		return
	}
	c.done(ctx, one, session, err)
}

// never takes a walk out of the register and leaves its query as the run found
// it: nothing collected, nothing written down, and not marked as one this run
// asked for.
//
// Not marking it is the whole of it. A query this run attempted and settled
// with neither pages nor a reason is reported as done and written to the
// history as done, and a job resumed afterwards never asks it again.
func (c *crew) never(one *walk, session int64) {
	c.walks.end(session)
	c.mu.Lock()
	c.results[one.at].Attempted = false
	c.results[one.at].Err = nil
	c.mu.Unlock()
}

// done takes the walk out of the register and settles its query with what it
// collected.
func (c *crew) done(ctx context.Context, one *walk, session int64, err error) {
	c.walks.end(session)
	if err != nil {
		c.mu.Lock()
		c.results[one.at].Err = err
		c.mu.Unlock()
	}
	c.r.Where.At(c.thread, DoingRecord)
	c.settle(ctx, one.at, one.began)
}

// keep adds a page to what its query has collected.
//
// The results are shared now: a query started by one thread is carried on by
// whichever thread the keeper hands its session to next.
func (c *crew) keep(at int, serp google.SERP) {
	c.mu.Lock()
	c.results[at].Pages = append(c.results[at].Pages, serp)
	c.mu.Unlock()
}

// attempted marks a query as one this run has asked for, which is what tells a
// query that failed apart from one that was never reached.
func (c *crew) attempted(at int) {
	c.mu.Lock()
	c.results[at].Attempted = true
	c.mu.Unlock()
}
