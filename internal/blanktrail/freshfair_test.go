// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Fairness for the fresh searches: once one has waited longer than
// freshWaitFair, ordinary searches are not let in ahead of it.

// budgetClock is a hand-wound clock for a budget.
type budgetClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *budgetClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *budgetClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// clocked is a budget on a hand-wound clock.
func clocked(limit, reserve int) (*Budget, *budgetClock) {
	b := NewBudget(limit, reserve)
	clock := &budgetClock{at: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	b.mu.Lock()
	b.now = clock.now
	b.mu.Unlock()
	return b, clock
}

// ordinaryNow asks for an ordinary search's place under a context already
// done: Acquire hands over a place that fits at once and gives up on one that
// would have to wait, so whether the rule lets it in decides, not a timer.
func ordinaryNow(t *testing.T, b *Budget) *Unit {
	t.Helper()
	done, cancel := context.WithCancel(t.Context())
	cancel()
	u, err := b.Acquire(done, ClassSearch)
	if err != nil {
		return nil
	}
	return u
}

func TestBudget_OrdinarySearchesStopPassingAFreshOneThatWaitedTooLong(t *testing.T) {
	// Ten with a reserve of five and k 1: six ordinary searches in flight, a
	// fresh one needs five and waits; an ordinary one needs one and fits.
	b, clock := clocked(10, 5)
	setAmp(b, 1)
	held := take(t, b, ClassSearch, 6)
	got := waiting(FreshSession(t.Context()), t, b, ClassSearch, "a fresh search at 6 of 10")
	// Just short of the bound the ordinary ones still pass it.
	clock.advance(freshWaitFair - time.Nanosecond)
	if u := ordinaryNow(t, b); u == nil {
		t.Fatal("an ordinary search at 6 of 10 was held back before the fresh one had waited too long")
	} else {
		u.Release()
	}
	// At the bound they stop: there is room for one, and it waits.
	clock.advance(time.Nanosecond)
	if u := ordinaryNow(t, b); u != nil {
		u.Release()
		t.Fatal("an ordinary search was let in ahead of a fresh one that had waited ten seconds")
	}
	// The places freed add up, and the fresh one goes at 5 + 5.
	held[0].Release()
	u := letIn(t, got, "the fresh search once five places were free")
	defer u.Release()
	// Then the ordinary ones are let in again as room allows.
	held[1].Release()
	if o := ordinaryNow(t, b); o == nil {
		t.Error("an ordinary search at 9 of 10 was held back after the fresh one went")
	} else {
		o.Release()
	}
}

func TestBudget_AFreshSearchThatCannotEverFitHoldsNoOneBack(t *testing.T) {
	// A limit of four with a reserve of five (the form refuses it; Set takes
	// it): no fresh search fits even an idle profile, and holding the
	// ordinary ones back for it would stand the job still for good.
	b, clock := clocked(4, 5)
	setAmp(b, 1)
	take(t, b, ClassSearch, 1)
	waiting(FreshSession(t.Context()), t, b, ClassSearch, "a fresh search on four with a reserve of five")
	clock.advance(time.Minute)
	if u := ordinaryNow(t, b); u == nil {
		t.Error("an ordinary search was held back behind a fresh one that can never fit")
	} else {
		u.Release()
	}
	// A limit of five fits it on an idle profile, so it does hold them.
	c, cclock := clocked(5, 5)
	setAmp(c, 1)
	take(t, c, ClassSearch, 1)
	waiting(FreshSession(t.Context()), t, c, ClassSearch, "a fresh search on five beside one")
	cclock.advance(freshWaitFair)
	if u := ordinaryNow(t, c); u != nil {
		u.Release()
		t.Error("an ordinary search on five passed a fresh one that had waited ten seconds and fits an idle profile")
	}
}

func TestBudget_AFreshSearchThatStopsWaitingHoldsNoOneBack(t *testing.T) {
	b, clock := clocked(10, 5)
	setAmp(b, 1)
	take(t, b, ClassSearch, 6)
	ctx, cancel := context.WithCancel(FreshSession(t.Context()))
	got := waiting(ctx, t, b, ClassSearch, "a fresh search at 6 of 10")
	clock.advance(freshWaitFair)
	cancel()
	if _, ok := <-got; ok {
		t.Fatal("the fresh search that was called off was let in")
	}
	if u := ordinaryNow(t, b); u == nil {
		t.Error("an ordinary search was held back behind a fresh one that had stopped waiting")
	} else {
		u.Release()
	}
}

func TestBudget_AFreshSearchLetInHoldsNoOneBack(t *testing.T) {
	// The fresh search went after waiting too long: nobody is waiting, and an
	// ordinary search with room goes.
	b, clock := clocked(20, 5)
	setAmp(b, 1)
	held := take(t, b, ClassSearch, 16)
	got := waiting(FreshSession(t.Context()), t, b, ClassSearch, "a fresh search at 16 of 20")
	clock.advance(freshWaitFair)
	held[0].Release()
	u := letIn(t, got, "the fresh search at 15 of 20")
	defer u.Release()
	for _, h := range held[1:6] {
		h.Release()
	}
	if o := ordinaryNow(t, b); o == nil {
		t.Error("an ordinary search at 15 of 20 was held back with no fresh search waiting")
	} else {
		o.Release()
	}
}

func TestBudget_AFreshSearchWaitingTooLongDoesNotHoldBackAnotherFreshOne(t *testing.T) {
	// The rule holds back the ordinary searches only: another fresh one is
	// let in where it fits, alike with the first. The waiter is put in by
	// hand, so no goroutine's luck in being woken first decides anything.
	b, clock := clocked(10, 5)
	setAmp(b, 1)
	take(t, b, ClassSearch, 5)
	b.mu.Lock()
	b.freshWaiters = map[*freshWaiter]struct{}{{since: clock.now()}: {}}
	b.mu.Unlock()
	clock.advance(freshWaitFair)
	b.mu.Lock()
	fresh, ordinary := b.admitsLocked(ClassSearch, true, 0), b.admitsLocked(ClassSearch, false, 0)
	b.mu.Unlock()
	if !fresh || ordinary {
		t.Errorf("at 5 of 10 behind a fresh search waiting ten seconds a fresh one is let in: %v, an ordinary one: %v; want true, false", fresh, ordinary)
	}
}

func TestBudget_OnlyAWaitingFreshSearchHoldsTheOthersBack(t *testing.T) {
	// A lookup is never asked for a fresh session; one marked so all the same
	// waits behind the searches as any lookup does, and holds none of them
	// back however long it waits.
	b, clock := clocked(10, 5)
	setAmp(b, 1)
	take(t, b, ClassSearch, 5)
	waiting(FreshSession(t.Context()), t, b, ClassLookup, "a lookup marked fresh at 5 of 10")
	clock.advance(time.Minute)
	if u := ordinaryNow(t, b); u == nil {
		t.Error("an ordinary search was held back behind a waiting lookup")
	} else {
		u.Release()
	}
}

func TestBudget_AFreshSearchThatCannotFitBesideWhatIsNotThisProgramsHoldsNoOneBack(t *testing.T) {
	// Thirty with a reserve of five; the reading found 27 on the profile's
	// ports and nothing of this program's in flight — another process on the
	// profile, or connections the service keeps. An ordinary search fits
	// (27 + 1), a fresh one never can (27 + 5) until that load goes, and
	// holding the ordinary ones back for it would stand the whole job still.
	b, clock := clocked(30, 5)
	setAmp(b, 1)
	b.observeLoad(27, 0, true)
	waiting(FreshSession(t.Context()), t, b, ClassSearch, "a fresh search beside 27 of 30 not this program's")
	clock.advance(time.Minute)
	if u := ordinaryNow(t, b); u == nil {
		t.Error("an ordinary search was held back behind a fresh one that cannot fit beside the excess")
	} else {
		u.Release()
	}

	// The same with five solves the service runs on its own (25 as their
	// reserves) and one connection over the prediction: 26 not this
	// program's, a fresh search needs 31.
	c, cclock := clocked(30, 5)
	setAmp(c, 1)
	readSolves(c, 26, 0, 20101, 20102, 20103, 20104, 20105)
	waiting(FreshSession(t.Context()), t, c, ClassSearch, "a fresh search beside five abandoned solves and one over")
	cclock.advance(time.Minute)
	if u := ordinaryNow(t, c); u == nil {
		t.Error("an ordinary search was held back behind a fresh one that cannot fit beside the service's own solves")
	} else {
		u.Release()
	}

	// Room for it once that load is gone (a reading of 20): it holds them.
	d, dclock := clocked(30, 5)
	setAmp(d, 1)
	d.observeLoad(20, 0, true)
	take(t, d, ClassSearch, 6)
	waiting(FreshSession(t.Context()), t, d, ClassSearch, "a fresh search beside 20 and six of this program's")
	dclock.advance(freshWaitFair)
	if u := ordinaryNow(t, d); u != nil {
		u.Release()
		t.Error("an ordinary search passed a fresh one that fits once this program's own drain")
	}
}
