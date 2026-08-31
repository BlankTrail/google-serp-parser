// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
