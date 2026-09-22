// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

var sessionAddrs = []Upstream{
	{Scheme: "socks5", Host: "192.0.2.1", Port: "1080"},
	{Scheme: "socks5", Host: "192.0.2.2", Port: "1080"},
	{Scheme: "socks5", Host: "192.0.2.3", Port: "1080"},
}

// sessionPool is a pool of sessions over the three addresses above: ports are
// places, and a session says where each goes out.
func sessionPool(t *testing.T, fake *fakebt.Server, ports int, choose func([]string, int) (string, bool)) (*Pool, *Rotor) {
	t.Helper()
	cfg := testPoolConfig(t, fake, newFakeClock(), ports, 1)
	rotor := NewStaticRotor(sessionAddrs, WithRest(time.Hour))
	cfg.Channels = []Channel{NewListChannel("list", rotor)}
	cfg.Sessions = true
	cfg.Choose = choose
	// A pool of identities keeps this long between two leases of one port. A
	// pool of sessions must not: the pause belongs to the session.
	cfg.Cooldown = time.Minute
	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, rotor
}

func TestSessionPool_KeepsNoPauseOfItsOwnOnAPort(t *testing.T) {
	// The pause is the session's, and the same port carries another session the
	// moment it is free. A port that kept a pause of its own would stand a
	// thread still behind a session that is not even on it any more.
	p, _ := sessionPool(t, fakebt.New(t), 1, nil)
	ctx := context.Background()
	p.PaceAt(5 * time.Second)

	l, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l.Release()
	if _, err := p.TryAcquire(ctx); err != nil {
		t.Errorf("the port just given back is not handed out again: %v", err)
	}
	if p.Cooldown() != 0 {
		t.Errorf("a pool of sessions keeps %v between two leases of a port, want none", p.Cooldown())
	}
	if d := p.NextDelay(); d < 5*time.Second {
		t.Errorf("the pause between two pages is %v, want the job's five seconds", d)
	}
}

func TestSessionPool_MovesAPortToTheAddressItIsGiven(t *testing.T) {
	fake := fakebt.New(t)
	p, _ := sessionPool(t, fake, 1, nil)
	ctx := context.Background()
	l, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	to := sessionAddrs[2].URL()
	if err := l.MoveTo(ctx, to); err != nil {
		t.Fatalf("MoveTo: %v", err)
	}
	if got := fake.UpstreamOf(l.Port()); got != to {
		t.Errorf("the service has the port on %q, want %q", got, to)
	}
	if l.Egress().Upstream != to || l.Exit() != "addr:"+to {
		t.Errorf("the pool records the port on %q (%q), want %q", l.Egress().Upstream, l.Exit(), to)
	}
}

func TestSessionPool_OffersOnlyAddressesItsListHoldsAndThatAreNotResting(t *testing.T) {
	p, rotor := sessionPool(t, fakebt.New(t), 1, nil)
	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	rotor.MarkDead(sessionAddrs[0])
	if l.Offers(sessionAddrs[0].URL()) {
		t.Error("an address resting after failing is offered")
	}
	if !l.Offers(sessionAddrs[1].URL()) {
		t.Error("an address in the list and not resting is not offered")
	}
	if l.Offers("socks5://198.51.100.9:1080") {
		t.Error("an address the list does not hold is offered")
	}
	if got := l.Candidates(); len(got) != 2 || slices.Contains(got, sessionAddrs[0].URL()) {
		t.Errorf("candidates %v, want the two addresses not resting", got)
	}
}

func TestSessionPool_MovesAPortOnwardWhereTheSessionsRuleSays(t *testing.T) {
	// A request an address did not carry is taken to another address, as ever;
	// in a pool of sessions which one is the sessions' decision, and the one it
	// is leaving is never among the choices.
	var offered []string
	choose := func(candidates []string, _ int) (string, bool) {
		offered = append([]string(nil), candidates...)
		return candidates[len(candidates)-1], true
	}
	fake := fakebt.New(t)
	p, _ := sessionPool(t, fake, 1, choose)
	ctx := context.Background()
	l, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()
	from := l.Egress().Upstream

	if err := p.rotateEgress(ctx, l.Port()); err != nil {
		t.Fatalf("rotateEgress: %v", err)
	}
	if len(offered) == 0 || slices.Contains(offered, from) {
		t.Fatalf("the rule was offered %v, which is empty or holds the address being left", offered)
	}
	if want := offered[len(offered)-1]; l.Egress().Upstream != want || fake.UpstreamOf(l.Port()) != want {
		t.Errorf("the port went to %q, want %q, the one the rule picked", l.Egress().Upstream, want)
	}
}

func TestSessionPool_HandsOutAPortWhoseLastAddressIsBusyElsewhere(t *testing.T) {
	// A free port's address is about to change — the session put on it says
	// where it goes — so what that address is carrying elsewhere is no reason
	// to keep the port back. How much an address carries is the sessions'
	// rule to keep, and it keeps it.
	p, _ := sessionPool(t, fakebt.New(t), 2, nil)
	ctx := context.Background()
	a, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer a.Release()
	b, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := b.MoveTo(ctx, a.Egress().Upstream); err != nil {
		t.Fatalf("MoveTo: %v", err)
	}
	b.Release()

	if _, err := p.TryAcquire(ctx); err != nil {
		t.Errorf("a free port was held back for the address it last stood on: %v", err)
	}
}

func TestLease_ReadsTheTemplateItWasOpenedUnder(t *testing.T) {
	cases := []struct {
		spec PortSpec
		want Worn
	}{
		{PortSpec{Browser: "chrome_153", OS: "windows"}, Worn{Browser: "chrome", OS: "windows", Release: 153}},
		{PortSpec{Browser: "firefox", OS: "linux"}, Worn{Browser: "firefox", OS: "linux"}},
	}
	for _, c := range cases {
		if got := wornOf(c.spec); got != c.want {
			t.Errorf("wornOf(%+v) = %+v, want %+v", c.spec, got, c.want)
		}
	}
}

func TestLease_SaysWhenThePortStandsElsewhere(t *testing.T) {
	p, _ := sessionPool(t, fakebt.New(t), 1, nil)
	ctx := context.Background()
	l, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	if elsewhere, err := l.PutTickets(ctx, l.Egress().Upstream, nil); err != nil || elsewhere {
		t.Errorf("on its own address: elsewhere=%v err=%v, want neither", elsewhere, err)
	}
	if elsewhere, _ := l.PutTickets(ctx, "socks5://198.51.100.9:1080", nil); !elsewhere {
		t.Error("named another address and the port did not say it stands elsewhere")
	}
}
