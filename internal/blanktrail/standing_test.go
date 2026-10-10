// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// warmOnce takes a standing port for warming and gives it back.
func warmOnce(t *testing.T, p *Pool, clock *fakeClock) (int, bool) {
	t.Helper()
	l, ok := p.AcquireIdleHot(0)
	if !ok {
		return 0, false
	}
	num := l.Port()
	l.Release()
	clock.Advance(time.Second)
	return num, true
}

func TestPool_KeepsItsStandingPortsFromBeingClosedForIdling(t *testing.T) {
	// The service closes a port nothing has gone through for half an hour. On
	// 2026-10-10 seven of the ten standing ports of the two idle demo parsers
	// were closed that way, and their warm sessions with them: a standing port
	// is one kept for the moment nothing is using it.
	fake := fakebt.New(t)
	p, clock := ownedPool(t, fake, 2)
	p.KeepWarm()
	for i := 0; i < 2; i++ {
		if _, ok := warmOnce(t, p, clock); !ok {
			t.Fatalf("warming %d: no standing port", i)
		}
	}
	for _, num := range fake.OpenPorts() {
		if n, ok := fake.IdleOf(num); !ok || n != 0 {
			t.Errorf("standing port %d: idle timeout %d (set %v), want 0 — never closed for idling", num, n, ok)
		}
	}
}

func TestPool_OpensTheReplacementOfAStandingPortNeverToBeClosedForIdling(t *testing.T) {
	fake := fakebt.New(t)
	p, clock := ownedPool(t, fake, 1)
	p.KeepWarm()
	num := fake.OpenPorts()[0]

	restartUnder(fake, clock, num)
	for i := 0; i < 3; i++ {
		got, ok := warmOnce(t, p, clock)
		if !ok || got == num {
			continue
		}
		if n, set := fake.IdleOf(got); !set || n != 0 {
			t.Errorf("the replacement %d: idle timeout %d (set %v), want 0", got, n, set)
		}
		return
	}
	t.Fatal("no replacement standing port was warmed")
}

func TestPool_WarmsAStandingPortTheServiceClosedForIdlingByOpeningItAgain(t *testing.T) {
	// Warming took standing ports past the acquire that opens a lost one again,
	// so a port the service had closed was warmed dead for hours: 381 warmings
	// on the two demo parsers ended in EOF between 02:45 and 11:03.
	fake := fakebt.New(t)
	p, clock := ownedPool(t, fake, 1)
	p.KeepWarm()
	num := fake.OpenPorts()[0]
	// Warmed once before: the service was told then never to close it, and
	// what it is opened again with has to say so by itself.
	if _, ok := warmOnce(t, p, clock); !ok {
		t.Fatal("no standing port to warm")
	}

	fake.CloseIdle(num)
	clock.Advance(ownFresh + time.Second)
	got, ok := warmOnce(t, p, clock)
	if !ok {
		t.Fatal("no standing port to warm after the service closed it")
	}
	if got != num || !slices.Contains(fake.OpenPorts(), num) {
		t.Errorf("warmed %d with the service holding %v, want %d opened again", got, fake.OpenPorts(), num)
	}
	if !strings.HasPrefix(fake.LabelOf(num), "gserp-") {
		t.Errorf("the port opened again carries label %q, want this run's", fake.LabelOf(num))
	}
	if n, set := fake.IdleOf(num); !set || n != 0 {
		t.Errorf("the port opened again: idle timeout %d (set %v), want 0", n, set)
	}
}

func TestClient_TellsTheServiceNeverToCloseAPortForIdling(t *testing.T) {
	fake := fakebt.New(t)
	p, _ := ownedPool(t, fake, 1)
	num := fake.OpenPorts()[0]
	if err := p.cl.NeverIdle(context.Background(), num); err != nil {
		t.Fatalf("NeverIdle: %v", err)
	}
	if n, set := fake.IdleOf(num); !set || n != 0 {
		t.Errorf("idle timeout %d (set %v), want 0", n, set)
	}
}
