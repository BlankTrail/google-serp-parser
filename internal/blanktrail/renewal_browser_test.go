// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

func TestPool_RenewsAVersionedBrowserOntoTheSameFamily(t *testing.T) {
	for _, browser := range []string{"chrome_153", "firefox_155", "edge_153", "safari_26"} {
		for _, pause := range []time.Duration{0, time.Minute} {
			t.Run(fmt.Sprintf("%s/pause=%s", browser, pause), func(t *testing.T) {
				fake := fakebt.New(t)
				clock := newFakeClock()
				cfg := testPoolConfig(t, fake, clock, 1, 1)
				spec := DefaultPortSpec()
				spec.Browser = browser
				if browser == "safari_26" {
					spec.OS = "macos"
				}
				cfg.Specs = []NamedSpec{{Name: browser, Spec: spec}}
				cfg.RenewAfterRequests = 1
				pool, err := NewPool(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer pool.Close()
				pool.PaceAt(pause)

				lease, err := pool.Acquire(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				port, before := lease.Port(), lease.Session()
				lease.Answered()
				lease.Release()

				// The port has served its one request, so the next acquire renews
				// it, without contacting Google. The proxy returns a valid family
				// such as "chrome".
				clock.Advance(pool.Cooldown())
				lease, err = pool.Acquire(context.Background())
				if err != nil {
					t.Fatalf("acquire after the port had served its count: %v; proxy ports=%v; pool=%+v",
						err, fake.OpenPorts(), pool.Stats())
				}
				if lease.Port() != port || lease.Session() == before {
					t.Errorf("renewal did not replace the identity on port %d", port)
				}
				lease.Answered()
				lease.Release()
				if stats := pool.Stats(); stats.Renewals != 1 || stats.Quarantined != 0 {
					t.Errorf("valid renewal should remain usable: %+v", stats)
				}
				clock.Advance(pause)
				lease, err = pool.Acquire(context.Background())
				if err != nil {
					t.Fatalf("next acquire after renewal: %v", err)
				}
				lease.Release()
				// One rotation for each renewal, and with a renewal owed before
				// each of the two acquires after the first, that is two: the port
				// went on serving through both of them.
				if got := fake.RotateCount(port); got != 2 {
					t.Errorf("rotated %d times, want one for each renewal", got)
				}
			})
		}
	}
}

func TestProfileMatchesSpec_BrowserFamily(t *testing.T) {
	for _, tc := range []struct {
		filter, browser string
		wantErr         bool
	}{
		{"chrome_153", "chrome", false},
		{"Chrome_153", "CHROME", false},
		{"chrome", "chrome", false},
		{"chrome_153", "firefox", true},
		{"chrome_153", "chromium", true},
		{"chrome_bad", "chrome", true},
	} {
		t.Run(tc.filter+"/"+tc.browser, func(t *testing.T) {
			err := profileMatchesSpec(PortSpec{Browser: tc.filter}, Profile{Browser: tc.browser})
			if (err != nil) != tc.wantErr {
				t.Errorf("profileMatchesSpec(%q, %q) = %v", tc.filter, tc.browser, err)
			}
		})
	}
}
