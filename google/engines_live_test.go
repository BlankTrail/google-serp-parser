//go:build live

// SPDX-License-Identifier: MIT

package google_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
)

// livePool opens a one-port pool for a live measurement, or skips.
//
// Both settings are read from the environment and neither is defaulted. An
// address written here would be one machine's setup committed to the
// repository, and a checkout with nothing to talk to must skip rather than
// spend its time failing against a host this file invented.
func livePool(t *testing.T) *blanktrail.Pool {
	t.Helper()
	control, key := os.Getenv("BLANKTRAIL_URL"), os.Getenv("BLANKTRAIL_API_KEY")
	if control == "" || key == "" {
		t.Skip("BLANKTRAIL_URL and BLANKTRAIL_API_KEY are not set")
	}

	ctx := context.Background()
	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		t.Fatalf("control client: %v", err)
	}
	rep := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: []string{"www.google.com", "google.com"},
		Ports:   1,
	})
	if !rep.OK() {
		for _, f := range rep.Blocking() {
			t.Logf("[%s] %s — %s → %s", f.Severity, f.Title, f.Detail, f.Action)
		}
		t.Fatal("preflight failed; fix the findings above")
	}

	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client:         client,
		Threads:        1,
		PortsPerThread: 1,
		Spec:           blanktrail.DefaultPortSpec(),
		CA:             rep.CA,
		Channels:       []blanktrail.Channel{blanktrail.NewDirectChannel("direct")},
		DelayMin:       3 * time.Second,
		DelayMax:       8 * time.Second,
	})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

// liveSession leases a port for the whole test and searches on it.
//
// One session per test rather than one per request: the engines under
// measurement make several requests in a row from a single session, and a
// measurement taken any other way would not be of them.
func liveSession(t *testing.T) (context.Context, *google.Session, *blanktrail.Lease) {
	t.Helper()
	pool := livePool(t)
	ctx := context.Background()
	lease, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(lease.Release)
	return ctx, google.NewSession(lease.Client().Transport), lease
}

// TestLiveEngines_Position measures where a known site is found and by which
// kind of match — the number that decides whether host matching alone carries
// the product.
//
// The address lookup is asked about an address the same query has just
// returned, rather than one this test guessed at. A guess that failed would
// leave two explanations — the site does not rank there, or matching by address
// does not work — and the measurement exists to tell those apart.
func TestLiveEngines_Position(t *testing.T) {
	ctx, s, _ := liveSession(t)
	q := google.Query{Text: "go programming language", Country: "us", Language: "en"}

	pos, err := google.FindPosition(ctx, s, q, "go.dev", 2)
	if err != nil {
		t.Errorf("MEASUREMENT position by host: error %v", err)
	}
	t.Logf("MEASUREMENT position by host: found=%v rank=%d page=%d how=%s scanned=%d url=%q",
		pos.Found, pos.Rank, pos.Page, pos.How, pos.Scanned, pos.Result.URL)
	if pos.Scanned == 0 {
		t.Error("the walk scanned nothing at all")
	}

	serp, err := s.Search(ctx, q)
	if err != nil {
		t.Fatalf("MEASUREMENT position by address: capture failed: %v", err)
	}
	var withAddress int
	var carried, target string
	for _, r := range serp.Results {
		if !r.Resolved() {
			continue
		}
		withAddress++
		// A site's root is read as a site rather than as a page, so asking
		// about one would measure host matching a second time and report it as
		// an address match that never happened.
		u, err := url.Parse(r.URL)
		if err != nil || strings.Trim(u.Path, "/") == "" {
			continue
		}
		if target == "" {
			carried, target = r.Host, r.URL
		}
	}
	if target == "" {
		t.Logf("MEASUREMENT position by address: of %d results, %d carried an address and none of those named a page; a lookup by address has nothing to match here",
			len(serp.Results), withAddress)
		return
	}
	byURL, err := google.FindPosition(ctx, s, q, target, 2)
	if err != nil {
		t.Errorf("MEASUREMENT position by address: error %v", err)
	}
	t.Logf("MEASUREMENT position by address (%s, %d of %d results carried one): found=%v rank=%d page=%d how=%s scanned=%d",
		carried, withAddress, len(serp.Results), byURL.Found, byURL.Rank, byURL.Page, byURL.How, byURL.Scanned)
}

// TestLiveEngines_Index measures the check the index feature is built on,
// against a site known to be held and a path that cannot be.
//
// Both answers are needed. A check that says "indexed" to everything is worth
// nothing, and only the second target shows whether the negative answer is
// reachable at all.
func TestLiveEngines_Index(t *testing.T) {
	ctx, s, _ := liveSession(t)
	q := google.Query{Country: "us", Language: "en"}

	for _, target := range []string{"go.dev", "go.dev/definitely-not-a-real-path-xyzzy"} {
		st, err := google.CheckIndexed(ctx, s, q, target)
		if err != nil {
			t.Logf("MEASUREMENT index %q: error %v", target, err)
			continue
		}
		t.Logf("MEASUREMENT index %q: indexed=%v hits=%d", target, st.Indexed, st.Hits)
		for _, r := range st.Sample {
			t.Logf("   sample: host=%q path=%q form=%s title=%q", r.Host, r.DisplayPath, r.Form, r.Title)
		}
	}
}

// recorder keeps every page a walk was answered with, so one walk settles both
// what the listing returned and what the pages behind it carried.
type recorder struct {
	inner google.Searcher
	pages []google.SERP
}

func (rec *recorder) Search(ctx context.Context, q google.Query) (google.SERP, error) {
	serp, err := rec.inner.Search(ctx, q)
	if err == nil {
		rec.pages = append(rec.pages, serp)
	}
	return serp, err
}

// identity is what tells one result from another without reading its href. The
// href is the thing under examination in TestLiveEngines_Listing, so keying on
// it there would assume the answer.
func identity(r google.Result) string {
	return r.Host + " | " + r.DisplayPath + " | " + r.Title
}

// TestLiveEngines_Listing measures the listing of a site's held pages, and with
// it the one thing the de-duplication inside ListIndexed rests on: whether the
// same result carries the same href on two different result pages.
//
// Under the encrypted link form the href is an origin-relative path rather than
// the destination, and nobody has yet seen a capture that says whether that
// path is derived from the destination or issued per page. If it varies, the
// de-duplication is inert for those results — still correct, and doing nothing.
//
// A repeat across pages is not guaranteed to occur, so the same page is taken a
// second time as well. Two captures of one page that disagree on the href
// settle the question on their own: a path that is not stable between two
// captures cannot be stable across two pages.
func TestLiveEngines_Listing(t *testing.T) {
	ctx, s, _ := liveSession(t)
	const site = "go.dev"
	q := google.Query{Country: "us", Language: "en"}

	rec := &recorder{inner: s}
	out, err := google.ListIndexed(ctx, rec, q, site, 2)
	if err != nil {
		t.Errorf("MEASUREMENT listing: error %v", err)
	}
	t.Logf("MEASUREMENT listing: %d pages walked, %d results listed", len(rec.pages), len(out))
	for i, serp := range rec.pages {
		forms := map[google.LinkForm]int{}
		for _, r := range serp.Results {
			forms[r.Form]++
		}
		t.Logf("   page %d: %d results, forms %v, pagination=%v maxOffset=%d",
			i+1, len(serp.Results), forms, serp.HasPagination, serp.MaxOffset)
	}
	if len(rec.pages) == 0 {
		t.Fatal("the listing walked no pages at all")
	}

	if len(rec.pages) > 1 {
		compareHrefs(t, "across two result pages", rec.pages[0], rec.pages[1])
	} else {
		t.Log("MEASUREMENT href across pages: the walk ended after one page, so there was no second page to compare")
	}

	// The same page again, from the same session. Its results are the same
	// results; anything that differs in their hrefs differs per capture.
	again, err := s.Search(ctx, google.Query{Text: "site:" + site, Country: "us", Language: "en", Page: 1})
	if err != nil {
		t.Logf("MEASUREMENT href across captures: second capture failed: %v", err)
		return
	}
	compareHrefs(t, "across two captures of page one", rec.pages[0], again)
}

// compareHrefs reports how many results the two captures have in common and how
// many of those carry the same href, counted apart for the encrypted form
// because under the direct form the href is the destination and the question
// does not arise.
func compareHrefs(t *testing.T, what string, a, b google.SERP) {
	t.Helper()
	first := map[string]google.Result{}
	for _, r := range a.Results {
		first[identity(r)] = r
	}
	var common, same, encrypted, encryptedSame int
	for _, r := range b.Results {
		prev, ok := first[identity(r)]
		if !ok {
			continue
		}
		common++
		if prev.Link == r.Link {
			same++
		}
		if r.Form == google.LinkEncrypted && prev.Form == google.LinkEncrypted {
			encrypted++
			if prev.Link == r.Link {
				encryptedSame++
			} else {
				t.Logf("   differing href for %q:\n      %s\n      %s", r.Title, prev.Link, r.Link)
			}
		}
	}
	t.Logf("MEASUREMENT href %s: %d results in common, %d with an identical href; of those %d encrypted, %d identical",
		what, common, same, encrypted, encryptedSame)
	if common == 0 {
		t.Logf("   nothing in common, so this pair says nothing about the href")
	}
}

// TestLiveEngines_Suggest confirms the completion endpoint answers and returns
// the shape the parser expects.
func TestLiveEngines_Suggest(t *testing.T) {
	pool := livePool(t)
	ctx := context.Background()
	lease, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lease.Release()

	sg := google.NewSuggester(lease.Client().Transport)
	got, err := sg.Suggest(ctx, google.Query{Text: "iphone 13", Country: "us", Language: "en"})
	if err != nil {
		t.Fatalf("MEASUREMENT suggest: error %v", err)
	}
	t.Logf("MEASUREMENT suggest: %d completions, first three: %v", len(got), firstN(got, 3))
	if len(got) == 0 {
		t.Error("a common phrase returned no completions")
	}
}

// TestLiveEngines_ResolveBatch measures the cost and the success rate of
// filling in the addresses a page did not carry — the numbers that decide
// whether addresses are resolved in the same pass as the capture or a later
// one.
func TestLiveEngines_ResolveBatch(t *testing.T) {
	ctx, s, lease := liveSession(t)

	// Two pages, always two, whatever the first one turns out to hold. The link
	// form varies from page to page, so one page is a thin sample of how much
	// resolution a run has to pay for — and a walk that took pages until it
	// found some work would report the cost of the sample it went looking for.
	serps, err := google.SearchDepth(ctx, s, google.Query{Text: "тестовый запрос", Country: "ru", Language: "ru"}, 2)
	if err != nil {
		t.Errorf("MEASUREMENT resolve: capture failed: %v", err)
	}
	if len(serps) == 0 {
		t.Fatal("the walk captured nothing at all")
	}
	var all []google.Result
	var missing int
	forms := map[google.LinkForm]int{}
	for _, serp := range serps {
		for _, r := range serp.Results {
			forms[r.Form]++
			if !r.Resolved() {
				missing++
			}
		}
		all = append(all, serp.Results...)
	}
	origin := serps[0].Origin
	t.Logf("MEASUREMENT capture: pages=%d results=%d without an address=%d forms=%v origin=%q",
		len(serps), len(all), missing, forms, origin)

	start := time.Now()
	rep := google.NewResolver(lease.Client().Transport).ResolveResults(ctx, origin, all, 4)
	elapsed := time.Since(start)
	t.Logf("MEASUREMENT resolve: attempted=%d resolved=%d failed=%d untried=%d in %v",
		rep.Attempted, rep.Resolved, rep.Failed, rep.Attempted-rep.Resolved-rep.Failed, elapsed)
	if rep.Attempted > 0 {
		t.Logf("MEASUREMENT resolve: %v per link over four workers", elapsed/time.Duration(rep.Attempted))
	}
	for i, e := range rep.Errs {
		if i < 3 {
			t.Logf("   failure: %v", e)
		}
	}
	if missing > 0 && rep.Attempted == 0 {
		t.Errorf("%d results carried no address and none was attempted", missing)
	}
}

func firstN(s []string, n int) []string {
	if len(s) < n {
		return s
	}
	return s[:n]
}
