// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"sync"
	"time"
)

// flying is the register of the queries an index or position check has in
// flight, and of the twins started beside the ones whose request has waited too
// long at the end of a job.
//
// It is twin.go for the two kinds that ask once per query. Those never had a
// twin: on job 18 of 2026-10-08, an index check of a thousand addresses, 999
// were answered in seven minutes and the last stood for a quarter of an hour
// with every other thread idle — two of its requests each held by the far end
// the whole 450 seconds a request may take, with twenty-eight tries still to
// go. The cure is the same as for a parse job: once the queue is out, a thread
// with nothing to do asks such a query again through another identity, and
// whichever answers first settles it.
type flying struct {
	mu sync.Mutex
	q  map[int]*aloft
}

// aloft is one query in flight.
type aloft struct {
	began   time.Time // when its first request set out
	askers  int       // requests still asking for it: one, or two with a twin
	twinned bool
	settled bool
	cancels []context.CancelFunc
}

// start registers a query whose first request sets out now.
func (f *flying) start(at int, began time.Time, cancel context.CancelFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.q == nil {
		f.q = map[int]*aloft{}
	}
	f.q[at] = &aloft{began: began, askers: 1, cancels: []context.CancelFunc{cancel}}
}

// hedge picks the query that has waited longest, at least after, with no twin
// yet, and counts the twin about to ask for it. False is nothing worth a twin.
func (f *flying) hedge(now time.Time, after time.Duration) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	best := -1
	for at, a := range f.q {
		if a.settled || a.twinned || now.Sub(a.began) < after {
			continue
		}
		if best < 0 || a.began.Before(f.q[best].began) || (a.began.Equal(f.q[best].began) && at < best) {
			best = at
		}
	}
	if best < 0 {
		return 0, false
	}
	a := f.q[best]
	a.twinned = true
	a.askers++
	return best, true
}

// joined gives the register a way to call off the twin's request. A query
// already settled calls it off at once.
func (f *flying) joined(at int, cancel context.CancelFunc) {
	f.mu.Lock()
	a := f.q[at]
	if a != nil && !a.settled {
		a.cancels = append(a.cancels, cancel)
		cancel = nil
	}
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// began is when the query's first request set out.
func (f *flying) began(at int) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.q[at]; a != nil {
		return a.began
	}
	return time.Time{}
}

// finish says whether this answer is the one that settles its query.
//
// The first answer settles it and calls off the other request. A failure does
// not, while the other is still asking — a refusal one of the two met is no
// answer for the other, which may yet come back with the page — and the one left
// settles the query either way.
func (f *flying) finish(at int, err error) bool {
	f.mu.Lock()
	a := f.q[at]
	if a == nil || a.settled {
		f.mu.Unlock()
		return false
	}
	if err != nil && a.askers > 1 {
		a.askers--
		f.mu.Unlock()
		return false
	}
	a.settled = true
	cancels := a.cancels
	a.cancels = nil
	f.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return true
}

// drop settles a query without an answer: the run is leaving it as it found it.
func (f *flying) drop(at int) {
	f.mu.Lock()
	if a := f.q[at]; a != nil {
		a.settled = true
	}
	f.mu.Unlock()
}

// open is how many queries are still waiting for an answer.
func (f *flying) open() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, a := range f.q {
		if !a.settled {
			n++
		}
	}
	return n
}
