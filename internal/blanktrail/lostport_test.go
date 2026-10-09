// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// lookupPool is one port opened as the hidden addresses' ports are: a list, no
// sessions, nothing kept between two requests.
func lookupPool(t *testing.T, fake *fakebt.Server) (*Pool, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(sessionAddrs))}
	cfg.Cooldown = time.Millisecond
	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, clock
}

func TestPool_OpensAgainAPortTheServiceLostRatherThanQuarantiningIt(t *testing.T) {
	// A port the service lost to a restart answers every move with "not open".
	// Counted as the address failing, each answer was a strike: the port went
	// into quarantine, every revival asked the service to move it again and
	// heard the same, and the lookup ports of the position check of 2026-10-09
	// were all quarantined for good after one restart of the service.
	fake := fakebt.New(t)
	p, clock := lookupPool(t, fake)
	ctx := context.Background()

	fake.Restart()
	for i := 0; i < 20; i++ {
		l, err := p.TryAcquire(ctx)
		if err != nil {
			t.Fatalf("rejection %d: no port to hand out: %v", i, err)
		}
		_ = l.Reject(ctx)
		l.Release()
		clock.Advance(time.Second)
	}
	if q := p.Stats().Quarantines; q != 0 {
		t.Errorf("%d ports were quarantined for a port the service had lost", q)
	}
	l, err := p.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("the lost port was not handed out again: %v", err)
	}
	defer l.Release()
	if !slices.Contains(fake.OpenPorts(), l.Port()) {
		t.Errorf("port %d was handed out and the service does not hold it", l.Port())
	}
}

func TestLease_RenewsAPortTheServiceLostByOpeningItAgain(t *testing.T) {
	// A lookup port is renewed once its lookups are done; renewed after a
	// restart, there is nothing to move, and the port is to be opened again
	// rather than reported as failing.
	fake := fakebt.New(t)
	p, clock := lookupPool(t, fake)
	ctx := context.Background()

	l, err := p.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	fake.Restart()
	if err := l.Renew(ctx); err != nil {
		t.Errorf("renewing a port the service lost: %v", err)
	}
	l.Release()
	clock.Advance(time.Second)

	back, err := p.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("the renewed port was not handed out again: %v", err)
	}
	defer back.Release()
	if !slices.Contains(fake.OpenPorts(), back.Port()) {
		t.Errorf("port %d was handed out and the service does not hold it", back.Port())
	}
}
