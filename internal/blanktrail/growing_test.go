// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// growingOver is a set over a fake service, with the clock in the test's hand.
func growingOver(t *testing.T, f *fakebt.Server, most int) (*Growing, *int) {
	t.Helper()
	cl, err := NewClient(f.URL(), f.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	opens := 0
	g := NewGrowing(most, func(ctx context.Context, ports int) (*Pool, error) {
		opens++
		return NewPool(ctx, PoolConfig{
			Client: cl, Threads: ports, PortsPerThread: 1,
			Spec:     DefaultPortSpec(),
			Channels: []Channel{NewDirectChannel("direct")},
			Insecure: true, Cooldown: time.Nanosecond,
			Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		})
	})
	g.gap = 0
	t.Cleanup(func() { _ = g.Close() })
	return g, &opens
}

func TestGrowing_OpensNothingUntilSomethingAsks(t *testing.T) {
	// The whole reason this exists. Whether a job needs these ports at all
	// depends on what Google answers with, and most regions state the address
	// in the markup — where reading it costs no request. A set opened before
	// anybody asked is, on those regions, ports standing idle for the length of
	// the run, taken out of a tariff that holds a fixed number for everything.
	f := fakebt.New(t)
	g, opens := growingOver(t, f, 8)

	if got := len(f.OpenPorts()); got != 0 {
		t.Errorf("%d ports are open before anything asked for one", got)
	}
	if g.Opened() != nil {
		t.Error("a pool exists before anything asked for one")
	}
	if *opens != 0 {
		t.Errorf("the set was opened %d times before anything asked", *opens)
	}
}

func TestGrowing_OpensOnePortWhenTheFirstAddressHasToBeRead(t *testing.T) {
	// One, and not the ceiling: whether a second is wanted is decided by
	// whether anybody ends up waiting for the first, which is the only honest
	// measure of it.
	f := fakebt.New(t)
	g, opens := growingOver(t, f, 8)

	pool, err := g.Identities(t.Context())
	if err != nil {
		t.Fatalf("Identities: %v", err)
	}
	if got := pool.Size(); got != 1 {
		t.Errorf("the set opened %d ports on the first ask, want one", got)
	}
	if got := len(f.OpenPorts()); got != 1 {
		t.Errorf("the service holds %d ports, want the one", got)
	}
	if *opens != 1 {
		t.Errorf("the set was opened %d times, want once", *opens)
	}

	// And asking again is asking for the same set, not another one.
	again, err := g.Identities(t.Context())
	if err != nil {
		t.Fatalf("Identities again: %v", err)
	}
	if again != pool {
		t.Error("a second ask opened a second set")
	}
	if *opens != 1 {
		t.Errorf("the set was opened %d times over two asks, want once", *opens)
	}
}

func TestGrowing_TakesAnotherPortOnlyWhileSomebodyIsQueueingForOne(t *testing.T) {
	// Demand, and nothing else, decides the size. A set that widened because it
	// was asked would climb to its ceiling on a region that hides one address
	// in a hundred, which is the waste this exists to avoid.
	f := fakebt.New(t)
	g, _ := growingOver(t, f, 4)

	pool, err := g.Identities(t.Context())
	if err != nil {
		t.Fatalf("Identities: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := g.Identities(t.Context()); err != nil {
			t.Fatalf("Identities: %v", err)
		}
	}
	if got := pool.Size(); got != 1 {
		t.Errorf("the set grew to %d ports with nobody waiting, want the one it opened", got)
	}

	// Somebody is waiting now.
	pool.waiting(1)
	defer pool.waiting(-1)
	for i := 0; i < 3; i++ {
		if _, err := g.Identities(t.Context()); err != nil {
			t.Fatalf("Identities: %v", err)
		}
	}
	if got := pool.Size(); got != 4 {
		t.Errorf("the set grew to %d ports over three asks with somebody waiting, want the 4 it is allowed", got)
	}

	// And it stops at the ceiling.
	for i := 0; i < 3; i++ {
		if _, err := g.Identities(t.Context()); err != nil {
			t.Fatalf("Identities: %v", err)
		}
	}
	if got := pool.Size(); got != 4 {
		t.Errorf("the set grew past its ceiling to %d ports", got)
	}
}

func TestGrowing_LeavesAGapBetweenTwoWidenings(t *testing.T) {
	// Every thread of a run asks this on every query. A set that widened on
	// each of those would spend its first second making a hundred calls to the
	// control service to arrive where two would have put it.
	f := fakebt.New(t)
	g, _ := growingOver(t, f, 8)
	g.gap = time.Minute
	at := time.Unix(0, 0)
	g.now = func() time.Time { return at }

	pool, err := g.Identities(t.Context())
	if err != nil {
		t.Fatalf("Identities: %v", err)
	}
	pool.waiting(1)
	defer pool.waiting(-1)

	for i := 0; i < 10; i++ {
		if _, err := g.Identities(t.Context()); err != nil {
			t.Fatalf("Identities: %v", err)
		}
	}
	if got := pool.Size(); got != 1 {
		t.Errorf("the set grew to %d ports inside one gap, want the one it opened", got)
	}

	at = at.Add(time.Minute)
	if _, err := g.Identities(t.Context()); err != nil {
		t.Fatalf("Identities: %v", err)
	}
	if got := pool.Size(); got != 2 {
		t.Errorf("the set holds %d ports once the gap is up, want 2", got)
	}
}

func TestGrowing_KeepsTheReasonItCouldNotOpenRatherThanAskingAgainForEver(t *testing.T) {
	// The reasons a pool will not open are the service being unreachable, a
	// licence, or no port left on the machine. None of them is answered by
	// asking again a hundred times a second, which is what every thread of a
	// run would do.
	refused := errors.New("the service would not open a port")
	tries := 0
	g := NewGrowing(4, func(context.Context, int) (*Pool, error) {
		tries++
		return nil, refused
	})

	for i := 0; i < 5; i++ {
		if _, err := g.Identities(t.Context()); !errors.Is(err, refused) {
			t.Fatalf("Identities gave %v, want the refusal", err)
		}
	}
	if tries != 1 {
		t.Errorf("the set was asked to open %d times, want the once", tries)
	}
}

func TestGrowing_ClosesNothingItNeverOpened(t *testing.T) {
	f := fakebt.New(t)
	g, _ := growingOver(t, f, 4)
	if err := g.Close(); err != nil {
		t.Errorf("closing a set nothing asked for: %v", err)
	}
	if got := len(f.OpenPorts()); got != 0 {
		t.Errorf("%d ports are open after closing a set nothing asked for", got)
	}
}
