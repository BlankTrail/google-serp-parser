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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// listCheckAddresses is how many are asked about, and listCheckAtOnce how many
// at a time. The sample is taken from across the whole list rather than off the
// top of it: the first thirty of a list are the same thirty every time, and a
// run does not walk them in that order.
var (
	listCheckAddresses = envNumber("GSERP_LIST_SAMPLE", 300)
	listCheckAtOnce    = envNumber("GSERP_LIST_AT_ONCE", 20)
)

// envNumber is a number the environment named, or the fallback.
func envNumber(name string, fallback int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

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

	// From across the whole list, so what comes out is about the list rather
	// than about its first page.
	step := len(ups) / listCheckAddresses
	if step < 1 {
		step = 1
	}
	var sample []Upstream
	for i := 0; i*step < len(ups) && len(sample) < listCheckAddresses; i++ {
		sample = append(sample, ups[i*step])
	}

	count := func(name string, through FirstHop) {
		var mu sync.Mutex
		ok, refused, broke := 0, 0, 0
		var took time.Duration

		work := make(chan Upstream)
		var wg sync.WaitGroup
		for i := 0; i < listCheckAtOnce; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for up := range work {
					at := time.Now()
					res, err := c.TestEgress(ctx, Egress{Upstream: up.URL()}, through, "http")
					spent := time.Since(at)
					good := len(res) > 0
					for _, one := range res {
						if !one.OK {
							good = false
						}
					}
					mu.Lock()
					took += spent
					switch {
					case err != nil:
						broke++
					case good:
						ok++
					default:
						refused++
					}
					mu.Unlock()
				}
			}()
		}
		began := time.Now()
		for _, up := range sample {
			work <- up
		}
		close(work)
		wg.Wait()

		t.Logf("MEASUREMENT %s: %d of %d answered (%d%%), %d refused, %d could not be asked; "+
			"%v an address, %v for the lot at %d at once",
			name, ok, len(sample), 100*ok/max(1, len(sample)), refused, broke,
			(took / time.Duration(max(1, len(sample)))).Round(time.Millisecond),
			time.Since(began).Round(time.Second), listCheckAtOnce)
	}

	count("straight to the address", FirstHop{})
	if hop.IsZero() {
		t.Skip("no first hop is named, so there is only one road to measure")
	}
	count("through the first hop", hop)
}

// portVersusCheck is how many addresses each way is tried on. Each one costs a
// port opened and closed, so this is minutes rather than seconds.
var portVersusCheck = envNumber("GSERP_BOTH_SAMPLE", 20)

func TestLiveList_ComparesTheServicesOwnCheckWithAPortOnTheSameAddress(t *testing.T) {
	// The service's check reaches half the list and a run through ports reaches
	// a fifth of it. Both are the same service, the same addresses and the same
	// road, so one of the two is not measuring what it is taken to measure.
	//
	// This asks both about one address at one moment. What comes out is a table
	// of four: answered both ways, answered neither way, and the two that
	// disagree — and it is the disagreements that say where the fifth went.
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
	if err != nil || len(ups) == 0 {
		t.Skipf("the list did not load: %v", err)
	}
	var hop FirstHop
	if kept := os.Getenv("GSERP_FIRST_HOP"); kept != "" {
		if hop, err = ParseFirstHop(kept); err != nil {
			t.Fatalf("GSERP_FIRST_HOP: %v", err)
		}
		keepOutOps(kept, hop.Proxy)
	}
	ca, err := c.FetchCAPool(ctx)
	if err != nil {
		t.Fatalf("fetching the CA: %v", hideOps(err.Error()))
	}
	spec := DefaultPortSpec()
	spec.FirstHop = hop
	if proto := os.Getenv("GSERP_PROTOCOL"); proto != "" {
		spec.Protocol = proto
	}
	num, err := c.SuggestPort(ctx)
	if err != nil {
		t.Fatalf("asking for a port number: %v", err)
	}
	defer func() { _ = c.ClosePort(context.Background(), num) }()

	step := len(ups) / portVersusCheck
	if step < 1 {
		step = 1
	}
	var bothOK, bothNo, checkOnly, portOnly int
	var unreachable int
	for i := 0; i < portVersusCheck && i*step < len(ups); i++ {
		up := ups[i*step]

		res, err := c.TestEgress(ctx, Egress{Upstream: up.URL()}, hop, "http")
		checked := err == nil && len(res) > 0
		for _, one := range res {
			if !one.OK {
				checked = false
			}
		}

		_ = c.ClosePort(ctx, num)
		if _, err := c.OpenPort(ctx, num, spec, Egress{Upstream: up.URL()}); err != nil {
			t.Fatalf("opening the port: %v", hideOps(err.Error()))
		}
		leg := chainFetch(t, c, ca, spec.Protocol, num)
		ported := leg.ok > 0
		if strings.Contains(leg.last, "523") || strings.Contains(strings.ToLower(leg.last), "unreachable") {
			unreachable++
		}

		switch {
		case checked && ported:
			bothOK++
		case checked:
			checkOnly++
		case ported:
			portOnly++
		default:
			bothNo++
		}
	}
	t.Logf("MEASUREMENT %d addresses asked both ways: %d answered both, %d answered neither, "+
		"%d answered the service's check but not a port on them, %d the other way round "+
		"(%d of the port's refusals named the address unreachable)",
		portVersusCheck, bothOK, bothNo, checkOnly, portOnly, unreachable)
}
