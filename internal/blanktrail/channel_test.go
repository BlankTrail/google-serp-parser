// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestListChannel_HandsOutAndRenewsFromTheList(t *testing.T) {
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2", "socks5")
	ch := NewListChannel("list-a", NewStaticRotor(ups))
	defer ch.Close()

	if ch.Kind() != KindList || ch.Name() != "list-a" {
		t.Fatalf("Kind=%q Name=%q, want list/list-a", ch.Kind(), ch.Name())
	}

	first, ok := ch.Next()
	if !ok {
		t.Fatal("Next returned false on a non-empty list")
	}
	if first.Upstream != "socks5://1.1.1.1:1" {
		t.Errorf("first egress=%q, want the first list entry", first.Upstream)
	}

	renewed, err := ch.Renew(context.Background(), first)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if renewed.Upstream == first.Upstream {
		t.Error("Renew handed back the same upstream; it must advance the list")
	}
}

func TestListChannel_EmptyListReportsFalse(t *testing.T) {
	ch := NewListChannel("empty", NewStaticRotor(nil))
	defer ch.Close()
	if _, ok := ch.Next(); ok {
		t.Error("Next on an empty list returned true")
	}
}

func TestRotatingChannel_HitsRotateURLAndRespectsMinInterval(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	now := time.Unix(1_700_000_000, 0)
	up, _ := Parse("9.9.9.9:1080", "socks5")
	ch := NewRotatingChannel("mobile", up[0], srv.URL, time.Minute,
		WithRotateHTTPClient(srv.Client()),
		WithRotateClock(func() time.Time { return now }))
	defer ch.Close()

	eg, ok := ch.Next()
	if !ok || eg.Upstream != "socks5://9.9.9.9:1080" {
		t.Fatalf("Next=%+v ok=%v, want the fixed entry point", eg, ok)
	}

	if _, err := ch.Renew(context.Background(), eg); err != nil {
		t.Fatalf("first Renew: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("rotate URL hits=%d, want 1", got)
	}

	// A second renewal inside the minimum interval must not hit the URL again:
	// providers rate-limit it and an over-eager caller burns the quota.
	if _, err := ch.Renew(context.Background(), eg); err != nil {
		t.Fatalf("second Renew: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("rotate URL hits=%d after an early second renew, want still 1", got)
	}

	now = now.Add(2 * time.Minute)
	if _, err := ch.Renew(context.Background(), eg); err != nil {
		t.Fatalf("third Renew: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("rotate URL hits=%d after the interval elapsed, want 2", got)
	}
}

func TestRotatingChannel_ReportsRotateFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	up, _ := Parse("9.9.9.9:1080", "socks5")
	ch := NewRotatingChannel("mobile", up[0], srv.URL, 0, WithRotateHTTPClient(srv.Client()))
	defer ch.Close()

	eg, _ := ch.Next()
	if _, err := ch.Renew(context.Background(), eg); err == nil {
		t.Error("Renew returned nil error on a failing rotate URL")
	}
}

func TestGatewayAndDirectChannels_CannotRenew(t *testing.T) {
	gw := NewGatewayChannel("nl", "vless-nl")
	defer gw.Close()
	eg, ok := gw.Next()
	if !ok || eg.Gateway != "vless-nl" {
		t.Fatalf("gateway Next=%+v ok=%v", eg, ok)
	}
	if _, err := gw.Renew(context.Background(), eg); !errors.Is(err, ErrRenewUnsupported) {
		t.Errorf("gateway Renew error=%v, want ErrRenewUnsupported", err)
	}

	d := NewDirectChannel("direct")
	defer d.Close()
	eg, ok = d.Next()
	if !ok || !eg.IsDirect() {
		t.Fatalf("direct Next=%+v ok=%v, want a direct egress", eg, ok)
	}
	if _, err := d.Renew(context.Background(), eg); !errors.Is(err, ErrRenewUnsupported) {
		t.Errorf("direct Renew error=%v, want ErrRenewUnsupported", err)
	}
}

func TestMixer_SpreadsPortsAcrossChannels(t *testing.T) {
	a := NewDirectChannel("a")
	b := NewGatewayChannel("b", "gw-b")
	c := NewGatewayChannel("c", "gw-c")
	m := NewMixer(a, b, c)
	defer m.Close()

	got := m.Assign(6)
	if len(got) != 6 {
		t.Fatalf("Assign(6) returned %d channels, want 6", len(got))
	}
	counts := map[string]int{}
	for _, ch := range got {
		counts[ch.Name()]++
	}
	for _, name := range []string{"a", "b", "c"} {
		if counts[name] != 2 {
			t.Errorf("channel %q got %d ports, want 2 with equal weights", name, counts[name])
		}
	}
}

func TestMixer_FewerPortsThanChannels(t *testing.T) {
	m := NewMixer(NewDirectChannel("a"), NewGatewayChannel("b", "gw"), NewGatewayChannel("c", "gw"))
	defer m.Close()

	got := m.Assign(2)
	if len(got) != 2 {
		t.Fatalf("Assign(2) returned %d, want 2", len(got))
	}
	if got[0].Name() == got[1].Name() {
		t.Errorf("both ports went to %q; distinct channels must be preferred", got[0].Name())
	}
}

func TestMixer_PenaltyExcludesAChannel(t *testing.T) {
	a := NewDirectChannel("a")
	b := NewGatewayChannel("b", "gw-b")
	m := NewMixer(a, b)
	defer m.Close()

	start := m.Weight(b)
	if start <= 0 {
		t.Fatalf("initial weight=%d, want > 0", start)
	}
	for i := 0; i < start; i++ {
		m.Penalise(b)
	}
	if w := m.Weight(b); w != 0 {
		t.Errorf("weight after %d penalties = %d, want 0", start, w)
	}

	healthy := m.Healthy()
	if len(healthy) != 1 || healthy[0].Name() != "a" {
		t.Errorf("Healthy=%v, want only \"a\"", names(healthy))
	}
	for _, ch := range m.Assign(4) {
		if ch.Name() == "b" {
			t.Fatal("Assign handed ports to a dead channel")
		}
	}
}

func TestMixer_RewardRecoversAPenalisedChannel(t *testing.T) {
	b := NewGatewayChannel("b", "gw-b")
	m := NewMixer(NewDirectChannel("a"), b)
	defer m.Close()

	m.Penalise(b)
	lowered := m.Weight(b)
	m.Reward(b)
	if w := m.Weight(b); w <= lowered {
		t.Errorf("weight after Reward = %d, want more than %d", w, lowered)
	}
}

func TestMixer_AllDeadAssignsNothing(t *testing.T) {
	a := NewDirectChannel("a")
	m := NewMixer(a)
	defer m.Close()
	start := m.Weight(a)
	for i := 0; i < start; i++ {
		m.Penalise(a)
	}
	if got := m.Assign(3); len(got) != 0 {
		t.Errorf("Assign with every channel dead = %v, want empty", names(got))
	}
}

func names(chs []Channel) []string {
	out := make([]string, 0, len(chs))
	for _, c := range chs {
		out = append(out, c.Name())
	}
	return out
}

func TestListChannelRenew_TakesTheNextAddressInTurnRatherThanRepeatingOne(t *testing.T) {
	// A rotation is a request for an address that is not the one just left, and
	// the list is what it is for: an address handed out again while others have
	// not been used at all is a list being worked like a shorter one, with the
	// favoured few collecting the attention of the origin while the rest sit
	// there paid for and idle.
	const addresses = 6
	lines := make([]string, 0, addresses)
	for i := range addresses {
		lines = append(lines, fmt.Sprintf("10.0.2.%d:1080", i+1))
	}
	ups, bad := Parse(strings.Join(lines, "\n"), "socks5")
	if len(bad) > 0 {
		t.Fatalf("Parse rejected %v", bad)
	}
	ch := NewListChannel("list", NewStaticRotor(ups))

	// One full turn of the list hands out every address once and no address
	// twice, which is the whole of what even means here.
	seen := map[string]int{}
	for range addresses {
		eg, err := ch.Renew(context.Background(), Egress{})
		if err != nil {
			t.Fatalf("Renew: %v", err)
		}
		seen[eg.Upstream]++
	}
	if len(seen) != addresses {
		t.Errorf("a full turn handed out %d of %d addresses", len(seen), addresses)
	}
	for at, n := range seen {
		if n != 1 {
			t.Errorf("%s was handed out %d times in one turn of the list", at, n)
		}
	}
}
