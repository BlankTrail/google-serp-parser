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

// A thread's round: several queries in the air at once, each on an identity of
// its own, taken a page at a time.
//
// What it replaces is a thread that took one query and walked it to the end
// before it looked at another. Two things were wrong with that, and they are
// the same thing seen twice.
//
// The pause was in the wrong place. A reader sets a gap between two requests on
// one identity, and it was taken between two queries — so a query taken to a
// hundred pages was a hundred requests through one identity with nothing
// between them, and the number the reader set applied to none of them. The pause
// belongs to the identity, and here it is kept there.
//
// And the thread stood still to take it. One query at a time means the pause is
// dead time, so a run either paced itself properly and went slowly or went
// quickly and paced nothing. With several queries in the air the pause on one
// identity is worked through on the others, and the thread only waits when
// every identity it holds is resting.
//
// How many it holds is not a number anybody sets. A thread takes another query
// whenever it is about to stand still and the pool has an identity to spare, so
// the count settles wherever the pause and the speed of the answers put it: a
// slow region needs fewer, a long pause needs more, and neither has to be
// worked out in advance. The ceiling is the pool's — in the usual arrangement
// the ports a thread was given, and in the whole-list one whatever the list and
// the tariff allow.
//
// A query keeps its identity for the whole of its walk, which is the one thing
// that does not change: a visitor paging through results does not change
// address between page one and page two.

// flight is one query being walked: the identity carrying it, how far the walk
// has got, and the earliest that identity may be asked again.
type flight struct {
	at    int // which query of the job this is
	q     google.Query
	lease *blanktrail.Lease
	// next is the page to take, and pages is what has been taken. A walk that
	// loses its identity resumes at next on another one rather than starting
	// again: the pages already taken are taken.
	next  int
	pages []google.SERP
	// tries counts the identities this query has been carried to, which is the
	// same budget a query gets anywhere else in this program.
	tries int
	ready time.Time
	began time.Time
	err   error
	// held is the session the walk is carried by, in a run that keeps
	// sessions. It is taken with the port and let go of with it.
	held *sessions.Held
}

// crew is what a thread needs to work a round. It is a value rather than a
// dozen parameters because every field is the job's or the run's and none of
// them changes while the thread runs.
type crew struct {
	r       *Runner
	a       *Attempt
	j       Job
	thread  int
	pages   int
	queue   <-chan int
	starved chan struct{}
	// starve is called when the pool turns out to have nothing to give, so
	// every other thread stops too rather than spending the rest of the list on
	// an empty pool.
	starve func()
	// results is the run's own slice. Every index belongs to one thread, so
	// nothing here is shared.
	results []QueryResult
	// settle is what a finished query goes through: its addresses read, its
	// results written down, its stages reported.
	settle func(ctx context.Context, at int, began time.Time)
}

// work takes queries off the queue and walks them until there are none left.
func (c *crew) work(ctx context.Context) {
	var flying []*flight
	pending := -1 // a query taken off the queue with no identity yet
	drained := false

	for {
		if ctx.Err() != nil {
			c.letGo(flying)
			return
		}

		// An identity for whatever is waiting for one: a flight whose own was
		// refused, and a query that has not started. Neither queues — a thread
		// with other flights to work has better things to do than stand in
		// line, and one with nothing else waits below.
		for _, f := range flying {
			if f.lease != nil {
				continue
			}
			lease, held, err := c.hold(ctx, false)
			if err != nil {
				break
			}
			f.lease, f.held = lease, held
		}
		if pending >= 0 {
			lease, held, err := c.hold(ctx, len(flying) == 0)
			switch {
			case err == nil:
				flying = append(flying, &flight{at: pending, q: c.j.Queries[pending],
					lease: lease, held: held, next: 1, began: time.Now()})
				c.results[pending].Attempted = true
				pending = -1
				continue
			case errors.Is(err, blanktrail.ErrPoolExhausted) && len(flying) == 0:
				// Nothing to ask through and nothing else to do. That is the
				// pool's condition rather than this query's, so the query is
				// left as it was found — untried, and pending in whatever is
				// writing the history — and the run stops rather than spending
				// the rest of the list on an empty pool.
				c.starve()
				return
			case ctx.Err() != nil:
				c.letGo(flying)
				return
			}
		}

		// The flight whose identity comes due first.
		var due *flight
		for _, f := range flying {
			if f.lease == nil {
				continue
			}
			if due == nil || f.ready.Before(due.ready) {
				due = f
			}
		}

		if due != nil && !due.ready.After(time.Now()) {
			if c.step(ctx, due) {
				// The walk is over, however it ended. What it took is what the
				// query produced, and a walk that failed on its fourth page
				// still hands back three.
				c.results[due.at].Pages = due.pages
				c.results[due.at].Err = due.err
				at, began := due.at, due.began
				c.letGoOf(due)
				flying = without(flying, due)
				c.settle(ctx, at, began)
			}
			continue
		}

		// Nothing can be asked this instant. Rather than stand still, take
		// another query: that is the whole of how a thread decides how many it
		// holds.
		if pending < 0 && !drained {
			select {
			case i, ok := <-c.queue:
				if !ok {
					drained = true
				} else {
					pending = i
					continue
				}
			case <-c.starved:
				c.letGo(flying)
				return
			case <-ctx.Done():
				c.letGo(flying)
				return
			default:
			}
		}

		if len(flying) == 0 && pending < 0 {
			if drained {
				return
			}
			// Nothing in the air and nothing in hand. Now waiting is the work.
			select {
			case i, ok := <-c.queue:
				if !ok {
					return
				}
				pending = i
			case <-c.starved:
				return
			case <-ctx.Done():
				return
			}
			continue
		}

		c.rest(ctx, due)
	}
}

// step takes one page of one flight and says whether the walk is over.
func (c *crew) step(ctx context.Context, f *flight) bool {
	text := f.q.Text
	q := f.q
	q.Page = f.next

	asked := time.Now()
	serp, err := boundSearcher{attempt: c.a, lease: f.lease, held: f.held}.Search(ctx, q)
	c.r.step(c.thread, StageAsk, asked, text, err)

	if err != nil {
		if ctx.Err() != nil {
			f.err = err
			return true
		}
		if f.held != nil {
			// The refusal is the session's — whether Google read the request
			// and judged it, or no address carried it. The address belongs to
			// sessions and stays where it is.
			_, _ = f.held.Failed(ctx)
			f.held = nil
		} else if _, judged := google.ClassOf(err); judged {
			// Only an answer that was read and judged counts as a refusal. A
			// request that never completed was already accounted for by the
			// pool.
			_ = f.lease.Reject(ctx)
		}
		c.letGoOf(f)
		f.tries++
		f.err = err
		// The pause is the identity's debt and not the query's. This identity
		// has just been let go of, so the one that takes the walk on owes
		// nothing and the walk resumes the moment there is one.
		f.ready = time.Time{}
		// The pages already taken are taken, and the walk resumes at the one it
		// stopped on rather than at the first: asking again for what is already
		// known spends requests to learn it twice.
		return f.tries >= c.triesAllowed()
	}

	// This identity has brought back a page, which is what makes it warm: the
	// next request through it costs seconds where the first cost minutes.
	if f.held != nil {
		// Written down page by page: a run can end between two pages.
		_ = f.held.Save(ctx, leasePort{f.lease})
	} else {
		f.lease.Answered()
	}
	// And it owes the pause before it is asked again. That is where the pause
	// belongs: between two requests on one identity, which on a walk of a
	// hundred pages is ninety-nine places it never used to be.
	f.ready = time.Now().Add(c.r.Pool.NextDelay())
	f.err = nil
	f.next++

	// A page that carried nothing is how the walk learned there is no more of
	// this query, not a page of it. Google answers a page past the end of the
	// results as an ordinary page with no results on it, so every query whose
	// results run out before the depth asked for ends on one — which is most
	// of them, because the depth is set once for a list and the lists are not
	// all equally deep. Kept, it would put a row in the history for a page
	// holding no result, report four pages where three were captured, and
	// count the request that found the end into the pages a minute the screens
	// report.
	if len(serp.Results) == 0 {
		return true
	}
	f.pages = append(f.pages, serp)
	return google.LastPage(f.next-1, serp) || f.next > c.pages
}

// triesAllowed is how many identities one query may be carried to.
func (c *crew) triesAllowed() int {
	if c.j.Tries > 0 {
		return c.j.Tries
	}
	return defaultTries
}

// hold takes an identity — a port, and in a run that keeps sessions a session
// put on it — queueing for the port only when the caller says it has nothing
// else to do.
func (c *crew) hold(ctx context.Context, wait bool) (*blanktrail.Lease, *sessions.Held, error) {
	var lease *blanktrail.Lease
	var err error
	switch {
	case wait:
		lease, err = c.a.lease(ctx)
	case c.a.SpecName == "":
		lease, err = c.r.Pool.TryAcquire(ctx)
	default:
		lease, err = c.r.Pool.TryAcquireSpec(ctx, c.a.SpecName)
	}
	if err != nil || c.a.Keeper == nil {
		return lease, nil, err
	}
	// Every address working as many sessions as it may is answered like a pool
	// with nothing free: the thread works what it holds and looks again.
	held, err := c.a.Keeper.Take(ctx, leasePort{lease}, c.a.Want)
	if err != nil {
		lease.Release()
		return nil, nil, err
	}
	return lease, held, nil
}

// rest waits until the earliest identity comes due, or a short moment when
// nothing is due because nothing has an identity yet.
func (c *crew) rest(ctx context.Context, due *flight) {
	wait := waitingForAnIdentity
	if due != nil {
		if until := time.Until(due.ready); until > 0 {
			wait = until
		} else {
			return
		}
	}
	paused := time.Now()
	if err := c.r.Pool.Sleep(ctx, wait); err != nil {
		return
	}
	c.r.step(c.thread, StagePause, paused, "", nil)
}

// waitingForAnIdentity is how long a thread waits before looking for an
// identity again, when everything it holds is waiting for one too. Nothing
// announces a port coming free, so it is looked for on a timer — short enough
// that the thread moves the moment one does, long enough that fifty threads are
// not asking the pool a thousand times a second between them.
const waitingForAnIdentity = 25 * time.Millisecond

// letGoOf gives one flight's port back, and its session with it.
//
// The session is only given back. Every page the walk took was written down as
// it came — the empty one that ended it included — and a refusal was recorded
// where it happened, so by the time a walk lets go there is nothing left to
// write.
func (c *crew) letGoOf(f *flight) {
	if f.held != nil {
		f.held.PutBack()
		f.held = nil
	}
	if f.lease == nil {
		return
	}
	f.lease.Release()
	f.lease = nil
}

// letGo gives back every identity a thread is holding. It runs where the thread
// stops without finishing what it held: the queries stay as they were, and the
// ports go back to the pool rather than out with the thread.
func (c *crew) letGo(flying []*flight) {
	for _, f := range flying {
		c.letGoOf(f)
	}
}

// without is flying with one flight taken out of it.
func without(flying []*flight, drop *flight) []*flight {
	out := flying[:0]
	for _, f := range flying {
		if f != drop {
			out = append(out, f)
		}
	}
	return out
}
