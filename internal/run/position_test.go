// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/google"
)

// pageOf builds one result page from a list of addresses, with a pagination bar
// that offers more. The bar matters: without one a walk stops after the first
// page whatever the depth says, and a test of a walk that goes deeper would be a
// test of a walk that never went anywhere.
func pageOf(urls ...string) string {
	return serpBodyOf(urls...) + `<div role="navigation"><a href="/search?q=x&amp;start=90">10</a></div>`
}

// strangers are other people's results, so no case here has the site it is
// looking for standing first or standing alone. Either arrangement makes "found
// where it stood" and "took whatever came back" the same answer.
var strangers = []string{"https://elsewhere.test/x", "https://another.test/y"}

func TestRunner_APositionJobRecordsWhereTheSiteStoodAndNothingElseFromThePage(t *testing.T) {
	// The whole of what this kind produces is the place, so the place is what is
	// written down: the one result that was the site, carrying the rank it held
	// among everything the page offered. The site stands third here and the page
	// carries a fourth result under it, so a job that recorded the first result,
	// or every result, or the site at the place it occupies among what was kept,
	// fails this.
	o := newOrigin(t, func(*http.Request, int) string {
		return pageOf(strangers[0], strangers[1],
			"https://example.com/wanted", "https://third.test/z")
	})
	f := poolFacing(t, o.addr(), 2)

	r := &Runner{Pool: f.Pool, Threads: 1}
	rep := r.Run(context.Background(), Job{
		Kind: Position, Target: "example.com", Queries: usQueries(1), Pages: 3})

	if rep.Done != 1 {
		t.Fatalf("Done=%d, want 1 (failed %d: %v)", rep.Done, rep.Failed, rep.Results[0].Err)
	}
	// The walk stops at the page that answers. A job that took the depth it was
	// given anyway would spend two more requests to confirm what it knew.
	if got := o.searches.Load(); got != 1 {
		t.Errorf("%d searches for a site found on the first page, want 1", got)
	}
	pages := rep.Results[0].Pages
	if len(pages) != 1 {
		t.Fatalf("recorded %d pages, want 1", len(pages))
	}
	if n := len(pages[0].Results); n != 1 {
		t.Fatalf("recorded %d results, want the 1 that was the site: %+v", n, pages[0].Results)
	}
	got := pages[0].Results[0]
	if got.URL != "https://example.com/wanted" {
		t.Errorf("recorded %q, want the site that was looked for", got.URL)
	}
	if got.Position != 3 {
		t.Errorf("recorded the site at %d, and it stood third on the page", got.Position)
	}
}

func TestRunner_APositionJobCountsTheRankAcrossThePagesItWalked(t *testing.T) {
	// The eleventh result is eleventh whichever page carried it. A rank counted
	// within the page the site was found on would report a site on page two as
	// standing second, which is the answer for a site three places above it.
	o := newOrigin(t, func(_ *http.Request, n int) string {
		if n == 1 {
			return pageOf(strangers[0], strangers[1], "https://third.test/z")
		}
		return pageOf("https://fourth.test/w", "https://example.com/wanted")
	})
	f := poolFacing(t, o.addr(), 2)

	r := &Runner{Pool: f.Pool, Threads: 1}
	rep := r.Run(context.Background(), Job{
		Kind: Position, Target: "example.com", Queries: usQueries(1), Pages: 4})

	if rep.Done != 1 {
		t.Fatalf("Done=%d, want 1 (%v)", rep.Done, rep.Results[0].Err)
	}
	if got := o.searches.Load(); got != 2 {
		t.Errorf("%d searches for a site found on the second page, want 2", got)
	}
	found := rep.Results[0].Pages[0].Results
	if len(found) != 1 {
		t.Fatalf("recorded %d results, want the 1 that was the site: %+v", len(found), found)
	}
	if found[0].Position != 5 {
		t.Errorf("recorded the site at %d, and it stood fifth counting from the first page",
			found[0].Position)
	}
}

func TestRunner_APositionJobWritesDownAPhraseTheSiteDidNotRankFor(t *testing.T) {
	// A page holding nothing is how "the site was not in what we took" is
	// recorded, and it is recorded rather than skipped: a query with nothing
	// written against it is one every later resume takes up again, so a phrase
	// the site genuinely does not rank for would be checked on every run for as
	// long as that stayed true.
	o := newOrigin(t, func(*http.Request, int) string {
		return serpBodyOf(strangers[0], strangers[1])
	})
	f := poolFacing(t, o.addr(), 2)

	r := &Runner{Pool: f.Pool, Threads: 1}
	rep := r.Run(context.Background(), Job{
		Kind: Position, Target: "example.com", Queries: usQueries(1), Pages: 1})

	if rep.Done != 1 {
		t.Fatalf("Done=%d, want 1 — a site that did not rank is not a failed query (%v)",
			rep.Done, rep.Results[0].Err)
	}
	pages := rep.Results[0].Pages
	if len(pages) != 1 {
		t.Fatalf("recorded %d pages, want the 1 that says the answer was taken", len(pages))
	}
	if n := len(pages[0].Results); n != 0 {
		t.Errorf("recorded %d results for a site that was not there: %+v", n, pages[0].Results)
	}
}

func TestRunner_APositionJobRecognisesTheSiteHoweverGoogleSpelledTheAddress(t *testing.T) {
	// Google renders the scheme, the www, the case and the trailing slash as it
	// pleases, and not one of those differences is a different page. A check that
	// compared the two literally would report a site as not ranking on the
	// strength of a capital letter, and nothing the reader could see would say so.
	//
	// The case that is compared is the host's. A path is the site's own spelling
	// and two paths differing in case are two addresses, which is the web's rule
	// and not this program's to soften.
	//
	// The site never stands first and never stands alone, so a version that
	// recognised nothing and took whatever came back would fail every case.
	for _, spelling := range []string{
		"http://example.com/wanted",
		"https://www.example.com/wanted",
		"https://EXAMPLE.COM/wanted",
		"https://example.com/wanted/",
	} {
		t.Run(spelling, func(t *testing.T) {
			o := newOrigin(t, func(*http.Request, int) string {
				return pageOf(strangers[0], strangers[1], spelling, "https://third.test/z")
			})
			f := poolFacing(t, o.addr(), 2)

			r := &Runner{Pool: f.Pool, Threads: 1}
			rep := r.Run(context.Background(), Job{
				Kind: Position, Target: "https://example.com/wanted",
				Queries: usQueries(1), Pages: 1})

			if rep.Done != 1 {
				t.Fatalf("Done=%d, want 1 (%v)", rep.Done, rep.Results[0].Err)
			}
			found := rep.Results[0].Pages[0].Results
			if len(found) != 1 {
				t.Fatalf("the site written %q was not recognised: %+v", spelling, found)
			}
			if found[0].Position != 3 {
				t.Errorf("recorded the site at %d, and it stood third", found[0].Position)
			}
		})
	}
}

func TestRunner_APositionJobWithNothingToLookForRefusesRatherThanAnswering(t *testing.T) {
	// The one answer that must not come out of this is "not found". A check with
	// no site recognises nothing, and reporting that as a site absent from the
	// results would be a wrong answer about every phrase in the list, written
	// into the history where somebody will read it as an answer.
	o := newOrigin(t, func(*http.Request, int) string {
		return pageOf(strangers[0], "https://example.com/wanted")
	})
	f := poolFacing(t, o.addr(), 2)

	r := &Runner{Pool: f.Pool, Threads: 1}
	rep := r.Run(context.Background(), Job{Kind: Position, Queries: usQueries(1), Pages: 1})

	if rep.Failed != 1 {
		t.Fatalf("Failed=%d, want 1 (done %d)", rep.Failed, rep.Done)
	}
	if !errors.Is(rep.Results[0].Err, google.ErrNoSite) {
		t.Errorf("the query was refused with %v, want it to name the site nobody gave",
			rep.Results[0].Err)
	}
	if pages := rep.Results[0].Pages; len(pages) != 0 {
		t.Errorf("a refused check wrote down %d pages, want none: %+v", len(pages), pages)
	}
}

func TestRunner_AParseJobKeepsEveryResultWhereAPositionCheckKeepsOne(t *testing.T) {
	// The two kinds over the same page, so that a parse quietly narrowed to one
	// site is a failure rather than a shorter table nobody counted. The site a
	// position check would look for is on this page, and a parse has no business
	// treating it as the answer.
	o := newOrigin(t, func(*http.Request, int) string {
		return serpBodyOf(strangers[0], strangers[1],
			"https://example.com/wanted", "https://third.test/z")
	})
	f := poolFacing(t, o.addr(), 2)

	r := &Runner{Pool: f.Pool, Threads: 1}
	rep := r.Run(context.Background(), Job{
		Kind: Parse, Target: "example.com", Queries: usQueries(1), Pages: 1})

	if rep.Done != 1 {
		t.Fatalf("Done=%d, want 1 (%v)", rep.Done, rep.Results[0].Err)
	}
	if n := len(rep.Results[0].Pages[0].Results); n != 4 {
		t.Errorf("a parse job recorded %d results, want the 4 the page carried", n)
	}
}
