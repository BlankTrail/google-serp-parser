// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"errors"
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

	if got.Failed != 1 {
		t.Fatalf("Failed=%d Resolved=%d, want the request bound to have ended the lookup", got.Failed, got.Resolved)
	}
	if len(got.Errs) != 1 || !errors.Is(got.Errs[0], context.DeadlineExceeded) {
		t.Errorf("the report says %v, want the bound to have ended it", got.Errs)
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
