// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"net/http"
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

// TestNewPool_DefaultsRequestTimeoutTo300s pins the default so a later edit
// cannot quietly lower the ceiling.
func TestNewPool_DefaultsRequestTimeoutTo300s(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	// RequestTimeout deliberately left zero.

	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	if got := p.cfg.RequestTimeout; got != 300*time.Second {
		t.Errorf("RequestTimeout=%v, want the default 300s", got)
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
