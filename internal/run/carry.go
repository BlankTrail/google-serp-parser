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
	for {
		if ctx.Err() != nil {
			return
		}
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
			c.letGoUntaken(ctx, lease, err)
			if ctx.Err() != nil {
				return
			}
			continue
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
	search := boundSearcher{attempt: c.a, lease: lease, held: held}

	// What Google's check left on this session before the request, so an answer
	// that comes back with a new one can be read as a check just paid for — and
	// whether Google had answered this session at all before, because the check
	// a fresh one pays to be let in says nothing about the pace.
	held0, admitted := held.Clearance(), held.Admitted()

	var serp google.SERP
	var err error
	moved := false
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
		if _, judged := google.ClassOf(err); judged {
			break
		}
		// Nothing reached Google, so nothing about this is the session's and
		// there is nothing to rest from. Another address, and asked again at
		// once — bounded by the job's tries per phrase, which is what bounds
		// the road to Google everywhere else too.
		if c.walks.spend(held.ID) >= c.triesAllowed() {
			break
		}
		if err := held.MoveOn(ctx, port); err != nil {
			break
		}
		moved = true
	}

	_, judged := google.ClassOf(err)
	switch {
	case ctx.Err() != nil:
		// The run is ending. The query stays as it was found, and the session
		// goes back untouched.
		held.PutBack()
	case err == nil:
		// Written down and given back: its rest starts here, and the keeper
		// will not hand it out again before the rest is over.
		c.r.Challenges.Answer(admitted, held0, held.Clearance())
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
	c.done(ctx, one, session, err)
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
