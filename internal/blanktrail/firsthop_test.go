// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

func TestParseFirstHop_ReadsTheWaysAProfileKeepsIt(t *testing.T) {
	cases := []struct {
		kept string
		want FirstHop
	}{
		{"", FirstHop{}},
		{"   ", FirstHop{}},
		{"gw:vless-185", FirstHop{Gateway: "vless-185"}},
		{"socks5://198.51.100.7:2334", FirstHop{Proxy: "socks5://198.51.100.7:2334"}},
		// A proxy written the way a list writes one is read the way a list is:
		// no scheme is SOCKS5, and the login may sit either side of the address.
		{"198.51.100.7:2334", FirstHop{Proxy: "socks5://198.51.100.7:2334"}},
		{"user:secret@198.51.100.7:2334", FirstHop{Proxy: "socks5://user:secret@198.51.100.7:2334"}},
		{"198.51.100.7:2334:user:secret", FirstHop{Proxy: "socks5://user:secret@198.51.100.7:2334"}},
		{"socks5h://198.51.100.7:2334", FirstHop{Proxy: "socks5h://198.51.100.7:2334"}},
	}
	for _, c := range cases {
		got, err := ParseFirstHop(c.kept)
		if err != nil || got != c.want {
			t.Errorf("ParseFirstHop(%q) = %+v, %v; want %+v", c.kept, got, err, c.want)
		}
	}
}

func TestParseFirstHop_RefusesWhatTheServiceCannotChainThrough(t *testing.T) {
	// The service reaches a first hop over SOCKS5 and nothing else: an HTTP
	// proxy named there would be a port that never carries anything, found out
	// an hour into a job rather than when the profile was saved.
	for _, kept := range []string{"http://198.51.100.7:8080", "socks4://198.51.100.7:1080",
		"gw:", "gw:   ", "not an address"} {
		if got, err := ParseFirstHop(kept); err == nil {
			t.Errorf("ParseFirstHop(%q) = %+v with no error", kept, got)
		}
	}
}

func TestFirstHop_IsKeptAsItIsRead(t *testing.T) {
	for _, h := range []FirstHop{{}, {Gateway: "vless-185"}, {Proxy: "socks5://user:secret@198.51.100.7:2334"}} {
		back, err := ParseFirstHop(h.String())
		if err != nil || back != h {
			t.Errorf("%+v kept as %q reads back as %+v, %v", h, h.String(), back, err)
		}
	}
	if (FirstHop{}).String() != "" {
		t.Errorf("no first hop is kept as %q, want nothing", FirstHop{}.String())
	}
}

// openBody is the body of the last port opened on the fake, decoded.
func openBody(t *testing.T, requests []fakebt.Recorded) map[string]any {
	t.Helper()
	var sent map[string]any
	for _, r := range requests {
		if r.Path == "/api/v1/ports/open" {
			if err := json.Unmarshal([]byte(r.Body), &sent); err != nil {
				t.Fatalf("decode recorded open body: %v", err)
			}
		}
	}
	if sent == nil {
		t.Fatal("no request recorded for /api/v1/ports/open")
	}
	return sent
}

func TestClient_OpenPortSendsTheFirstHopItNames(t *testing.T) {
	// The port reaches its address through the first hop from its very first
	// request: a port that met Google once without it has already shown Google
	// the route the first hop is there to avoid.
	cases := []struct {
		hop         FirstHop
		proxy, gate any
	}{
		{FirstHop{Proxy: "socks5://user:secret@198.51.100.7:2334"}, "socks5://user:secret@198.51.100.7:2334", nil},
		{FirstHop{Gateway: "vless-185"}, nil, "vless-185"},
		{FirstHop{}, nil, nil},
	}
	for i, c := range cases {
		cl, fake := newTestClient(t)
		spec := DefaultPortSpec()
		spec.FirstHop = c.hop
		if _, err := cl.OpenPort(context.Background(), 20020+i, spec, Egress{Upstream: "socks5://192.0.2.1:1080"}); err != nil {
			t.Fatalf("OpenPort: %v", err)
		}
		sent := openBody(t, fake.Requests())
		if sent["chain_proxy"] != c.proxy || sent["chain_gateway"] != c.gate {
			t.Errorf("first hop %+v opened with chain_proxy=%v chain_gateway=%v, want %v and %v",
				c.hop, sent["chain_proxy"], sent["chain_gateway"], c.proxy, c.gate)
		}
	}
}

func TestPool_OpensAPortTheServiceLostOnTheFirstHopItWasOpenedOn(t *testing.T) {
	// A restart of the service takes every port with it, and the pool opens each
	// again the moment it finds it gone. It has to come back on the same road: a
	// port reopened straight to its address is the route the first hop was
	// there to avoid, and a job on it goes back to pages nobody can pass.
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(sessionAddrs, WithRest(time.Hour)))}
	cfg.Sessions = true
	cfg.Spec.FirstHop = FirstHop{Gateway: "vless-185"}
	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ctx := context.Background()
	l, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	num := l.Port()
	l.Release()

	fake.Restart()
	opensBefore := len(fake.Requests())
	p.reopenPort(num)
	back, err := p.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("the port was not opened again: %v", err)
	}
	defer back.Release()
	after := fake.Requests()[opensBefore:]
	sent := openBody(t, after)
	if sent["chain_gateway"] != "vless-185" {
		t.Errorf("the port came back with chain_gateway=%v, want the gateway it was opened through", sent["chain_gateway"])
	}
}
