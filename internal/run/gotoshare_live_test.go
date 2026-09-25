//go:build live

// SPDX-License-Identifier: MIT

package run

// Three questions about the hidden addresses — the "/goto?url=…" links a lookup
// has to read — put to the live list before the run is changed on the answers.
//
//   - Whether the same address comes back as the same link: one phrase through
//     five identities on five addresses, and through one of them a second time.
//     If it does, a link read once never has to be read again, and in a job
//     where half the addresses on the pages are repeats, half the lookups go.
//   - How long a link can still be read after it was captured, for anything
//     that would read them later rather than at once.
//   - Whether one port can carry several lookups at once. A lookup set reading
//     one link a port at a time is the ceiling a job with addresses runs into,
//     and ports are what the service has least of.
//
// Run through the helper that fills the environment from the program's own
// settings and default profile, so the ports are made as a job's are:
//
//	python liveenv4.py go test -tags live -count=1 -run TestLiveGoto -timeout 90m ./internal/run/ -v
//
// Nothing printed names a link, an address or a phrase other than the ones
// this file chose. The links and what they led to are written to the file
// GSERP_GOTO_KEEP names, when it names one, for the lifetime check to read.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

// gotoSpec is a port as the default profile makes it, read from what the helper
// put in the environment.
func gotoSpec(t *testing.T) blanktrail.PortSpec {
	t.Helper()
	spec := blanktrail.DefaultPortSpec()
	spec.FirstHop = liveFirstHop(t)
	spec.Protocol = envOr("GSERP_PROTOCOL", spec.Protocol)
	spec.AllowMITMUpstream = envInt("GSERP_PACE_MITM", 1) != 0
	spec.Resolver = envOr("GSERP_PACE_RESOLVER", "")
	spec.VDNSStrictBypass = envInt("GSERP_PACE_STRICT", 0) != 0
	if mode := envOr("GSERP_PACE_VDNS", ""); mode != "" {
		spec.VDNSMode = mode
	}
	spec.JSSolver = envInt("GSERP_PACE_SOLVER", 1) != 0
	return spec
}

// openGotoPool opens ports of one of the two kinds a job with addresses holds:
// searching ones, each keeping a cookie jar of its own so that a port is one
// session, or lookup ones, made as the program makes them — no solver, no jar,
// no pause between two uses.
func openGotoPool(ctx context.Context, t *testing.T, ports int, searching bool) *blanktrail.Pool {
	t.Helper()
	control, key, listURL := liveEnv(t)
	ups := addressList(ctx, t, listURL)
	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		fatalf(t, "control client: %v", err)
	}
	pre := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: []string{"www.google.com", "google.com"}, Ports: ports})
	if !pre.OK() {
		for _, f := range pre.Findings {
			logf(t, "MEASUREMENT preflight: [%s] %s — %s", f.Severity, f.Title, f.Detail)
		}
		t.Skip("preflight refused the run")
	}
	spec := gotoSpec(t)
	ban := time.Duration(envInt("GSERP_BAN_MS", 0)) * time.Millisecond
	cfg := blanktrail.PoolConfig{
		Client: client, Threads: ports, PortsPerThread: 1, Spec: spec, CA: pre.CA,
		Channels:       []blanktrail.Channel{blanktrail.NewListChannel("list", blanktrail.NewStaticRotor(ups, blanktrail.WithRest(ban)))},
		MaxPerUpstream: envInt("GSERP_PER_UPSTREAM", 1),
		ReviveAfter:    time.Minute, WaitForIdentity: true,
	}
	if searching {
		cfg.Spec.KeepSessions = true
	} else {
		cfg.Spec.JSSolver, cfg.Spec.KeepSessions = false, false
		cfg.Cooldown = time.Millisecond
		if n := envInt("GSERP_GOTO_MAX_CONCURRENT", 0); n > 0 {
			cfg.Spec.MaxConcurrent = n
			logf(t, "MEASUREMENT a lookup port holds at most %d requests at once", n)
		}
	}
	p, err := blanktrail.NewPool(ctx, cfg)
	if err != nil {
		fatalf(t, "opening %d ports: %v", ports, err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// captureOn asks one identity for the first page of a phrase.
func captureOn(ctx context.Context, lease *blanktrail.Lease, q google.Query) (google.SERP, time.Duration, error) {
	cl := lease.Client()
	session := google.NewSession(cl.Transport)
	session.Client.Timeout = cl.Timeout
	began := time.Now()
	got, err := google.SearchDepth(ctx, session, q, 1)
	took := time.Since(began)
	if err != nil {
		return google.SERP{}, took, err
	}
	if len(got) == 0 {
		return google.SERP{}, took, errors.New("no page came back")
	}
	return got[0], took, nil
}

// hiddenOf is every link of a page that carries no address, made absolute.
func hiddenOf(serp google.SERP) []string {
	var out []string
	for _, one := range serp.Results {
		if one.Resolved() || one.Link == "" {
			continue
		}
		if link, err := absolute(serp.Origin, one.Link); err == nil {
			out = append(out, link)
		}
	}
	return out
}

// lookedUp is what one read of one link came to.
type lookedUp struct {
	// outcome names it: "site" is the address, "translate" an address behind
	// Google's translator, "sorry" and "stays on Google" a redirect that is not
	// an address, "429" and "200" answers that are not redirects at all, and
	// "no answer" a request that got nothing back.
	outcome string
	dest    string
	took    time.Duration
}

// lookUp reads one link through a lease, following nothing.
func lookUp(ctx context.Context, lease *blanktrail.Lease, link string) lookedUp {
	cl := lease.Client()
	client := &http.Client{Transport: cl.Transport, Timeout: cl.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return lookedUp{outcome: "unbuildable"}
	}
	began := time.Now()
	resp, err := client.Do(req)
	took := time.Since(began)
	if err != nil {
		return lookedUp{outcome: "no answer", took: took}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	out := lookedUp{took: took}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		out.outcome = "429"
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		loc, err := resp.Location()
		if err != nil {
			out.outcome = "redirect to nowhere"
			break
		}
		host := strings.ToLower(loc.Hostname())
		switch {
		case host == "translate.google.com" || strings.HasSuffix(host, ".translate.goog"):
			out.outcome, out.dest = "translate", loc.Query().Get("u")
			if out.dest == "" {
				out.dest = loc.String()
			}
		case strings.HasPrefix(host, "google.") || strings.Contains(host, ".google."):
			out.outcome = "stays on Google"
			if strings.HasPrefix(loc.Path, "/sorry") {
				out.outcome = "sorry"
			}
		default:
			out.outcome, out.dest = "site", loc.String()
		}
	case resp.StatusCode == http.StatusOK:
		out.outcome = "200"
	default:
		out.outcome = "status " + strconv.Itoa(resp.StatusCode)
	}
	return out
}

// readLinks reads links through a pool one lease at a time, as the program
// reads them today, and says what each led to.
func readLinks(ctx context.Context, t *testing.T, pool *blanktrail.Pool, links []string) map[string]lookedUp {
	t.Helper()
	out := make(map[string]lookedUp, len(links))
	var mu sync.Mutex
	var wg sync.WaitGroup
	next := atomic.Int64{}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(links) || ctx.Err() != nil {
					return
				}
				// A link that met a dead address is carried to another one, as
				// the program carries it; what the far end said is keptLink.
				var got lookedUp
				for try := 0; try < 5; try++ {
					lease, err := pool.Acquire(ctx)
					if err != nil {
						return
					}
					got = lookUp(ctx, lease, links[i])
					if got.outcome == "no answer" || strings.HasPrefix(got.outcome, "status 5") {
						_ = lease.Reject(ctx)
						lease.Release()
						continue
					}
					lease.Release()
					break
				}
				mu.Lock()
				out[links[i]] = got
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}

// keptLink is one link written down for the lifetime check.
type keptLink struct {
	At   time.Time `json:"at"`
	Link string    `json:"link"`
	Dest string    `json:"dest"`
}

// sharedEnds is how much of two links' url= values is the same at the start and
// at the end, as parts of the shorter one.
func sharedEnds(a, b string) (prefix, suffix float64) {
	va, vb := valueOf(a), valueOf(b)
	n := min(len(va), len(vb))
	if n == 0 {
		return 0, 0
	}
	p := 0
	for p < n && va[p] == vb[p] {
		p++
	}
	s := 0
	for s < n-p && va[len(va)-1-s] == vb[len(vb)-1-s] {
		s++
	}
	return float64(p) / float64(n), float64(s) / float64(n)
}

// valueOf is the url= parameter of a hidden link, or the whole link.
func valueOf(link string) string {
	if _, v, ok := strings.Cut(link, "url="); ok {
		v, _, _ = strings.Cut(v, "&")
		return v
	}
	return link
}

func TestLiveGoto_TheSameAddressComesBackAsTheSameLink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	identities := envInt("GSERP_GOTO_SESSIONS", 5)
	search := openGotoPool(ctx, t, identities+3, true)
	lookups := openGotoPool(ctx, t, 3, false)

	phrase := envOr("GSERP_GOTO_PHRASE", "купить кондиционер")
	// Country and language as job 12 has them: unset, which is google.com.
	q := google.Query{Text: phrase}
	logf(t, "MEASUREMENT the phrase %q through %d identities, each on an address of its own", phrase, identities)

	type capture struct {
		who   int
		again bool
		at    time.Time
		links []string
	}
	var caps []capture
	var held []*blanktrail.Lease
	defer func() {
		for _, l := range held {
			l.Release()
		}
	}()
	exits := map[string]bool{}
	var skipped []*blanktrail.Lease
	for tries := 0; len(held) < identities && tries < 4*identities; tries++ {
		lease, err := search.Acquire(ctx)
		if err != nil {
			fatalf(t, "acquiring an identity: %v", err)
		}
		if eg := lease.Egress().String(); exits[eg] {
			// Another identity already stands on this address: held aside
			// until the end, so the next acquire hands out a different port.
			skipped = append(skipped, lease)
			continue
		}
		serp, took, err := captureOn(ctx, lease, q)
		if err != nil {
			logf(t, "MEASUREMENT a capture did not get through in %v: %v", took.Round(time.Millisecond), err)
			_ = lease.Reject(ctx)
			lease.Release()
			continue
		}
		links := hiddenOf(serp)
		exits[lease.Egress().String()] = true
		held = append(held, lease)
		caps = append(caps, capture{who: len(held) - 1, at: time.Now(), links: links})
		logf(t, "MEASUREMENT identity %d: %d results, %d of them hidden, captured in %v",
			len(held), len(serp.Results), len(links), took.Round(time.Millisecond))
	}
	for _, l := range skipped {
		l.Release()
	}
	if len(held) < 2 {
		t.Fatal("fewer than two identities carried the phrase")
	}

	// The first identity asked again, after the rest a session takes between
	// two of its requests: the same address, the same cookie jar.
	rest := time.Duration(envInt("GSERP_GOTO_REST", 35)) * time.Second
	time.Sleep(rest)
	if serp, took, err := captureOn(ctx, held[0], q); err != nil {
		logf(t, "MEASUREMENT the second capture on identity 1 did not get through: %v", err)
	} else {
		caps = append(caps, capture{who: 0, again: true, at: time.Now(), links: hiddenOf(serp)})
		logf(t, "MEASUREMENT identity 1 again after %v: %d results, %d of them hidden, captured in %v",
			rest, len(serp.Results), len(caps[len(caps)-1].links), took.Round(time.Millisecond))
	}

	// Every link read through the lookup ports: other addresses, no cookies.
	var all []string
	for _, c := range caps {
		all = append(all, c.links...)
	}
	read := readLinks(ctx, t, lookups, all)
	outcomes := map[string]int{}
	for _, got := range read {
		outcomes[got.outcome]++
	}
	logf(t, "MEASUREMENT %d hidden links read through other addresses without cookies: %s", len(read), tally(outcomes))
	gotoCaught.add(all)

	if path := envOr("GSERP_GOTO_KEEP", ""); path != "" {
		var b strings.Builder
		for _, c := range caps {
			for _, link := range c.links {
				if got := read[link]; got.dest != "" {
					line, _ := json.Marshal(keptLink{At: c.at, Link: link, Dest: got.dest})
					b.Write(line)
					b.WriteByte('\n')
				}
			}
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			fatalf(t, "writing the keptLink links: %v", err)
		}
		logf(t, "MEASUREMENT the links and their addresses are keptLink for the lifetime check")
	}

	// Each capture as address → link.
	byDest := make([]map[string]string, len(caps))
	for i, c := range caps {
		byDest[i] = map[string]string{}
		for _, link := range c.links {
			if got := read[link]; got.dest != "" {
				byDest[i][got.dest] = link
			}
		}
	}
	type pairs struct{ pairs, common, same int }
	var across, again pairs
	var prefixes, suffixes []float64
	for i := range caps {
		for j := i + 1; j < len(caps); j++ {
			into := &across
			if caps[i].who == caps[j].who {
				into = &again
			}
			into.pairs++
			for dest, a := range byDest[i] {
				b, ok := byDest[j][dest]
				if !ok {
					continue
				}
				into.common++
				if a == b {
					into.same++
					continue
				}
				p, s := sharedEnds(a, b)
				prefixes, suffixes = append(prefixes, p), append(suffixes, s)
			}
		}
	}
	logf(t, "MEASUREMENT different identities: %d pairs of captures, %d addresses in common, %d of them under the same link",
		across.pairs, across.common, across.same)
	logf(t, "MEASUREMENT one identity twice: %d pairs, %d addresses in common, %d of them under the same link",
		again.pairs, again.common, again.same)
	if len(prefixes) > 0 {
		logf(t, "MEASUREMENT where one address came under two links, their url= values share %.0f%% at the start and %.0f%% at the end on average (most: %.0f%%, %.0f%%)",
			100*mean(prefixes), 100*mean(suffixes), 100*maxOf(prefixes), 100*maxOf(suffixes))
	}
	// And whether a link is only ever one address: two addresses under one link
	// would make a cache keyed by the link wrong, not merely useless.
	destOf := map[string]string{}
	clash := 0
	for _, c := range caps {
		for _, link := range c.links {
			got := read[link]
			if got.dest == "" {
				continue
			}
			if d, ok := destOf[link]; ok && d != got.dest {
				clash++
			}
			destOf[link] = got.dest
		}
	}
	logf(t, "MEASUREMENT links that led to two different addresses: %d", clash)
}

func TestLiveGoto_ALinkCanStillBeReadLater(t *testing.T) {
	path := envOr("GSERP_GOTO_AGAIN", "")
	if path == "" {
		t.Skip("GSERP_GOTO_AGAIN names no file of keptLink links")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	f, err := os.Open(path)
	if err != nil {
		fatalf(t, "reading the keptLink links: %v", err)
	}
	var old []keptLink
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var k keptLink
		if json.Unmarshal(sc.Bytes(), &k) == nil && k.Link != "" {
			old = append(old, k)
		}
	}
	_ = f.Close()
	if len(old) == 0 {
		t.Skip("the file holds no links")
	}
	lookups := openGotoPool(ctx, t, 3, false)
	links := make([]string, len(old))
	for i, k := range old {
		links[i] = k.Link
	}
	read := readLinks(ctx, t, lookups, links)
	same, other := 0, 0
	outcomes := map[string]int{}
	var ages []float64
	for _, k := range old {
		got := read[k.Link]
		outcomes[got.outcome]++
		ages = append(ages, time.Since(k.At).Minutes())
		switch {
		case got.dest == k.Dest:
			same++
		case got.dest != "":
			other++
		}
	}
	sort.Float64s(ages)
	logf(t, "MEASUREMENT %d links %.0f–%.0f minutes old: %d still lead where they led, %d lead elsewhere; %s",
		len(old), ages[0], ages[len(ages)-1], same, other, tally(outcomes))
}

func TestLiveGoto_OnePortCarriesSeveralLookupsAtOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	links := gotoCaught.get()
	if want := envInt("GSERP_GOTO_LINKS", 80); len(links) < want {
		links = append(links, captureMore(ctx, t, want-len(links))...)
	}
	if len(links) == 0 {
		t.Fatal("no hidden link was captured to read")
	}
	ports := envInt("GSERP_GOTO_PORTS", 5)
	perPort := envInt("GSERP_GOTO_PER_PORT", 40)
	var levels []int
	for _, one := range strings.Split(envOr("GSERP_GOTO_LEVELS", "1,2,5,10"), ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(one)); err == nil && n > 0 {
			levels = append(levels, n)
		}
	}
	lookups := openGotoPool(ctx, t, ports, false)

	// The same ports for every level, held from the first to the last, so the
	// levels differ in how many lookups each port carries at once and in
	// nothing else: not in the addresses behind them.
	var held []*blanktrail.Lease
	for len(held) < ports {
		lease, err := lookups.Acquire(ctx)
		if err != nil {
			fatalf(t, "acquiring a lookup port: %v", err)
		}
		held = append(held, lease)
	}
	defer func() {
		for _, l := range held {
			l.Release()
		}
	}()
	logf(t, "MEASUREMENT %d distinct hidden links, read through %d ports, %d lookups a port at each of %v at once",
		len(links), ports, perPort, levels)

	cursor := atomic.Int64{}
	for _, at := range levels {
		var mu sync.Mutex
		var tooks []time.Duration
		outcomes := map[string]int{}
		perLease := make([]map[string]int, len(held))
		began := time.Now()
		var wg sync.WaitGroup
		for i, lease := range held {
			perLease[i] = map[string]int{}
			var done atomic.Int64
			for w := 0; w < at; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for done.Add(1) <= int64(perPort) && ctx.Err() == nil {
						link := links[int(cursor.Add(1)-1)%len(links)]
						got := lookUp(ctx, lease, link)
						mu.Lock()
						tooks = append(tooks, got.took)
						outcomes[got.outcome]++
						perLease[i][got.outcome]++
						mu.Unlock()
					}
				}()
			}
		}
		wg.Wait()
		wall := time.Since(began)
		sort.Slice(tooks, func(a, b int) bool { return tooks[a] < tooks[b] })
		n := len(tooks)
		if n == 0 {
			continue
		}
		walled := 0
		for _, one := range perLease {
			if one["sorry"]+one["429"]+one["stays on Google"]+one["200"] > 0 {
				walled++
			}
		}
		logf(t, "MEASUREMENT %2d at once a port: %d lookups in %v, %.0f a minute a port; median %v, p90 %v, slowest %v; %s; ports that met a refusal: %d of %d",
			at, n, wall.Round(time.Millisecond), float64(n)/float64(len(held))/wall.Minutes(),
			tooks[n/2].Round(time.Millisecond), tooks[n*9/10].Round(time.Millisecond), tooks[n-1].Round(time.Millisecond),
			tally(outcomes), walled, len(held))
	}
}

// gotoCaught is the links the capture test read, handed on to the test that
// reads links many at a time when both run in one process.
var gotoCaught caught

type caught struct {
	mu    sync.Mutex
	links []string
	seen  map[string]bool
}

func (c *caught) add(links []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	for _, l := range links {
		if !c.seen[l] {
			c.seen[l] = true
			c.links = append(c.links, l)
		}
	}
}

func (c *caught) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.links...)
}

// captureMore captures pages of neutral phrases, one identity each, until it
// holds at least want hidden links.
func captureMore(ctx context.Context, t *testing.T, want int) []string {
	t.Helper()
	search := openGotoPool(ctx, t, 6, true)
	var out []string
	for i := 0; len(out) < want && i < len(pacePhrases) && i < 24; i++ {
		lease, err := search.Acquire(ctx)
		if err != nil {
			fatalf(t, "acquiring an identity: %v", err)
		}
		serp, took, err := captureOn(ctx, lease, google.Query{Text: pacePhrases[i]})
		if err != nil {
			logf(t, "MEASUREMENT a capture did not get through in %v: %v", took.Round(time.Millisecond), err)
			_ = lease.Reject(ctx)
			lease.Release()
			continue
		}
		lease.Release()
		out = append(out, hiddenOf(serp)...)
	}
	logf(t, "MEASUREMENT %d hidden links captured for reading", len(out))
	return out
}

// tally writes a count of outcomes in a stable order.
func tally(counts map[string]int) string {
	var keys []string
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func maxOf(xs []float64) float64 {
	m := 0.0
	for _, x := range xs {
		m = max(m, x)
	}
	return m
}
