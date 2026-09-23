// SPDX-License-Identifier: MIT

package run

import (
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
}

func newWalks() *walks { return &walks{carrying: map[int64]*walk{}} }

// start puts a query in the hands of a session.
func (w *walks) start(session int64, at int, q google.Query, began time.Time) *walk {
	one := &walk{at: at, q: q, began: began}
	w.mu.Lock()
	w.carrying[session] = one
	w.mu.Unlock()
	return one
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

// end takes a walk out of the register and hands it back, so the caller can
// settle the query with what it took.
func (w *walks) end(session int64) (*walk, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	one, ok := w.carrying[session]
	delete(w.carrying, session)
	return one, ok
}

// open is how many queries are in flight. A thread stops when the queue is
// drained and this is nought: until then there are pages to ask for.
func (w *walks) open() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.carrying)
}
