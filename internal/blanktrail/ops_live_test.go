//go:build live

// SPDX-License-Identifier: MIT

package blanktrail

// What one call to the service costs.
//
// A run that keeps its own sessions makes several of these calls around every
// request: the fingerprint onto the port, the address under it, the session's
// tickets on top, and the tickets off again when the session is given back. A
// request measured in seconds can carry that; a call measured in seconds cannot
// be carried by anything, and a hundred threads making four of them a page turn
// a slow call into the whole speed of the run.
//
// So each one is timed on its own here, on a port of this test's own, with
// nothing else running. What comes out is a price list: what the run pays per
// page before Google is asked anything at all.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// opsRounds is how many times each call is made. Enough that one slow answer
// does not become the number, few enough that the whole list takes a minute.
const opsRounds = 15

func TestLiveOps_PricesTheCallsARunMakesAroundEveryRequest(t *testing.T) {
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
	if err != nil || len(ups) < 2 {
		t.Skipf("the list did not load: %v", err)
	}

	num, err := c.SuggestPort(ctx)
	if err != nil {
		t.Fatalf("asking for a port number: %v", err)
	}
	spec := DefaultPortSpec()
	if hop := os.Getenv("GSERP_FIRST_HOP"); hop != "" {
		h, err := ParseFirstHop(hop)
		if err != nil {
			t.Fatalf("GSERP_FIRST_HOP: %v", err)
		}
		keepOutOps(hop, h.Proxy)
		spec.FirstHop = h
	}
	if proto := os.Getenv("GSERP_PROTOCOL"); proto != "" {
		spec.Protocol = proto
	}
	if _, err := c.OpenPort(ctx, num, spec, Egress{Upstream: ups[0].URL()}); err != nil {
		t.Fatalf("opening a port of this test's own: %v", hideOps(err.Error()))
	}
	defer func() {
		if err := c.ClosePort(context.Background(), num); err != nil {
			t.Logf("closing the port: %v", hideOps(err.Error()))
		}
	}()

	// A session's worth of tickets to put on and take off, taken from the port
	// itself so what is timed is the service's work rather than this test's.
	exported, err := c.ExportSession(ctx, num)
	if err != nil {
		t.Logf("reading the port's session: %v", hideOps(err.Error()))
	}
	tickets := exported.Tickets
	if len(tickets) == 0 {
		tickets = json.RawMessage("[]")
	}

	price := func(name string, call func(i int) error) {
		took := make([]time.Duration, 0, opsRounds)
		failed := 0
		for i := 0; i < opsRounds; i++ {
			at := time.Now()
			err := call(i)
			took = append(took, time.Since(at))
			if err != nil {
				failed++
				if failed == 1 {
					t.Logf("MEASUREMENT %s refused: %v", name, hideOps(err.Error()))
				}
			}
		}
		t.Logf("MEASUREMENT %-22s %s%s", name, opsSpread(took),
			map[bool]string{true: "", false: fmt.Sprintf(" — %d of %d refused", failed, opsRounds)}[failed == 0])
	}

	price("health", func(int) error { return c.Health(ctx) })
	price("solver queue", func(int) error { _, err := c.SolverQueue(ctx); return err })
	price("move to an address", func(i int) error {
		return c.SetUpstream(ctx, num, ups[(i+1)%len(ups)].URL())
	})
	price("wear a fingerprint", func(int) error {
		_, err := c.WearSession(ctx, num, "")
		return err
	})
	price("read the session", func(int) error { _, err := c.ExportSession(ctx, num); return err })
	price("put the session on", func(int) error {
		_, err := c.ImportSession(ctx, num, "", tickets)
		return err
	})
	price("the port's profile", func(int) error { _, err := c.PortProfile(ctx, num); return err })
	price("what ports exist", func(int) error { _, err := c.ListPorts(ctx); return err })
}

// opsSpread is the middle and the tail of a set of durations.
func opsSpread(ds []time.Duration) string {
	if len(ds) == 0 {
		return "(none)"
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var total time.Duration
	for _, d := range s {
		total += d
	}
	at := func(q float64) time.Duration { return s[int(q*float64(len(s)-1))].Round(time.Millisecond) }
	return fmt.Sprintf("median %v, 90th %v, longest %v, mean %v (%d calls)",
		at(0.5), at(0.9), s[len(s)-1].Round(time.Millisecond),
		(total / time.Duration(len(s))).Round(time.Millisecond), len(s))
}

// withheldOps holds what must not reach a log: the control address, the key and
// the first hop may each carry a password, and a failure quotes what it failed
// on. Nothing printed by this file is trusted to be free of them.
var withheldOps []string

func keepOutOps(values ...string) {
	for _, v := range values {
		if len(v) > 3 {
			withheldOps = append(withheldOps, v)
		}
	}
}

// hideOps takes the withheld values out of a line.
func hideOps(s string) string {
	for _, v := range withheldOps {
		s = strings.ReplaceAll(s, v, "«withheld»")
	}
	return s
}
