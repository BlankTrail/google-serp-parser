// SPDX-License-Identifier: MIT

package run

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// serpBody is the smallest page ParseSERP accepts, carrying one result.
func serpBody(host string) string {
	return `<!doctype html><html><body><div id="search"><div data-snc="x">` +
		`<a href="https://` + host + `/page" data-ved="2"><h3>Title</h3></a>` +
		`<cite>` + host + `</cite></div></div></body></html>`
}

// serpBodyWithBar is a result page whose pagination bar links further than the
// page in hand. Without a bar reaching onwards a walk stops on the first page,
// and a test of what a multi-page walk costs would measure a single page.
func serpBodyWithBar(host string) string {
	return serpBody(host) + `<div role="navigation"><a href="/search?q=x&amp;start=90">10</a></div>`
}

// shellBody is the page that carries no results at all.
const shellBody = `<!doctype html><html><body><div id="main"></div></body></html>`

// emptyBody is Google stating it found nothing. A successful capture.
const emptyBody = `<!doctype html><html><body><div id="search"></div>` +
	`<div>did not match any documents</div></body></html>`

// origin stands in for the search host. It counts the front page and the
// searches apart, because a session visits the front page before its first
// search and a test that lumped the two together could not see whether the
// session was kept or rebuilt.
type origin struct {
	*httptest.Server
	homes    atomic.Int64
	searches atomic.Int64
}

// newOrigin serves TLS, because that is the scheme a search request is built
// with and the request has to arrive as it would in a run.
//
// The search callback is handed the request as well as the count, so a test
// that has to tell one query's answer from another can answer them differently.
func newOrigin(t *testing.T, search func(r *http.Request, n int) string) *origin {
	t.Helper()
	o := &origin{}
	o.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		if !strings.HasPrefix(r.URL.Path, "/search") {
			o.homes.Add(1)
			_, _ = io.WriteString(w, shellBody)
			return
		}
		_, _ = io.WriteString(w, search(r, int(o.searches.Add(1))))
	}))
	t.Cleanup(o.Close)
	return o
}

func (o *origin) addr() string { return o.Listener.Addr().String() }

// facing is a pool whose ports are answered in this process, together with a
// record of which of them carried a request. Without that record a test cannot
// tell a query taken to another port from the same port asked twice.
type facing struct {
	Pool *blanktrail.Pool

	mu     sync.Mutex
	byPort map[int]int
}

func (f *facing) carried(port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byPort[port]++
}

// portsUsed is how many distinct ports carried at least one request.
func (f *facing) portsUsed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byPort)
}

// instantSleep is the clock seam the pool offers, wound forward. Every pause a
// test would otherwise sit through is a pause it does not need to measure.
func instantSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// poolFacing opens a pool of ports whose clients reach originAddr.
//
// The port numbers are chosen here rather than left to the proxy, because
// something in this process has to be listening on each of them: a leased
// client sends its request to the port, and a port nobody answers reaches
// nothing at all.
func poolFacing(t *testing.T, originAddr string, ports int, tune ...func(*blanktrail.PoolConfig)) *facing {
	t.Helper()

	fake := fakebt.New(t)
	cl, err := blanktrail.NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	f := &facing{byPort: map[int]int{}}
	// Reserving the numbers is what makes them a free consecutive run; holding
	// them while the pool opens would defeat it. NewPool asks this machine
	// whether a number is free before it asks the proxy to stand on it, because
	// a number something here already holds is one the proxy reports "opened"
	// for and nothing ever answers on. So the reservation is let go before the
	// pool looks, and the stand-ins take the same numbers back once the pool
	// holds them — which is also the order the real thing runs in: the proxy
	// binds the port when it is opened, and not before.
	lns := reserveConsecutive(t, ports)
	first := lns[0].Addr().(*net.TCPAddr).Port
	for _, ln := range lns {
		_ = ln.Close()
	}

	ups, bad := blanktrail.Parse("192.0.2.1:1080\n192.0.2.2:1080\n192.0.2.3:1080\n192.0.2.4:1080", "socks5")
	if len(bad) > 0 {
		t.Fatalf("Parse rejected %v", bad)
	}

	cfg := blanktrail.PoolConfig{
		Client:           cl,
		Threads:          ports,
		PortsPerThread:   1,
		Spec:             blanktrail.DefaultPortSpec(),
		Channels:         []blanktrail.Channel{blanktrail.NewListChannel("list", blanktrail.NewStaticRotor(ups))},
		PortRange:        [2]int{first, first + ports - 1},
		Insecure:         true, // the stand-in origin serves a certificate of its own
		Cooldown:         time.Nanosecond,
		MaxRetriesPerReq: 1,
		Sleep:            instantSleep,
	}
	for _, fn := range tune {
		fn(&cfg)
	}

	p, err := blanktrail.NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	for i := 0; i < ports; i++ {
		port := first + i
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatalf("the stand-in for port %d could not take the number back: %v", port, err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go serveStandIn(ln, originAddr, func() { f.carried(port) })
	}

	f.Pool = p
	return f
}

// reserveConsecutive holds a run of consecutive loopback ports, which is what
// the pool can be pointed at: it takes a range and opens the first free ports
// inside it.
//
// The run is searched for in a fixed band rather than taken from the numbers
// the operating system hands out on its own. Those are the numbers it also
// gives to outgoing connections, and this run is let go of before the pool
// opens on it: on Windows a number released at 61000 was handed straight to one
// of the pool's own control-API connections and was gone by the time the pool
// asked for it. 31000–32000 is below both the Windows dynamic range and the
// Linux one, and above the band the product's own proxies live in. The pool's
// own tests search a band below this one, so two packages under test at once do
// not take numbers from each other.
func reserveConsecutive(t *testing.T, n int) []net.Listener {
	t.Helper()
	for base := 31000; base+n <= 32000; base += n {
		lns := make([]net.Listener, 0, n)
		for i := 0; i < n; i++ {
			ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(base+i)))
			if err != nil {
				break
			}
			lns = append(lns, ln)
		}
		if len(lns) == n {
			t.Cleanup(func() {
				for _, ln := range lns {
					_ = ln.Close()
				}
			})
			return lns
		}
		for _, ln := range lns {
			_ = ln.Close()
		}
	}
	t.Fatalf("could not reserve %d consecutive loopback ports", n)
	return nil
}

// serveStandIn answers on one of the pool's ports. A leased client asks for a
// tunnel and then speaks TLS through it end to end, so joining the caller
// straight to the test origin delivers the whole request there and nowhere
// else.
func serveStandIn(ln net.Listener, originAddr string, carried func()) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		carried()
		go joinToOrigin(c, originAddr)
	}
}

func joinToOrigin(c net.Conn, originAddr string) {
	defer func() { _ = c.Close() }()

	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		return
	}
	up, err := net.Dial("tcp", originAddr)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer func() { _ = up.Close() }()

	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	go func() {
		_, _ = io.Copy(up, br)
		_ = up.Close()
	}()
	_, _ = io.Copy(c, up)
}

// exhaustedPool opens a pool whose only port is already spent, so an acquire
// has nothing left to hand out.
func exhaustedPool(t *testing.T) *blanktrail.Pool {
	t.Helper()

	fake := fakebt.New(t)
	cl, err := blanktrail.NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	p, err := blanktrail.NewPool(context.Background(), blanktrail.PoolConfig{
		Client:              cl,
		Threads:             1,
		PortsPerThread:      1,
		Spec:                blanktrail.DefaultPortSpec(),
		Channels:            []blanktrail.Channel{blanktrail.NewDirectChannel("direct")},
		Insecure:            true,
		Cooldown:            time.Nanosecond,
		RotateAfterFailures: 1,
		MaxPortStrikes:      1,
		ReviveAfter:         -1,
		Sleep:               instantSleep,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	lease, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := lease.Reject(context.Background()); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	lease.Release()

	if _, err := p.Acquire(context.Background()); !errors.Is(err, blanktrail.ErrPoolExhausted) {
		t.Fatalf("the pool still has a port to hand out (Acquire: %v)", err)
	}
	return p
}

func usQuery(text string) google.Query {
	return google.Query{Text: text, Country: "us", Language: "en"}
}

func TestAttempt_TakesARefusedQueryToAnotherIdentity(t *testing.T) {
	// A refusal answers the identity that sent the request, not the query.
	// Asking again from the same one spends a try to be told the same thing.
	o := newOrigin(t, func(_ *http.Request, n int) string {
		if n == 1 {
			return shellBody
		}
		return serpBody("example.com")
	})
	f := poolFacing(t, o.addr(), 2)

	a := &Attempt{Pool: f.Pool, Tries: 3}
	serp, err := a.Search(context.Background(), usQuery("x"))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(serp.Results) == 0 {
		t.Fatal("the retry produced no results")
	}
	if got := o.searches.Load(); got != 2 {
		t.Errorf("%d searches, want 2 - the refused one to have been asked again", got)
	}
	if got := f.portsUsed(); got != 2 {
		t.Errorf("%d ports carried the query, want 2 - it was asked again from the one that refused it", got)
	}
	if r := f.Pool.Stats().Rejections; r != 1 {
		t.Errorf("Stats().Rejections=%d, want 1 - the refusal was not reported to the pool", r)
	}
}

func TestAttempt_GivesUpAfterTheTriesItWasAllowed(t *testing.T) {
	// Without a ceiling a permanently refused query walks the whole pool, and
	// on a job of thousands that is the entire budget spent on one query.
	o := newOrigin(t, func(*http.Request, int) string { return shellBody })
	f := poolFacing(t, o.addr(), 4)

	// A ceiling that is not enforced shows up as a search that never comes
	// back, so this one is bounded too: it has to end by giving up, and the
	// deadline is there only to make the other outcome visible.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a := &Attempt{Pool: f.Pool, Tries: 2}
	_, err := a.Search(ctx, usQuery("x"))
	if !errors.Is(err, ErrNoIdentityLeft) {
		t.Fatalf("Search returned %v, want it to wrap ErrNoIdentityLeft", err)
	}
	if !strings.Contains(err.Error(), string(google.ClassShell)) {
		t.Errorf("error %q does not say what the last answer was", err)
	}
	if got := o.searches.Load(); got != 2 {
		t.Errorf("%d searches, want exactly the 2 tries allowed", got)
	}
	if r := f.Pool.Stats().Rejections; r != 2 {
		t.Errorf("Stats().Rejections=%d, want 2 - a refusal went unreported", r)
	}
}

func TestAttempt_DoesNotRetryAnHonestEmptyAnswer(t *testing.T) {
	// An empty answer is a result. Retrying it turns "this page is not indexed"
	// into a walk of the whole pool, and the index check is the feature that
	// asks that question.
	o := newOrigin(t, func(*http.Request, int) string { return emptyBody })
	f := poolFacing(t, o.addr(), 3)

	a := &Attempt{Pool: f.Pool, Tries: 3}
	serp, err := a.Search(context.Background(), usQuery("zzqqxx"))
	if err != nil {
		t.Fatalf("an empty answer was treated as a failure: %v", err)
	}
	if len(serp.Results) != 0 {
		t.Errorf("Results=%d, want none", len(serp.Results))
	}
	if got := o.searches.Load(); got != 1 {
		t.Errorf("%d searches, want 1 - an empty answer is an answer", got)
	}
	if r := f.Pool.Stats().Rejections; r != 0 {
		t.Errorf("Stats().Rejections=%d, want 0 - an answer was penalised", r)
	}
}

func TestAttempt_DoesNotPenaliseAPortForATransportFailure(t *testing.T) {
	// The pool already accounted for a request that never completed, because it
	// carried that request itself. Counting it again here would spend two of
	// the port's chances on one failure.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	f := poolFacing(t, srv.Listener.Addr().String(), 2)

	a := &Attempt{Pool: f.Pool, Tries: 2}
	if _, err := a.Search(context.Background(), usQuery("x")); err == nil {
		t.Fatal("a broken connection was reported as a result")
	}
	if r := f.Pool.Stats().Rejections; r != 0 {
		t.Errorf("Stats().Rejections=%d, want 0 - nothing was classified, so nothing was refused", r)
	}
}

func TestAttempt_SaysThePoolIsEmptyRatherThanBlamingTheQuery(t *testing.T) {
	// "Every identity refused this query" and "there were no identities left"
	// send a reader to different places. Reporting the first for the second
	// sends them to their query list when the answer is in their address list.
	a := &Attempt{Pool: exhaustedPool(t), Tries: 3}
	_, err := a.Search(context.Background(), usQuery("x"))
	if !errors.Is(err, blanktrail.ErrPoolExhausted) {
		t.Errorf("Search returned %v, want ErrPoolExhausted", err)
	}
	if errors.Is(err, ErrNoIdentityLeft) {
		t.Errorf("Search returned %v, which blames the query for an empty pool", err)
	}

	// A page walk reads the same way. It is the entry point a job uses, so the
	// wrong answer here is the one a user would actually be handed.
	_, err = a.Walk(context.Background(), usQuery("x"), 3)
	if !errors.Is(err, blanktrail.ErrPoolExhausted) {
		t.Errorf("Walk returned %v, want ErrPoolExhausted", err)
	}
	if errors.Is(err, ErrNoIdentityLeft) {
		t.Errorf("Walk returned %v, which blames the query for an empty pool", err)
	}
}

func TestAttempt_TakesANonPositiveDepthAsOnePage(t *testing.T) {
	// A depth of zero is a caller who did not say how deep, not a caller who
	// wants nothing. Passing it down would come back as a bad range, and the
	// walk would report the query refused by every identity it never asked.
	o := newOrigin(t, func(*http.Request, int) string { return serpBodyWithBar("example.com") })
	f := poolFacing(t, o.addr(), 2)

	a := &Attempt{Pool: f.Pool, Tries: 2}
	serps, err := a.Walk(context.Background(), usQuery("x"), 0)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(serps) != 1 {
		t.Errorf("collected %d pages, want the one page a walk that named no depth is worth", len(serps))
	}
}

func TestAttempt_StopsWhenTheContextIsCancelled(t *testing.T) {
	o := newOrigin(t, func(*http.Request, int) string { return shellBody })
	f := poolFacing(t, o.addr(), 2)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a := &Attempt{Pool: f.Pool, Tries: 5}
	if _, err := a.Search(ctx, usQuery("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("Search returned %v, want the cancellation", err)
	}
	if got := o.searches.Load(); got != 0 {
		t.Errorf("%d searches, want none after the caller had already gone", got)
	}
}

func TestAttempt_KeepsTheRequestBoundThePoolWasGiven(t *testing.T) {
	// A request that never comes back is this identity being unreachable, not
	// the caller changing its mind: the query is still worth taking to another
	// identity, and a search with no deadline of its own must not wait forever
	// to find that out.
	o := newOrigin(t, func(*http.Request, int) string {
		time.Sleep(300 * time.Millisecond)
		return serpBody("example.com")
	})
	f := poolFacing(t, o.addr(), 2, func(c *blanktrail.PoolConfig) {
		c.RequestTimeout = 20 * time.Millisecond
	})

	a := &Attempt{Pool: f.Pool, Tries: 2}
	_, err := a.Search(context.Background(), usQuery("x"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Search returned %v, want the request bound to have ended it", err)
	}
	if !errors.Is(err, ErrNoIdentityLeft) {
		t.Errorf("Search returned %v, want the query to have been taken to every try it was allowed", err)
	}
}

func TestAttempt_ReusesOnePortsSessionAcrossQueries(t *testing.T) {
	// The first search of a session visits the front page, so that the search is
	// a navigation from somewhere. A session built fresh for every query pays
	// that visit every time: two requests where one was needed, on every query
	// of the job.
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 1)

	a := &Attempt{Pool: f.Pool, Tries: 2}
	for i := 0; i < 3; i++ {
		if _, err := a.Search(context.Background(), usQuery("x")); err != nil {
			t.Fatalf("search %d: %v", i+1, err)
		}
	}

	if got := o.searches.Load(); got != 3 {
		t.Errorf("%d searches, want 3", got)
	}
	if got := o.homes.Load(); got != 1 {
		t.Errorf("%d visits to the front page, want 1 - the session was rebuilt", got)
	}
}

func TestAttempt_DropsASessionWhenThePortsIdentityChanges(t *testing.T) {
	// A session belongs to the identity it was opened under. Handing it to a
	// port whose identity has since changed presents one visitor who changed
	// underneath themselves.
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 1, func(c *blanktrail.PoolConfig) {
		c.RotateAfterFailures = 1
	})

	a := &Attempt{Pool: f.Pool, Tries: 1}
	if _, err := a.Search(context.Background(), usQuery("x")); err != nil {
		t.Fatalf("first search: %v", err)
	}

	lease, err := f.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	was := lease.Session()
	if err := lease.Reject(context.Background()); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	now := lease.Session()
	lease.Release()
	if now == was {
		t.Fatalf("the port still answers to %q, so this test cannot see a stale session", was)
	}

	if _, err := a.Search(context.Background(), usQuery("x")); err != nil {
		t.Fatalf("second search: %v", err)
	}
	if got := o.homes.Load(); got != 2 {
		t.Errorf("%d visits to the front page, want 2 - the stale session was reused", got)
	}
}

func TestAttempt_APageWalkKeepsOneIdentityForItsWholeDepth(t *testing.T) {
	// A visitor paging through results does not change address between page one
	// and page two. A second page arriving from an identity with nothing behind
	// it is the shape this layer exists to avoid drawing, and it also throws away
	// the referrer chain and pays a fresh visit to the front page per page.
	o := newOrigin(t, func(*http.Request, int) string { return serpBodyWithBar("example.com") })
	f := poolFacing(t, o.addr(), 4)

	a := &Attempt{Pool: f.Pool, Tries: 2}
	serps, err := a.Walk(context.Background(), usQuery("x"), 3)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(serps) != 3 {
		t.Fatalf("collected %d pages, want the 3 that were asked for", len(serps))
	}
	if got := o.searches.Load(); got != 3 {
		t.Errorf("%d searches, want 3", got)
	}
	if got := f.portsUsed(); got != 1 {
		t.Errorf("%d identities carried one walk, want 1", got)
	}
	if got := o.homes.Load(); got != 1 {
		t.Errorf("%d visits to the front page over one walk, want 1 - the walk changed identity", got)
	}
}

func TestAttempt_APageWalkThePageItselfEndedIsFinishedNotInterrupted(t *testing.T) {
	// The bar on these pages offers nothing past page two, so the walk ends
	// there of its own accord. Reading a walk shorter than the depth asked for
	// as one that was cut off would carry the rest to another identity, spend
	// every try on pages Google has said are not there, and then report the
	// query as failed.
	o := newOrigin(t, func(*http.Request, int) string {
		return serpBody("example.com") +
			`<div role="navigation"><a href="/search?q=x&amp;start=10">2</a></div>`
	})
	f := poolFacing(t, o.addr(), 4)

	a := &Attempt{Pool: f.Pool, Tries: 3}
	serps, err := a.Walk(context.Background(), usQuery("x"), 5)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(serps) != 2 {
		t.Errorf("collected %d pages, want the 2 the bar offers", len(serps))
	}
	if got := o.searches.Load(); got != 2 {
		t.Errorf("%d searches, want 2 - the depth that was not there was asked for anyway", got)
	}
	if got := f.portsUsed(); got != 1 {
		t.Errorf("%d identities carried the walk, want 1", got)
	}
}

func TestAttempt_APageWalkThatIsRefusedResumesOnTheNextIdentity(t *testing.T) {
	// Starting the walk over on the new identity would pay for the pages already
	// captured a second time; giving up on it would lose them.
	var mu sync.Mutex
	var offsets []string
	o := newOrigin(t, func(r *http.Request, n int) string {
		mu.Lock()
		offsets = append(offsets, r.URL.Query().Get("start"))
		mu.Unlock()
		if n == 2 {
			return shellBody
		}
		return serpBodyWithBar("example.com")
	})
	f := poolFacing(t, o.addr(), 4)

	a := &Attempt{Pool: f.Pool, Tries: 3}
	serps, err := a.Walk(context.Background(), usQuery("x"), 3)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	mu.Lock()
	// The second page was refused, so it is the one the walk comes back to. An
	// empty offset is the first page: the request carries no start at all.
	want := []string{"", "10", "10", "20"}
	got := append([]string(nil), offsets...)
	mu.Unlock()
	if !slices.Equal(got, want) {
		t.Errorf("pages asked for at offsets %v, want %v", got, want)
	}

	if len(serps) != 3 {
		t.Errorf("collected %d pages, want 3", len(serps))
	}
	if got := f.portsUsed(); got != 2 {
		t.Errorf("%d identities carried the walk, want 2 - one refusal is worth one move", got)
	}
	if r := f.Pool.Stats().Rejections; r != 1 {
		t.Errorf("Stats().Rejections=%d, want 1 - the refusal was not reported", r)
	}
}

func TestAttempt_LetsAPageWalkSurviveARefusalUnchanged(t *testing.T) {
	// The point of this shape: the engines built in the previous milestone gain
	// the retry without being touched, and never learn that one happened.
	var _ google.Searcher = (*Attempt)(nil)

	o := newOrigin(t, func(_ *http.Request, n int) string {
		if n == 1 {
			return shellBody
		}
		return serpBody("example.com")
	})
	f := poolFacing(t, o.addr(), 2)

	a := &Attempt{Pool: f.Pool, Tries: 3}
	var walked int
	err := google.SearchUntil(context.Background(), a, usQuery("x"), 1, func(_ int, s google.SERP) bool {
		if len(s.Results) > 0 {
			walked++
		}
		return false
	})
	if err != nil {
		t.Fatalf("SearchUntil: %v", err)
	}
	if walked != 1 {
		t.Errorf("the walk saw %d pages of results, want 1", walked)
	}
}

func TestDefaultTries_OutlastsAListThatRefusesMostRequests(t *testing.T) {
	// Three was the number until a live run measured what it costs: 77% of
	// requests refused, and two answers out of ten queries. A limit that low
	// loses the whole job on a poor list, and what the operator sees is not
	// "the list is poor" but "the parser does not work".
	//
	// The bound is stated rather than the number, so the figure can be tuned on
	// later measurements without this test having to be edited to agree — but a
	// quiet slide back to a handful is caught.
	const refusalRate = 0.77
	// At this refusal rate, a limit of n leaves refusalRate^n as the chance a
	// query is written off having never been answered. Ten tries put that under
	// one in ten; three leave it at almost one in two.
	if odds := math.Pow(refusalRate, float64(defaultTries)); odds > 0.1 {
		t.Errorf("at %d tries a query is written off unanswered %.0f%% of the time on the list "+
			"this was measured against", defaultTries, odds*100)
	}
}

func TestAttempt_TellsThePoolWhichIdentitiesHaveAnswered(t *testing.T) {
	// What makes an identity warm, and the only place it can be said: a challenge
	// comes back as a request that succeeded, so nothing below this layer can
	// tell a page from a refusal. The pool offers a warm identity before a cold
	// one — the first request on a cold one costs one to three minutes and every
	// one after it costs one to two seconds — and an identity nobody vouches for
	// is one that preference can never reach.
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2)
	a := &Attempt{Pool: f.Pool}

	if got := f.Pool.Stats().Warm; got != 0 {
		t.Fatalf("%d identities are warm before anything was asked, want none", got)
	}
	if _, err := a.Search(context.Background(), usQuery("golang channels")); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := f.Pool.Stats().Warm; got != 1 {
		t.Errorf("%d identities are warm after one answered a search, want one", got)
	}

	// And the walk, which holds one identity for every page it takes and is the
	// path an ordinary parse job runs on.
	walked := poolFacing(t, o.addr(), 2)
	w := &Attempt{Pool: walked.Pool}
	if _, err := w.Walk(context.Background(), usQuery("golang channels"), 2); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got := walked.Pool.Stats().Warm; got != 1 {
		t.Errorf("%d identities are warm after one answered a walk, want one", got)
	}
}
