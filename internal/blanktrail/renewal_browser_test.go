// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

func TestPool_RenewsVersionedBrowserAfterInterval(t *testing.T) {
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
				cfg.RenewAfterInterval = time.Hour
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

				// Advance the identity TTL, without waiting an hour or contacting
				// Google. The proxy returns a valid family such as "chrome".
				clock.Advance(time.Hour)
				lease, err = pool.Acquire(context.Background())
				if err != nil {
					t.Fatalf("acquire after identity TTL: %v; proxy ports=%v; pool=%+v",
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
				if got := fake.RotateCount(port); got != 1 {
					t.Errorf("rotated %d times, want one successful renewal", got)
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
