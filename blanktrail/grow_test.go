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
	pool, _ := standing(t, 1)
	if err := pool.Grow(context.Background(), 3); err != nil {
		t.Fatalf("Grow: %v", err)
	}

	// Every port is ready and none has been used, so the only thing that can
	// decide between them is which is warm.
	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()

	pool.mu.Lock()
	defer pool.mu.Unlock()
	got := pool.byNum[lease.Port()]
	if got == nil || !got.hot {
		t.Errorf("the first lease went to port %d, which is not one of the warm ones", lease.Port())
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
