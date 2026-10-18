// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"math"
	"sync"
)

// Class is what a place in the budget is wanted for.
type Class int

const (
	// ClassSearch is a search request, a result page the job is after.
	ClassSearch Class = iota
	// ClassLookup is everything that is not the job's own search: the
	// suggestions and the /goto lookups. It gives way to searches and to solves.
	ClassLookup
)

// Budget is how many connections to the proxy a profile may have open at once,
// shared by every pool of the profile in this process. It exists because a
// measured job (twenty threads, up to a hundred and twenty connections to one
// proxy at the peak) went over a tariff that counts connections, and the
// service then cut connections, solves in progress among them. A solve is
// what a search cannot go on without, so it keeps its room (the reserve) for as
// long as it lasts, searches come before lookups, and a lookup goes only while
// a solve met right now would still fit beside it.
//
// Two counts decide. This program's own (what it sent, plus the reserve for
// each solve) is free and exact for what it knows of; the service's (read now
// and then from its listing) also sees what the service opened on its own:
// retries, kept connections, the solver's own. "In use" is the larger of the
// two. Searches are admitted by the own count alone, because the service's
// reading is up to ten seconds old and a search held on a stale figure is a
// thread standing still for nothing; lookups, which can wait, answer to both.
//
// A limit of nought is how every profile ran before there was a limit: nothing
// waits and nothing is counted.
type Budget struct {
	mu       sync.Mutex
	limit    int
	reserve  int
	inFlight int
	solves   int
	observed int
	known    bool
	// searchWaiting is how many searches wait for room; while any does, no
	// lookup is let in. lookupWaiting only tells the tests when a waiter has
	// settled to wait.
	searchWaiting int
	lookupWaiting int
	solving       map[int]int
	holders       map[budgetHolder]struct{}
	// changed is closed and replaced whenever anything a waiter decides on
	// moves, so every waiter looks again; a sync.Cond cannot be left on a
	// cancelled context.
	changed chan struct{}
}

// budgetHolder is a pool as the budget sees it: the ports it holds, and a way
// to tell it that the service counts more than the limit allows.
type budgetHolder interface {
	portNumbers() []int
	overLimit()
}

// NewBudget makes a budget of limit connections, reserve of which a solve in
// progress holds. A limit of nought or less is no limit.
func NewBudget(limit, reserve int) *Budget {
	b := &Budget{solving: map[int]int{}, holders: map[budgetHolder]struct{}{}, changed: make(chan struct{})}
	b.Set(limit, reserve)
	return b
}

// Set changes the limit and the reserve of a budget in use, and lets the
// waiting look again: a raised limit is room they are waiting for.
func (b *Budget) Set(limit, reserve int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.limit, b.reserve = max(limit, 0), max(reserve, 0)
	if b.limit == 0 {
		b.observed, b.known = 0, false
	}
	b.wakeLocked()
	b.mu.Unlock()
}

func (b *Budget) wakeLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// computedLocked is this program's own count: free, exact for what it sent,
// and blind to what the service opens on its own.
func (b *Budget) computedLocked() int {
	return b.inFlight + b.reserve*b.solves
}

// inUseLocked is the count corrected by the service's last reading.
func (b *Budget) inUseLocked() int {
	return max(b.computedLocked(), b.observed)
}

func (b *Budget) admitsLocked(c Class) bool {
	if b.limit <= 0 {
		return true
	}
	if c == ClassLookup {
		return b.searchWaiting == 0 && b.inUseLocked()+1+b.reserve <= b.limit
	}
	// A search goes by this program's own count alone. The service's reading
	// is up to ten seconds old and a search held on a stale figure is a
	// thread standing still for nothing; what the service opened on its own
	// is answered by holding the lookups instead.
	return b.computedLocked()+1 <= b.limit
}

// Acquire waits for a place of the given class and never refuses one: it
// returns only when it has the place, or when ctx is done (then the error is
// ctx's and nothing is held). Without a budget, or with a limit of nought, it
// returns at once with no unit, and a nil unit is safe to use.
func (b *Budget) Acquire(ctx context.Context, c Class) (*Unit, error) {
	if b == nil {
		return nil, nil
	}
	b.mu.Lock()
	if b.limit <= 0 {
		b.mu.Unlock()
		return nil, nil
	}
	waiting := false
	for !b.admitsLocked(c) {
		if !waiting {
			waiting = true
			b.countWaiterLocked(c, 1)
		}
		ch := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			b.mu.Lock()
			b.countWaiterLocked(c, -1)
			// A search that stops waiting may be the last thing the lookups
			// were standing behind.
			b.wakeLocked()
			b.mu.Unlock()
			return nil, ctx.Err()
		case <-ch:
		}
		b.mu.Lock()
	}
	if waiting {
		b.countWaiterLocked(c, -1)
	}
	b.inFlight++
	b.wakeLocked()
	b.mu.Unlock()
	return &Unit{b: b}, nil
}

func (b *Budget) countWaiterLocked(c Class, d int) {
	if c == ClassSearch {
		b.searchWaiting += d
	} else {
		b.lookupWaiting += d
	}
}

// Unit is one place held in a budget. Release gives it back, once.
type Unit struct {
	b        *Budget
	port     int
	solving  bool
	released bool
}

// Solving turns the place into a solve on the given port: from now on it holds
// the reserve instead of one connection, until it is released. The service
// says when a challenge is being solved; the budget only counts it.
func (u *Unit) Solving(port int) {
	if u == nil {
		return
	}
	b := u.b
	b.mu.Lock()
	if !u.released && !u.solving {
		u.solving, u.port = true, port
		b.inFlight--
		b.solves++
		b.solving[port]++
		// With a reserve below one a solve holds less than the connection it
		// replaces, and that is room for the waiting.
		b.wakeLocked()
	}
	b.mu.Unlock()
}

// Release gives the place back. Calling it again does nothing.
func (u *Unit) Release() {
	if u == nil {
		return
	}
	b := u.b
	b.mu.Lock()
	if !u.released {
		u.released = true
		if u.solving {
			b.solves--
			if b.solving[u.port]--; b.solving[u.port] <= 0 {
				delete(b.solving, u.port)
			}
		} else {
			b.inFlight--
		}
		b.wakeLocked()
	}
	b.mu.Unlock()
}

// waitsOnASolve reports whether the unit is a solve in progress.
func (u *Unit) waitsOnASolve() bool {
	if u == nil {
		return false
	}
	u.b.mu.Lock()
	defer u.b.mu.Unlock()
	return u.solving && !u.released
}

// Free is how many more places of the class may go now, by the same measure
// admission uses; with no limit there is no end.
func (b *Budget) Free(c Class) int {
	if b == nil {
		return math.MaxInt
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		return math.MaxInt
	}
	if c == ClassLookup {
		return max(b.limit-b.inUseLocked()-b.reserve, 0)
	}
	return max(b.limit-b.computedLocked(), 0)
}

// BudgetReading is a budget's figures at one moment.
type BudgetReading struct {
	Limit, Reserve, InFlight, Solves, Observed int
	// Known is whether the service's count has been read.
	Known bool
}

// Computed is this program's own count: what is in flight plus the reserve of
// each solve.
func (r BudgetReading) Computed() int { return r.InFlight + r.Reserve*r.Solves }

// InUse is the larger of this program's count and the service's.
func (r BudgetReading) InUse() int { return max(r.Computed(), r.Observed) }

// Reading copies the budget's figures; a nil budget reads as zeros.
func (b *Budget) Reading() BudgetReading {
	if b == nil {
		return BudgetReading{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return BudgetReading{
		Limit: b.limit, Reserve: b.reserve, InFlight: b.inFlight,
		Solves: b.solves, Observed: b.observed, Known: b.known,
	}
}

// join and leave tell the budget which pools draw on it.
func (b *Budget) join(h budgetHolder) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.holders[h] = struct{}{}
	b.mu.Unlock()
}

func (b *Budget) leave(h budgetHolder) {
	if b == nil {
		return
	}
	b.mu.Lock()
	delete(b.holders, h)
	b.mu.Unlock()
}

func (b *Budget) holdersLocked() []budgetHolder {
	out := make([]budgetHolder, 0, len(b.holders))
	for h := range b.holders {
		out = append(out, h)
	}
	return out
}

// ports lists the ports of every pool in the budget. The pools are asked after
// the budget's lock is let go: a pool takes its own lock to answer, and the
// order of locks is the budgets' first, then this one, never a pool's under it.
func (b *Budget) ports() []int {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	hs := b.holdersLocked()
	b.mu.Unlock()
	var out []int
	for _, h := range hs {
		out = append(out, h.portNumbers()...)
	}
	return out
}

// solvingOn reports whether a solve in progress is on the port.
func (b *Budget) solvingOn(port int) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.solving[port] > 0
}

// observe records the service's count of connections and lets the waiting look
// again, since a lower count is room. over is whether the count is above the
// limit; then holders are the pools to tell.
func (b *Budget) observe(n int, known bool) (over bool, holders []budgetHolder) {
	if b == nil {
		return false, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.observed, b.known = n, known
	b.wakeLocked()
	over = b.limit > 0 && known && n > b.limit
	if over {
		holders = b.holdersLocked()
	}
	return over, holders
}
