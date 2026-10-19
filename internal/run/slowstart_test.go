// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"sync"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
)

// The slow start from the run's side: a request for a session Google has never
// answered goes out marked (blanktrail.FreshSession), and the profile's budget
// counts its attempt as a possible solve; a session that has answered goes out
// as before.

// budgetWatch is an origin that writes down the profile's budget as each search
// arrives, while the request is in flight.
type budgetWatch struct {
	*cookieOrigin
	mu   sync.Mutex
	seen []blanktrail.BudgetReading
}

func watchBudget(t *testing.T, b *blanktrail.Budget) *budgetWatch {
	t.Helper()
	w := &budgetWatch{}
	w.cookieOrigin = newCookieOrigin(t, func(int) string {
		r := b.Reading()
		w.mu.Lock()
		w.seen = append(w.seen, r)
		w.mu.Unlock()
		return serpBody("example.com")
	})
	return w
}

func (w *budgetWatch) readings() []blanktrail.BudgetReading {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]blanktrail.BudgetReading(nil), w.seen...)
}

func TestAttempt_CountsAFreshSessionsSearchAsAPossibleSolveInTheBudget(t *testing.T) {
	// The first search makes a session and goes out as a fresh one's; the
	// second takes the same session, which Google has answered now, and goes
	// out as an ordinary attempt.
	b := blanktrail.NewBudget(30, 5)
	o := watchBudget(t, b)
	f := poolFacing(t, o.addr(), 1, inSessions, func(c *blanktrail.PoolConfig) { c.Budget = b })
	a := &Attempt{Pool: f.Pool, Keeper: sessions.NewKeeper(sessions.NewMemory()), Want: searchDesktop}
	for i := 0; i < 2; i++ {
		if _, err := a.Search(context.Background(), usQuery("x")); err != nil {
			t.Fatalf("Search %d: %v", i, err)
		}
	}
	seen := o.readings()
	if len(seen) != 2 {
		t.Fatalf("the origin saw %d searches, want 2", len(seen))
	}
	if seen[0].Fresh != 1 || seen[0].InFlight != 0 {
		t.Errorf("a new session's search went out with the budget at %+v, want one fresh attempt", seen[0])
	}
	if seen[1].Fresh != 0 || seen[1].InFlight != 1 {
		t.Errorf("an answered session's search went out with the budget at %+v, want one ordinary attempt", seen[1])
	}
	if r := b.Reading(); r.Fresh != 0 || r.InFlight != 0 {
		t.Errorf("after both searches the budget reads %+v, want nothing", r)
	}
}

func TestBoundSearcher_MarksTheRequestsOfASessionGoogleHasNotAnsweredYet(t *testing.T) {
	// Both doors a walk's page goes out by — the query's first page and a page
	// at the address the one before it carried — until the session is written
	// down as answered, and neither after.
	b := blanktrail.NewBudget(30, 5)
	o := watchBudget(t, b)
	f := poolFacing(t, o.addr(), 1, inSessions, func(c *blanktrail.PoolConfig) { c.Budget = b })
	keeper := sessions.NewKeeper(sessions.NewMemory())
	a := &Attempt{Pool: f.Pool, Keeper: keeper, Want: searchDesktop}
	ctx := context.Background()
	lease, err := f.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()
	held, err := keeper.Take(ctx, leasePort{lease}, searchDesktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	search := boundSearcher{attempt: a, lease: lease, held: held}
	const next = "https://www.google.com/search?q=x&start=10"
	if _, err := search.Search(ctx, usQuery("x")); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if _, err := search.SearchAt(ctx, usQuery("x"), next); err != nil {
		t.Fatalf("SearchAt: %v", err)
	}
	if err := held.Save(ctx, leasePort{lease}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := search.SearchAt(ctx, usQuery("x"), next); err != nil {
		t.Fatalf("SearchAt after the answer: %v", err)
	}
	if _, err := search.Search(ctx, usQuery("x")); err != nil {
		t.Fatalf("Search after the answer: %v", err)
	}
	seen := o.readings()
	if len(seen) != 4 {
		t.Fatalf("the origin saw %d searches, want 4", len(seen))
	}
	for i, want := range []bool{true, true, false, false} {
		if fresh := seen[i].Fresh == 1 && seen[i].InFlight == 0; fresh != want {
			t.Errorf("search %d went out with the budget at %+v, want counted as fresh: %v", i+1, seen[i], want)
		}
	}
}

func TestAskingAs_LeavesTheContextAloneWithoutALimitOrASessionToTellApart(t *testing.T) {
	// No limit: the context goes out as it came, with nothing made for it —
	// not even for a session Google has never seen.
	o := newCookieOrigin(t, nil)
	for _, b := range []*blanktrail.Budget{nil, blanktrail.NewBudget(0, 5)} {
		f := poolFacing(t, o.addr(), 1, inSessions, func(c *blanktrail.PoolConfig) { c.Budget = b })
		keeper := sessions.NewKeeper(sessions.NewMemory())
		ctx := context.Background()
		lease, err := f.Pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		held, err := keeper.Take(ctx, leasePort{lease}, searchDesktop)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		if got := askingAs(ctx, lease, held); got != ctx {
			t.Errorf("with budget %v a fresh session's context was replaced", b)
		}
		held.PutBack()
		lease.Release()
	}
	// A limit, but no session: the port's own identity is not told apart.
	f := poolFacing(t, o.addr(), 1, func(c *blanktrail.PoolConfig) { c.Budget = blanktrail.NewBudget(30, 5) })
	lease, err := f.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()
	if ctx := context.Background(); askingAs(ctx, lease, nil) != ctx {
		t.Error("a request with no held session was marked as a fresh session's")
	}
}
