// SPDX-License-Identifier: MIT

package run

import (
	"sort"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/google"
)

// walk is one query a session is carrying: where it stands in the job, the
// address of the page to ask for next, and how far it has got.
//
// A query deeper than one page belongs to the session that opened it. The
// address of each page after the first is the one the page before it carried,
// and Google issued that address to this session — its tags name the search
// this session was shown. So the walk travels with the session rather than
// with the thread, and a thread only carries out whatever the session it was
// handed owes.
type walk struct {
	// at is the query's place in the job, which is how the run files the pages
	// and settles the query when the walk ends.
	at int
	q  google.Query
	// next is where the page after the one in hand lives, as that page wrote
	// it. Empty is a walk that has taken no page yet: its first request is the
	// query's own address.
	next string
	// page counts the pages taken, against the depth the job asked for.
	page int
	// tries counts what has been spent since Google last answered. A request
	// that never reached Google is the road's failure rather than the
	// session's, and the job's tries per phrase is what bounds it.
	tries int
	// began is when the query was first asked, for the reading a screen shows.
	began time.Time
}

// walks is the register of queries in flight, each under the session carrying
// it. It is one per job and read by every thread: the keeper hands a session to
// whichever thread asks for one, and that thread then continues whatever walk
// the session owes.
type walks struct {
	mu sync.Mutex
	// carrying is the walk each session owes, by session id.
	carrying map[int64]*walk
	// waiting are walks with no session: a query whose first page was refused,
	// or one whose session died before it took a page. They keep what they have
	// spent and are given to the next session that owes nothing, which is how a
	// query is carried to another session without going back to the end of the
	// queue.
	//
	// Only a walk that has taken no page waits. Once a page is in hand the walk
	// is that session's alone — the address of its next page was issued to it —
	// and it ends where the session does.
	waiting []*walk
	// live counts the walks that have begun and not ended, wherever they sit —
	// carried, waiting, or in the hands of a thread between the two. A thread
	// stops when the queue is drained and this is nought, so a walk counted only
	// where it happens to be sitting would let the last thread go home while
	// another was still carrying one from one place to the other.
	live int
}

func newWalks() *walks { return &walks{carrying: map[int64]*walk{}} }

// begin opens a walk on a query. It belongs to no session yet: the thread that
// opened it goes looking for one, and hands it over with give.
func (w *walks) begin(at int, q google.Query, began time.Time) *walk {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.live++
	return &walk{at: at, q: q, began: began}
}

// give puts a walk in the hands of a session, and says whether the session took
// it. A session already carrying a query does not take another: the walk it
// carries is the pages of one search Google showed it, and handed a second the
// first would be lost — its query would never be settled and never reported.
func (w *walks) give(session int64, one *walk) (*walk, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if held, ok := w.carrying[session]; ok {
		return held, false
	}
	w.carrying[session] = one
	return one, true
}

// carriers are the sessions carrying a query, which is what a thread with
// nothing new to start asks the keeper for.
func (w *walks) carriers() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	ids := make([]int64, 0, len(w.carrying))
	for id := range w.carrying {
		ids = append(ids, id)
	}
	return ids
}

// park takes a walk off its session and puts it back among those waiting for
// one. It is for a query that has taken no page: whatever refused it refused
// the session, and another may still carry it.
func (w *walks) park(session int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	one, ok := w.carrying[session]
	if !ok {
		return
	}
	delete(w.carrying, session)
	one.next = ""
	w.waiting = append(w.waiting, one)
}

// waitFor puts a walk back among those waiting for a session. It is for a
// thread that took one and could not find a session to carry it.
func (w *walks) waitFor(one *walk) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.waiting = append(w.waiting, one)
}

// resume takes a walk that has been waiting for a session, if any is. It comes
// out of the register until a session takes it, so two threads never carry the
// same query.
func (w *walks) resume() (*walk, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.waiting) == 0 {
		return nil, false
	}
	one := w.waiting[0]
	w.waiting = w.waiting[1:]
	return one, true
}

// of is the walk a session owes, if it owes one.
func (w *walks) of(session int64) (*walk, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	one, ok := w.carrying[session]
	return one, ok
}

// carry records a page taken and where the next one is, and starts the tries
// afresh: Google has answered, so nothing is owed from the road before it.
func (w *walks) carry(session int64, next string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if one, ok := w.carrying[session]; ok {
		one.page++
		one.next = next
		one.tries = 0
	}
}

// spend counts one try against the walk a session carries and says how many it
// has now spent. A try is what the road to Google costs, or a refusal that came
// back before the walk had a page: both are reasons to try again, and the job's
// tries per phrase is what bounds them.
func (w *walks) spend(session int64) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	one, ok := w.carrying[session]
	if !ok {
		return 0
	}
	one.tries++
	return one.tries
}

// spent is how many tries the walk a session carries has spent.
func (w *walks) spent(session int64) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if one, ok := w.carrying[session]; ok {
		return one.tries
	}
	return 0
}

// end takes a walk out of the register and hands it back, so the caller can
// settle the query with what it took.
func (w *walks) end(session int64) (*walk, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	one, ok := w.carrying[session]
	if !ok {
		return nil, false
	}
	delete(w.carrying, session)
	w.live--
	return one, true
}

// left takes every walk still in the register — carried and waiting alike —
// and hands them back, leaving it empty.
//
// It is for the end of a run. A walk is settled by the thread that finishes it,
// and a run that stops has walks in neither state: some in a thread's hands,
// the rest sitting under sessions nobody will come back for. What they
// collected is in the results already; without this, nothing ever writes it
// down.
func (w *walks) left() []*walk {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*walk, 0, len(w.carrying)+len(w.waiting))
	for id, one := range w.carrying {
		out = append(out, one)
		delete(w.carrying, id)
	}
	out = append(out, w.waiting...)
	w.waiting = nil
	w.live = 0
	// In the order the queries were opened, so a run settles what it has the
	// way it took it rather than in whatever order a map hands them over.
	sort.Slice(out, func(i, j int) bool { return out[i].at < out[j].at })
	return out
}

// open is how many queries are in flight: begun and not settled. A thread stops
// when the queue is drained and this is nought — until then there are pages to
// ask for, on this thread or another.
func (w *walks) open() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.live
}
