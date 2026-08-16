// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"testing"
)

// serpOf builds a page from a list of hosts, so a ranking case reads as the
// ranking it describes.
func serpOf(hosts ...string) SERP {
	s := SERP{}
	for i, h := range hosts {
		s.Results = append(s.Results, Result{Position: i + 1, Host: h, Form: LinkEncrypted})
	}
	return s
}

func TestFindPosition_FindsASiteByHostOnTheFirstPage(t *testing.T) {
	f := &fakeSearcher{pages: []SERP{serpOf("a.test", "example.com", "c.test")}}
	got, err := FindPosition(context.Background(), f, Query{Text: "x"}, "example.com", 3)
	if err != nil {
		t.Fatalf("FindPosition: %v", err)
	}
	if !got.Found || got.Rank != 2 || got.Page != 1 {
		t.Errorf("got found=%v rank=%d page=%d, want true 2 1", got.Found, got.Rank, got.Page)
	}
	if got.How != MatchHost {
		t.Errorf("How=%q, want %q", got.How, MatchHost)
	}
}

func TestFindPosition_RankIsCumulativeAcrossPages(t *testing.T) {
	// A rank is the site's place in the whole result list, not its place on
	// the page it happened to land on. Reporting "3" for the third entry of
	// page two would understate the position by a whole page.
	first := serpOf("a.test", "b.test", "c.test", "d.test", "e.test",
		"f.test", "g.test", "h.test", "i.test", "j.test")
	second := serpOf("k.test", "l.test", "example.com")
	f := &fakeSearcher{pages: []SERP{first, second}}
	got, err := FindPosition(context.Background(), f, Query{Text: "x"}, "example.com", 3)
	if err != nil {
		t.Fatalf("FindPosition: %v", err)
	}
	if got.Rank != 13 || got.Page != 2 {
		t.Errorf("rank=%d page=%d, want 13 on page 2", got.Rank, got.Page)
	}
}

func TestFindPosition_MatchesBySubdomainAndIgnoresWWW(t *testing.T) {
	// A site is a site whether Google rendered it with www or without, and a
	// blog on a subdomain is still the site's. Failing either would report a
	// loss the site never suffered.
	cases := []struct{ name, site, host string }{
		{"bare site, www host", "example.com", "www.example.com"},
		{"www site, bare host", "www.example.com", "example.com"},
		{"subdomain of the site", "example.com", "blog.example.com"},
		{"scheme and trailing slash in the site", "https://example.com/", "example.com"},
		{"trailing dot on the host", "example.com", "example.com."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSearcher{pages: []SERP{serpOf("a.test", tc.host)}}
			got, err := FindPosition(context.Background(), f, Query{Text: "x"}, tc.site, 1)
			if err != nil {
				t.Fatalf("FindPosition: %v", err)
			}
			if !got.Found || got.Rank != 2 {
				t.Errorf("found=%v rank=%d, want true 2", got.Found, got.Rank)
			}
		})
	}
}

func TestFindPosition_DoesNotMatchALookalikeDomain(t *testing.T) {
	// notexample.com ends with the site's name but is a different site, and
	// counting it would report a position the site does not hold.
	f := &fakeSearcher{pages: []SERP{serpOf("notexample.com", "example.com.evil.test")}}
	got, err := FindPosition(context.Background(), f, Query{Text: "x"}, "example.com", 1)
	if err != nil {
		t.Fatalf("FindPosition: %v", err)
	}
	if got.Found {
		t.Errorf("matched a lookalike at rank %d: %+v", got.Rank, got.Result)
	}
}

func TestFindPosition_ReportsNotFoundWithWhatItScanned(t *testing.T) {
	// "Not in the top 30" and "not in the top 100" are different answers, and
	// a caller cannot tell them apart from Found alone.
	f := &fakeSearcher{pages: []SERP{serpOf("a.test", "b.test"), serpOf("c.test")}}
	got, err := FindPosition(context.Background(), f, Query{Text: "x"}, "example.com", 5)
	if err != nil {
		t.Fatalf("FindPosition: %v", err)
	}
	if got.Found || got.How != MatchNone {
		t.Errorf("got found=%v how=%q, want false and %q", got.Found, got.How, MatchNone)
	}
	if got.Scanned != 3 {
		t.Errorf("Scanned=%d, want 3 — the caller needs the depth actually reached", got.Scanned)
	}
}

func TestFindPosition_PrefersAnExactURLWhenTheSiteNamesAPage(t *testing.T) {
	// Asking for a page rather than a site is a different question, and it can
	// only be answered where the address is known — which under the encrypted
	// link form is not everywhere.
	s := SERP{Results: []Result{
		{Position: 1, Host: "example.com", URL: "https://example.com/other", Form: LinkDirect},
		{Position: 2, Host: "example.com", URL: "https://example.com/wanted", Form: LinkDirect},
	}}
	f := &fakeSearcher{pages: []SERP{s}}
	got, err := FindPosition(context.Background(), f, Query{Text: "x"}, "https://example.com/wanted", 1)
	if err != nil {
		t.Fatalf("FindPosition: %v", err)
	}
	if !got.Found || got.Rank != 2 || got.How != MatchURL {
		t.Errorf("got found=%v rank=%d how=%q, want true 2 %q", got.Found, got.Rank, got.How, MatchURL)
	}
}

func TestFindPosition_AnswersAPageQuestionWithNothingRatherThanTheSite(t *testing.T) {
	// A caller who named a page asked where that page ranks. The site ranking
	// on some other page of its own is not that answer, and returning it would
	// be a position the named page does not hold. The second result below
	// carries no address at all, the ordinary case for about a third of them,
	// and that is exactly when falling back on the host is tempting.
	s := SERP{Results: []Result{
		{Position: 1, Host: "example.com", URL: "https://example.com/other", Form: LinkDirect},
		{Position: 2, Host: "example.com", Form: LinkEncrypted},
	}}
	f := &fakeSearcher{pages: []SERP{s}}
	got, err := FindPosition(context.Background(), f, Query{Text: "x"}, "https://example.com/wanted", 1)
	if err != nil {
		t.Fatalf("FindPosition: %v", err)
	}
	if got.Found {
		t.Errorf("answered a page question with rank %d, how=%q: %+v", got.Rank, got.How, got.Result)
	}
	if got.Scanned != 2 {
		t.Errorf("Scanned=%d, want 2", got.Scanned)
	}
}

func TestFindPosition_MatchesAPageAcrossSchemeAndTrailingSlash(t *testing.T) {
	// Google renders the same address with either scheme, with or without www,
	// and with or without the trailing slash. A caller who typed one of those
	// forms meant all of them.
	s := SERP{Results: []Result{
		{Position: 1, Host: "www.example.com", URL: "http://www.example.com/wanted/", Form: LinkDirect},
	}}
	f := &fakeSearcher{pages: []SERP{s}}
	got, err := FindPosition(context.Background(), f, Query{Text: "x"}, "https://example.com/wanted", 1)
	if err != nil {
		t.Fatalf("FindPosition: %v", err)
	}
	if !got.Found || got.How != MatchURL {
		t.Errorf("got found=%v how=%q, want true %q", got.Found, got.How, MatchURL)
	}
}

func TestFindPosition_StopsAtThePageThatAnswersTheQuestion(t *testing.T) {
	// Taking the remaining pages after the site is found spends a request each
	// to confirm what is already known. On a run of thousands of queries that
	// is most of the budget.
	f := &fakeSearcher{pages: []SERP{serpOf("a.test", "example.com"), serpOf("c.test"), serpOf("d.test")}}
	got, err := FindPosition(context.Background(), f, Query{Text: "x"}, "example.com", 3)
	if err != nil {
		t.Fatalf("FindPosition: %v", err)
	}
	if !got.Found {
		t.Fatal("the site was on page one and was not found")
	}
	if len(f.asked) != 1 {
		t.Errorf("took %d pages, want 1 - the answer was on the first", len(f.asked))
	}
}

func TestFindPosition_RejectsAnEmptySite(t *testing.T) {
	f := &fakeSearcher{pages: []SERP{serpOf("a.test")}}
	if _, err := FindPosition(context.Background(), f, Query{Text: "x"}, "  ", 1); err == nil {
		t.Error("FindPosition accepted an empty site")
	}
}
