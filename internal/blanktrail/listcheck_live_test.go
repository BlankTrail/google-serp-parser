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
	"regexp"
	"sort"
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

	// Both roads are asked about one address before the next one is taken up.
	// Measured on this list: two readings twenty seconds apart share half their
	// addresses, so a road measured after the other one is measured on
	// addresses that have since been handed to somebody else — and the two arms
	// would differ by the minutes between them rather than by the road.
	type verdict struct {
		ok   bool
		says string
	}
	ask := func(ctx context.Context, eg Egress, through FirstHop) (verdict, time.Duration) {
		at := time.Now()
		res, err := c.TestEgress(ctx, eg, through, "http")
		took := time.Since(at)
		if err != nil {
			return verdict{says: shapeOf(err.Error())}, took
		}
		out := verdict{ok: len(res) > 0}
		for _, one := range res {
			if !one.OK {
				out.ok = false
				out.says = shapeOf(one.Detail)
			}
		}
		return out, took
	}

	type road struct {
		ok, no int
		took   time.Duration
		why    map[string]int
	}
	roads := map[string]*road{
		"straight to the address": {why: map[string]int{}},
		"through the first hop":   {why: map[string]int{}},
	}
	var mu sync.Mutex
	work := make(chan Upstream)
	var wg sync.WaitGroup
	for i := 0; i < listCheckAtOnce; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for up := range work {
				for _, leg := range []struct {
					name    string
					through FirstHop
				}{
					{"through the first hop", hop},
					{"straight to the address", FirstHop{}},
				} {
					if leg.through.IsZero() && !hop.IsZero() && leg.name == "through the first hop" {
						continue
					}
					got, took := ask(ctx, Egress{Upstream: up.URL()}, leg.through)
					mu.Lock()
					r := roads[leg.name]
					r.took += took
					if got.ok {
						r.ok++
					} else {
						r.no++
						r.why[got.says]++
					}
					mu.Unlock()
				}
			}
		}()
	}
	began := time.Now()
	for _, up := range sample {
		work <- up
	}
	close(work)
	wg.Wait()

	for _, name := range []string{"through the first hop", "straight to the address"} {
		r := roads[name]
		asked := r.ok + r.no
		if asked == 0 {
			continue
		}
		t.Logf("MEASUREMENT %s: %d of %d answered (%d%%), %d did not; %v an address",
			name, r.ok, asked, 100*r.ok/asked, r.no, (r.took / time.Duration(asked)).Round(time.Millisecond))
		for _, one := range byCount(r.why) {
			t.Logf("MEASUREMENT     %4d × %s", one.n, one.what)
		}
	}
	t.Logf("MEASUREMENT the whole of it took %v at %d addresses at once",
		time.Since(began).Round(time.Second), listCheckAtOnce)

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

// shapeOf is what a refusal says, with everything particular to one address
// taken out of it: the addresses themselves, because they are the list and the
// list is not a thing to print, and the numbers that differ between two of the
// same refusal.
func shapeOf(said string) string {
	said = strings.TrimSpace(said)
	if said == "" {
		return "(the service said nothing)"
	}
	said = addressLike.ReplaceAllString(said, "«address»")
	said = digits.ReplaceAllString(said, "N")
	// The same refusal twice over — once about the chain and once about the
	// address — is one shape, so the counts are of kinds rather than of
	// spellings.
	said = strings.ReplaceAll(said, "«address»N", "«address»")
	if len(said) > 150 {
		said = said[:150] + "…"
	}
	return said
}

var (
	addressLike = regexp.MustCompile(`[0-9a-zA-Z.-]+:[0-9]{2,5}`)
	digits      = regexp.MustCompile(`[0-9]{3,}`)
)

// refusalShape is one shape of refusal and how often it came back.
type refusalShape struct {
	what string
	n    int
}

// byCount is the shapes, commonest first.
func byCount(why map[string]int) []refusalShape {
	out := make([]refusalShape, 0, len(why))
	for what, n := range why {
		out = append(out, refusalShape{what: what, n: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].what < out[j].what
	})
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// staleAges are how long after the list was read each batch is asked about.
var staleAges = staleAgesAsked()

// staleAgesAsked is the ages the batches are asked at. The long set is for the
// question the short one cannot answer: whether a list dies all at once at some
// hour of its own rather than fading.
func staleAgesAsked() []time.Duration {
	if envNumber("GSERP_STALE_LONG", 0) == 0 {
		return []time.Duration{0, 30 * time.Second, time.Minute, 2 * time.Minute,
			5 * time.Minute, 10 * time.Minute}
	}
	var out []time.Duration
	for at := 0; at <= envNumber("GSERP_STALE_LONG", 60); at += envNumber("GSERP_STALE_EVERY", 5) {
		out = append(out, time.Duration(at)*time.Minute)
	}
	return out
}

// staleBatch is how many addresses each age is judged on, and staleAtOnce how
// many of them are asked at a time — enough that a batch is answered inside a
// few seconds, so the age it stands for is the age it was asked at.
const (
	staleBatch   = 40
	staleAtOnce  = 20
	staleTimeout = 25 * time.Second
)

func TestLiveList_SaysHowFastAnAddressGoesStaleAfterTheListIsRead(t *testing.T) {
	// A list read twice twenty seconds apart shares half its addresses with
	// itself. So an address is not a place — it is a place for a while, and how
	// long decides everything about how a run should use the list: a profile
	// that re-reads every ten minutes is working from a list that was mostly
	// handed to somebody else nine minutes ago, and every request it sends to
	// one of those comes back "the service could not reach the address".
	//
	// One reading of the list, batches of it asked about at ages from nothing
	// to ten minutes. Each batch is its own addresses, so nothing is asked
	// twice and no address is warmed by the asking.
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
	var hop FirstHop
	if kept := os.Getenv("GSERP_FIRST_HOP"); kept != "" {
		if hop, err = ParseFirstHop(kept); err != nil {
			t.Fatalf("GSERP_FIRST_HOP: %v", err)
		}
		keepOutOps(kept, hop.Proxy)
	}

	read := time.Now()
	ups, _, err := Source{Kind: "url", Location: listURL, DefaultScheme: "socks5"}.Load(ctx)
	if err != nil || len(ups) < staleBatch*len(staleAges) {
		t.Skipf("the list did not load: %v", err)
	}
	t.Logf("MEASUREMENT the list was read at once: %d addresses", len(ups))

	// Disjoint batches, each spread over the whole list rather than taken from
	// one stretch of it: a list handed out in blocks is not the same list at
	// its ends.
	step := len(ups) / (staleBatch * len(staleAges))
	if step < 1 {
		step = 1
	}
	batches := make([][]Upstream, len(staleAges))
	at := 0
	for i := range batches {
		for len(batches[i]) < staleBatch && at < len(ups) {
			batches[i] = append(batches[i], ups[at])
			at += step
		}
	}

	for i, age := range staleAges {
		if wait := time.Until(read.Add(age)); wait > 0 {
			time.Sleep(wait)
		}
		ok, no := 0, 0
		var took time.Duration
		var mu sync.Mutex
		work := make(chan Upstream)
		var wg sync.WaitGroup
		for w := 0; w < staleAtOnce; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for up := range work {
					one, cancel := context.WithTimeout(ctx, staleTimeout)
					began := time.Now()
					res, err := c.TestEgress(one, Egress{Upstream: up.URL()}, hop, "http")
					cancel()
					good := err == nil && len(res) > 0
					for _, r := range res {
						if !r.OK {
							good = false
						}
					}
					mu.Lock()
					took += time.Since(began)
					if good {
						ok++
					} else {
						no++
					}
					mu.Unlock()
				}
			}()
		}
		for _, up := range batches[i] {
			work <- up
		}
		close(work)
		wg.Wait()

		t.Logf("MEASUREMENT %-4v after the list was read: %d of %d answered (%d%%), %v an address",
			age, ok, ok+no, 100*ok/max(1, ok+no), (took / time.Duration(max(1, ok+no))).Round(time.Millisecond))
	}
}

// The shape of a sustained ask: how long it is kept up, how many are in flight
// the whole time, and how often the reading is printed.
var (
	sustainFor    = time.Duration(envNumber("GSERP_SUSTAIN_SECONDS", 300)) * time.Second
	sustainAtOnce = envNumber("GSERP_SUSTAIN_AT_ONCE", 20)
	sustainWindow = 30 * time.Second
)

func TestLiveList_SaysWhatSustainedAskingCostsTheList(t *testing.T) {
	// Forty addresses asked in one burst answer 95 of a hundred. Three hundred
	// asked over four minutes answer ten. Neither the age of an address nor the
	// road explains it — the batches above hold their 95% at every age out to
	// ten minutes — so what is left is the asking itself: how much of it, kept
	// up for how long.
	//
	// This keeps a fixed number of checks in flight and reads the share off
	// each half minute, with the list read afresh every window so nothing here
	// is measuring an address that has aged. A share that starts high and falls
	// is a limit somewhere on the way, and where it settles is what a run at a
	// hundred threads actually gets.
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
	var hop FirstHop
	if kept := os.Getenv("GSERP_FIRST_HOP"); kept != "" {
		if hop, err = ParseFirstHop(kept); err != nil {
			t.Fatalf("GSERP_FIRST_HOP: %v", err)
		}
		keepOutOps(kept, hop.Proxy)
	}

	// A fresh list per window, handed to the askers one address at a time.
	addresses := make(chan Upstream)
	feeding, stopFeeding := context.WithCancel(ctx)
	defer stopFeeding()
	go func() {
		defer close(addresses)
		for {
			ups, _, err := Source{Kind: "url", Location: listURL, DefaultScheme: "socks5"}.Load(feeding)
			if err != nil || len(ups) == 0 {
				return
			}
			read := time.Now()
			for _, up := range ups {
				select {
				case <-feeding.Done():
					return
				case addresses <- up:
				}
				// A list is read again once the one in hand is half a minute
				// old, so no asker is ever given an address older than a window.
				if time.Since(read) > sustainWindow {
					break
				}
			}
		}
	}()

	type window struct{ ok, no int }
	var mu sync.Mutex
	windows := map[int]*window{}
	began := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < sustainAtOnce; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for up := range addresses {
				if time.Since(began) > sustainFor {
					stopFeeding()
					return
				}
				one, cancel := context.WithTimeout(ctx, staleTimeout)
				res, err := c.TestEgress(one, Egress{Upstream: up.URL()}, hop, "http")
				cancel()
				good := err == nil && len(res) > 0
				for _, r := range res {
					if !r.OK {
						good = false
					}
				}
				at := int(time.Since(began) / sustainWindow)
				mu.Lock()
				if windows[at] == nil {
					windows[at] = &window{}
				}
				if good {
					windows[at].ok++
				} else {
					windows[at].no++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	t.Logf("MEASUREMENT %d checks in flight the whole time, the list read afresh every %v:",
		sustainAtOnce, sustainWindow)
	for at := 0; ; at++ {
		w := windows[at]
		if w == nil {
			break
		}
		asked := w.ok + w.no
		t.Logf("MEASUREMENT   %-5v %d of %d answered (%d%%)",
			time.Duration(at)*sustainWindow, w.ok, asked, 100*w.ok/max(1, asked))
	}
}

// crowdSizes are how many requests are put through one address at once.
var crowdSizes = []int{1, 2, 5, 10}

// crowdAddresses is how many addresses the question is asked of, so one bad
// address does not become the answer.
const crowdAddresses = 3

func TestLiveList_SaysHowManyAtOnceOneAddressCarries(t *testing.T) {
	// A profile says how many identities may work through one address at once,
	// and this list is run at ten. Nothing has ever measured what one of these
	// addresses actually carries: everything else about the road checks out —
	// the list answers 95 of a hundred through the hop, at any age, under
	// sustained asking, and a port with the chain does as well as the service's
	// own check — and yet a run at a hundred threads has more than half its
	// requests come back "the service could not reach the address".
	//
	// So: one address, several ports on it, and the same request through all of
	// them at once. If the share falls as the crowd grows, the setting is the
	// answer and it is one number in a form.
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

	most := crowdSizes[len(crowdSizes)-1]
	ports := make([]int, 0, most)
	defer func() {
		for _, num := range ports {
			_ = c.ClosePort(context.Background(), num)
		}
	}()
	for len(ports) < most {
		num, err := c.SuggestPort(ctx)
		if err != nil {
			t.Fatalf("asking for a port number: %v", err)
		}
		ports = append(ports, num)
	}

	for round := 0; round < crowdAddresses; round++ {
		// A fresh address for each round, and every port put on that one.
		ups, _, err := Source{Kind: "url", Location: listURL, DefaultScheme: "socks5"}.Load(ctx)
		if err != nil || len(ups) == 0 {
			t.Skipf("the list did not load: %v", err)
		}
		up := ups[round*37%len(ups)]
		for _, num := range ports {
			_ = c.ClosePort(ctx, num)
			if _, err := c.OpenPort(ctx, num, spec, Egress{Upstream: up.URL()}); err != nil {
				t.Fatalf("opening a port on the address: %v", hideOps(err.Error()))
			}
		}
		// One request through one port first, so a dead address is told from a
		// crowded one before anything is read into the rest.
		if alone := chainFetch(t, c, ca, spec.Protocol, ports[0]); alone.ok == 0 {
			t.Logf("MEASUREMENT round %d: the address answered nothing on its own, so it says nothing "+
				"about a crowd", round+1)
			continue
		}
		for _, crowd := range crowdSizes {
			var mu sync.Mutex
			ok, no := 0, 0
			var took time.Duration
			var wg sync.WaitGroup
			for i := 0; i < crowd; i++ {
				wg.Add(1)
				go func(num int) {
					defer wg.Done()
					at := time.Now()
					leg := chainFetch(t, c, ca, spec.Protocol, num)
					mu.Lock()
					took += time.Since(at)
					ok += leg.ok
					no += leg.failed
					mu.Unlock()
				}(ports[i])
			}
			wg.Wait()
			t.Logf("MEASUREMENT round %d, %2d at once through one address: %d of %d answered (%d%%), %v each",
				round+1, crowd, ok, ok+no, 100*ok/max(1, ok+no),
				(took / time.Duration(max(1, crowd))).Round(time.Millisecond))
		}
	}
}
