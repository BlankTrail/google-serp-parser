// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// countingTLSOrigin is a TLS origin that answers "ok" and adds one to conns for
// every connection opened to it, so a test can tell reuse from a fresh dial.
//
// It is built unstarted and started by hand because the counter has to be in
// place before anything is served. httptest.NewTLSServer starts accepting
// inside the constructor, and the accept loop reads Config.ConnState — so a
// counter attached to the server it returns is written while that loop reads
// it. The counts would come out right often enough to look fine; what fails is
// the race detector, and it fails the whole package.
func countingTLSOrigin(t *testing.T, conns *atomic.Int64) *httptest.Server {
	t.Helper()
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	origin.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	origin.StartTLS()
	t.Cleanup(origin.Close)
	return origin
}

// TestRotateEgress_DoesNotCarryAConnectionOverToTheNewIdentity is about the one
// thing an egress rotation must leave behind: the connections that were open
// through the address it just left.
//
// A port's transport keeps idle connections alive so a second request costs no
// handshake. Every one of them is a tunnel the proxy opened through the egress
// that was current when it was made. Change the egress and those tunnels do not
// change with it — they still run through the old address, which is usually the
// address that just failed. Handing one back on the next request sends the work
// down the very route the rotation was meant to abandon, and what comes back is
// a torn-down connection: EOF, or a reset. That failure then counts against the
// port, rotates the egress again, and the next request finds another stale
// tunnel — a spiral that produces a great many rotations, almost no answers,
// and no sign of the target ever being reached at all.
//
// The origin here counts connections rather than requests, because reuse is
// exactly what is being measured: a second request that arrives on the same
// connection is a second request that went through the abandoned address.
func TestRotateEgress_DoesNotCarryAConnectionOverToTheNewIdentity(t *testing.T) {
	var conns atomic.Int64
	origin := countingTLSOrigin(t, &conns)

	pool, port := poolOnAStandIn(t, origin.Listener.Addr().String())

	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	ask := func(what string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, origin.URL, nil)
		if err != nil {
			t.Fatalf("building the %s request: %v", what, err)
		}
		resp, err := lease.Do(req)
		if err != nil {
			t.Fatalf("the %s request: %v", what, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	ask("first")
	if got := conns.Load(); got != 1 {
		t.Fatalf("the first request opened %d connections, want one", got)
	}

	// A second request with nothing changed in between reuses the connection —
	// which is the whole point of keeping it, and is what makes the check below
	// mean something.
	ask("second")
	if got := conns.Load(); got != 1 {
		t.Fatalf("a second request with the same identity opened %d connections in all, "+
			"want the one it should have reused", got)
	}

	if err := pool.rotateEgress(context.Background(), port); err != nil {
		t.Fatalf("rotateEgress: %v", err)
	}

	ask("third")
	if got := conns.Load(); got != 2 {
		t.Errorf("after the egress was rotated the request arrived on the old connection "+
			"(%d in all, want a second one): the tunnel still runs through the address "+
			"the rotation abandoned", got)
	}
}

// poolOnAStandIn opens a one-port pool whose port is answered on this machine
// and joined straight to the given origin, so a leased client's traffic really
// travels through it. It returns the pool and the port number.
func poolOnAStandIn(t *testing.T, originAddr string, tune ...func(*PoolConfig)) (*Pool, int) {
	t.Helper()

	fake := fakebt.New(t)
	cl, err := NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	lns := freeRun(t, 1)
	port := portOf(lns[0])
	closeAll(t, lns)

	ups, bad := Parse("192.0.2.1:1080\n192.0.2.2:1080\n192.0.2.3:1080", "socks5")
	if len(bad) > 0 {
		t.Fatalf("Parse rejected %v", bad)
	}

	cfg := PoolConfig{
		Client:         cl,
		Threads:        1,
		PortsPerThread: 1,
		Spec:           DefaultPortSpec(),
		Channels:       []Channel{NewListChannel("list", NewStaticRotor(ups))},
		PortRange:      [2]int{port, port},
		Insecure:       true, // the stand-in origin serves a certificate of its own
		Cooldown:       time.Nanosecond,
	}
	for _, fn := range tune {
		fn(&cfg)
	}

	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("the stand-in could not take port %d back: %v", port, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go standInProxy(ln, originAddr)

	return pool, port
}

// standInProxy answers CONNECT on one of the pool's ports and joins the caller
// to the origin, so the tunnel is real and end-to-end TLS reaches the origin.
// standInProxy answers on one of the pool's ports, in whichever protocol the
// pool dialled it with, and joins the caller to the origin so the tunnel is
// real and end-to-end TLS reaches the origin.
func standInProxy(ln net.Listener, originAddr string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go fakebt.Join(c, originAddr)
	}
}

var _ = tls.Config{}

// TestTrace_SaysWhereARequestWasWhenItStopped proves the trace reports the
// stages a request actually passed, in order, and what it ended with.
//
// It is the only way to tell three faults apart that look identical from
// outside: a tunnel that never opened, one that opened and then heard nothing
// back, and a target that answered slowly. A trace that reported only a total
// would leave all three reading the same.
func TestTrace_SaysWhereARequestWasWhenItStopped(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(origin.Close)

	var seen []RequestTrace
	pool, _ := poolOnAStandIn(t, origin.Listener.Addr().String(), func(cfg *PoolConfig) {
		cfg.Trace = func(tr RequestTrace) { seen = append(seen, tr) }
	})

	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	resp, err := lease.Do(req)
	if err != nil {
		t.Fatalf("the request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if len(seen) != 1 {
		t.Fatalf("%d requests were traced, want the one that was made", len(seen))
	}
	tr := seen[0]
	if tr.Status != http.StatusOK || tr.Err != nil {
		t.Errorf("the trace says status %d, err %v — the request succeeded", tr.Status, tr.Err)
	}
	if tr.Total <= 0 {
		t.Error("the trace gives the request no duration at all")
	}
	// In order, and every stage reached: a request that got an answer went
	// through all of them, and a zero anywhere would mean a stage this trace
	// cannot see.
	if !(tr.Connect > 0 && tr.TLS >= tr.Connect && tr.Wrote >= tr.TLS &&
		tr.FirstByte >= tr.Wrote && tr.Total >= tr.FirstByte) {
		t.Errorf("the stages are not in order or one is missing: connect=%v tls=%v "+
			"wrote=%v firstByte=%v total=%v", tr.Connect, tr.TLS, tr.Wrote, tr.FirstByte, tr.Total)
	}
	if tr.Reused {
		t.Error("the first request on a new port is reported as reusing a connection")
	}
}

// TestNoKeepAlives_GivesEveryRequestAConnectionOfItsOwn is the switch the
// measurement asked for.
//
// The connection this program keeps alive ends at the proxy on this machine and
// not at the address the work travels through, so it goes on looking healthy
// long after the route behind it has died — and the next request is handed a
// tunnel to nowhere, which is time spent waiting on something that cannot
// answer. On a live list, keeping them alive answered a quarter of what opening
// one per request did.
func TestNoKeepAlives_GivesEveryRequestAConnectionOfItsOwn(t *testing.T) {
	var conns atomic.Int64
	origin := countingTLSOrigin(t, &conns)

	pool, _ := poolOnAStandIn(t, origin.Listener.Addr().String(), func(cfg *PoolConfig) {
		cfg.NoKeepAlives = true
	})
	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	for i := range 3 {
		req, err := http.NewRequest(http.MethodGet, origin.URL, nil)
		if err != nil {
			t.Fatalf("building request %d: %v", i+1, err)
		}
		resp, err := lease.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	if got := conns.Load(); got != 3 {
		t.Errorf("three requests arrived on %d connections, want one each: a request "+
			"handed a connection somebody else opened is a request that cannot tell "+
			"whether the route behind it is still there", got)
	}
}
