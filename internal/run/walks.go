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

	// asking is when the request in flight set out, and stop calls it off; both
	// are empty between two requests. They are what the end of a job reads to
	// find a request that has waited too long; see hedge.
	asking time.Time
	stop   func()
	// race is set on a walk carried twice, and on its twin; see race. twin says
	// this is the second, and pages is what it has collected: a twin keeps its
	// pages to itself until it has settled the query.
	race  *race
	twin  bool
	pages []google.SERP
	// ended says the walk has left the register, settled or dropped. A walk that
	// lost its race may still be in a thread's hands, and it must not be put back.
	ended bool
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
	// aside are walks whose every try went to addresses that carried nothing:
	// phrases never put to Google. They are asked again in another pass once the
	// queue is out, and only while the run is getting answers; see again and
	// abandon. They are not this run's attempts while they sit here.
	aside []*walk
	// answers counts the pages Google answered in this run, and mark is what it
	// stood at when the phrases put aside were last sent round again: a pass is
	// worth starting only if something has been answered since.
	answers, mark int
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
	if one.ended {
		// It lost its race while the thread was finding it a session.
		return nil, false
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

// putAside takes a walk off its session and keeps it for another pass: every
// try it had went to addresses that carried nothing. It stays live, so the run
// does not end with it unasked while another pass could still ask it.
func (w *walks) putAside(session int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	one, ok := w.carrying[session]
	if !ok {
		return
	}
	delete(w.carrying, session)
	one.next, one.tries = "", 0
	w.aside = append(w.aside, one)
}

// answered counts a page Google answered.
func (w *walks) answered() {
	w.mu.Lock()
	w.answers++
	w.mu.Unlock()
}

// again sends the walks put aside round once more, if there are any and the
// run has had an answer since they were last sent round. It says whether it
// did.
//
// An answer since is the whole of the test. On the wingate list most addresses
// are dead at any moment, and a phrase whose three tries all met dead ones is
// asked again with good odds; a pass in which nothing at all reached Google is
// the road to the list gone, and asking round again would be asking forever.
func (w *walks) again() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.aside) == 0 || w.answers <= w.mark {
		return false
	}
	w.mark = w.answers
	w.waiting = append(w.waiting, w.aside...)
	w.aside = nil
	return true
}

// abandon lets go of the walks put aside once nothing else is in flight that
// could still bring an answer: the last pass of them brought none. They leave
// the run as it found them, for a later one to take up.
func (w *walks) abandon() {
	w.mu.Lock()
	defer w.mu.Unlock()
	// An answer since the last pass means another pass is due, not an end.
	if len(w.aside) == 0 || w.live != len(w.aside) || w.answers > w.mark {
		return
	}
	for _, one := range w.aside {
		one.ended = true
	}
	w.live -= len(w.aside)
	w.aside = nil
}

// waitFor puts a walk back among those waiting for a session. It is for a
// thread that took one and could not find a session to carry it.
func (w *walks) waitFor(one *walk) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if one.ended {
		return
	}
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
	one.ended = true
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
		// A twin's pages are its own until it wins, and the walk it was
		// started beside is in the register too: what that one collected is
		// the query's, and it is settled once, through that one.
		if !one.twin {
			out = append(out, one)
		}
		delete(w.carrying, id)
	}
	for _, one := range w.waiting {
		if !one.twin {
			out = append(out, one)
		}
	}
	w.waiting = nil
	// Walks put aside collected nothing and are not this run's attempts: they
	// are left as the run found them.
	w.aside = nil
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
