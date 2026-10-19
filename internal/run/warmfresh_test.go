// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
)

// The warmer's searches on a profile with a limit go out as fresh sessions':
// the sessions it warms have rested for hours and are the likeliest to be
// checked, so each is counted as the solve it may turn into.

func TestWarmer_CountsAWarmingOnALimitedProfileAsAPossibleSolve(t *testing.T) {
	for _, path := range []string{"running out", "coldest"} {
		b := blanktrail.NewBudget(30, 5)
		o := watchBudget(t, b)
		f := poolFacing(t, o.addr(), 1, inSessions, func(c *blanktrail.PoolConfig) { c.Budget = b })
		f.Pool.KeepWarm()
		k := sessions.NewKeeper(sessions.NewMemory())
		keptSessions(t, f, k, 1)
		time.Sleep(50 * time.Millisecond)
		w := &Warmer{Pool: f.Pool, Keeper: k, Want: sessions.Want{Device: "desktop"},
			warmAtOnce: 1, spacing: time.Millisecond}
		if path == "running out" {
			w.warmAfter = time.Nanosecond
		} else {
			w.warmAfter = time.Hour
		}
		w.oneRound(context.Background(), 0)
		seen := o.readings()
		if len(seen) != 1 {
			t.Fatalf("%s: the origin saw %d warming searches, want 1", path, len(seen))
		}
		if seen[0].Fresh != 1 || seen[0].InFlight != 0 {
			t.Errorf("%s: the warming went out with the budget at %+v, want one fresh attempt", path, seen[0])
		}
		if r := b.Reading(); r.Fresh != 0 || r.InFlight != 0 {
			t.Errorf("%s: after the warming the budget reads %+v, want nothing", path, r)
		}
	}
}

func TestWarmingAs_LeavesTheContextAloneWithoutALimit(t *testing.T) {
	o := watchBudget(t, blanktrail.NewBudget(0, 5))
	f := poolFacing(t, o.addr(), 1, inSessions, func(c *blanktrail.PoolConfig) { c.Budget = blanktrail.NewBudget(0, 5) })
	lease, err := f.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()
	ctx := context.Background()
	if got := warmingAs(ctx, lease); got != ctx {
		t.Error("a warming on a profile with no limit was given a context of its own")
	}
}
