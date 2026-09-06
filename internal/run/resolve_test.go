// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

// destination stands in for the address behind a result link. Resolving reads
// the redirect and never follows it, so the site itself is never contacted and
// this one server answers for every result in a fixture.
//
// It serves TLS because that is the scheme a captured page is served over, and
// a link joined to that origin is fetched over it too.
type destination struct {
	*httptest.Server
	asked atomic.Int64
}

func newDestination(t *testing.T) *destination {
	t.Helper()
	d := &destination{}
	d.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.asked.Add(1)
		w.Header().Set("Location", "https://example.com"+r.URL.Path)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(d.Close)
	return d
}

func (d *destination) addr() string { return d.Listener.Addr().String() }

// unread is the shape a result arrives in when the page carried no address for
// it: the link is origin-relative and the host is known, the address is not.
func unread(path, host string) google.Result {
	return google.Result{Link: path, Host: host, Form: google.LinkEncrypted, Title: host}
}

// stated is the shape a result arrives in when the page said the address
// outright. Both fields carry it, which is what a parsed page holds: a fixture
// that filled only the address would be passed over for want of a link, and
// could not show whether this layer had looked at it at all.
func stated(addr, host string) google.Result {
	return google.Result{Link: addr, URL: addr, Host: host, Form: google.LinkDirect, Title: host}
}

// identitiesTaken counts what a call spent from the pool.
func identitiesTaken(f *facing, during func()) int64 {
	before := f.Pool.Stats().Requests
	during()
	return f.Pool.Stats().Requests - before
}

func TestRunner_FillsInTheAddressesThePageDidNotCarry(t *testing.T) {
	// Doing this during the walk doubles the requests exactly when it matters
	// most to finish the capture. The address behind such a link is measured to
	// be readable afterwards, from anywhere, so afterwards is where it belongs.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 2)

	rep := Report{Results: []QueryResult{{
		Attempted: true,
		Pages: []google.SERP{{
			Origin:  d.URL,
			Results: []google.Result{unread("/goto/one", "example.com")},
		}},
	}}}

	r := &Runner{Pool: f.Pool, Threads: 1}
	got := r.ResolveLinks(context.Background(), &rep, 2)

	if got.Attempted != 1 {
		t.Fatalf("Attempted=%d, want 1", got.Attempted)
	}
	if got.Resolved != 1 {
		t.Fatalf("Resolved=%d, want 1 (failed %d: %v)", got.Resolved, got.Failed, got.Errs)
	}
	if url := rep.Results[0].Pages[0].Results[0].URL; url != "https://example.com/goto/one" {
		t.Errorf("URL=%q, want the address the link led to", url)
	}
}

func TestRunner_SpendsNothingOnAPageWhoseAddressesAreAllKnown(t *testing.T) {
	// Every request through a leased identity costs, and so does the identity
	// itself: taking one to learn an address the page already stated is the
	// cheapest mistake there is to avoid.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 2)

	rep := Report{Results: []QueryResult{{
		Attempted: true,
		Pages: []google.SERP{{
			Origin:  d.URL,
			Results: []google.Result{stated("https://example.com/page", "example.com")},
		}},
	}}}

	r := &Runner{Pool: f.Pool, Threads: 1}
	var got google.ResolveReport
	spent := identitiesTaken(f, func() {
		got = r.ResolveLinks(context.Background(), &rep, 2)
	})

	if got.Attempted != 0 {
		t.Errorf("Attempted=%d, want 0 - the address was already known", got.Attempted)
	}
	if spent != 0 {
		t.Errorf("%d identities taken, want none for a page with nothing to look up", spent)
	}
	if n := d.asked.Load(); n != 0 {
		t.Errorf("%d requests made for an address the page already carried", n)
	}
}

func TestRunner_TakesOneIdentityForAWholePageOfLinks(t *testing.T) {
	// The address behind a link does not depend on who captured the page, so a
	// separate identity per link buys nothing and spends the whole page's worth
	// of the pool to get it.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 4)

	rep := Report{Results: []QueryResult{{
		Attempted: true,
		Pages: []google.SERP{{
			Origin: d.URL,
			Results: []google.Result{
				unread("/goto/one", "one.example"),
				unread("/goto/two", "two.example"),
				unread("/goto/three", "three.example"),
			},
		}},
	}}}

	r := &Runner{Pool: f.Pool, Threads: 1}
	var got google.ResolveReport
	spent := identitiesTaken(f, func() {
		got = r.ResolveLinks(context.Background(), &rep, 1)
	})

	if got.Resolved != 3 {
		t.Fatalf("Resolved=%d, want 3 (failed %d: %v)", got.Resolved, got.Failed, got.Errs)
	}
	if spent != 1 {
		t.Errorf("%d identities taken for one page of 3 links, want 1", spent)
	}
}

func TestRunner_CostsNothingForAQueryThatCameBackWithNoPages(t *testing.T) {
	// A query that produced nothing has nothing to look up, and a page slice
	// that was never filled is the shape most likely to be walked into.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 2)

	rep := Report{Results: []QueryResult{
		{Attempted: true, Err: context.DeadlineExceeded},
		{Attempted: false},
	}}

	r := &Runner{Pool: f.Pool, Threads: 1}
	var got google.ResolveReport
	spent := identitiesTaken(f, func() {
		got = r.ResolveLinks(context.Background(), &rep, 2)
	})

	if got.Attempted != 0 {
		t.Errorf("Attempted=%d, want 0", got.Attempted)
	}
	if spent != 0 {
		t.Errorf("%d identities taken, want none - there was nothing to look up", spent)
	}
	if n := d.asked.Load(); n != 0 {
		t.Errorf("%d requests made on behalf of a query that produced nothing", n)
	}
}

func TestRunner_FillsInThePagesAWalkTookBeforeItFailed(t *testing.T) {
	// A walk that failed on the second page still captured the first, and hands
	// it back for exactly that reason. Passing over those results because the
	// query as a whole is recorded as failed leaves the caller holding rankings
	// with no addresses, and the only way to get them back is to capture the
	// page again.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 2)

	rep := Report{Results: []QueryResult{{
		Attempted: true,
		Err:       errors.New("run: page 2 was refused by every identity"),
		Pages: []google.SERP{{
			Origin:  d.URL,
			Results: []google.Result{unread("/goto/one", "example.com")},
		}},
	}}}

	r := &Runner{Pool: f.Pool, Threads: 1}
	got := r.ResolveLinks(context.Background(), &rep, 2)

	if got.Resolved != 1 {
		t.Fatalf("Resolved=%d, want 1 (failed %d: %v)", got.Resolved, got.Failed, got.Errs)
	}
	if url := rep.Results[0].Pages[0].Results[0].URL; url != "https://example.com/goto/one" {
		t.Errorf("URL=%q, want the page the walk did capture to have been completed", url)
	}
}

func TestRunner_KeepsTheRequestBoundThePoolWasGiven(t *testing.T) {
	// A lookup that never comes back is this identity being unreachable. With no
	// bound of its own it holds the whole report open, and a caller who only
	// wanted the addresses filled in waits on one link forever.
	slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Location", "https://example.com/real")
		w.WriteHeader(http.StatusFound)
	}))
	defer slow.Close()

	f := poolFacing(t, slow.Listener.Addr().String(), 2, func(c *blanktrail.PoolConfig) {
		c.RequestTimeout = 20 * time.Millisecond
		// The page is carried to every identity it is allowed, and this test is
		// about the bound rather than about a port being put away, so the ports
		// stay in the rotation for the whole of it.
		c.MaxPortStrikes = 1 << 20
	})

	rep := Report{Results: []QueryResult{{
		Attempted: true,
		Pages: []google.SERP{{
			Origin:  slow.URL,
			Results: []google.Result{unread("/goto/one", "example.com")},
		}},
	}}}

	r := &Runner{Pool: f.Pool, Threads: 1}
	got := r.ResolveLinks(context.Background(), &rep, 1)

	// Once per identity the page is carried to, and it is carried to every one
	// it is allowed: the bound ends each attempt, and a slow address is passed
	// over the same way a dead one is.
	if got.Failed != resolveTries || got.Resolved != 0 {
		t.Fatalf("Failed=%d Resolved=%d, want the request bound to have ended every one of the %d attempts",
			got.Failed, got.Resolved, resolveTries)
	}
	if len(got.Errs) != resolveTries {
		t.Fatalf("the report carries %d errors, want one per attempt: %v", len(got.Errs), got.Errs)
	}
	for i, err := range got.Errs {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %d says %v, want the bound to have ended it", i+1, err)
		}
	}
}

func TestRunner_StopsOnceTheCallerHasGoneAndSaysSoOnce(t *testing.T) {
	// A caller who cancelled gets one account of why the rest was left, not one
	// per page. A report carrying an error for every page still to come buries
	// the reason among repetitions of itself.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 2)

	page := google.SERP{
		Origin:  d.URL,
		Results: []google.Result{unread("/goto/one", "example.com")},
	}
	rep := Report{Results: []QueryResult{
		{Attempted: true, Pages: []google.SERP{page, page}},
		{Attempted: true, Pages: []google.SERP{page, page}},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := &Runner{Pool: f.Pool, Threads: 1}
	got := r.ResolveLinks(ctx, &rep, 2)

	if len(got.Errs) != 1 {
		t.Fatalf("%d errors for one cancellation, want 1: %v", len(got.Errs), got.Errs)
	}
	if !errors.Is(got.Errs[0], context.Canceled) {
		t.Errorf("the report says %v, want the cancellation", got.Errs[0])
	}
	if got.Attempted != 0 {
		t.Errorf("Attempted=%d, want none after the caller had already gone", got.Attempted)
	}
	if n := d.asked.Load(); n != 0 {
		t.Errorf("%d requests made after the caller had already gone", n)
	}
}

// hidingOrigin is a search host of the kind that answers with encrypted links:
// the markup carries "/goto?url=…" and no address at all, and the address is
// readable only by asking that link where it goes.
//
// One server answers both, because that is how the page arrives — the link is
// origin-relative, so it is fetched from the host that served the page.
func hidingOrigin(t *testing.T, searches, lookups *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/goto"):
			lookups.Add(1)
			w.Header().Set("Location", "https://example.com/page")
			w.WriteHeader(http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/search"):
			searches.Add(1)
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			_, _ = io.WriteString(w, `<!doctype html><html><body><div id="search"><div data-snc="x">`+
				`<a href="/goto?url=CAESXAHuR6pN7OGc" data-ved="2"><h3>Title</h3></a>`+
				`<cite>example.com</cite></div></div></body></html>`)
		default:
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			_, _ = io.WriteString(w, `<!doctype html><html><body><div id="main"></div></body></html>`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunner_WritesDownTheAddressOnARegionThatHidesIt(t *testing.T) {
	// A whole region is answered with encrypted links, and the parser leaves the
	// address empty for them on purpose: it is not in the markup. Filling it in
	// is a step of its own, and for a job that writes as it goes that step has to
	// happen before the write — a row already in the history has nowhere to put
	// an address found later, and there is no pass that goes back for one.
	//
	// This was live for a whole release. A job of 36 044 results was recorded
	// with an empty address in every one of them, its "unique by url" filter
	// dropped nothing because an empty address is no key, and the operator was
	// left with a page of ranks against blanks.
	var searches, lookups atomic.Int64
	o := hidingOrigin(t, &searches, &lookups)
	f := poolFacing(t, o.Listener.Addr().String(), 2)

	sink := &recordingSink{}
	r := &Runner{Pool: f.Pool, Threads: 1, Sink: sink}
	rep := r.Run(context.Background(), Job{
		Queries:   usQueries(1),
		Pages:     1,
		Addresses: true,
	})

	if rep.Done != 1 {
		t.Fatalf("Done=%d, want 1 (failed %d, untried %d)", rep.Done, rep.Failed, rep.Untried)
	}
	got := sink.records()
	if len(got) != 1 || len(got[0].Pages) != 1 || len(got[0].Pages[0].Results) != 1 {
		t.Fatalf("the sink was handed %+v, want one result on one page", got)
	}
	res := got[0].Pages[0].Results[0]
	if res.URL == "" {
		t.Errorf("the result reached the sink with no address, only the link %q — "+
			"everything written from this page is a rank against a blank", res.Link)
	}
	if lookups.Load() == 0 {
		t.Error("the hidden address was never asked for")
	}
}

func TestRunner_LeavesTheHiddenAddressesAloneForAJobThatKeepsNone(t *testing.T) {
	// The lookups cost a request each. A job that does not keep the address has
	// nowhere to put one, so it must not pay for them — which is also what makes
	// the switch above worth having rather than always resolving.
	var searches, lookups atomic.Int64
	o := hidingOrigin(t, &searches, &lookups)
	f := poolFacing(t, o.Listener.Addr().String(), 2)

	sink := &recordingSink{}
	r := &Runner{Pool: f.Pool, Threads: 1, Sink: sink}
	r.Run(context.Background(), Job{Queries: usQueries(1), Pages: 1})

	if lookups.Load() != 0 {
		t.Errorf("%d addresses were looked up for a job that keeps none", lookups.Load())
	}
}

// dropping is a destination that drops the connection for the first n requests
// and answers after that: an address that is dead, then one that is not.
//
// The connection is dropped rather than an error status returned, because that
// is what a dead proxy does and what every failure of the live measurement was:
// "the address dropped the connection".
func dropping(t *testing.T, n int64) *destination {
	t.Helper()
	d := &destination{}
	d.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.asked.Add(1) <= n {
			// Taking the connection away underneath the client, which is the
			// only way to make this look like the far end going away.
			if hijacked, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = hijacked.Close()
				return
			}
		}
		w.Header().Set("Location", "https://example.com"+r.URL.Path)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(d.Close)
	return d
}

func TestRunner_CarriesTheLookupToAnotherIdentityWhenTheAddressIsDead(t *testing.T) {
	// A search is carried to a fresh identity when the address it went through
	// is dead. The lookups had no such thing: one address, one attempt, and
	// every link on the page lost together when it was a dead one.
	//
	// Measured live on a fifteen-thousand-address list, three pages of a region
	// that hides its addresses: 5 of 14 read through the identity that captured
	// the page, 6 of 15 through another, and every single failure the same one —
	// the address dropped the connection. On the job this was found on, 859 of
	// 2 691 results were written with an address and the rest without.
	d := dropping(t, 2)
	f := poolFacing(t, d.addr(), 4)

	rep := Report{Results: []QueryResult{{
		Attempted: true,
		Pages: []google.SERP{{
			Origin:  d.URL,
			Results: []google.Result{unread("/goto/one", "example.com")},
		}},
	}}}

	r := &Runner{Pool: f.Pool, Threads: 1}
	got := r.ResolveLinks(context.Background(), &rep, 2)

	if got.Resolved != 1 {
		t.Errorf("resolved %d of %d, want the one link read after the dead addresses were passed over (failed %d)",
			got.Resolved, got.Attempted, got.Failed)
	}
	if url := rep.Results[0].Pages[0].Results[0].URL; url == "" {
		t.Error("the result still carries no address, so nothing carried the lookup past the first dead address")
	}
	if d.asked.Load() < 3 {
		t.Errorf("the address was asked %d times, want the two that dropped and the one that answered", d.asked.Load())
	}
}

func TestRunner_GivesUpOnAPageOnceItHasBeenCarriedToEveryIdentityItIsAllowed(t *testing.T) {
	// And it stops. A list where every address is dead is the operator's
	// problem, not something to spend the pool on for ever: once the page has
	// been carried to as many identities as a query is, it is left as it is and
	// the job goes on.
	d := dropping(t, 1000)
	f := poolFacing(t, d.addr(), 6, func(c *blanktrail.PoolConfig) {
		// What is being counted is identities the page was carried to, so the
		// ports stay in the rotation rather than being put away part way.
		c.MaxPortStrikes = 1 << 20
	})

	rep := Report{Results: []QueryResult{{
		Attempted: true,
		Pages: []google.SERP{{
			Origin:  d.URL,
			Results: []google.Result{unread("/goto/one", "example.com")},
		}},
	}}}

	r := &Runner{Pool: f.Pool, Threads: 1}
	got := r.ResolveLinks(context.Background(), &rep, 2)

	if got.Resolved != 0 {
		t.Errorf("resolved %d against an address that answers nothing", got.Resolved)
	}
	if got.Attempted != resolveTries {
		t.Errorf("the page was attempted %d times, want %d", got.Attempted, resolveTries)
	}
}

// grudging answers one request in three and takes the connection away for the
// rest, so a page of several links comes back a few at a time however often it
// is asked.
//
// It is the shape the live list has: not an address that answers everything or
// nothing, but one that answers some of what is asked of it. A page like this
// used to be left with gaps in the middle of it.
func grudging(t *testing.T) *destination {
	t.Helper()
	d := &destination{}
	d.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.asked.Add(1)%3 != 1 {
			if hijacked, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = hijacked.Close()
				return
			}
		}
		w.Header().Set("Location", "https://example.com"+r.URL.Path)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(d.Close)
	return d
}

func TestRunner_WorksAPageUntilEveryAddressIsHadRatherThanForAFixedNumberOfTries(t *testing.T) {
	// A page that arrived is a page whose addresses can be had, and the reader
	// wants all of them: a result written with no address is a row of a report
	// with a hole in it, and a hole in the middle of a page reads as a result
	// that has no address rather than as one nobody could reach.
	//
	// Counting rounds is what left those holes. Every round brought some of what
	// was left back, three rounds ran out, and the rest were written empty. The
	// rule is about progress now: while rounds keep bringing something, the page
	// is worked at.
	d := grudging(t)
	f := poolFacing(t, d.addr(), 8)

	var links []google.Result
	for i := 0; i < 6; i++ {
		links = append(links, unread(fmt.Sprintf("/goto/%d", i+1), "example.com"))
	}
	rep := Report{Results: []QueryResult{{
		Attempted: true,
		Pages:     []google.SERP{{Origin: d.URL, Results: links}},
	}}}

	r := &Runner{Pool: f.Pool, Threads: 1}
	got := r.ResolveLinks(context.Background(), &rep, 1)

	for i, one := range rep.Results[0].Pages[0].Results {
		if !one.Resolved() {
			t.Errorf("result %d was left with no address after %d attempts of which %d came back",
				i+1, got.Attempted, got.Resolved)
		}
	}
	if got.Resolved != len(links) {
		t.Fatalf("resolved %d of %d", got.Resolved, len(links))
	}
}

// stingy answers one request in four and takes the connection away for the
// rest, which is the rate the live list was measured at: 29 hidden addresses
// cost 114 attempts to read, and every failure was the address dropping the
// connection.
func stingy(t *testing.T) *destination {
	t.Helper()
	d := &destination{}
	d.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.asked.Add(1)%4 != 1 {
			if hijacked, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = hijacked.Close()
				return
			}
		}
		w.Header().Set("Location", "https://example.com"+r.URL.Path)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(d.Close)
	return d
}

func TestRunner_FillsAWholePageAgainstTheRateTheLiveListWasMeasuredAt(t *testing.T) {
	// The report the operator reads has one row per result, and a row with no
	// address in the middle of nine that have one reads as a result that has no
	// address rather than as one nobody could reach. On the measured rate a rule
	// of three identities reads about half of them; this is the whole page.
	d := stingy(t)
	f := poolFacing(t, d.addr(), 8, func(c *blanktrail.PoolConfig) {
		c.MaxPortStrikes = 1 << 20
	})

	var links []google.Result
	for i := 0; i < 10; i++ {
		links = append(links, unread(fmt.Sprintf("/goto/%d", i+1), "example.com"))
	}
	rep := Report{Results: []QueryResult{{
		Attempted: true,
		Pages:     []google.SERP{{Origin: d.URL, Results: links}},
	}}}

	r := &Runner{Pool: f.Pool, Threads: 1}
	got := r.ResolveLinks(context.Background(), &rep, 2)

	var holes int
	for i, one := range rep.Results[0].Pages[0].Results {
		if !one.Resolved() {
			holes++
			t.Errorf("result %d was left with no address", i+1)
		}
	}
	if holes > 0 {
		t.Fatalf("%d of %d addresses were left unread over %d attempts of which %d came back",
			holes, len(links), got.Attempted, got.Resolved)
	}
}

func TestRunner_SpendsNoneOfAPagesAllowanceOnAPoolWithNothingToGive(t *testing.T) {
	// An identity that was never handed out asked nothing, so it says nothing
	// about the addresses still missing. Counting it against the page would let
	// a pool that is shutting down decide that a page has no more addresses to
	// be had — and the page would be written with holes nobody ever asked about.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 2)
	_ = f.Pool.Close()

	rep := Report{Results: []QueryResult{{
		Attempted: true,
		Pages: []google.SERP{{
			Origin:  d.URL,
			Results: []google.Result{unread("/goto/one", "example.com")},
		}},
	}}}

	r := &Runner{Pool: f.Pool, Threads: 1}
	got := r.ResolveLinks(context.Background(), &rep, 1)

	if got.Attempted != 0 || got.Failed != 0 {
		t.Errorf("Attempted=%d Failed=%d, want nothing asked of a pool with nothing to give", got.Attempted, got.Failed)
	}
	if len(got.Errs) != 1 {
		t.Fatalf("the report carries %d errors, want the one account of why nothing was looked up: %v", len(got.Errs), got.Errs)
	}
	if d.asked.Load() != 0 {
		t.Errorf("the address was asked %d times through a pool that had no identity to give", d.asked.Load())
	}
}

func TestRunner_DoesNotCostThePortItsAddressForAnsweringTheLookups(t *testing.T) {
	// A hidden address is read out of a Location header, so every lookup that
	// works answers with a redirect — and a redirect is what the wall looks
	// like to a search. Held against the port the same way, a page of lookups
	// takes the address that was carrying them out of the port, three at a
	// time, and the searches that follow go out through whatever the list
	// offers next. On a region that hides its addresses that is most of the
	// requests a job makes, and it is the run taking its own pool apart.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 2, func(c *blanktrail.PoolConfig) {
		c.RotateAfterFailures = 3
	})

	var links []google.Result
	for i := 0; i < 9; i++ {
		links = append(links, unread(fmt.Sprintf("/goto/%d", i+1), "example.com"))
	}
	rep := Report{Results: []QueryResult{{
		Attempted: true,
		Pages:     []google.SERP{{Origin: d.URL, Results: links}},
	}}}

	r := &Runner{Pool: f.Pool, Threads: 1}
	got := r.ResolveLinks(context.Background(), &rep, 1)
	if got.Resolved != len(links) {
		t.Fatalf("resolved %d of %d", got.Resolved, len(links))
	}

	st := f.Pool.Stats()
	if st.EgressRotations != 0 {
		t.Errorf("the port was moved to another address %d times by lookups that all worked", st.EgressRotations)
	}
	if st.Failures[blanktrail.FailureWall] != 0 {
		t.Errorf("%d lookups were counted as the wall, want none: the redirect is the answer they asked for",
			st.Failures[blanktrail.FailureWall])
	}
}

// walling answers one named link with a redirect that stays on Google and every
// other link with a real address. That is what an identity being refused looks
// like from here — a proper 302, a proper Location, and nothing behind it but
// the challenge — and mixing the two is what makes the case its own: a round
// that read something is not a round that brought nothing.
func walling(t *testing.T, refuse string) *destination {
	t.Helper()
	d := &destination{}
	d.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.asked.Add(1)
		if r.URL.Path == refuse {
			w.Header().Set("Location", "https://www.google.ru/sorry/index?continue=https://www.google.ru/search")
		} else {
			w.Header().Set("Location", "https://example.com"+r.URL.Path)
		}
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(d.Close)
	return d
}

func TestRunner_WritesNoAddressWhenTheLookupIsSentToGooglesOwnPage(t *testing.T) {
	// Measured on a live run: a result was written with a Google address —
	// the redirector's own — because the Location header was taken at face
	// value. An empty address says nobody could reach it; that one says the
	// ranking site is Google, and every export and rank history downstream
	// believes it.
	//
	// The identity is put out of the rotation too. It succeeded, as far as the
	// pool can see: a request went out and a redirect came back. Only this
	// layer knows the redirect was a refusal.
	d := walling(t, "/goto/two")
	f := poolFacing(t, d.addr(), 4, func(c *blanktrail.PoolConfig) {
		c.MaxPortStrikes = 1 << 20
	})

	rep := Report{Results: []QueryResult{{
		Attempted: true,
		Pages: []google.SERP{{
			Origin: d.URL,
			Results: []google.Result{
				unread("/goto/one", "example.com"),
				unread("/goto/two", "example.com"),
			},
		}},
	}}}

	r := &Runner{Pool: f.Pool, Threads: 1}
	got := r.ResolveLinks(context.Background(), &rep, 1)

	if one := rep.Results[0].Pages[0].Results[0]; !one.Resolved() {
		t.Error("the link that was answered with an address was left without one")
	}
	if one := rep.Results[0].Pages[0].Results[1]; one.Resolved() {
		t.Errorf("the result was written with %q as its address", one.URL)
	}
	if got.Resolved != 1 {
		t.Errorf("resolved %d, want the one real address and not the challenge", got.Resolved)
	}
	if !walled(got.Errs) {
		t.Errorf("the report says %v, want it to name the redirect that stayed on Google", got.Errs)
	}
}

func TestRunner_PutsAnIdentitySentToTheChallengeOutOfTheRotation(t *testing.T) {
	// One round, and a round that read something: the identity answered, the
	// pool saw a request go out and a redirect come back, and only this layer
	// knows that one of those redirects was a refusal. Rejecting on the round
	// that brought nothing cannot stand in for it — by then the round is a
	// round of one link, and the identity has been handed out again.
	d := walling(t, "/goto/two")
	f := poolFacing(t, d.addr(), 4, func(c *blanktrail.PoolConfig) {
		c.MaxPortStrikes = 1 << 20
	})

	serp := google.SERP{
		Origin: d.URL,
		Results: []google.Result{
			unread("/goto/one", "example.com"),
			unread("/goto/two", "example.com"),
		},
	}

	r := &Runner{Pool: f.Pool, Threads: 1}
	got, err := r.resolveOnce(context.Background(), &serp, 1)
	if err != nil {
		t.Fatalf("resolveOnce: %v", err)
	}
	if got.Resolved != 1 || got.Failed != 1 {
		t.Fatalf("Resolved=%d Failed=%d, want the round to have read one and been refused one",
			got.Resolved, got.Failed)
	}
	if st := f.Pool.Stats(); st.Rejections != 1 {
		t.Errorf("%d identities were put out of the rotation, want the one that was sent to the challenge",
			st.Rejections)
	}
}
