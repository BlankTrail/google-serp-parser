//go:build live

// SPDX-License-Identifier: MIT

package blanktrail

// Whether a port keeps its first hop when its address is changed.
//
// The first hop travels with the open: chain_proxy goes in the body that opens
// the port. Changing the address afterwards sends the address and nothing else
// — PUT /api/v1/port/{n}/upstream — and what the service does with the chain at
// that moment is not something this program can see from the outside. It
// matters more than anything else in a run that keeps its own sessions, because
// there the address under a port is changed constantly: every session taken
// onto a port standing somewhere else is a move.
//
// The trouble with asking the question naively is that the list answers over
// it. An address that carries nothing after a move may have been dead all
// along, and one that carries everything may be one of the few this machine can
// reach without a hop at all. So the addresses are chosen first: only those the
// service itself cannot reach directly are used. Through the hop they work;
// without it they are silent. On those, a port that answers has its chain and a
// port that does not has lost it, and no third reading is possible.

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// chainTarget answers 204 with nothing in it, so what is measured is whether
// the road carries anything rather than what came back.
const chainTarget = "https://www.google.com/generate_204"

const (
	// chainAddresses is how many addresses each leg is tried on. One address is
	// a coin toss on a list where a fifth of them flicker.
	chainAddresses = 4
	// chainTries is how many fetches each address is judged on.
	chainTries = 3
	// chainCandidates is how far down the list the search for addresses the
	// service cannot reach directly may go.
	chainCandidates = 14
)

func TestLiveChain_SaysWhetherAPortKeepsItsFirstHopWhenTheAddressChanges(t *testing.T) {
	ctx := context.Background()
	control, key := os.Getenv("BLANKTRAIL_URL"), os.Getenv("BLANKTRAIL_API_KEY")
	if control == "" || key == "" {
		t.Skip("BLANKTRAIL_URL and BLANKTRAIL_API_KEY are not set")
	}
	keepOutOps(control, key)
	c, err := NewClient(control, key)
	if err != nil {
		t.Fatalf("control client: %v", err)
	}
	listURL := os.Getenv("GSERP_PROXY_LIST_URL")
	if listURL == "" {
		t.Skip("GSERP_PROXY_LIST_URL is not set")
	}
	ups, _, err := Source{Kind: "url", Location: listURL, DefaultScheme: "socks5"}.Load(ctx)
	if err != nil || len(ups) < chainCandidates {
		t.Skipf("the list did not load: %v", err)
	}
	hop := os.Getenv("GSERP_FIRST_HOP")
	if hop == "" {
		t.Skip("GSERP_FIRST_HOP is not set, so there is no first hop to lose")
	}
	h, err := ParseFirstHop(hop)
	if err != nil {
		t.Fatalf("GSERP_FIRST_HOP: %v", err)
	}
	keepOutOps(hop, h.Proxy)

	// The service can reach the hop itself. Without this the rest means nothing:
	// a chained port that carries nothing would be the hop being unreachable
	// rather than the chain being dropped.
	res, err := c.TestEgress(ctx, Egress{Upstream: h.Proxy}, FirstHop{}, "http")
	if err != nil {
		t.Fatalf("asking the service to go out through the first hop: %v", hideOps(err.Error()))
	}
	for name, one := range res {
		t.Logf("MEASUREMENT the service going out through the first hop (%s): ok=%v %s",
			name, one.OK, hideOps(one.Detail))
		if !one.OK {
			t.Skip("the service cannot use the first hop at all, so nothing below can be read")
		}
	}

	// The addresses this measurement stands on: those the service cannot reach
	// without the hop. On them, carrying anything at all is proof of the chain.
	var silent []Upstream
	for i := 0; i < chainCandidates && len(silent) < chainAddresses; i++ {
		out, err := c.TestEgress(ctx, Egress{Upstream: ups[i].URL()}, FirstHop{}, "http")
		if err != nil {
			continue
		}
		ok := true
		for _, one := range out {
			if !one.OK {
				ok = false
			}
		}
		if !ok {
			silent = append(silent, ups[i])
		}
	}
	t.Logf("MEASUREMENT %d of the first %d addresses answer nothing without the first hop, and are what "+
		"the legs below are measured on", len(silent), chainCandidates)
	if len(silent) < chainAddresses {
		t.Skipf("only %d addresses were silent without the hop; the legs would be read off too few", len(silent))
	}

	ca, err := c.FetchCAPool(ctx)
	if err != nil {
		t.Fatalf("fetching the CA: %v", hideOps(err.Error()))
	}
	spec := DefaultPortSpec()
	spec.FirstHop = h
	if proto := os.Getenv("GSERP_PROTOCOL"); proto != "" {
		spec.Protocol = proto
	}
	num, err := c.SuggestPort(ctx)
	if err != nil {
		t.Fatalf("asking for a port number: %v", err)
	}
	defer func() { _ = c.ClosePort(context.Background(), num) }()

	// Leg one: the port is opened on each address with the chain in the body.
	var opened chainLeg
	for _, up := range silent {
		_ = c.ClosePort(ctx, num)
		if _, err := c.OpenPort(ctx, num, spec, Egress{Upstream: up.URL()}); err != nil {
			t.Fatalf("opening the port: %v", hideOps(err.Error()))
		}
		opened.add(chainFetch(t, c, ca, spec.Protocol, num))
	}
	t.Logf("MEASUREMENT opened with the first hop on each address: %s", opened)

	// Leg two: the port is opened once with the chain and then moved onto each
	// of the same addresses, which is what a session does.
	_ = c.ClosePort(ctx, num)
	if _, err := c.OpenPort(ctx, num, spec, Egress{Upstream: silent[0].URL()}); err != nil {
		t.Fatalf("opening the port for the moves: %v", hideOps(err.Error()))
	}
	var moved chainLeg
	for _, up := range silent {
		if err := c.SetUpstream(ctx, num, up.URL()); err != nil {
			t.Fatalf("moving the port: %v", hideOps(err.Error()))
		}
		moved.add(chainFetch(t, c, ca, spec.Protocol, num))
	}
	t.Logf("MEASUREMENT moved onto each address with PUT /upstream: %s", moved)

	// And the mend, if the first leg is the one that works: the chain said in
	// the move as well, in the fields that carry it when a port is opened.
	var withChain chainLeg
	_ = c.ClosePort(ctx, num)
	if _, err := c.OpenPort(ctx, num, spec, Egress{Upstream: silent[0].URL()}); err != nil {
		t.Fatalf("opening the port for the moves that carry the chain: %v", hideOps(err.Error()))
	}
	for _, up := range silent {
		if err := setUpstreamWithChain(ctx, c, num, up.URL(), h); err != nil {
			t.Logf("moving the port with the chain in the body: %v", hideOps(err.Error()))
			break
		}
		withChain.add(chainFetch(t, c, ca, spec.Protocol, num))
	}
	t.Logf("MEASUREMENT moved with the chain in the body as well: %s", withChain)

	if opened.ok == 0 {
		t.Skip("nothing answered even on a freshly opened port, so the move cannot be judged")
	}
	if moved.ok == 0 {
		t.Logf("MEASUREMENT the first hop does not survive PUT /upstream")
	}
}

// chainLeg is what one leg of the road managed.
type chainLeg struct {
	ok, failed int
	took       time.Duration
	last       string
}

func (l *chainLeg) add(o chainLeg) {
	l.ok += o.ok
	l.failed += o.failed
	l.took += o.took
	if o.last != "" {
		l.last = o.last
	}
}

func (l chainLeg) String() string {
	tries := l.ok + l.failed
	if tries == 0 {
		return "nothing was tried"
	}
	s := fmt.Sprintf("%d of %d answered, %v a try", l.ok, tries,
		(l.took / time.Duration(tries)).Round(time.Millisecond))
	if l.last != "" {
		s += "; last refusal: " + l.last
	}
	return s
}

// chainFetch asks the target through the port a few times and says how it went.
func chainFetch(t *testing.T, c *Client, ca *x509.CertPool, protocol string, port int) chainLeg {
	t.Helper()
	tr := newBaseTransport(c.ControlHost(), port, protocol, ca, false, true)
	defer tr.CloseIdleConnections()
	cl := &http.Client{Transport: tr, Timeout: 20 * time.Second}
	var leg chainLeg
	for i := 0; i < chainTries; i++ {
		at := time.Now()
		req, _ := http.NewRequest(http.MethodGet, chainTarget, nil)
		resp, err := cl.Do(req)
		leg.took += time.Since(at)
		if err != nil {
			leg.failed++
			leg.last = hideOps(err.Error())
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode/100 == 2 || resp.StatusCode/100 == 3 {
			leg.ok++
			continue
		}
		leg.failed++
		leg.last = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return leg
}

// setUpstreamWithChain changes a port's address and says the first hop in the
// same call, in the fields that carry it when a port is opened. It is here
// rather than in the client because whether the service reads them there is the
// question this test is asking.
func setUpstreamWithChain(ctx context.Context, c *Client, port int, upstream string, h FirstHop) error {
	body := map[string]string{
		"upstream":      upstream,
		"chain_proxy":   h.Proxy,
		"chain_gateway": h.Gateway,
	}
	return c.doJSON(ctx, http.MethodPut,
		fmt.Sprintf("/api/v1/port/%d/upstream", port), body, nil)
}
