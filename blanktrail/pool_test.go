// SPDX-License-Identifier: MIT

package blanktrail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// fakeClock is a manually advanced clock shared by a pool's Now and Sleep.
// Sleep advances the clock instead of blocking, so cooldown tests are instant
// and deterministic.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.Advance(d)
	return nil
}

func testPoolConfig(t *testing.T, fake *fakebt.Server, clock *fakeClock, threads, perThread int) PoolConfig {
	t.Helper()
	c, err := NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return PoolConfig{
		Client:         c,
		Threads:        threads,
		PortsPerThread: perThread,
		Spec:           DefaultPortSpec(),
		Channels:       []Channel{NewDirectChannel("direct")},
		Insecure:       true, // the fake serves plain HTTP; no MITM CA involved
		DelayMin:       2 * time.Second,
		DelayMax:       4 * time.Second,
		Now:            clock.Now,
		Sleep:          clock.Sleep,
	}
}

func TestDeriveCooldown_MatchesTheRingItReplaces(t *testing.T) {
	// Ten ports per thread with a 3–8 s delay is the worked example from the
	// spec: the port must not come back sooner than a ten-port ring would give.
	got := DeriveCooldown(10, 3*time.Second, 8*time.Second)
	if want := 55 * time.Second; got != want {
		t.Errorf("DeriveCooldown=%v, want %v", got, want)
	}
	if got := DeriveCooldown(1, 4*time.Second, 4*time.Second); got != 4*time.Second {
		t.Errorf("single-port cooldown=%v, want 4s", got)
	}
	if got := DeriveCooldown(0, time.Second, time.Second); got != time.Second {
		t.Errorf("cooldown with a zero ring=%v, want 1s (treat 0 as 1)", got)
	}
}

func TestNewPool_OpensThreadsTimesPortsPerThread(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 3, 4)

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	if p.Size() != 12 {
		t.Errorf("Size=%d, want 12", p.Size())
	}
	if got := len(fake.OpenPorts()); got != 12 {
		t.Errorf("proxy has %d open ports, want 12", got)
	}
	if want := DeriveCooldown(4, 2*time.Second, 4*time.Second); p.Cooldown() != want {
		t.Errorf("Cooldown=%v, want the derived %v", p.Cooldown(), want)
	}
}

func TestNewPool_ExplicitCooldownWins(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 4)
	cfg.Cooldown = 90 * time.Second

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	if p.Cooldown() != 90*time.Second {
		t.Errorf("Cooldown=%v, want the explicit 90s", p.Cooldown())
	}
}

func TestNewPool_APoolToldNothingAboutPacingLeavesTwoSecondsBetweenTwoRequestsOnOnePort(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 2, 4)
	// Neither the gap nor the delay range is named, which is the case the
	// documented default answers.
	cfg.DelayMin, cfg.DelayMax = 0, 0

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	if p.Cooldown() != 2*time.Second {
		t.Errorf("Cooldown=%v, want the documented 2s", p.Cooldown())
	}
	if p.Cooldown() != DefaultCooldown {
		t.Errorf("Cooldown=%v, want DefaultCooldown %v", p.Cooldown(), DefaultCooldown)
	}
}

func TestNewPool_ADescribedDelayRangeStillDecidesTheGapRatherThanTheDefault(t *testing.T) {
	// The default answers a caller who said nothing. A caller who did describe a
	// delay range is asking a different question, and answering it with two
	// seconds would throw their answer away.
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 2, 4)
	cfg.DelayMin, cfg.DelayMax = 5*time.Second, 5*time.Second

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	if want := 20 * time.Second; p.Cooldown() != want {
		t.Errorf("Cooldown=%v, want the derived %v", p.Cooldown(), want)
	}
}

func TestNewPool_ADelayFloorAloneIsEnoughToDeriveTheGap(t *testing.T) {
	// DelayMax alone bounds the pause as much as DelayMin does, and a caller who
	// named only one of the two has still described the ring the derivation is
	// about.
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 2, 4)
	cfg.DelayMin, cfg.DelayMax = 0, 6*time.Second

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	if p.Cooldown() == DefaultCooldown {
		t.Fatalf("Cooldown=%v, want the delay range to have been taken into account", p.Cooldown())
	}
	if want := DeriveCooldown(4, 3*time.Second, 6*time.Second); p.Cooldown() != want {
		t.Errorf("Cooldown=%v, want %v", p.Cooldown(), want)
	}
}

func TestNewPool_RollsBackOnFailure(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 3)
	// Two ports fit the range and open for real; the third has nowhere to go, so
	// NewPool fails half way and must close what it already opened.
	//
	// The failure has to be a real one. The fake's FailNext short-circuits before
	// a port is ever registered, so faking a "successful" open would leave nothing
	// for the rollback to close and this test would pass either way.
	cfg.PortRange = [2]int{20000, 20001}

	if _, err := NewPool(context.Background(), cfg); err == nil {
		t.Fatal("NewPool returned nil error when the port range could not cover the pool")
	}
	if got := fake.OpenPorts(); len(got) != 0 {
		t.Errorf("ports left open after a rolled-back NewPool: %v", got)
	}
}

func TestNewPool_RejectsMissingClient(t *testing.T) {
	if _, err := NewPool(context.Background(), PoolConfig{Threads: 1, PortsPerThread: 1}); err == nil {
		t.Error("NewPool without a Client returned nil error")
	}
}

func TestPool_AcquireHandsOutDistinctPortsThenWaitsForCooldown(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	p, err := NewPool(context.Background(), testPoolConfig(t, fake, clock, 1, 2))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	l1, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	l2, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if l1.Port() == l2.Port() {
		t.Fatalf("both leases got port %d; a port serves one request at a time", l1.Port())
	}

	first := l1.Port()
	l1.Release()
	l2.Release()

	start := clock.Now()
	l3, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("third Acquire: %v", err)
	}
	defer l3.Release()

	waited := clock.Now().Sub(start)
	if waited < p.Cooldown() {
		t.Errorf("Acquire waited %v before reusing a port, want at least the cooldown %v", waited, p.Cooldown())
	}
	if l3.Port() != first {
		t.Errorf("reused port %d, want the coldest one %d", l3.Port(), first)
	}
}

func TestPool_AcquireHonoursContextCancellation(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	p, err := NewPool(context.Background(), testPoolConfig(t, fake, clock, 1, 1))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Acquire on a cancelled context returned %v, want context.Canceled", err)
	}
}

func TestPool_ReleaseIsIdempotent(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	p, err := NewPool(context.Background(), testPoolConfig(t, fake, clock, 1, 1))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l.Release()
	l.Release() // must not corrupt the pool

	clock.Advance(p.Cooldown())
	if _, err := p.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire after a double Release: %v", err)
	}
}

func TestPool_CloseClosesEveryPort(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	p, err := NewPool(context.Background(), testPoolConfig(t, fake, clock, 2, 2))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := fake.OpenPorts(); len(got) != 0 {
		t.Errorf("ports still open after Close: %v", got)
	}
	if err := p.Close(); err != nil {
		t.Errorf("second Close returned %v, want nil (Close is idempotent)", err)
	}
}

func TestPool_RemedyRotatesProfileAndEgress(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2", "socks5")
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(ups))}

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	port := fake.OpenPorts()[0]
	before := fake.UpstreamOf(port)

	if err := p.rotateProfile(context.Background(), port); err != nil {
		t.Fatalf("rotateProfile: %v", err)
	}
	if n := fake.RotateCount(port); n != 1 {
		t.Errorf("RotateCount=%d, want 1", n)
	}

	if err := p.rotateEgress(context.Background(), port); err != nil {
		t.Fatalf("rotateEgress: %v", err)
	}
	if after := fake.UpstreamOf(port); after == before {
		t.Errorf("upstream still %q after rotateEgress; it must advance the list", after)
	}
}

func TestPoolRemedy_RotatesEgressAfterConsecutiveFailures(t *testing.T) {
	// The pool and the ladder were each tested against a stand-in for the other.
	// This drives the real seam: a ladder whose remedy is the pool itself.
	fake := fakebt.New(t)
	clock := newFakeClock()
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(ups))}
	cfg.RotateAfterFailures = 2
	cfg.MaxRetriesPerReq = 3

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	port := fake.OpenPorts()[0]
	before := fake.UpstreamOf(port)

	rt := &fakeRT{steps: []func() (*http.Response, error){
		respond(500, nil, "boom"),
		respond(500, nil, "boom"),
		respond(200, nil, "data"),
	}}
	l := &ladder{rt: rt, port: port, rem: p}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	if after := fake.UpstreamOf(port); after == before {
		t.Errorf("upstream still %q after two consecutive failures, want a rotation", after)
	}
	if st := p.Stats(); st.EgressRotations != 1 {
		t.Errorf("Stats.EgressRotations=%d, want 1", st.EgressRotations)
	}
}

func TestPoolRemedy_SuccessClearsTheFailureCount(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(ups))}
	cfg.RotateAfterFailures = 2
	cfg.MaxRetriesPerReq = 3

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	port := fake.OpenPorts()[0]
	before := fake.UpstreamOf(port)

	// One failure, a success, then another failure. The count is consecutive, so
	// these two failures must not add up to a rotation.
	rt := &fakeRT{steps: []func() (*http.Response, error){
		respond(500, nil, "boom"),
		respond(200, nil, "data"),
	}}
	l := &ladder{rt: rt, port: port, rem: p}
	if _, err := l.RoundTrip(newReq(t, http.MethodGet, "")); err != nil {
		t.Fatalf("first RoundTrip: %v", err)
	}

	rt2 := &fakeRT{steps: []func() (*http.Response, error){respond(404, nil, "gone")}}
	l2 := &ladder{rt: rt2, port: port, rem: p}
	if _, err := l2.RoundTrip(newReq(t, http.MethodGet, "")); err != nil {
		t.Fatalf("second RoundTrip: %v", err)
	}

	if after := fake.UpstreamOf(port); after != before {
		t.Errorf("upstream changed to %q; a success between two failures must clear the count", after)
	}
}

func TestPoolRemedy_TransportErrorMarksTheEgressWithoutBurningTheChannel(t *testing.T) {
	// The last uncovered strand of the pool-ladder seam: a connection-level
	// failure. It must blame the egress without spending the channel's weight —
	// markBadEgress runs on every attempt, so penalising there let a single
	// request against a dead proxy exhaust a healthy channel.
	fake := fakebt.New(t)
	clock := newFakeClock()
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	ch := NewListChannel("list", NewStaticRotor(ups))
	cfg.Channels = []Channel{ch}
	cfg.RotateAfterFailures = 2
	cfg.MaxRetriesPerReq = 3
	// Room to be carried past two dead addresses, which is what this is about.
	// The default is one, because the looking is done by the tries a phrase gets
	// rather than inside a single request.
	cfg.AddressesPerRequest = 3

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	weightBefore := p.mixer.Weight(ch)
	port := fake.OpenPorts()[0]

	boom := errors.New("dial tcp: connection refused")
	rt := &fakeRT{steps: []func() (*http.Response, error){
		failWith(boom),
		failWith(boom),
		respond(200, nil, "data"),
	}}
	l := &ladder{rt: rt, port: port, rem: p}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d, want 200 after rotating away from the dead egress", resp.StatusCode)
	}
	if w := p.mixer.Weight(ch); w != weightBefore {
		t.Errorf("channel weight %d → %d; one request must not spend it", weightBefore, w)
	}
}

func TestPool_NextDelayStaysInsideTheRange(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	p, err := NewPool(context.Background(), testPoolConfig(t, fake, clock, 1, 1))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	for i := 0; i < 200; i++ {
		d := p.NextDelay()
		if d < 2*time.Second || d > 4*time.Second {
			t.Fatalf("NextDelay=%v, want it inside [2s, 4s]", d)
		}
	}
}

func TestPool_SleepPausesOnTheClockTheRestOfThePoolRunsOn(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	p, err := NewPool(context.Background(), testPoolConfig(t, fake, clock, 1, 1))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	was := clock.Now()
	if err := p.Sleep(context.Background(), time.Minute); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if got := clock.Now().Sub(was); got != time.Minute {
		t.Errorf("the clock moved %v, want a minute - the pause ran on a clock of its own", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep returned %v, want the cancellation - a caller told to stop must not sit out the pause", err)
	}
}

func TestPool_RenewsIdentityAfterNRequests(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(ups))}
	cfg.RenewAfterRequests = 2

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	port := fake.OpenPorts()[0]
	before := fake.UpstreamOf(port)

	// Two leases reach the threshold; the third acquire must renew first.
	for i := 0; i < 2; i++ {
		l, err := p.Acquire(ctx)
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		l.Release()
		clock.Advance(p.Cooldown())
	}
	if fake.UpstreamOf(port) != before {
		t.Fatal("upstream changed before the request threshold was reached")
	}

	l, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("third Acquire: %v", err)
	}
	defer l.Release()

	if after := fake.UpstreamOf(port); after == before {
		t.Errorf("upstream still %q after %d requests, want a renewed identity", after, cfg.RenewAfterRequests)
	}
	if fake.RotateCount(port) == 0 {
		t.Error("the fingerprint was not rotated during a renewal")
	}
	if st := p.Stats(); st.Renewals != 1 {
		t.Errorf("Stats.Renewals=%d, want 1", st.Renewals)
	}
}

func TestPool_RenewsIdentityAfterInterval(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2", "socks5")
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(ups))}
	cfg.RenewAfterInterval = 10 * time.Minute

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	port := fake.OpenPorts()[0]
	before := fake.UpstreamOf(port)

	clock.Advance(11 * time.Minute)
	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	if after := fake.UpstreamOf(port); after == before {
		t.Errorf("upstream still %q after the renewal interval elapsed", after)
	}
}

func TestPool_QuarantinesAPortAfterRepeatedExhaustion(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 2)
	cfg.MaxPortStrikes = 2

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	victim := fake.OpenPorts()[0]
	p.exhausted(victim)
	if st := p.Stats(); st.Quarantined != 0 {
		t.Errorf("Quarantined=%d after one strike, want 0", st.Quarantined)
	}
	p.exhausted(victim)

	st := p.Stats()
	if st.Quarantined != 1 {
		t.Errorf("Quarantined=%d after two strikes, want 1", st.Quarantined)
	}
	if st.Available != 1 {
		t.Errorf("Available=%d, want 1", st.Available)
	}

	// The pool must keep working on the surviving port.
	for i := 0; i < 3; i++ {
		l, err := p.Acquire(context.Background())
		if err != nil {
			t.Fatalf("Acquire %d after quarantine: %v", i, err)
		}
		if l.Port() == victim {
			t.Fatal("a quarantined port was handed out")
		}
		l.Release()
		clock.Advance(p.Cooldown())
	}
}

func TestPool_AcquireReportsExhaustionWhenEveryPortIsQuarantined(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.MaxPortStrikes = 1

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	p.exhausted(fake.OpenPorts()[0])
	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrPoolExhausted) {
		t.Errorf("Acquire=%v, want ErrPoolExhausted rather than an endless wait", err)
	}
}

func TestPool_StatsCountRequestsAndRotations(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	p, err := NewPool(context.Background(), testPoolConfig(t, fake, clock, 1, 1))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	port := fake.OpenPorts()[0]
	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l.Release()

	if err := p.rotateProfile(context.Background(), port); err != nil {
		t.Fatalf("rotateProfile: %v", err)
	}

	st := p.Stats()
	if st.Ports != 1 {
		t.Errorf("Stats.Ports=%d, want 1", st.Ports)
	}
	if st.Requests != 1 {
		t.Errorf("Stats.Requests=%d, want 1", st.Requests)
	}
	if st.ProfileRotations != 1 {
		t.Errorf("Stats.ProfileRotations=%d, want 1", st.ProfileRotations)
	}
}

func TestPool_RepairsAPortWhoseRenewalFailed(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.RenewAfterRequests = 1

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	port := fake.OpenPorts()[0]
	l, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	l.Release()
	clock.Advance(p.Cooldown())

	// The renewal closes the port and then fails to reopen it. A lease handed out
	// now would point at a port that no longer exists on the proxy.
	fake.FailNext("/api/v1/ports/open", 500, `{"error":"boom"}`)

	l2, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire after a failed renewal: %v", err)
	}
	defer l2.Release()

	if got := fake.OpenPorts(); len(got) != 1 || got[0] != port {
		t.Errorf("proxy has ports %v, want the port repaired and open again", got)
	}
	if st := p.Stats(); st.Quarantined != 0 {
		t.Errorf("Quarantined=%d, want 0: one failure must cost a retry, not the port", st.Quarantined)
	}
}

func TestPool_ClosingDuringRenewalDoesNotOrphanAPort(t *testing.T) {
	// A renewal reopens a port through several calls. If the pool is closed in
	// between, the reopened port would outlive the program with nobody to close
	// it — and no later run may reclaim it, because taking over a port this
	// program did not open is exactly what the pool refuses to do.
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.RenewAfterRequests = 1

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	ctx := context.Background()

	l, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l.Release()

	pt := p.ports[0]
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := fake.OpenPorts(); len(got) != 0 {
		t.Fatalf("Close left ports open: %v", got)
	}

	// Now run the renewal that was already in flight when Close happened.
	if err := p.renewIfDue(ctx, pt); err == nil {
		t.Error("renewIfDue on a closed pool returned nil error")
	}
	if got := fake.OpenPorts(); len(got) != 0 {
		t.Errorf("renewal after Close left port %v open on the proxy", got)
	}
}

func TestPool_QuarantinesAPortItCannotReopen(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.RenewAfterRequests = 1
	cfg.MaxPortStrikes = 2

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	l, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	l.Release()
	clock.Advance(p.Cooldown())

	// Every reopen fails, so the port can never be repaired and must be given up.
	for i := 0; i < 4; i++ {
		fake.FailNext("/api/v1/ports/open", 500, `{"error":"boom"}`)
	}

	if _, err := p.Acquire(ctx); !errors.Is(err, ErrPoolExhausted) {
		t.Errorf("Acquire=%v, want ErrPoolExhausted once the port cannot be reopened", err)
	}
	if st := p.Stats(); st.Quarantined != 1 {
		t.Errorf("Quarantined=%d, want 1", st.Quarantined)
	}
}

func TestLease_SessionIsStableAcrossOrdinaryUse(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	p, err := NewPool(context.Background(), testPoolConfig(t, fake, clock, 1, 1))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	l, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	first := l.Session()
	l.Release()
	clock.Advance(p.Cooldown())

	l2, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	defer l2.Release()
	if l2.Session() != first {
		t.Errorf("session %q → %q without an identity change; per-session state would be thrown away for nothing",
			first, l2.Session())
	}
	if first == "" {
		t.Error("Session() is empty")
	}
}

func TestLease_SessionChangesWhenTheIdentityIsRenewed(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.RenewAfterRequests = 1

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	l, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	first := l.Session()
	l.Release()
	clock.Advance(p.Cooldown())

	l2, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire after renewal: %v", err)
	}
	defer l2.Release()
	if l2.Session() == first {
		t.Errorf("session still %q after the port was reopened; the proxy has discarded the solved challenge and any state bound to it is stale", first)
	}
}

func TestLease_SessionChangesWhenTheEgressRotates(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(ups))}

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	l, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	first := l.Session()
	port := l.Port()
	l.Release()

	if err := p.rotateEgress(ctx, port); err != nil {
		t.Fatalf("rotateEgress: %v", err)
	}
	clock.Advance(p.Cooldown())

	l2, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire after egress rotation: %v", err)
	}
	defer l2.Release()
	if l2.Session() == first {
		t.Errorf("session still %q after the egress changed; clearance cookies bind to the exit IP and are gone", first)
	}
}

// countFailurePairConfig builds the config shared by the two tests below. Both
// drive RotateAfterFailures 403 responses through the real ladder on the same
// port; the only thing that differs between them is CountFailure. 403 is not
// retryable (see Retryable), so one RoundTrip call is exactly one attempt —
// reaching the threshold takes RotateAfterFailures separate calls, not one
// call with a multi-step script the ladder would never get through.
func countFailurePairConfig(t *testing.T, fake *fakebt.Server, clock *fakeClock) PoolConfig {
	t.Helper()
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(ups))}
	cfg.RotateAfterFailures = 2
	return cfg
}

func TestPool_CountFailureLetsTheConsumerDecideWhatCounts(t *testing.T) {
	// Some targets answer a malformed request with a status that says "your
	// request was wrong", not "this egress is bad". Rotating the egress on those
	// burns proxies for a fault that travels with the request. The pool does not
	// reason about why — it asks.
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := countFailurePairConfig(t, fake, clock)
	cfg.CountFailure = func(status int) bool { return status != 403 }

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	port := fake.OpenPorts()[0]
	before := fake.UpstreamOf(port)

	rt := &fakeRT{steps: []func() (*http.Response, error){
		respond(403, nil, "bad headers"),
		respond(403, nil, "bad headers"),
	}}
	l := &ladder{rt: rt, port: port, rem: p}
	for i := 0; i < cfg.RotateAfterFailures; i++ {
		resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
		if err != nil {
			t.Fatalf("RoundTrip %d: %v", i, err)
		}
		resp.Body.Close()
	}

	if after := fake.UpstreamOf(port); after != before {
		t.Errorf("egress rotated to %q after %d attempts on a status the consumer excluded", after, cfg.RotateAfterFailures)
	}
}

func TestPool_CountFailureDefaultsToCountingEveryNon2xx(t *testing.T) {
	// Same setup as the excluded-status test above — same number of attempts,
	// same status, same port — with only CountFailure differing (left nil
	// here). The two must land on opposite outcomes, or the exclusion test above
	// proves nothing about CountFailure actually being consulted.
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := countFailurePairConfig(t, fake, clock)
	// CountFailure deliberately left nil.

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	port := fake.OpenPorts()[0]
	before := fake.UpstreamOf(port)

	rt := &fakeRT{steps: []func() (*http.Response, error){
		respond(403, nil, "bad headers"),
		respond(403, nil, "bad headers"),
	}}
	l := &ladder{rt: rt, port: port, rem: p}
	for i := 0; i < cfg.RotateAfterFailures; i++ {
		resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
		if err != nil {
			t.Fatalf("RoundTrip %d: %v", i, err)
		}
		resp.Body.Close()
	}

	if after := fake.UpstreamOf(port); after == before {
		t.Errorf("upstream still %q after %d attempts with CountFailure unset; the default must count every non-2xx", after, cfg.RotateAfterFailures)
	}
}

// markWatch records every address a channel is told to stop handing out. That
// verdict is the channel's to act on and has no effect the pool can observe
// until an address has collected several of them, so a test that wants to pin
// it has to watch the channel.
type markWatch struct {
	Channel
	mu     sync.Mutex
	marked []string
}

func (c *markWatch) MarkBad(eg Egress) {
	c.mu.Lock()
	c.marked = append(c.marked, eg.Upstream)
	c.mu.Unlock()
	c.Channel.MarkBad(eg)
}

func (c *markWatch) blamed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.marked...)
}

func TestLease_AnAnswerHandedBackCountsTowardsANewEgress(t *testing.T) {
	// A refusal arrives with a 2xx status, and to the transport that is a
	// success: the port's consecutive-failure count is cleared and its egress
	// kept. Unless the caller's verdict counts, the port keeps that egress and
	// every thread that leases it next gets the same answer.
	fake := fakebt.New(t)
	clock := newFakeClock()
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(ups))}
	cfg.RotateAfterFailures = 2

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	before := l.Egress()

	if err := l.Reject(context.Background()); err != nil {
		t.Fatalf("first Reject: %v", err)
	}
	if got := l.Egress(); got != before {
		t.Fatalf("egress %v -> %v after one rejection; it must take RotateAfterFailures of them", before, got)
	}

	if err := l.Reject(context.Background()); err != nil {
		t.Fatalf("second Reject: %v", err)
	}
	if got := l.Egress(); got == before {
		t.Errorf("egress still %v after two rejections with RotateAfterFailures=2", got)
	}
	l.Release()

	if n := p.Stats().Rejections; n != 2 {
		t.Errorf("Stats().Rejections=%d, want 2", n)
	}
}

func TestLease_AnAnswerHandedBackBlamesTheAddressItCameThrough(t *testing.T) {
	// The request itself succeeded, so the fault does not travel with the
	// request — it belongs to the address the answer came back to. Saying so is
	// what stops the channel handing that same address to the rest of the pool.
	fake := fakebt.New(t)
	clock := newFakeClock()
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	ch := &markWatch{Channel: NewListChannel("list", NewStaticRotor(ups))}
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{ch}
	// Two rejections would replace the egress; one leaves the blame as the only
	// thing this test can be reading.
	cfg.RotateAfterFailures = 2

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	addr := l.Egress().Upstream
	if err := l.Reject(context.Background()); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	l.Release()

	blamed := ch.blamed()
	if len(blamed) != 1 || blamed[0] != addr {
		t.Errorf("channel was told %q was bad, want exactly [%q]", blamed, addr)
	}
}

func TestLease_EnoughAnswersHandedBackQuarantineThePort(t *testing.T) {
	// A port every one of whose addresses is refused is not worth handing out: a
	// pool of one healthy port and nine refused ones spends nine tenths of the
	// run collecting refusals.
	fake := fakebt.New(t)
	clock := newFakeClock()
	// The default channel has one fixed address, so each rejection spends the
	// port's whole budget at once and counts as a strike.
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.RotateAfterFailures = 1
	cfg.MaxPortStrikes = 2

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for i := 0; i < cfg.MaxPortStrikes; i++ {
		if err := l.Reject(context.Background()); err != nil {
			t.Fatalf("Reject %d: %v", i+1, err)
		}
	}
	l.Release()

	if q := p.Stats().Quarantined; q != 1 {
		t.Errorf("Quarantined=%d, want 1 once the port has spent its strikes", q)
	}
}

func TestLease_AnAnswerHandedBackAfterTheLeaseIsReturnedChangesNothing(t *testing.T) {
	// Release puts the port back and another thread may hold it already. A late
	// verdict would land on that thread's work, and with one strike left it
	// would take the port out of the pool for an answer nobody read.
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.RotateAfterFailures = 1
	cfg.MaxPortStrikes = 1

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l.Release()

	if err := l.Reject(context.Background()); err != nil {
		t.Fatalf("Reject after Release: %v", err)
	}
	st := p.Stats()
	if st.Rejections != 0 {
		t.Errorf("Stats().Rejections=%d after a rejection on a returned lease, want 0", st.Rejections)
	}
	if st.Quarantined != 0 {
		t.Errorf("Quarantined=%d; a returned lease took a port out of the pool", st.Quarantined)
	}
}

func TestPool_AQuarantinedPortComesBackOnAnotherEgress(t *testing.T) {
	// A bought list is dead in large part by definition, so a port that drew
	// three bad addresses in a row is unlucky rather than broken, and holding it
	// out for the life of the pool stops a job in which nothing is wrong. Handing
	// it back with the address it was quarantined for repeats that draw at once,
	// so coming back has to start with another one.
	fake := fakebt.New(t)
	clock := newFakeClock()
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(ups))}
	cfg.MaxPortStrikes = 1
	cfg.ReviveAfter = time.Minute
	cfg.MaxRevivals = 2

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	port := fake.OpenPorts()[0]
	dead := fake.UpstreamOf(port)
	p.exhausted(port)

	if q := p.Stats().Quarantined; q != 1 {
		t.Fatalf("Quarantined=%d before the wait, want 1", q)
	}
	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("Acquire before ReviveAfter=%v, want ErrPoolExhausted", err)
	}

	clock.Advance(time.Minute)

	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire after ReviveAfter: %v", err)
	}
	defer l.Release()

	if got := l.Egress().Upstream; got == dead {
		t.Errorf("the port came back on %q, the address it was quarantined for", got)
	}
	if got := fake.UpstreamOf(port); got == dead {
		t.Errorf("port %d still egresses through %q; the lease and the proxy disagree", port, got)
	}
	st := p.Stats()
	if st.Revivals != 1 {
		t.Errorf("Stats().Revivals=%d, want 1", st.Revivals)
	}
	if st.Quarantined != 0 {
		t.Errorf("Quarantined=%d once the port is back in rotation, want 0", st.Quarantined)
	}
}

func TestPool_APortQuarantinedOnceTooOftenStopsComingBack(t *testing.T) {
	// Bringing a port back without end turns one that is genuinely finished into
	// a slow leak: every acquisition pays for a rotation and the port is
	// quarantined again a moment later.
	fake := fakebt.New(t)
	clock := newFakeClock()
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(ups))}
	cfg.MaxPortStrikes = 1
	cfg.ReviveAfter = time.Minute
	cfg.MaxRevivals = 1

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	port := fake.OpenPorts()[0]
	p.exhausted(port)
	clock.Advance(time.Minute)

	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire inside MaxRevivals: %v", err)
	}
	l.Release()

	p.exhausted(port)
	clock.Advance(time.Minute)

	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrPoolExhausted) {
		t.Errorf("Acquire past MaxRevivals=%v, want ErrPoolExhausted", err)
	}
	if n := p.Stats().Revivals; n != 1 {
		t.Errorf("Stats().Revivals=%d, want 1", n)
	}
}

func TestPool_ANegativeReviveAfterKeepsAQuarantinedPortOut(t *testing.T) {
	// Coming back is a policy, and a caller that wants a quarantined port to stay
	// out has to be able to say so without reading the default. The channel here
	// has addresses left, so the port would come back if the setting were ignored.
	fake := fakebt.New(t)
	clock := newFakeClock()
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2", "socks5")
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(ups))}
	cfg.MaxPortStrikes = 1
	cfg.ReviveAfter = -1

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	p.exhausted(fake.OpenPorts()[0])
	clock.Advance(time.Hour)

	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrPoolExhausted) {
		t.Errorf("Acquire with a negative ReviveAfter=%v, want ErrPoolExhausted", err)
	}
	if n := p.Stats().Revivals; n != 0 {
		t.Errorf("Stats().Revivals=%d with revival switched off, want 0", n)
	}
}

func TestPool_AQuarantineWithNoBeginningIsNotRevived(t *testing.T) {
	// The wait is measured from the moment the quarantine began, and every place
	// that quarantines a port records it. A port flagged without that moment has
	// no wait to measure and stays out, rather than being handed another egress
	// on the first acquire that sees it.
	fake := fakebt.New(t)
	clock := newFakeClock()
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2", "socks5")
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(ups))}
	cfg.ReviveAfter = time.Minute

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	pt := p.ports[0]
	pt.mu.Lock()
	pt.quarantined = true
	pt.mu.Unlock()

	clock.Advance(time.Hour)

	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrPoolExhausted) {
		t.Errorf("Acquire=%v, want ErrPoolExhausted", err)
	}
	if n := p.Stats().Revivals; n != 0 {
		t.Errorf("Stats().Revivals=%d, want 0", n)
	}
}

// renewWatch counts how many times a channel is asked for another address. The
// pool's counters record what came back, so attempts that never produced one are
// only visible from the channel's side.
type renewWatch struct {
	Channel
	mu sync.Mutex
	n  int
}

func (c *renewWatch) Renew(ctx context.Context, cur Egress) (Egress, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.Channel.Renew(ctx, cur)
}

func (c *renewWatch) asked() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func TestPool_APortWhoseEgressCannotBeReplacedStaysQuarantined(t *testing.T) {
	// Coming back starts with another egress and cannot be anything less, so a
	// channel holding one fixed address has nothing to bring the port back to.
	// The pool has to answer that the port is gone rather than spend every
	// acquisition on a rotation that can never finish. Each failed attempt costs
	// the port one chance and a fresh wait, so the chances are spread over time
	// rather than spent in one burst on the first acquire that asks.
	fake := fakebt.New(t)
	clock := newFakeClock()
	ch := &renewWatch{Channel: NewDirectChannel("direct")} // one fixed address
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{ch}
	cfg.MaxPortStrikes = 1
	cfg.ReviveAfter = time.Minute
	cfg.MaxRevivals = 3

	// A pool that keeps retrying never returns, and a hung test says nothing. A
	// Sleep that gives up turns that into a failed assertion.
	errKeptTrying := errors.New("the pool kept waiting for a rotation that cannot happen")
	sleeps := 0
	cfg.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps++
		if sleeps > 4 {
			return errKeptTrying
		}
		return clock.Sleep(ctx, d)
	}

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	p.exhausted(fake.OpenPorts()[0])
	clock.Advance(time.Minute)

	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("Acquire=%v, want ErrPoolExhausted", err)
	}
	if n := ch.asked(); n != 1 {
		t.Errorf("the channel was asked for another address %d times on one acquire, want 1", n)
	}

	// The next wait buys the port its next chance, and no more than that.
	clock.Advance(time.Minute)
	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("second Acquire=%v, want ErrPoolExhausted", err)
	}
	if n := ch.asked(); n != 2 {
		t.Errorf("the channel was asked %d times after two waits, want 2", n)
	}

	st := p.Stats()
	if st.Quarantined != 1 {
		t.Errorf("Quarantined=%d after a rotation that failed, want 1", st.Quarantined)
	}
	if st.Revivals != 0 {
		t.Errorf("Stats().Revivals=%d, want 0: the port never came back", st.Revivals)
	}
}

func TestPool_KeepsAPortsAddressWhenTheListNoLongerHasIt(t *testing.T) {
	// The rotor hands addresses out; the pool remembers what it got. A refresh
	// replaces what is handed out and must not reach into what was.
	//
	// A port that lost its address when the list changed would move to another
	// egress in the middle of the work it is doing, taking its cookies and its
	// solved challenges to an IP they were not issued for — and a list is
	// reloaded on a timer, so it would happen to every port at once, on a
	// schedule nobody watching the run had any reason to connect it to.
	//
	// Both halves are checked here. Without the second, this test would also
	// pass on a rotor that never reloads at all, which is the same green for the
	// opposite reason.
	fake := fakebt.New(t)
	clock := newFakeClock()
	list := filepath.Join(t.TempDir(), "list.txt")
	writeList(t, list, "1.1.1.1:1080\n")

	const held = "socks5://1.1.1.1:1080"
	const fresh = "socks5://9.9.9.9:9090"

	rotor, err := NewRotor(context.Background(), Source{
		Kind: "file", Location: list, Refresh: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRotor: %v", err)
	}
	ch := NewListChannel("list", rotor)
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{ch}

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close() // closes the channel, and with it the reloader

	port := fake.OpenPorts()[0]
	if got := fake.UpstreamOf(port); got != held {
		t.Fatalf("the port opened on %q, want %q", got, held)
	}

	writeList(t, list, "9.9.9.9:9090\n")

	// The reload is on a timer, so it is waited for by asking what the list
	// hands out now. The wait has a limit of its own: a list that never reloads
	// has to fail here, and say so, rather than run this test out of time.
	deadline := time.Now().Add(5 * time.Second)
	for {
		eg, ok := ch.Next()
		if !ok {
			t.Fatal("the list had nothing to hand out")
		}
		if eg.Upstream == fresh {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the list is still handing out %q long after the file changed, so it was never read again", eg.Upstream)
		}
		time.Sleep(2 * time.Millisecond)
	}

	if got := fake.UpstreamOf(port); got != held {
		t.Errorf("the open port moved to %q when the list stopped naming %q", got, held)
	}

	lease, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got := lease.Egress().Upstream; got != held {
		t.Errorf("the lease came back on %q, want the address the port was opened on", got)
	}
	if got := fake.UpstreamOf(port); got != held {
		t.Errorf("acquiring the port moved it to %q, want %q", got, held)
	}
	lease.Release()

	// And the list that was read is the list the next address comes from. A
	// rotation is where a port is given another one, so it is where the fresh
	// list has to show up.
	if err := p.rotateEgress(context.Background(), port); err != nil {
		t.Fatalf("rotateEgress: %v", err)
	}
	if got := fake.UpstreamOf(port); got != fresh {
		t.Errorf("the port rotated onto %q, want %q from the list as it now stands", got, fresh)
	}
}

// refuseOpen sits in front of the control API and refuses to open the port
// numbers a test chooses, passing every other call through and recording the
// number each open was attempted on.
//
// The fake control API can be told to fail the next open, but not to fail the
// open of a particular NUMBER — and which number a failure lands on is the whole
// question here. A stand that refuses whichever number happens to come first
// cannot tell a pool that steps over a taken number from one that never tries.
type refuseOpen struct {
	// rt carries the calls that are not refused. Left nil it is the ordinary
	// transport; a test that wants a control API which cannot be reached at all
	// sets its own.
	rt http.RoundTripper
	// refuse decides, for the number an open names and which attempt this is,
	// whether to answer it with status and body instead of opening it.
	refuse func(port, attempt int) (status int, body string, refuse bool)

	mu    sync.Mutex
	tried []int
}

func (r *refuseOpen) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != "/api/v1/ports/open" {
		return r.rt.RoundTrip(req)
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(raw))

	var open struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(raw, &open); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.tried = append(r.tried, open.Port)
	attempt := len(r.tried)
	r.mu.Unlock()

	status, body, refuse := r.refuse(open.Port, attempt)
	if !refuse {
		return r.rt.RoundTrip(req)
	}
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// attempts lists the port numbers opens were attempted on, in order.
func (r *refuseOpen) attempts() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.tried...)
}

// deadTransport stands for a control API that cannot be reached at all — the
// failure that arrives without any answer from the proxy to read.
type deadTransport struct{}

func (deadTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: connect: connection refused")
}

// portTakenBody is what a control API answers when the number it was asked for
// is already held.
const portTakenBody = `{"error":"port already open"}`

// poolConfigRefusing is testPoolConfig with every open passing through r first.
func poolConfigRefusing(t *testing.T, fake *fakebt.Server, clock *fakeClock, threads, perThread int, r *refuseOpen) PoolConfig {
	t.Helper()
	cfg := testPoolConfig(t, fake, clock, threads, perThread)
	if r.rt == nil {
		r.rt = http.DefaultTransport
	}
	c, err := NewClient(fake.URL(), fake.Key(), WithHTTPClient(&http.Client{Transport: r, Timeout: 15 * time.Second}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cfg.Client = c
	return cfg
}

func TestNewPool_APortNumberAlreadyTakenCostsAnotherNumberNotThePool(t *testing.T) {
	// The number came from the proxy, which knows about its own ports and
	// nothing else on the machine. Something else holding it is a fact about
	// that number, not about the pool.
	fake := fakebt.New(t)
	clock := newFakeClock()
	// Two numbers this machine has just proved free, so that the pool's own
	// check of them (numberFree) has nothing to say and the refusal under test
	// is the only thing standing in the way.
	run := freeRun(t, 2)
	taken, free := portOf(run[0]), portOf(run[1])
	closeAll(t, run)

	r := &refuseOpen{refuse: func(port, _ int) (int, string, bool) {
		return http.StatusConflict, portTakenBody, port == taken
	}}
	cfg := poolConfigRefusing(t, fake, clock, 1, 1, r)
	cfg.PortRange = [2]int{taken, free}

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool gave up because one number was taken: %v", err)
	}
	if p.Size() != 1 {
		t.Errorf("Size=%d, want 1", p.Size())
	}
	if got := fake.OpenPorts(); len(got) != 1 || got[0] != free {
		t.Fatalf("open ports=%v, want just %d", got, free)
	}
	// A refused number has to be marked used. pickPort avoids only what the pool
	// marked, so a number left unmarked is handed straight back and refused
	// again until the attempts run out.
	if got, want := r.attempts(), []int{taken, free}; !sameInts(got, want) {
		t.Errorf("opens were attempted on %v, want %v", got, want)
	}
	// And the number the pool wrote down is the number that opened. Keeping the
	// refused one would hand out leases dialling a port this program never held.
	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if l.Port() != free {
		t.Errorf("the lease came back on port %d, want the one that opened, %d", l.Port(), free)
	}
	l.Release()

	// Whatever holds the taken number, this program did not open it, and closing
	// a port it does not own is the one thing the pool must never do.
	p.Close()
	for _, req := range fake.Requests() {
		if req.Path == "/api/v1/ports/close" && strings.Contains(req.Body, fmt.Sprintf(`"port":%d`, taken)) {
			t.Errorf("the pool closed port %d, which it never opened", taken)
		}
	}
}

func TestNewPool_TheLastPortOfThePoolStepsOverATakenNumberToo(t *testing.T) {
	// The hard arrangement: the taken number is the one the LAST port of the
	// pool was given. A retry reached only while ports remain to be opened — an
	// off-by-one, or a step tucked inside the wrong loop — passes a stand where
	// the first number is taken and fails here.
	fake := fakebt.New(t)
	clock := newFakeClock()
	const size = 3

	r := &refuseOpen{refuse: func(_, attempt int) (int, string, bool) {
		return http.StatusConflict, portTakenBody, attempt == size
	}}
	p, err := NewPool(context.Background(), poolConfigRefusing(t, fake, clock, 1, size, r))
	if err != nil {
		t.Fatalf("NewPool gave up on the last port of the pool: %v", err)
	}
	defer p.Close()

	if p.Size() != size {
		t.Errorf("Size=%d, want %d", p.Size(), size)
	}
	tried := r.attempts()
	if len(tried) != size+1 {
		t.Fatalf("opens were attempted on %v, want %d numbers: one of them was taken", tried, size+1)
	}
	open := fake.OpenPorts()
	if len(open) != size {
		t.Fatalf("the proxy holds %v, want %d ports", open, size)
	}
	for _, n := range open {
		if n == tried[size-1] {
			t.Errorf("port %d is open although the proxy refused it", n)
		}
	}
}

func TestNewPool_WithNoNumberTakenNothingExtraIsAskedFor(t *testing.T) {
	// The other half of the same arrangement: with nothing in the way, the pool
	// asks for exactly as many numbers as it opens. A retry that fires on a
	// success, or a number marked used twice, shows up here and nowhere else —
	// on a live proxy each wasted number is another port opened and abandoned.
	fake := fakebt.New(t)
	clock := newFakeClock()
	const size = 4

	r := &refuseOpen{refuse: func(int, int) (int, string, bool) { return 0, "", false }}
	p, err := NewPool(context.Background(), poolConfigRefusing(t, fake, clock, 2, 2, r))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	if got := r.attempts(); len(got) != size {
		t.Errorf("opens were attempted on %v, want %d — one number per port", got, size)
	}
	if got := fake.OpenPorts(); len(got) != size {
		t.Errorf("the proxy holds %v, want %d ports", got, size)
	}
}

func TestNewPool_ARefusalNoOtherNumberWouldFixIsNotRepeated(t *testing.T) {
	// Such a refusal answers the same on every number. Trying more of them turns
	// a sentence the operator can act on — the key was rejected, the tariff does
	// not allow a pool — into a long wait ending in a vaguer one.
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"the key was rejected", http.StatusUnauthorized, `{"error":"authentication required"}`},
		{"the tariff does not include a pool", http.StatusForbidden, `{"error":"pool not licensed"}`},
		{"the request itself was wrong", http.StatusBadRequest, `{"error":"invalid JSON body"}`},
		{"the proxy broke", http.StatusInternalServerError, `{"error":"boom"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := fakebt.New(t)
			clock := newFakeClock()
			r := &refuseOpen{refuse: func(int, int) (int, string, bool) {
				return tc.status, tc.body, true
			}}
			p, err := NewPool(context.Background(), poolConfigRefusing(t, fake, clock, 1, 2, r))
			if err == nil {
				p.Close()
				t.Fatal("NewPool came up although every open was refused")
			}
			if n := len(r.attempts()); n != 1 {
				t.Errorf("%d numbers were tried after %q, want 1", n, tc.name)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != tc.status {
				t.Errorf("error %v does not carry the %d the proxy answered", err, tc.status)
			}
		})
	}
}

func TestNewPool_ARefusalWithNoAnswerAtAllIsNotRepeatedEither(t *testing.T) {
	// Nothing was heard from the proxy, so there is nothing in the answer to
	// read a number out of. The port range keeps the failure on the open itself:
	// with it set, no number is asked for over the wire.
	fake := fakebt.New(t)
	clock := newFakeClock()
	r := &refuseOpen{
		rt:     deadTransport{},
		refuse: func(int, int) (int, string, bool) { return 0, "", false },
	}
	cfg := poolConfigRefusing(t, fake, clock, 1, 1, r)
	cfg.PortRange = [2]int{20000, 20010}

	p, err := NewPool(context.Background(), cfg)
	if err == nil {
		p.Close()
		t.Fatal("NewPool came up although the control API was not answering")
	}
	if n := len(r.attempts()); n != 1 {
		t.Errorf("%d numbers were tried against a control API that is not there, want 1", n)
	}
}

func TestNewPool_GivesUpOnTakenNumbersAtTheCeilingTheSearchAlreadyUses(t *testing.T) {
	// Two things belong together here: the retry ends, and it ends where
	// pickPort's own search for a number ends. Two ceilings in two places drift.
	fake := fakebt.New(t)
	clock := newFakeClock()
	// A retry that never stops has to fail this test rather than run it out of
	// time, so past a generous bound the stand answers something not retryable.
	const guard = 500

	r := &refuseOpen{refuse: func(_, attempt int) (int, string, bool) {
		switch {
		case attempt == 1:
			return 0, "", false // the first port opens, so the rollback has work to do
		case attempt > guard:
			return http.StatusUnauthorized, `{"error":"a retry that would not stop"}`, true
		default:
			return http.StatusConflict, portTakenBody, true
		}
	}}
	cfg := poolConfigRefusing(t, fake, clock, 1, 2, r)

	p, err := NewPool(context.Background(), cfg)
	if err == nil {
		p.Close()
		t.Fatal("NewPool came up although every number for its second port was taken")
	}
	if n, want := len(r.attempts()), 1+3*cfg.Size()+12; n != want {
		t.Errorf("%d numbers were tried, want %d: one that opened, then the ceiling pickPort uses", n, want)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Errorf("error %v does not carry the refusal that ended the search", err)
	}
	if got := fake.OpenPorts(); len(got) != 0 {
		t.Errorf("ports left open after a pool that never came up: %v", got)
	}
}

// liveLikeControl stands in for the control API as it was actually measured,
// not as a well-behaved one would be.
//
// Two things it does that the plain fake cannot:
//
//   - It hands out the port numbers the test lists, in the order listed, so a
//     test can put a number this machine holds anywhere in the sequence rather
//     than wherever the machine happened to put it. When the list runs out the
//     real fake answers, which is a genuinely free number.
//   - It answers every open with the fake's own 200 "opened" — and separately
//     tries to bind the number itself, the way a proxy that really opened a port
//     would have to, recording the numbers where that failed. Against the live
//     service on 127.0.0.1:8891 the answer was 200 for a number this machine
//     already held; the port was then listed and dead. So "opened" proves
//     nothing, and what the test asks instead is whether a port could have
//     existed at all.
type liveLikeControl struct {
	rt      http.RoundTripper
	suggest []int

	mu     sync.Mutex
	handed int
	opened []int
	dead   []int
	held   []net.Listener
}

func (m *liveLikeControl) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Path {
	case "/api/v1/ports/suggest":
		m.mu.Lock()
		var num int
		if m.handed < len(m.suggest) {
			num = m.suggest[m.handed]
			m.handed++
		}
		m.mu.Unlock()
		if num == 0 {
			return m.rt.RoundTrip(req)
		}
		return jsonResponse(req, http.StatusOK, fmt.Sprintf(`{"port":%d}`, num))
	case "/api/v1/ports/open":
		return m.serveOpen(req)
	}
	return m.rt.RoundTrip(req)
}

func (m *liveLikeControl) serveOpen(req *http.Request) (*http.Response, error) {
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(raw))

	var open struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(raw, &open); err != nil {
		return nil, err
	}
	// A proxy opening a port has to bind it. This one does the same, and keeps
	// what it bound: a number handed back to the pool has to stay the pool's.
	ln, bindErr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", open.Port))

	m.mu.Lock()
	m.opened = append(m.opened, open.Port)
	if bindErr != nil {
		m.dead = append(m.dead, open.Port)
	} else {
		m.held = append(m.held, ln)
	}
	m.mu.Unlock()

	return m.rt.RoundTrip(req)
}

// opens lists the numbers the control API was asked to open, in order.
func (m *liveLikeControl) opens() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.opened...)
}

// deadPorts lists the numbers it answered "opened" on and could not bind — the
// ports that are listed, counted, leased out, and answer nothing.
func (m *liveLikeControl) deadPorts() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.dead...)
}

func (m *liveLikeControl) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ln := range m.held {
		_ = ln.Close()
	}
	m.held = nil
}

func jsonResponse(req *http.Request, status int, body string) (*http.Response, error) {
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// poolConfigVia is testPoolConfig with every control-API call going through rt.
func poolConfigVia(t *testing.T, fake *fakebt.Server, clock *fakeClock, threads, perThread int, rt http.RoundTripper) PoolConfig {
	t.Helper()
	cfg := testPoolConfig(t, fake, clock, threads, perThread)
	c, err := NewClient(fake.URL(), fake.Key(), WithHTTPClient(&http.Client{Transport: rt, Timeout: 15 * time.Second}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cfg.Client = c
	return cfg
}

// freeRun binds n consecutive port numbers on the loopback and hands back the
// listeners, still open — so the numbers are this machine's until the test lets
// them go. Holding them is what makes them dependable: a number merely reported
// free a moment ago can be taken by anything before it is used.
//
// The band matters. Numbers the operating system hands out by itself are the
// ones it also gives to outgoing connections, and these are let go of before
// the pool looks at them: a number released at 61000 on Windows was handed to
// one of the test's own connections before the pool got there. 30000–31000 is
// below the dynamic range of both Windows and Linux and above the band the
// product's own proxies live in, and the run package searches the band above
// this one, so two packages under test at once do not take numbers from each
// other.
func freeRun(t *testing.T, n int) []net.Listener {
	t.Helper()
	for base := 30000; base+n <= 31000; base += n {
		lns := make([]net.Listener, 0, n)
		for i := 0; i < n; i++ {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
			if err != nil {
				break
			}
			lns = append(lns, ln)
		}
		if len(lns) == n {
			t.Cleanup(func() { closeAll(t, lns) })
			return lns
		}
		closeAll(t, lns)
	}
	t.Fatalf("no run of %d free ports between 30000 and 31000 on this machine", n)
	return nil
}

// portOf reports the number a held listener occupies.
func portOf(ln net.Listener) int { return ln.Addr().(*net.TCPAddr).Port }

// closeAll releases held numbers at once. Closing is not deferred anywhere a
// number is about to be opened: the point of the arrangement is which numbers
// are free at the moment the pool looks.
func closeAll(t *testing.T, lns []net.Listener) {
	t.Helper()
	for _, ln := range lns {
		_ = ln.Close()
	}
}

func TestNewPool_StepsOverANumberThisMachineHoldsAlthoughTheProxySaysYes(t *testing.T) {
	// The measured fault, whole: the service answers 200 and lists a port it
	// cannot possibly have bound, so the pool has to ask the machine instead.
	//
	// The arrangement is deliberately not "the first number is held". Both
	// held numbers come after one that opens, and they come in a pair, so a
	// check made once per port — or only on the first number of an open, or only
	// after the first refusal — fails here while passing the easy stand.
	fake := fakebt.New(t)
	clock := newFakeClock()

	run := freeRun(t, 4)
	first, busy, alsoBusy, second := portOf(run[0]), portOf(run[1]), portOf(run[2]), portOf(run[3])
	// The two in the middle stay held for the whole test; the outer two are the
	// machine's answer to what the pool may have.
	closeAll(t, []net.Listener{run[0], run[3]})

	m := &liveLikeControl{
		rt:      http.DefaultTransport,
		suggest: []int{first, busy, alsoBusy, second},
	}
	defer m.close()
	cfg := poolConfigVia(t, fake, clock, 1, 2, m)
	cfg.ProxyHost = "127.0.0.1"

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	if got, want := m.opens(), []int{first, second}; !sameInts(got, want) {
		t.Errorf("the proxy was asked to open %v, want %v: %d and %d are held by this machine and would answer nothing",
			got, want, busy, alsoBusy)
	}
	if got := m.deadPorts(); len(got) != 0 {
		t.Errorf("the proxy could not bind %v although it answered that they opened — "+
			"a number checked and not let go again is a number the proxy cannot have", got)
	}
	if got, want := fake.OpenPorts(), []int{first, second}; !sameInts(got, want) {
		t.Errorf("the pool holds %v, want %v", got, want)
	}
	if p.Size() != 2 {
		t.Errorf("Size=%d, want 2", p.Size())
	}
}

func TestNewPool_ANumberThisMachineHoldsIsNotOfferedToItselfTwice(t *testing.T) {
	// A held number has to be written down as spent exactly like a refused one.
	// The port range makes the pool walk the numbers in order, so one it stepped
	// over and did not write down is the very next number it is handed — and the
	// open runs out of attempts on a range with one busy number in it.
	fake := fakebt.New(t)
	clock := newFakeClock()

	run := freeRun(t, 3)
	lo, held, hi := portOf(run[0]), portOf(run[1]), portOf(run[2])
	closeAll(t, []net.Listener{run[0], run[2]}) // run[1] stays this machine's

	cfg := testPoolConfig(t, fake, clock, 1, 2)
	cfg.ProxyHost = "127.0.0.1"
	cfg.PortRange = [2]int{lo, hi}

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	if got, want := fake.OpenPorts(), []int{lo, hi}; !sameInts(got, want) {
		t.Errorf("the pool holds %v, want %v — %d is held by this machine", got, want, held)
	}
}

func TestNewPool_SaysWhichNumberThisMachineHeldWhenNoneAreLeft(t *testing.T) {
	// Every number in the range is held here. "The range is exhausted" alone
	// reads as a range too small for the pool and sends the operator to widen
	// it; the number that was actually in the way has to come out with it.
	fake := fakebt.New(t)
	clock := newFakeClock()

	run := freeRun(t, 2) // both stay held for the whole test
	lo, hi := portOf(run[0]), portOf(run[1])

	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.ProxyHost = "127.0.0.1"
	cfg.PortRange = [2]int{lo, hi}

	p, err := NewPool(context.Background(), cfg)
	if err == nil {
		p.Close()
		t.Fatal("NewPool came up on a range where this machine holds every number")
	}
	if got := err.Error(); !strings.Contains(got, fmt.Sprintf("port %d is already held", hi)) {
		t.Errorf("error %q does not name a number this machine is holding", got)
	}
	if got := fake.OpenPorts(); len(got) != 0 {
		t.Errorf("the proxy was asked to open %v, and every one of them is held here", got)
	}
}

func TestNewPool_AsksAboutTheAddressThePortsWillLiveOnAndNoOther(t *testing.T) {
	// A number is taken only on the address it is taken on: 127.0.0.1:N and
	// 127.0.0.2:N are two sockets, and this machine holds both at once —
	// measured, on Windows and on Linux. The pool's ports live on ProxyHost, so
	// ProxyHost is what the question has to be asked about; a check nailed to
	// the loopback would step over numbers that are free where the ports are.
	//
	// On a machine with no second loopback address the check falls silent and
	// the number goes through for that reason instead, which this test cannot
	// tell apart — it can only fail when the check is asked about the wrong one.
	fake := fakebt.New(t)
	clock := newFakeClock()

	run := freeRun(t, 1)
	num := portOf(run[0]) // held on 127.0.0.1, and only there

	m := &liveLikeControl{rt: http.DefaultTransport, suggest: []int{num}}
	cfg := poolConfigVia(t, fake, clock, 1, 1, m)
	cfg.ProxyHost = "127.0.0.2"

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	if got, want := fake.OpenPorts(), []int{num}; !sameInts(got, want) {
		t.Errorf("the pool holds %v, want %v: %d is held on 127.0.0.1, and the ports live on 127.0.0.2",
			got, want, num)
	}
}

func TestNewPool_DoesNotJudgeNumbersOnAHostThatIsNotThisMachine(t *testing.T) {
	// The check speaks for one machine: the one it runs on. Point ProxyHost at a
	// proxy running elsewhere and every bind here fails for a reason that has
	// nothing to do with the number. Reading those as "taken" would walk the
	// whole range and open nothing at all — the check has to fall silent
	// instead and let the proxy answer.
	fake := fakebt.New(t)
	clock := newFakeClock()

	run := freeRun(t, 1)
	num := portOf(run[0]) // held here, and here is not where the proxy is

	m := &liveLikeControl{rt: http.DefaultTransport, suggest: []int{num}}
	cfg := poolConfigVia(t, fake, clock, 1, 1, m)
	// TEST-NET-1 (RFC 5737): reserved for documentation, so it is not an address
	// of this machine and nothing can be bound on it.
	cfg.ProxyHost = "192.0.2.1"

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool refused a number it cannot have an opinion about: %v", err)
	}
	defer p.Close()

	if got, want := fake.OpenPorts(), []int{num}; !sameInts(got, want) {
		t.Errorf("the pool holds %v, want %v", got, want)
	}
}

// sameInts reports whether two number lists are equal, order included.
func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeList puts a proxy list where a source can read it, and replaces one that
// is already there.
func writeList(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("writing the list: %v", err)
	}
}

func TestPool_CarriesOneRequestToOneAddressAndLeavesTheLookingToTheTries(t *testing.T) {
	// Where the looking for an address that answers is done, which is the number
	// that decides what a run gets out of a large cheap list.
	//
	// It used to be done inside a single request, which walked one port through
	// fifteen addresses. That is the same looking done twice over: a phrase is
	// already taken to thirty identities, each a fresh port with its own address,
	// and doing it inside the request as well burns addresses fifteen at a time
	// on one port while the rest of the pool sits idle. Measured against a
	// reference client on the same list, which carries a request to one address
	// and moves on: it changed address 0.2 times per result and answered 71% of
	// its requests, against 5.7 and 29% here.
	f := fakebt.New(t)
	clock := newFakeClock()
	p, err := NewPool(context.Background(), testPoolConfig(t, f, clock, 1, 1))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if got := p.hunt(); got != 1 {
		t.Errorf("one request may be carried to %d addresses, want one", got)
	}
}

func TestPool_TakesAHuntBudgetTheCallerNamed(t *testing.T) {
	// An operator who has measured their own list should be able to say how far
	// a request may be carried on it.
	f := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, f, clock, 1, 1)
	cfg.AddressesPerRequest = 3
	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if got := p.hunt(); got != 3 {
		t.Errorf("hunt=%d, want the three that were asked for", got)
	}
}

func TestPoolLeaveAddress_GivesEveryAddressTwoMissesInARow(t *testing.T) {
	// The policy itself, where it lives. On a list where roughly one address in
	// twelve carries anything, a live address answers every time it is asked —
	// 60 of 60, measured — so one miss is a hiccup rather than a verdict, on an
	// address that has answered here and on one that has not yet.
	//
	// It used to drop an address that had not answered on its first miss. An
	// address rotated to a moment ago has not answered either, so that ejected
	// every fresh address on its first miss, whatever it was worth: measured
	// against a reference client on the same list, 5.7 address changes per
	// result against its 0.2, and 29% of requests answered against its 71%.
	f := fakebt.New(t)
	clock := newFakeClock()
	p, err := NewPool(context.Background(), testPoolConfig(t, f, clock, 1, 2))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	proven, unproven := p.ports[0], p.ports[1]
	proven.mu.Lock()
	proven.answered = true
	proven.mu.Unlock()

	if p.leaveAddress(unproven.num) != false {
		t.Error("an address that has not answered yet was given up after one miss")
	}
	if p.leaveAddress(unproven.num) != true {
		t.Error("an address that has not answered was kept through two misses in a row")
	}
	if p.leaveAddress(proven.num) != false {
		t.Error("an address that has answered was given up after one miss")
	}
	if p.leaveAddress(proven.num) != true {
		t.Error("an address that has answered was kept through two misses in a row")
	}

	// And a success clears the count, which is what makes "twice in a row" mean
	// twice in a row rather than twice ever.
	p.attemptSucceeded(proven.num)
	if p.leaveAddress(proven.num) != false {
		t.Error("a miss after a success was treated as the second of a pair")
	}
}

func TestPoolInUse_CountsTheIdentitiesInHand(t *testing.T) {
	// What tells a pool a job is working through from a pool sitting idle. The
	// standing set is tended while nobody is using it and left alone while
	// somebody is, and this is the whole of how it knows which it is.
	f := fakebt.New(t)
	clock := newFakeClock()
	p, err := NewPool(context.Background(), testPoolConfig(t, f, clock, 1, 3))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if got := p.InUse(); got != 0 {
		t.Errorf("InUse=%d on a pool nobody has taken anything from, want 0", got)
	}
	one, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	two, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got := p.InUse(); got != 2 {
		t.Errorf("InUse=%d with two identities in hand, want 2", got)
	}
	one.Release()
	if got := p.InUse(); got != 1 {
		t.Errorf("InUse=%d after one was given back, want 1", got)
	}
	two.Release()
	if got := p.InUse(); got != 0 {
		t.Errorf("InUse=%d after both were given back, want 0", got)
	}
}
