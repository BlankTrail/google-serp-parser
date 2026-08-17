// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// standing is a pool of hot ports over the stand-in service, the way a machine
// that keeps some open between jobs holds them.
func standing(t *testing.T, hot int) (*Pool, *fakebt.Server) {
	t.Helper()
	f := fakebt.New(t)
	pool, err := NewPool(context.Background(), testPoolConfig(t, f, newFakeClock(), hot, 1))
	if err != nil {
		t.Fatalf("opening %d standing ports: %v", hot, err)
	}
	// Said out loud, as a machine that keeps identities warm says it: a pool is
	// the standing one only because somebody declared it so, and every other pool
	// this program opens belongs to one job and is closed with it.
	if got := pool.KeepWarm(); got != hot {
		t.Fatalf("KeepWarm marked %d ports, want the %d that are open", got, hot)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool, f
}

func TestGrow_OpensTheDifferenceAndShrinkGivesBackOnlyThat(t *testing.T) {
	// The whole point of keeping ports warm: a job larger than the standing set
	// opens what it is short of, runs on everything, and gives back only what it
	// opened. Giving back the standing ports too would mean the next job pays for
	// warming them all over again, which is what keeping them was for.
	pool, f := standing(t, 10)
	if got := pool.Hot(); got != 10 {
		t.Fatalf("the standing pool holds %d hot ports, want 10", got)
	}

	if err := pool.Grow(context.Background(), 90); err != nil {
		t.Fatalf("growing to a hundred: %v", err)
	}
	if got := len(f.OpenPorts()); got != 100 {
		t.Errorf("%d ports are open, want the hundred the job asked for", got)
	}
	if got := pool.Hot(); got != 10 {
		t.Errorf("growing changed the standing set to %d, and it is ten", got)
	}

	gone, err := pool.Shrink(context.Background())
	if err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if gone != 90 {
		t.Errorf("Shrink gave back %d ports, want the ninety that were grown", gone)
	}
	if got := len(f.OpenPorts()); got != 10 {
		t.Errorf("%d ports are still open, want the ten that are kept warm", got)
	}
	if got := pool.Hot(); got != 10 {
		t.Errorf("the standing set is %d after a shrink, want ten", got)
	}
}

func TestShrink_LeavesAPoolThatWasNeverGrownAloneAndCanBeRunAgain(t *testing.T) {
	// Shrinking twice, or shrinking a job that never grew anything, must not
	// close the standing ports: it is called when a job lets go, and jobs end
	// twice more often than anybody expects.
	pool, f := standing(t, 4)
	for range 3 {
		gone, err := pool.Shrink(context.Background())
		if err != nil {
			t.Fatalf("Shrink: %v", err)
		}
		if gone != 0 {
			t.Errorf("Shrink gave back %d ports of a pool that grew none", gone)
		}
	}
	if got := len(f.OpenPorts()); got != 4 {
		t.Errorf("%d ports survived three shrinks, want the four that are kept", got)
	}
	// And it still hands out leases: a pool shrunk to its standing set is the
	// pool the next job grows again.
	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("leasing from the standing pool: %v", err)
	}
	lease.Release()
}

func TestTake_OffersAWarmPortBeforeAColdOne(t *testing.T) {
	// A warm port answers where a cold one waits on a challenge, so the run
	// starts producing at once instead of a minute in. This is what "the
	// standing ports are used first" means in the one place it can be true.
	f := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, f, clock, 1, 1)
	cfg.Cooldown = time.Minute
	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("opening one standing port: %v", err)
	}
	defer func() { _ = pool.Close() }()
	pool.KeepWarm()
	if err := pool.Grow(context.Background(), 3); err != nil {
		t.Fatalf("Grow: %v", err)
	}

	// The warm port is made the most recently used of them, which is what it is
	// on a real machine: the warmer has just been through it, and the ones the
	// job opened have never been used at all. The clock is then moved past the
	// cooldown so it is a candidate again.
	//
	// Now "the one that has rested longest" and "the warm one" are different
	// ports, and only a rule that prefers the warm one picks it. Without the
	// clock they are the same port and this test would pass on either rule.
	warmed, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("warming the standing port: %v", err)
	}
	warmedPort := warmed.Port()
	warmed.Release()
	clock.Advance(2 * time.Minute)

	pool.mu.Lock()
	hot := pool.byNum[warmedPort]
	pool.mu.Unlock()
	if hot == nil || !hot.hot {
		t.Fatalf("the fixture warmed port %d, which is not the standing one", warmedPort)
	}

	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()
	if lease.Port() != warmedPort {
		t.Errorf("the lease went to port %d, and the warm one is %d — the coldest was "+
			"preferred over the warmest", lease.Port(), warmedPort)
	}
}

func TestTake_SpreadsOntoTheColdOnesRatherThanHammeringTheWarm(t *testing.T) {
	// Preferring the warm ones must not become "always the same ten". What stops
	// it is the cooldown: a port that has just been used is not ready again until
	// it has passed, so the work spreads by itself — and the cold ports warm up
	// as it does.
	f := fakebt.New(t)
	cfg := testPoolConfig(t, f, newFakeClock(), 1, 1)
	cfg.Cooldown = time.Hour
	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("opening one standing port: %v", err)
	}
	defer func() { _ = pool.Close() }()
	pool.KeepWarm()
	if err := pool.Grow(context.Background(), 2); err != nil {
		t.Fatalf("Grow: %v", err)
	}

	// Three leases in a row, each released before the next. With an hour's
	// cooldown the warm one can only answer the first.
	seen := map[int]bool{}
	for range 3 {
		lease, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		seen[lease.Port()] = true
		lease.Release()
	}
	if len(seen) != 3 {
		t.Errorf("three leases went to %d ports, want all three: the warm one was hammered", len(seen))
	}
}

func TestReduceTo_ClosesTheNewestAndKeepsTheOnesThatHaveBeenWarmLongest(t *testing.T) {
	// Lowering how many identities a machine keeps warm has to close some, and
	// which ones matters: the earliest have been warm longest, and warmth is the
	// whole reason any of them are open.
	pool, f := standing(t, 2)
	if err := pool.Grow(context.Background(), 4); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	pool.KeepWarm()

	pool.mu.Lock()
	oldest := pool.ports[0].num
	pool.mu.Unlock()

	gone, err := pool.ReduceTo(context.Background(), 3)
	if err != nil {
		t.Fatalf("ReduceTo: %v", err)
	}
	if gone != 3 {
		t.Errorf("%d ports were closed, want the three above the number asked for", gone)
	}
	if got := len(f.OpenPorts()); got != 3 {
		t.Errorf("%d ports are open, want the three that were kept", got)
	}
	if got := pool.Hot(); got != 3 {
		t.Errorf("the standing set reads as %d, want the three that are left", got)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.ports[0].num != oldest {
		t.Errorf("the port kept first is %d, and the one open longest is %d",
			pool.ports[0].num, oldest)
	}
}

func TestReduceTo_LeavesAlonePortsSomebodyIsHolding(t *testing.T) {
	// A number lowered while a job runs must take effect as the job lets go
	// rather than by closing a port out from under it: what is on the other end
	// of a leased port is a request somebody is waiting for.
	pool, f := standing(t, 3)

	held, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	if _, err := pool.ReduceTo(context.Background(), 0); err != nil {
		t.Fatalf("ReduceTo: %v", err)
	}
	open := f.OpenPorts()
	if len(open) != 1 || open[0] != held.Port() {
		t.Errorf("the ports left open are %v, want only the one being held (%d)", open, held.Port())
	}
}
