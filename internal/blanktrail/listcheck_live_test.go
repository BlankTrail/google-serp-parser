//go:build live

// SPDX-License-Identifier: MIT

package blanktrail

// How much of a list answers, along the road its ports will take and along the
// one they will not.
//
// It is the question a reader asks first about a list, and until now nothing
// asked it: the check the service offers takes whatever road the caller names,
// and this program named none. On a list that refuses this machine's own
// address outright, that is the difference between "everything is dead" and
// "most of it answers".

import (
	"context"
	"os"
	"testing"
	"time"
)

// listCheckAddresses is how many are asked about. Enough for a share to mean
// something, few enough that the whole thing takes two minutes.
const listCheckAddresses = 30

func TestLiveList_SaysHowMuchOfTheListAnswersWithAndWithoutTheFirstHop(t *testing.T) {
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
	if err != nil || len(ups) < listCheckAddresses {
		t.Skipf("the list did not load: %v", err)
	}
	var hop FirstHop
	if kept := os.Getenv("GSERP_FIRST_HOP"); kept != "" {
		if hop, err = ParseFirstHop(kept); err != nil {
			t.Fatalf("GSERP_FIRST_HOP: %v", err)
		}
		keepOutOps(kept, hop.Proxy)
	}

	count := func(name string, through FirstHop) {
		ok, refused, broke := 0, 0, 0
		var took time.Duration
		for i := 0; i < listCheckAddresses; i++ {
			at := time.Now()
			res, err := c.TestEgress(ctx, Egress{Upstream: ups[i].URL()}, through, "http")
			took += time.Since(at)
			if err != nil {
				broke++
				continue
			}
			good := len(res) > 0
			for _, one := range res {
				if !one.OK {
					good = false
				}
			}
			if good {
				ok++
			} else {
				refused++
			}
		}
		t.Logf("MEASUREMENT %s: %d of %d answered, %d refused, %d could not be asked; %v an address",
			name, ok, listCheckAddresses, refused, broke,
			(took / listCheckAddresses).Round(time.Millisecond))
	}

	count("straight to the address", FirstHop{})
	if hop.IsZero() {
		t.Skip("no first hop is named, so there is only one road to measure")
	}
	count("through the first hop", hop)
}
