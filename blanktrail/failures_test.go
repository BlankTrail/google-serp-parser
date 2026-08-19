// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

func TestFailureOf_TellsTheKindsApart(t *testing.T) {
	// "The run had errors" is not a fact anybody can act on. A dead address
	// wants another address, a refused relay wants a setting changed, a wall
	// wants a different identity and no amount of rotating will help it, and our
	// own clock running out is this program's own patience. Added together they
	// all read the same, which is why they are not.
	for _, tc := range []struct {
		name   string
		err    error
		status int
		want   Failure
	}{
		{"connection reset", errors.New("read tcp: connection was forcibly closed"), 0, FailureTransport},
		{"nothing came back", errors.New("EOF"), 0, FailureTransport},
		{"our own deadline", context.DeadlineExceeded, 0, FailureTimeout},
		{"the caller gave up", context.Canceled, 0, FailureTimeout},
		{"a timeout in words", errors.New("net/http: request canceled (Client.Timeout exceeded)"), 0, FailureTimeout},
		{"upstream terminates TLS", nil, 526, FailureRelay},
		{"rate limited", nil, 429, FailureWall},
		{"sent to the challenge", nil, 302, FailureWall},
		{"something else", nil, 503, FailureOther},
	} {
		if got := failureOf(tc.err, tc.status); got != tc.want {
			t.Errorf("%s: named %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPoolResetStats_ClearsTheCountsAndLeavesTheStateAlone(t *testing.T) {
	// A reading has to be startable from a known point without restarting
	// anything. What it must not do is clear the state: an address resting off a
	// failure goes on resting, because that is not a count of what happened but
	// what is true now — and putting the pool back into addresses it has already
	// found dead would make the next reading worse than the one before it.
	f := fakebt.New(t)
	clock := newFakeClock()
	p, err := NewPool(context.Background(), testPoolConfig(t, f, clock, 1, 2))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	p.failed(FailureTransport)
	p.failed(FailureTransport)
	p.failed(FailureWall)
	p.mu.Lock()
	p.stats.Requests = 7
	p.stats.EgressRotations = 3
	p.mu.Unlock()

	st := p.Stats()
	if st.Failures[FailureTransport] != 2 || st.Failures[FailureWall] != 1 {
		t.Fatalf("failures read %v, want two transport and one wall", st.Failures)
	}

	// The snapshot is a copy: a screen holding one while the pool goes on
	// counting is two goroutines on one map.
	st.Failures[FailureTransport] = 99
	if again := p.Stats(); again.Failures[FailureTransport] != 2 {
		t.Errorf("the pool's own count changed to %d when a caller wrote to its snapshot",
			again.Failures[FailureTransport])
	}

	quarantined := p.ports[0]
	quarantined.mu.Lock()
	quarantined.quarantined = true
	quarantined.mu.Unlock()

	p.ResetStats()
	after := p.Stats()
	if len(after.Failures) != 0 {
		t.Errorf("failures read %v after a reset, want none", after.Failures)
	}
	if after.Requests != 0 || after.EgressRotations != 0 {
		t.Errorf("requests=%d rotations=%d after a reset, want nought",
			after.Requests, after.EgressRotations)
	}
	if after.Quarantined != 1 {
		t.Errorf("%d ports are set aside after a reset, want the one that still is",
			after.Quarantined)
	}
}

func TestPoolAddresses_CountsTheListAndWhatIsRestingOffIt(t *testing.T) {
	// What a screen calls banned. A run whose resting count climbs towards its
	// total is a run about to have nothing left to try, and that is worth seeing
	// before it happens rather than after.
	f := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, f, clock, 1, 1)
	ups, bad := Parse(strings.Join([]string{
		"1.1.1.1:1", "2.2.2.2:2", "3.3.3.3:3", "4.4.4.4:4",
	}, "\n"), "socks5")
	if len(bad) > 0 {
		t.Fatalf("Parse rejected %v", bad)
	}
	rotor := NewStaticRotor(ups)
	cfg.Channels = []Channel{NewListChannel("list", rotor)}

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	total, resting := p.Addresses()
	if total != 4 {
		t.Errorf("the pool draws on %d addresses, want the four in the list", total)
	}
	if resting != 0 {
		t.Errorf("%d addresses are resting before anything failed, want none", resting)
	}

	rotor.MarkDead(ups[0])
	if total, resting = p.Addresses(); resting != 1 {
		t.Errorf("%d of %d addresses are resting after one was found dead, want one",
			resting, total)
	}
}
