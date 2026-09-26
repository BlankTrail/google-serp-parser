// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// threePortsAt opens three ports over a fake service, with the clock at *at.
func threePortsAt(t *testing.T, f *fakebt.Server, at *time.Time, warmest bool) *Pool {
	t.Helper()
	cl, err := NewClient(f.URL(), f.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ups, bad := Parse("192.0.2.1:1080\n192.0.2.2:1080\n192.0.2.3:1080\n192.0.2.4:1080\n192.0.2.5:1080", "socks5")
	if len(bad) > 0 {
		t.Fatalf("Parse rejected %v", bad)
	}
	p, err := NewPool(t.Context(), PoolConfig{
		Client: cl, Threads: 3, PortsPerThread: 1, Spec: DefaultPortSpec(),
		Channels: []Channel{NewListChannel("list", NewStaticRotor(ups))},
		Insecure: true, Cooldown: time.Nanosecond, MaxPerUpstream: 1,
		Now:     func() time.Time { return *at },
		Sleep:   func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		Warmest: warmest,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestPool_HandsOutTheWarmestPortWhenToldTo(t *testing.T) {
	// The ports the hidden addresses are read through carry nothing between
	// two requests, so resting is worth nothing to them and being warm is worth
	// most of the request: a port asked rarely pays the whole road to Google
	// again every time. Told so, the pool hands out the free port used last;
	// otherwise, as ever, the one that has rested longest.
	for _, c := range []struct {
		warmest bool
		which   int // of the three used in order, the one handed out next
	}{{false, 0}, {true, 2}} {
		f := fakebt.New(t)
		at := time.Unix(1000, 0)
		p := threePortsAt(t, f, &at, c.warmest)

		var used []*Lease
		for i := 0; i < 3; i++ {
			l, err := p.TryAcquire(t.Context())
			if err != nil {
				t.Fatalf("take %d: %v", i+1, err)
			}
			used = append(used, l)
		}
		for _, l := range used {
			at = at.Add(time.Second)
			l.Release()
		}
		// Past the pause a port takes after a use, so every one of them is free.
		at = at.Add(time.Second)
		next, err := p.TryAcquire(t.Context())
		if err != nil {
			t.Fatalf("take after: %v", err)
		}
		if next.Port() != used[c.which].Port() {
			t.Errorf("warmest=%v handed out port %d, want %d (the %d-th used)", c.warmest, next.Port(), used[c.which].Port(), c.which+1)
		}
		next.Release()
	}
}

func TestLease_RenewGivesThePortAnotherAddressAndAnotherFingerprint(t *testing.T) {
	// What a lookup port gets after a failure and every so often besides, in
	// place of any rest: both things a far end could know it by, changed.
	f := fakebt.New(t)
	at := time.Unix(1000, 0)
	p := threePortsAt(t, f, &at, true)

	l, err := p.TryAcquire(t.Context())
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	defer l.Release()
	before := f.UpstreamOf(l.Port())
	rotated := func() int {
		n := 0
		for _, r := range f.Requests() {
			if r.Method == "POST" && strings.HasSuffix(r.Path, "/rotate") && strings.Contains(r.Path, "/"+strconv.Itoa(l.Port())+"/") {
				n++
			}
		}
		return n
	}
	fingerprints := rotated()

	if err := l.Renew(t.Context()); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if after := f.UpstreamOf(l.Port()); after == before {
		t.Error("the port stands on the address it stood on before")
	}
	if got := rotated() - fingerprints; got != 1 {
		t.Errorf("the port was given %d new fingerprints, want one", got)
	}
	if st := p.Stats(); st.EgressRotations != 1 || st.ProfileRotations != 1 {
		t.Errorf("the pool counts %d address changes and %d fingerprint changes, want one of each",
			st.EgressRotations, st.ProfileRotations)
	}
}

func TestLease_RefreshKeepsTheAddressAndTellsTheServiceToDropWhatItHolds(t *testing.T) {
	// The remedy for the service's TLS defect: the address is alive, and what
	// fails is the ticket the port keeps. Setting the port on the address it
	// already stands on is how the service is told to drop its tickets and its
	// pooled connections.
	f := fakebt.New(t)
	at := time.Unix(1000, 0)
	p := threePortsAt(t, f, &at, true)

	l, err := p.TryAcquire(t.Context())
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	defer l.Release()
	before := f.UpstreamOf(l.Port())
	puts := func() int {
		n := 0
		for _, r := range f.Requests() {
			if r.Method == "PUT" && strings.HasSuffix(r.Path, "/"+strconv.Itoa(l.Port())+"/upstream") {
				n++
			}
		}
		return n
	}
	was := puts()
	if err := l.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := puts() - was; got != 1 {
		t.Errorf("the service was told %d times, want once", got)
	}
	if after := f.UpstreamOf(l.Port()); after != before {
		t.Error("the port was moved to another address")
	}
	if st := p.Stats(); st.EgressRotations != 0 || st.ProfileRotations != 0 {
		t.Errorf("the pool counts %d address changes and %d fingerprint changes, want none", st.EgressRotations, st.ProfileRotations)
	}
}
