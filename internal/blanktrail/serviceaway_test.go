// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// awayPool is a pool of sessions on one port, with the clock in the test's hand.
func awayPool(t *testing.T, fake *fakebt.Server) (*Pool, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(sessionAddrs, WithRest(time.Hour)))}
	cfg.Sessions = true
	cfg.ReviveAfter = time.Minute
	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, clock
}

func TestPool_OpensItsPortsAgainWhenTheServiceComesBackWithoutQuarantiningThem(t *testing.T) {
	// A restart of the service — an update is one — takes every port with it,
	// and the service answers nothing while it starts. Every attempt to open a
	// port meanwhile fails, and none of it is the port's doing: counted against
	// the ports, a restart of a minute quarantined the whole pool.
	fake := fakebt.New(t)
	p, clock := awayPool(t, fake)
	ctx := context.Background()
	l, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	num := l.Port()
	l.Release()

	fake.Restart()
	fake.SetDown(true)
	p.reopenPort(num) // what a request through the port finds: nothing listening
	for i := 0; i < 10; i++ {
		if got, err := p.TryAcquire(ctx); err == nil {
			got.Release()
			t.Fatal("a port was handed out while the service could not open it")
		}
		clock.Advance(3 * time.Second)
	}
	if q := p.Stats().Quarantines; q != 0 {
		t.Errorf("%d ports were quarantined while the service was away", q)
	}

	fake.SetDown(false)
	clock.Advance(3 * time.Second)
	back, err := p.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("the service is back and the port was not opened again: %v", err)
	}
	defer back.Release()
	if !slices.Contains(fake.OpenPorts(), back.Port()) {
		t.Errorf("port %d was handed out and the service does not have it open", back.Port())
	}
}

func TestPool_DoesNotAskAServiceThatIsAwayAgainAtOnce(t *testing.T) {
	// A pool of sessions keeps no pause on a port, so a port the service could
	// not open would be asked for again in a tight loop for as long as the
	// service stayed away.
	fake := fakebt.New(t)
	p, clock := awayPool(t, fake)
	ctx := context.Background()
	l, _ := p.Acquire(ctx)
	num := l.Port()
	l.Release()
	fake.SetDown(true)
	p.reopenPort(num)

	_, _ = p.TryAcquire(ctx)
	asked := len(fake.Requests())
	_, _ = p.TryAcquire(ctx)
	if again := len(fake.Requests()) - asked; again != 0 {
		t.Errorf("the service was asked %d more times at the same instant", again)
	}
	clock.Advance(3 * time.Second)
	_, _ = p.TryAcquire(ctx)
	if len(fake.Requests()) == asked {
		t.Error("the port was never tried again once its pause was over")
	}
}

func TestPool_SpendsNoRevivalOnAServiceThatIsAway(t *testing.T) {
	// A quarantined port is offered a way back a few times and then given up.
	// An offer the service was not there to take is not one of them.
	fake := fakebt.New(t)
	p, clock := awayPool(t, fake)
	ctx := context.Background()
	l, _ := p.Acquire(ctx)
	num := l.Port()
	l.Release()
	pt := p.port(num)
	pt.mu.Lock()
	pt.quarantined, pt.quarantinedAt = true, clock.Now()
	pt.mu.Unlock()

	fake.SetDown(true)
	for i := 0; i < 5; i++ {
		clock.Advance(2 * time.Minute)
		if got, err := p.TryAcquire(ctx); err == nil {
			got.Release()
		}
	}
	pt.mu.Lock()
	revivals := pt.revivals
	pt.mu.Unlock()
	if revivals != 0 {
		t.Errorf("%d revivals were spent while the service was away", revivals)
	}
}
