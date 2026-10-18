// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waiters reads how many searches and lookups wait in the budget.
func waiters(b *Budget) (search, lookup int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.searchWaiting, b.lookupWaiting
}

// spinUntil yields until cond holds; a deadline stands in for a hang, never
// for a wait the test depends on.
func spinUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("gave up waiting for %s", what)
		}
		runtime.Gosched()
	}
}

// acquiring starts an Acquire in the background and returns once it has
// either been let in or settled to wait (it is counted among the waiters then,
// having looked at the budget and found no room); no sleeping is involved. The
// channel carries the unit it was let in with, or is closed if it gave up.
func acquiring(ctx context.Context, t *testing.T, b *Budget, c Class) <-chan *Unit {
	t.Helper()
	before := func() int {
		s, l := waiters(b)
		if c == ClassSearch {
			return s
		}
		return l
	}
	was := before()
	got := make(chan *Unit, 1)
	go func() {
		u, err := b.Acquire(ctx, c)
		if err != nil {
			close(got)
			return
		}
		got <- u
	}()
	spinUntil(t, "an acquire to be let in or to wait", func() bool {
		return before() > was || len(got) > 0
	})
	return got
}

// waiting is an acquire that has to be seen waiting, not let in.
func waiting(ctx context.Context, t *testing.T, b *Budget, c Class, what string) <-chan *Unit {
	t.Helper()
	got := acquiring(ctx, t, b, c)
	if len(got) > 0 {
		t.Fatalf("%s was let in, want it waiting", what)
	}
	return got
}

// stillWaiting checks that nothing a waiter decides on lets it in. The waiter
// was counted after finding no room; here the rule is asked again, so a change
// that should not have let it in is caught by the rule and not by luck.
func stillWaiting(t *testing.T, b *Budget, c Class, got <-chan *Unit, what string) {
	t.Helper()
	b.mu.Lock()
	admits := b.admitsLocked(c)
	b.mu.Unlock()
	if admits {
		t.Fatalf("%s would be let in now, want it waiting", what)
	}
	select {
	case <-got:
		t.Fatalf("%s was let in, want it waiting", what)
	default:
	}
}

func letIn(t *testing.T, got <-chan *Unit, what string) *Unit {
	t.Helper()
	select {
	case u, ok := <-got:
		if !ok {
			t.Fatalf("%s gave up, want it let in", what)
		}
		return u
	case <-time.After(5 * time.Second):
		t.Fatalf("%s is still waiting, want it let in", what)
	}
	return nil
}

// take is n places let in at once, failing the test rather than hanging.
func take(t *testing.T, b *Budget, c Class, n int) []*Unit {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var out []*Unit
	for i := 0; i < n; i++ {
		u, err := b.Acquire(ctx, c)
		if err != nil {
			t.Fatalf("place %d of %d was not given: %v", i+1, n, err)
		}
		out = append(out, u)
	}
	return out
}

func TestBudget_LetsEverythingThroughWhereThereIsNoLimit(t *testing.T) {
	// Nought is how every profile ran before there was a limit, and it has to
	// stay exactly that: nothing waits and nothing is counted.
	b := NewBudget(0, 5)
	for _, u := range take(t, b, ClassLookup, 100) {
		if u != nil {
			t.Fatal("a budget with no limit handed out a unit to count")
		}
	}
	if got := b.Free(ClassLookup); got != math.MaxInt {
		t.Errorf("a budget with no limit has %d free for lookups, want no end", got)
	}
	if got := b.Free(ClassSearch); got != math.MaxInt {
		t.Errorf("a budget with no limit has %d free for searches, want no end", got)
	}
	if r := b.Reading(); r.InFlight != 0 {
		t.Errorf("a budget with no limit counted %d in flight", r.InFlight)
	}
	var none *Budget
	if u, err := none.Acquire(t.Context(), ClassSearch); u != nil || err != nil {
		t.Errorf("no budget at all gave %v, %v", u, err)
	}
	if got := none.Free(ClassSearch); got != math.MaxInt {
		t.Errorf("no budget at all has %d free, want no end", got)
	}
	if r := none.Reading(); r != (BudgetReading{}) {
		t.Errorf("no budget at all reads %+v, want zeros", r)
	}
	var nu *Unit
	nu.Solving(1)
	nu.Release()
	if nu.waitsOnASolve() {
		t.Error("no unit at all waits on a solve")
	}
}

func TestBudget_ASearchWaitsForRoomAndIsNeverRefused(t *testing.T) {
	b := NewBudget(2, 0)
	held := take(t, b, ClassSearch, 2)
	third := waiting(t.Context(), t, b, ClassSearch, "a third search on a limit of two")
	stillWaiting(t, b, ClassSearch, third, "a third search on a limit of two")
	held[0].Release()
	if u := letIn(t, third, "the third search once a place came free"); u == nil {
		t.Error("the third search was let in with nothing held for it")
	}
	if r := b.Reading(); r.InFlight != 2 {
		t.Errorf("%d in flight with two places held and one given back and taken again, want two", r.InFlight)
	}
}

func TestBudget_ALookupLeavesRoomForOneMoreSolve(t *testing.T) {
	// A lookup goes only while a challenge met right now could still be
	// solved beside it: the solve is what a search cannot do without, and its
	// connections are the ones that must not be cut.
	b := NewBudget(10, 5)
	take(t, b, ClassLookup, 5)
	sixth := waiting(t.Context(), t, b, ClassLookup, "a sixth lookup on a limit of ten with five kept for a solve")
	// The room kept for a solve is the searches' to use.
	take(t, b, ClassSearch, 5)
	stillWaiting(t, b, ClassLookup, sixth, "the sixth lookup once the searches took the room")
}

func TestBudget_ALookupWaitsBehindAWaitingSearch(t *testing.T) {
	b := NewBudget(3, 0)
	held := take(t, b, ClassSearch, 3)
	search := waiting(t.Context(), t, b, ClassSearch, "a fourth search")
	lookup := waiting(t.Context(), t, b, ClassLookup, "a lookup behind a waiting search")
	held[0].Release()
	letIn(t, search, "the waiting search")
	stillWaiting(t, b, ClassLookup, lookup, "the lookup while the search took the place")
	held[1].Release()
	letIn(t, lookup, "the lookup once no search waits")
}

func TestBudget_ALookupIsNotLetInWhileASearchWaits(t *testing.T) {
	// The rule itself, away from timing: with room for a lookup and a search
	// waiting, the lookup is not let in.
	b := NewBudget(10, 0)
	b.mu.Lock()
	b.searchWaiting = 1
	ok := b.admitsLocked(ClassLookup)
	b.searchWaiting = 0
	ok2 := b.admitsLocked(ClassLookup)
	b.mu.Unlock()
	if ok || !ok2 {
		t.Errorf("a lookup with room reads admitted=%v behind a waiting search and %v without one, want false and true", ok, ok2)
	}
}

func TestBudget_ASolveHoldsTheReserveInsteadOfOne(t *testing.T) {
	b := NewBudget(10, 5)
	solve := take(t, b, ClassSearch, 1)[0]
	if solve.waitsOnASolve() {
		t.Error("a unit not yet told it is a solve says it waits on one")
	}
	solve.Solving(20101)
	if !solve.waitsOnASolve() {
		t.Error("a solve does not say it waits on one")
	}
	if r := b.Reading(); r.InFlight != 0 || r.Solves != 1 || r.InUse() != 5 {
		t.Errorf("a solve reads %+v (in use %d), want one solve counted as five", r, r.InUse())
	}
	if !b.solvingOn(20101) {
		t.Error("the port the solve is on is not known to be solving")
	}
	take(t, b, ClassSearch, 5)
	sixth := waiting(t.Context(), t, b, ClassSearch, "a search past a solve and five searches on a limit of ten")
	solve.Release()
	letIn(t, sixth, "the search once the solve ended")
	if b.solvingOn(20101) {
		t.Error("the port is still known to be solving after the solve ended")
	}
	if solve.waitsOnASolve() {
		t.Error("a solve that ended says it still waits on one")
	}
	b.mu.Lock()
	left := len(b.solving)
	b.mu.Unlock()
	if left != 0 {
		t.Errorf("%d ports still on the books as solving after the only solve ended", left)
	}
}

func TestBudget_ASolveIsCountedOnceAndNotAfterItEnded(t *testing.T) {
	b := NewBudget(20, 5)
	u := take(t, b, ClassSearch, 1)[0]
	u.Solving(20101)
	u.Solving(20101)
	if r := b.Reading(); r.Solves != 1 || r.InFlight != 0 {
		t.Errorf("a unit told twice that it is a solve reads %+v, want one solve", r)
	}
	u.Release()
	u.Release()
	u.Solving(20101)
	if r := b.Reading(); r.Solves != 0 || r.InFlight != 0 || b.solvingOn(20101) {
		t.Errorf("after the solve ended and was released twice the budget reads %+v (solving: %v), want nothing held", r, b.solvingOn(20101))
	}
	// A place given back cannot turn into a solve afterwards.
	gone := take(t, b, ClassSearch, 1)[0]
	gone.Release()
	gone.Solving(20103)
	if r := b.Reading(); r.Solves != 0 || r.InFlight != 0 || b.solvingOn(20103) {
		t.Errorf("a released place told it is a solve reads %+v, want nothing held", r)
	}
	// Two solves on one port: the port is solving until the last one ends.
	a, c := take(t, b, ClassSearch, 1)[0], take(t, b, ClassSearch, 1)[0]
	a.Solving(20102)
	c.Solving(20102)
	a.Release()
	if !b.solvingOn(20102) {
		t.Error("the port stopped solving while a second solve was still on it")
	}
	c.Release()
	if b.solvingOn(20102) {
		t.Error("the port is solving after both solves ended")
	}
}

func TestBudget_ASolveThatCostsNothingMoreLetsTheWaitingIn(t *testing.T) {
	// With no reserve a solve holds nothing beyond its own place, and turning
	// into one is room too: a search waiting for it must hear.
	b := NewBudget(2, 0)
	held := take(t, b, ClassSearch, 2)
	search := waiting(t.Context(), t, b, ClassSearch, "a third search on a limit of two")
	held[0].Solving(20101)
	letIn(t, search, "the search once a place turned into a solve that holds nothing")
}

func TestBudget_WhatTheServiceCountsHoldsTheLookupsButNotTheSearches(t *testing.T) {
	// The service opens connections the parser never asked for — retries, kept
	// connections, the solver's own — and the tariff counts those too. Its
	// count is read rarely (ten seconds apart while there is room), so it is a
	// correction and not the gate: a count over the limit holds new lookups
	// until a reading shows room again, and the searches go on by this
	// program's own count, which keeps them within the limit meanwhile.
	b := NewBudget(10, 0)
	b.observe(12, true)
	lookup := waiting(t.Context(), t, b, ClassLookup, "a lookup while the service counts twelve against ten")
	stillWaiting(t, b, ClassLookup, lookup, "a lookup while the service counts twelve against ten")
	take(t, b, ClassSearch, 1)
	b.observe(3, true)
	letIn(t, lookup, "the lookup once the service counted three")
	if r := b.Reading(); r.InUse() != 3 || r.Computed() != 2 || !r.Known {
		t.Errorf("reads %+v (in use %d, counted here %d), want the service's three over the two counted here",
			r, r.InUse(), r.Computed())
	}
}

func TestBudget_ASearchGoesByItsOwnCountWhateverTheServiceSays(t *testing.T) {
	b := NewBudget(10, 0)
	b.observe(40, true)
	take(t, b, ClassSearch, 10)
	if got := b.Free(ClassSearch); got != 0 {
		t.Errorf("%d free for searches after ten of ten, want none", got)
	}
}

func TestBudget_AWaitCalledOffLeavesNothingBehind(t *testing.T) {
	// A job stopped while its searches wait: the searches go, and nothing of
	// them is left standing in front of the lookups.
	b := NewBudget(1, 0)
	held := take(t, b, ClassSearch, 1)
	ctx, cancel := context.WithCancel(t.Context())
	search := waiting(ctx, t, b, ClassSearch, "a second search")
	lookup := waiting(t.Context(), t, b, ClassLookup, "a lookup behind it")
	cancel()
	if _, ok := <-search; ok {
		t.Fatal("the search called off was let in")
	}
	if s, l := waiters(b); s != 0 || l != 1 {
		t.Errorf("%d searches and %d lookups wait after the search was called off, want none and the one lookup", s, l)
	}
	held[0].Release()
	letIn(t, lookup, "a lookup after the only waiting search was called off")

	gone, stop := context.WithCancel(t.Context())
	stop()
	full := NewBudget(1, 0)
	take(t, full, ClassSearch, 1)
	if _, err := full.Acquire(gone, ClassSearch); !errors.Is(err, context.Canceled) {
		t.Errorf("an acquire on a called-off request answered %v, want the cancellation", err)
	}
	if r := full.Reading(); r.InFlight != 1 {
		t.Errorf("%d in flight after an acquire was called off, want only the one held", r.InFlight)
	}
}

func TestBudget_ACalledOffSearchLetsTheLookupsBehindItIn(t *testing.T) {
	// The lookup looked while the search it stood behind was still counted, saw
	// room, and went back to sleep; the search then gave up, and nothing else
	// would ever wake the lookup. The room is made here without a wake, as it
	// is when a release comes just before the cancellation, so the cancellation
	// has to be the one that tells.
	b := NewBudget(1, 0)
	take(t, b, ClassSearch, 1)
	ctx, cancel := context.WithCancel(t.Context())
	search := waiting(ctx, t, b, ClassSearch, "a second search")
	lookup := waiting(t.Context(), t, b, ClassLookup, "a lookup behind it")
	b.mu.Lock()
	b.inFlight = 0
	b.mu.Unlock()
	cancel()
	<-search
	letIn(t, lookup, "the lookup once the search it stood behind gave up")
}

func TestBudget_NothingWaitsOrLeaksWhenManyAreCalledOff(t *testing.T) {
	b := NewBudget(1, 0)
	held := take(t, b, ClassSearch, 1)
	ctx, cancel := context.WithCancel(t.Context())
	var done sync.WaitGroup
	const n = 20
	for i := range n {
		c := ClassSearch
		if i%2 == 1 {
			c = ClassLookup
		}
		done.Add(1)
		go func() {
			defer done.Done()
			if u, err := b.Acquire(ctx, c); err == nil {
				u.Release()
			}
		}()
	}
	spinUntil(t, "all of them to wait", func() bool {
		s, l := waiters(b)
		return s+l == n
	})
	cancel()
	done.Wait()
	if s, l := waiters(b); s != 0 || l != 0 {
		t.Errorf("%d searches and %d lookups still counted as waiting after all were called off", s, l)
	}
	held[0].Release()
	if r := b.Reading(); r.InFlight != 0 || r.Solves != 0 {
		t.Errorf("reads %+v after everything was given back, want nothing held", r)
	}
}

func TestBudget_ReleasingTwiceGivesBackOnce(t *testing.T) {
	b := NewBudget(2, 0)
	u := take(t, b, ClassSearch, 2)[0]
	u.Release()
	u.Release()
	if r := b.Reading(); r.InFlight != 1 {
		t.Errorf("%d in flight after one of two was released twice, want one", r.InFlight)
	}
}

func TestBudget_ChangingTheLimitLetsTheWaitingIn(t *testing.T) {
	b := NewBudget(1, 0)
	take(t, b, ClassSearch, 1)
	search := waiting(t.Context(), t, b, ClassSearch, "a search on a limit of one")
	b.Set(2, 0)
	letIn(t, search, "the search once the limit was raised to two")
	lookup := waiting(t.Context(), t, b, ClassLookup, "a lookup on a full limit of two")
	b.Set(0, 5)
	letIn(t, lookup, "the lookup once the limit was taken off")
}

func TestBudget_ChangingTheReserveLetsALookupIn(t *testing.T) {
	b := NewBudget(10, 10)
	lookup := waiting(t.Context(), t, b, ClassLookup, "a lookup that must leave ten for a solve on a limit of ten")
	b.Set(10, 5)
	letIn(t, lookup, "the lookup once the reserve was lowered to five")
}

func TestBudget_FreeSaysHowManyMoreOfEachMayGoNow(t *testing.T) {
	b := NewBudget(20, 5)
	take(t, b, ClassSearch, 4)
	if s, l := b.Free(ClassSearch), b.Free(ClassLookup); s != 16 || l != 11 {
		t.Errorf("free reads %d for searches and %d for lookups, want 16 and 11", s, l)
	}
	b.observe(18, true)
	if s, l := b.Free(ClassSearch), b.Free(ClassLookup); s != 16 || l != 0 {
		t.Errorf("with the service at 18, free reads %d and %d, want 16 by this program's count and none", s, l)
	}
}

func TestBudget_KnowsThePortsOfEveryPoolInIt(t *testing.T) {
	b := NewBudget(30, 5)
	one, two := &fakeHolder{nums: []int{20101, 20102}}, &fakeHolder{nums: []int{20200}}
	b.join(one)
	b.join(two)
	if got := len(b.ports()); got != 3 {
		t.Errorf("the budget knows %d ports, want the three its two pools hold", got)
	}
	b.leave(one)
	if got := b.ports(); len(got) != 1 || got[0] != 20200 {
		t.Errorf("after one pool left the budget knows %v, want the other's one", got)
	}
	if over, holders := b.observe(31, true); !over || len(holders) != 1 {
		t.Errorf("31 against 30 reads over=%v with %d pools to tell, want over and one", over, len(holders))
	}
	if over, _ := b.observe(30, true); over {
		t.Error("30 against 30 reads as over")
	}
	if over, _ := b.observe(99, false); over {
		t.Error("a count that is not a reading reads as over")
	}
	b.Set(0, 5)
	if over, _ := b.observe(99, true); over {
		t.Error("a count against no limit reads as over")
	}
	var none *Budget
	none.join(one)
	none.leave(one)
	if over, holders := none.observe(5, true); over || holders != nil || none.ports() != nil || none.solvingOn(1) {
		t.Error("no budget at all knows something")
	}
}

func TestBudget_AskingThePortsDoesNotHoldTheBudget(t *testing.T) {
	// A pool answers with its own lock, and a pool may be in the middle of
	// acquiring from the budget while holding it; the budget must not be locked
	// while it asks.
	b := NewBudget(5, 0)
	h := &lockProbe{b: b}
	b.join(h)
	b.ports()
	if !h.freeToLock {
		t.Error("the budget was locked while a pool was asked for its ports")
	}
}

// TestBudget_ManyThreadsNeverGoOverTheLimit hammers one budget with searches
// and lookups from many goroutines and checks the one thing that must hold
// throughout: what this program counts never passes the limit. A place that
// turns into a solve may pass it, because the service decides when that is and
// a solve must not be cut; it is the next ones that wait, which the second
// round shows by ending clean.
func TestBudget_ManyThreadsNeverGoOverTheLimit(t *testing.T) {
	const limit, reserve = 12, 3
	for _, withSolves := range []bool{false, true} {
		b := NewBudget(limit, reserve)
		var over atomic.Int64
		var wg sync.WaitGroup
		for g := range 24 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range 200 {
					c := ClassSearch
					if (g+i)%3 == 0 {
						c = ClassLookup
					}
					u, err := b.Acquire(t.Context(), c)
					if err != nil {
						return
					}
					if r := b.Reading(); !withSolves && r.Computed() > limit {
						over.Add(1)
					}
					if withSolves && c == ClassSearch && (g+i)%5 == 0 {
						u.Solving(20000 + g)
					}
					runtime.Gosched()
					u.Release()
				}
			}()
		}
		wg.Wait()
		if n := over.Load(); n != 0 {
			t.Errorf("the count passed the limit %d times", n)
		}
		if r := b.Reading(); r.InFlight != 0 || r.Solves != 0 {
			t.Errorf("reads %+v after every thread gave back (solves: %v), want nothing held", r, withSolves)
		}
		if s, l := waiters(b); s != 0 || l != 0 {
			t.Errorf("%d searches and %d lookups still waiting at the end", s, l)
		}
	}
}

// TestBudget_ALookupBehindASearchIsNotLeftWhenTheSearchTakesTheFirstPlace puts
// two places free at once with a search and a lookup waiting. The lookup may
// look while the search still counts as waiting and sleep; the search taking
// its place has to wake it again for the second place. The interleaving is the
// scheduler's, so it is tried again and again.
func TestBudget_ALookupBehindASearchIsNotLeftWhenTheSearchTakesTheFirstPlace(t *testing.T) {
	for i := range 300 {
		b := NewBudget(3, 0)
		take(t, b, ClassSearch, 3)
		search := waiting(t.Context(), t, b, ClassSearch, "a fourth search")
		lookup := waiting(t.Context(), t, b, ClassLookup, "a lookup behind it")
		// Room for both appears in one move, without a wake of its own.
		b.mu.Lock()
		b.inFlight = 1
		b.wakeLocked()
		b.mu.Unlock()
		if letIn(t, search, "the search") == nil || letIn(t, lookup, "the lookup") == nil {
			t.Fatalf("round %d", i)
		}
	}
}

// fakeHolder is a pool as the budget sees it.
type fakeHolder struct {
	nums []int
	over int
}

func (h *fakeHolder) portNumbers() []int { return h.nums }
func (h *fakeHolder) overLimit()         { h.over++ }

// lockProbe tries the budget's lock while it is being asked for its ports.
type lockProbe struct {
	b          *Budget
	freeToLock bool
}

func (h *lockProbe) portNumbers() []int {
	if h.b.mu.TryLock() {
		h.freeToLock = true
		h.b.mu.Unlock()
	}
	return nil
}
func (h *lockProbe) overLimit() {}
