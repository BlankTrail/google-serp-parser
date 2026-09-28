// SPDX-License-Identifier: MIT

package run

import (
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/google"
)

// twinAfter is how long a request of one of a job's last queries waits on the
// far end before a second session starts the same query alongside it.
//
// A request that meets Google's check is held by the service until the check
// is solved, and a solve is usually under a minute but not always: on the speed
// test of 2026-09-28 (8514 queries, a hundred threads) 559 requests waited one
// to five minutes and twelve over five and a half, eight of those ending in a
// refusal at 5m50s. In the middle of a job nobody notices — the other threads go
// on. At the end the job is as long as the slowest of its last requests, and the
// last thirty queries of that test took four minutes with every other thread
// idle.
//
// A minute is past the ordinary solve, so a twin is started for the requests
// that have gone wrong rather than for every check, and short beside the five
// minutes the bad ones take.
const twinAfter = time.Minute

// race is a query being carried twice: the walk that was carrying it, and a
// twin started when that walk's request had waited too long at the end of a
// job. Whichever finishes first settles the query, once; the other is dropped
// and what it collected with it.
//
// The twin starts from the first page, through a session of its own. It cannot
// take over from where the walk stood: the address of every page after the
// first was issued to the session that was shown the one before, and asked for
// by another session it is a search Google never showed that one.
type race struct {
	mu sync.Mutex
	// settled says one of the two has settled the query; the other's pages are
	// not kept from then on.
	settled bool
	// left is the one of the two that failed and stepped aside while the other
	// was still carrying the query. The one still carrying it settles it, with
	// its failure if it fails too.
	left       *walk
	walk, twin *walk
}

// other is the one of the two that is not this one.
func (r *race) other(one *walk) *walk {
	if one == r.twin {
		return r.walk
	}
	return r.twin
}

// hedge starts a twin for the query whose request has waited longest on the far
// end, if one has waited at least after and has no twin already, and hands it
// back to be carried like any walk opened afresh. Nil is nothing worth a twin.
//
// It is only asked at the end of a job, by a thread that has nothing else to
// do: the twin costs a session and, likely, a check, and what it buys is the
// time the job would otherwise spend waiting on one request.
func (w *walks) hedge(now time.Time, after time.Duration) *walk {
	w.mu.Lock()
	defer w.mu.Unlock()
	var longest *walk
	for _, one := range w.carrying {
		if one.ended || one.race != nil || one.asking.IsZero() || now.Sub(one.asking) < after {
			continue
		}
		if longest == nil || one.asking.Before(longest.asking) ||
			(one.asking.Equal(longest.asking) && one.at < longest.at) {
			longest = one
		}
	}
	if longest == nil {
		return nil
	}
	twin := &walk{at: longest.at, q: longest.q, began: longest.began, twin: true}
	r := &race{walk: longest, twin: twin}
	longest.race, twin.race = r, r
	w.live++
	return twin
}

// asking notes that a request of this walk has set out, and how to call it off.
func (w *walks) asking(one *walk, since time.Time, stop func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	one.asking, one.stop = since, stop
}

// asked notes that the walk's request has come back.
func (w *walks) asked(one *walk) {
	w.mu.Lock()
	defer w.mu.Unlock()
	one.asking, one.stop = time.Time{}, nil
}

// lost says the walk has left the register — the other of a race settled the
// query — so whatever it is doing is for nobody.
func (w *walks) lost(one *walk) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return one.ended
}

// drop takes a walk that lost its race out of the register wherever it sits,
// and calls off the request it has in flight. A walk in a thread's hands is
// found lost by that thread when its request comes back.
func (w *walks) drop(one *walk) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if one.ended {
		return
	}
	one.ended = true
	w.live--
	for id, held := range w.carrying {
		if held == one {
			delete(w.carrying, id)
		}
	}
	for i, waiting := range w.waiting {
		if waiting == one {
			w.waiting = append(w.waiting[:i:i], w.waiting[i+1:]...)
			break
		}
	}
	if one.stop != nil {
		one.stop()
	}
}

// win says whether this walk is the one that settles its query, and drops the
// other if it is.
//
// A walk that failed does not settle anything while the other is still carrying
// the query: a refusal one of them met is no answer for the other, which may yet
// come back with the pages. It steps aside, and the one left settles the query
// either way. A twin that settles hands its pages over to the query, in place of
// whatever the walk had taken before it stepped aside or lost.
func (c *crew) win(one *walk, err error) bool {
	r := one.race
	r.mu.Lock()
	if r.settled {
		r.mu.Unlock()
		return false
	}
	if err != nil && r.left != r.other(one) {
		r.left = one
		r.mu.Unlock()
		return false
	}
	r.settled = true
	if one.twin {
		c.mu.Lock()
		c.results[one.at].Pages = one.pages
		c.results[one.at].Err = nil
		c.mu.Unlock()
	}
	r.mu.Unlock()
	c.walks.drop(r.other(one))
	return true
}

// aside says whether a walk that ends having reached nothing — every try spent
// on the road — should leave without touching its query, because the other of
// its race is still carrying it or has settled it already.
func (c *crew) aside(one *walk) bool {
	r := one.race
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settled {
		return true
	}
	if r.left == r.other(one) {
		return false
	}
	r.left = one
	return true
}

// keepRaced adds a page to a walk that has a twin: to the twin's own pages, or
// to the query's if it is the walk. Once the race is settled the loser's pages
// go nowhere.
func (c *crew) keepRaced(one *walk, serp google.SERP) {
	r := one.race
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settled {
		return
	}
	if one.twin {
		one.pages = append(one.pages, serp)
		return
	}
	c.mu.Lock()
	c.results[one.at].Pages = append(c.results[one.at].Pages, serp)
	c.mu.Unlock()
}

// twin is the twin a thread with nothing to do starts, if any is due.
//
// Only once the queue is drained. Before that a thread finds nothing to do only
// for the instant the queue is between two queries, and a request waiting over a
// minute is ordinary at the start of a run — a fresh session's check — where a
// twin would be one more check on a solver that is already the limit.
func (c *crew) twin(drained bool, now time.Time) *walk {
	if !drained {
		return nil
	}
	return c.walks.hedge(now, c.twinWait())
}

// twinWait is the run's twinAfter, which a test can shorten.
func (c *crew) twinWait() time.Duration {
	if c.r != nil && c.r.TwinAfter > 0 {
		return c.r.TwinAfter
	}
	return twinAfter
}
