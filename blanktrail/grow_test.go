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

func TestTake_OffersTheIdentityThatHasRestedLongest(t *testing.T) {
	// The one rule a lease follows, and the reason it is the only one.
	//
	// It used to prefer an identity that had answered before. That is right about
	// one request and wrong about a pool: the few that answer take every lease,
	// cool down, and are asked again the moment they may be, while the others are
	// never tried. Traced on a live run, twenty-one ports of forty carried every
	// request and nineteen sat untouched for minutes — a hundred identities
	// working like twenty.
	f := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, f, clock, 1, 3)
	cfg.Cooldown = time.Minute
	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("opening three ports: %v", err)
	}
	defer func() { _ = pool.Close() }()

	// One of them answers, and is therefore the most recently used. Under the old
	// rule it would be handed out again at once; under this one it is last.
	answered := leaseNum(t, pool, pool.ports[0].num)
	answered.Answered()
	answered.Release()
	clock.Advance(2 * time.Minute)

	// Every port is ready now, and the one that answered rested least.
	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()
	if lease.Port() == answered.Port() {
		t.Errorf("the lease went straight back to the identity that had just answered; " +
			"the other two had rested longer and neither was tried")
	}
}

func TestTake_SpreadsOverEveryIdentityRatherThanFavouringAFew(t *testing.T) {
	// The whole pool, not a corner of it. Six identities and twelve leases: each
	// should carry two, because after every turn the one that has rested longest
	// is a different one.
	f := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, f, clock, 1, 6)
	cfg.Cooldown = time.Second
	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("opening six ports: %v", err)
	}
	defer func() { _ = pool.Close() }()

	used := map[int]int{}
	for range 12 {
		lease, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		// Some answer and some do not, which under the old rule was the whole
		// difference and under this one is none of it.
		if len(used)%2 == 0 {
			lease.Answered()
		}
		used[lease.Port()]++
		lease.Release()
		clock.Advance(2 * time.Second)
	}

	if len(used) != 6 {
		t.Errorf("%d of six identities carried anything: %v", len(used), used)
	}
	for port, n := range used {
		if n != 2 {
			t.Errorf("identity %d carried %d of twelve leases, want the even two", port, n)
		}
	}
}

func TestStats_CountTheIdentitiesThatHaveAnswered(t *testing.T) {
	// Twelve kept and two warm is a set worth almost nothing yet, and a screen
	// that can only say "twelve" cannot tell anybody that.
	f := fakebt.New(t)
	clock := newFakeClock()
	pool, err := NewPool(context.Background(), testPoolConfig(t, f, clock, 1, 3))
	if err != nil {
		t.Fatalf("opening three ports: %v", err)
	}
	defer func() { _ = pool.Close() }()

	if got := pool.Stats().Warm; got != 0 {
		t.Errorf("%d of three ports are warm before any of them has answered, want none", got)
	}
	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	lease.Answered()
	lease.Release()
	if got := pool.Stats().Warm; got != 1 {
		t.Errorf("%d ports are warm after one answered, want one", got)
	}
}

func TestAcquireIdleHot_WarmsThePortThatHasRestedLongest(t *testing.T) {
	// The same rule a lease follows, for the same reason: warming that returned
	// to whichever port it found interesting would leave the rest of the set
	// untouched. The one that has waited longest is also the one closest to
	// being forgotten by the far end, which is what warming is against.
	f := fakebt.New(t)
	clock := newFakeClock()
	pool, err := NewPool(context.Background(), testPoolConfig(t, f, clock, 1, 2))
	if err != nil {
		t.Fatalf("opening two ports: %v", err)
	}
	defer func() { _ = pool.Close() }()
	pool.KeepWarm()

	// The first port in the list is the one that has answered, so a warmer that
	// simply walks the list would take it.
	pool.mu.Lock()
	first := pool.ports[0].num
	pool.mu.Unlock()
	answered := leaseNum(t, pool, first)
	answered.Answered()
	answered.Release()

	got, ok := pool.AcquireIdleHot(0)
	if !ok {
		t.Fatal("nothing was offered for warming, and both ports are idle")
	}
	defer got.Release()
	if got.Port() == first {
		t.Errorf("warming went to port %d, which was used a moment ago, while the other "+
			"had rested longer", got.Port())
	}
}

// hotAndGrown names the standing port and the one a Grow added, for a pool of
// exactly one of each.
func hotAndGrown(t *testing.T, p *Pool) (standing, grown int) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pt := range p.ports {
		if pt.hot {
			standing = pt.num
			continue
		}
		grown = pt.num
	}
	if standing == 0 || grown == 0 {
		t.Fatalf("the fixture holds standing %d and grown %d, want one of each", standing, grown)
	}
	return standing, grown
}

// leaseNum takes leases until the wanted port comes up, holding the others so
// none of them can come up twice, and gives those back before it returns.
func leaseNum(t *testing.T, p *Pool, num int) *Lease {
	t.Helper()
	var held []*Lease
	defer func() {
		for _, l := range held {
			l.Release()
		}
	}()
	p.mu.Lock()
	tries := len(p.ports)
	p.mu.Unlock()
	for i := 0; i < tries; i++ {
		l, err := p.Acquire(context.Background())
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if l.Port() == num {
			return l
		}
		held = append(held, l)
	}
	t.Fatalf("port %d never came up in %d leases", num, tries)
	return nil
}
