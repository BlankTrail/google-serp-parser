// SPDX-License-Identifier: MIT

package google

import (
	"strings"
	"testing"
)

func parseFixture(t *testing.T, name string) SERP {
	t.Helper()
	s, err := ParseSERP("test", fixture(t, name))
	if err != nil {
		t.Fatalf("ParseSERP(%s): %v", name, err)
	}
	return s
}

func TestParseSERP_CountsAndFormsMatchTheCapturedPages(t *testing.T) {
	// Every number here was measured on the full captured page before it was
	// minimised, so a mismatch means either the parser or the fixture is
	// wrong — which is the point.
	cases := []struct {
		file      string
		organic   int
		form      LinkForm
		topAds    int
		bottomAds int
	}{
		{"serp_goto_ru.html", 8, LinkEncrypted, 0, 0},
		{"serp_cards_ru.html", 10, LinkEncrypted, 0, 0},
		{"serp_site_ru.html", 10, LinkEncrypted, 0, 0},
		{"serp_ads_us.html", 10, LinkEncrypted, 4, 3},
		{"serp_direct_us.html", 8, LinkDirect, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			s := parseFixture(t, tc.file)
			if len(s.Results) != tc.organic {
				t.Errorf("organic=%d, want %d", len(s.Results), tc.organic)
			}
			for i, r := range s.Results {
				if r.Form != tc.form {
					t.Errorf("result %d form=%q, want %q", i+1, r.Form, tc.form)
				}
				if r.Position != i+1 {
					t.Errorf("result %d carries position %d", i+1, r.Position)
				}
				if r.Title == "" {
					t.Errorf("result %d has no title", i+1)
				}
			}
			var top, bottom int
			for _, a := range s.Ads {
				switch a.Placement {
				case PlacementTop:
					top++
				case PlacementBottom:
					bottom++
				}
			}
			if top != tc.topAds || bottom != tc.bottomAds {
				t.Errorf("ads top=%d bottom=%d, want %d and %d", top, bottom, tc.topAds, tc.bottomAds)
			}
		})
	}
}

func TestParseSERP_HostsComeOutExact(t *testing.T) {
	s := parseFixture(t, "serp_goto_ru.html")
	want := []string{"habr.com", "software-testing.ru", "testgrow.ru", "www.politerm.com"}
	for i, w := range want {
		if s.Results[i].Host != w {
			t.Errorf("result %d host=%q, want %q", i+1, s.Results[i].Host, w)
		}
	}

	direct := parseFixture(t, "serp_direct_us.html")
	wantDirect := []string{"www.apple.com", "en.wikipedia.org", "www.bestbuy.com", "www.verizon.com"}
	for i, w := range wantDirect {
		if direct.Results[i].Host != w {
			t.Errorf("direct result %d host=%q, want %q", i+1, direct.Results[i].Host, w)
		}
	}
}

func TestParseSERP_DirectLinksArriveResolved(t *testing.T) {
	// Under the direct form the address is already in the page; asking the
	// resolver for it would be a wasted request per result.
	s := parseFixture(t, "serp_direct_us.html")
	for i, r := range s.Results {
		if !r.Resolved() {
			t.Errorf("direct result %d is not resolved, URL=%q", i+1, r.URL)
		}
	}
}

func TestParseSERP_EncryptedLinksArriveUnresolvedButKeepTheirHost(t *testing.T) {
	// This is the honest half of the encrypted form: no address, but an exact
	// host, and a link the resolver can act on later.
	s := parseFixture(t, "serp_goto_ru.html")
	for i, r := range s.Results {
		if r.Resolved() {
			t.Errorf("encrypted result %d claims an address: %q", i+1, r.URL)
		}
		if r.Host == "" {
			t.Errorf("encrypted result %d lost its host", i+1)
		}
		if r.Link == "" {
			t.Errorf("encrypted result %d kept no link to resolve later", i+1)
		}
	}
}

func TestParseSERP_TheCardLayoutHasNoCiteAndSaysSo(t *testing.T) {
	// The card layout carries no cite at all, so the host is simply not
	// available until the link is resolved. Reporting an empty host is
	// correct; inventing one from the title would not be.
	s := parseFixture(t, "serp_cards_ru.html")
	if len(s.Results) != 10 {
		t.Fatalf("organic=%d, want 10", len(s.Results))
	}
	for i, r := range s.Results {
		if r.Host != "" {
			t.Errorf("card result %d produced host %q from a page with no cite", i+1, r.Host)
		}
		if r.Title == "" {
			t.Errorf("card result %d has no title", i+1)
		}
	}
}

func TestParseSERP_AdsCarryTheirAddressInPlainText(t *testing.T) {
	// Unlike organic results, a text ad's destination is in the page even when
	// the click URL is encrypted — data-pcu holds it. No resolution needed.
	//
	// The assertion is per placement, not over the whole slice: "some ad has a
	// URL" passes happily while an entire placement comes back with none,
	// which is exactly what the product listings were doing.
	s := parseFixture(t, "serp_ads_us.html")
	if len(s.Ads) == 0 {
		t.Fatal("no ads parsed from the ads fixture")
	}
	byPlacement := map[Placement][]Ad{}
	for _, a := range s.Ads {
		byPlacement[a.Placement] = append(byPlacement[a.Placement], a)
	}

	for _, p := range []Placement{PlacementTop, PlacementBottom} {
		ads := byPlacement[p]
		if len(ads) == 0 {
			t.Errorf("placement %q produced no ads at all", p)
			continue
		}
		for i, a := range ads {
			if a.URL == "" {
				t.Errorf("%s ad %d carries no destination; data-pcu was not read", p, i+1)
			}
			if a.Host == "" {
				t.Errorf("%s ad %d carries no host", p, i+1)
			}
		}
	}

	// The product listings are documented as carrying a title and nothing
	// else. If that ever stops being true, Ad's doc comment has to change with
	// it — which is what this half of the test is for.
	product := byPlacement[PlacementProduct]
	if len(product) == 0 {
		t.Error("the product ad block was not parsed")
	}
	for i, a := range product {
		if a.URL != "" || a.Host != "" {
			t.Errorf("product ad %d carries URL %q host %q — Ad says this placement has neither",
				i+1, a.URL, a.Host)
		}
	}
}

func TestParseSERP_SnippetLeavesOutTheDuplicatedByline(t *testing.T) {
	// A result's header field holds the byline twice — one copy inside the
	// result's own anchor, one beside it for a hover animation, both in the
	// DOM at all times. A walker with no notion of the header field returned
	// the site name doubled in every snippet on every real page: measured as
	// "Хабр Хабр 7 дек. 2022 г. — …" and "blanktrail.com blanktrail.com · …".
	for _, name := range []string{"serp_goto_ru.html", "serp_site_ru.html", "serp_direct_us.html"} {
		for i, r := range parseFixture(t, name).Results {
			if r.Host == "" {
				continue
			}
			if strings.Contains(r.Snippet, r.Host) {
				t.Errorf("%s result %d snippet carries its byline %q: %q",
					name, i+1, r.Host, r.Snippet)
			}
		}
	}
}

func TestParseSERP_AdSnippetLeavesOutTheDuplicatedByline(t *testing.T) {
	// An ad renders its byline — the advertiser's name beside its displayed
	// address — twice, once inside its own anchor and once beside it. Both
	// copies are in the DOM at all times, and a walker that does not know the
	// block opens every ad snippet with them: measured on real pages as
	// "Apple https://www.apple.com Apple https://www.apple.com Introducing…".
	//
	// The address carries data-dtld; the name beside it carries nothing, so
	// skipping only the marked element leaves "Apple Apple" behind. Asserting
	// on both halves is what keeps that half-fix from passing.
	s := parseFixture(t, "serp_ads_us.html")
	if len(s.Ads) == 0 {
		t.Fatal("the ads fixture parsed no ads")
	}
	for i, a := range s.Ads {
		if a.Host == "" {
			continue
		}
		if strings.Contains(a.Snippet, a.Host) {
			t.Errorf("ad %d snippet carries its address %q: %q", i+1, a.Host, a.Snippet)
		}
		// The display name is the host without its scheme and www prefix —
		// the label the page draws next to the address.
		name := strings.TrimPrefix(a.Host, "www.")
		if name != "" && strings.Contains(a.Snippet, name) {
			t.Errorf("ad %d snippet carries its advertiser name %q: %q", i+1, name, a.Snippet)
		}
		if a.Snippet == "" {
			t.Errorf("ad %d lost its description entirely", i+1)
		}
	}
}

func TestParseSERP_SnippetLeavesOutWhatThePageHidesFromReaders(t *testing.T) {
	// A video card draws its running time on the thumbnail inside an
	// aria-hidden="true" wrapper. It is beside the description, not part of
	// it, and a duration inside a sentence is a wrong answer that reads like a
	// right one.
	s := parseFixture(t, "serp_direct_us.html")
	for i, r := range s.Results {
		if strings.Contains(r.Snippet, "13:00") {
			t.Errorf("result %d snippet swallowed the hidden video overlay: %q", i+1, r.Snippet)
		}
	}
	if s.Results[len(s.Results)-1].Snippet != "Trade-in values by model." {
		t.Errorf("the video result's snippet=%q, want the description alone",
			s.Results[len(s.Results)-1].Snippet)
	}
}

func TestParseSERP_ProductAdsAreCountedSeparately(t *testing.T) {
	// The right-hand block is product listing ads — different markup,
	// different product, its own placement.
	s := parseFixture(t, "serp_ads_us.html")
	var product int
	for _, a := range s.Ads {
		if a.Placement == PlacementProduct {
			product++
		}
	}
	if product == 0 {
		t.Error("the product ad block was not parsed")
	}
}

func TestParseSERP_TheShellYieldsNothingAndAnError(t *testing.T) {
	if _, err := ParseSERP("test", fixture(t, "jsshell.html")); err == nil {
		t.Fatal("ParseSERP accepted the JavaScript shell")
	}
}

func TestParseSERP_NoResultPointsAtGoogleItself(t *testing.T) {
	// Google's own properties appear in the markup and are not organic
	// entries; one slipping in shifts every position below it.
	for _, name := range []string{"serp_goto_ru.html", "serp_direct_us.html", "serp_ads_us.html"} {
		for i, r := range parseFixture(t, name).Results {
			if r.Host != "" && isGoogleHost(r.Host) {
				t.Errorf("%s result %d is a Google property: %q", name, i+1, r.Host)
			}
		}
	}
}

func TestParseSERP_CiteDerivedGoogleHostIsExcluded(t *testing.T) {
	// Under the encrypted form the host comes only from the cite, never from
	// an address classifyLink could reject. It must be filtered through
	// isGoogleHost the same way classifyLink filters direct and redirect
	// links, or the result set depends on which link form Google happened to
	// answer with.
	s := parseFixture(t, "serp_goto_ru.html")
	for _, r := range s.Results {
		if r.Title == "Google Search Help" {
			t.Error("a result whose cite read a Google host was not filtered out")
		}
	}
}

func TestParseSERP_SnippetRecoversEmphasisedTerms(t *testing.T) {
	// Google wraps the query's own terms in em inside a real snippet.
	// ownText alone cannot see through that wrapping — the direct-children
	// rule drops the emphasised word entirely — so this snippet must come
	// back whole, not with a hole where "Разбор" belongs.
	s := parseFixture(t, "serp_goto_ru.html")
	want := "Разбор подходов к тестированию REST API и типичных ошибок."
	if s.Results[0].Snippet != want {
		t.Errorf("snippet=%q, want %q", s.Results[0].Snippet, want)
	}
}

// withNavigation wraps one organic result and one navigation block into a page
// the parser will accept, so a pagination case reads as the bar it describes.
func withNavigation(nav string) []byte {
	return []byte(`<!doctype html><html><body>` +
		`<div id="rso"><div data-snc="r0">` +
		`<a href="https://example.com/a" data-ved="x"><h3>A result</h3></a>` +
		`</div></div>` +
		`<div role="navigation">` + nav + `</div>` +
		`</body></html>`)
}

func TestParseSERP_ReadsHowFarThePaginationBarOffersToGo(t *testing.T) {
	// The bar is the page's own statement about how much further the results
	// go, and it is the only such statement on it: no page measured, captured
	// or live, has ever carried a result total.
	s := parseFixture(t, "serp_goto_ru.html")
	if !s.HasPagination {
		t.Fatal("the fixture's pagination bar was not found")
	}
	if s.MaxOffset != 40 {
		t.Errorf("MaxOffset=%d, want 40 — the furthest page the bar links to", s.MaxOffset)
	}
}

func TestParseSERP_ABarLinkingOnlyBackwardsIsStillFound(t *testing.T) {
	// The last page of a result set links to the pages before it and to nothing
	// after. That shape is the whole signal a walk stops on, so the bar has to
	// come back found with its furthest offset behind the page in hand.
	s, err := ParseSERP("x", withNavigation(
		`<a href="/search?q=x&amp;start=0">1</a><a href="/search?q=x&amp;start=10">2</a>`))
	if err != nil {
		t.Fatalf("ParseSERP: %v", err)
	}
	if !s.HasPagination {
		t.Fatal("a bar linking only to earlier pages was not found")
	}
	if s.MaxOffset != 10 {
		t.Errorf("MaxOffset=%d, want 10", s.MaxOffset)
	}
}

func TestParseSERP_NavigationCarryingNoOffsetIsNotAPaginationBar(t *testing.T) {
	// The vertical switches — Images, Videos, News — sit under role="navigation"
	// and link back into search exactly as the bar does. Counting them as a bar
	// would have the parser state that the results end on page one of every
	// query, which is the reverse of what this field is for.
	s, err := ParseSERP("x", withNavigation(
		`<a href="/search?q=x&amp;udm=2">Images</a><a href="/search?q=x&amp;tbm=vid">Videos</a>`))
	if err != nil {
		t.Fatalf("ParseSERP: %v", err)
	}
	if s.HasPagination {
		t.Errorf("a vertical switch was read as a pagination bar, MaxOffset=%d", s.MaxOffset)
	}
}

func TestParseSERP_APageWithNoNavigationAtAllReportsNoBar(t *testing.T) {
	// Absence has to stay absence. A layout whose control this parser cannot
	// find must not be reported as a page that offered nothing further.
	s, err := ParseSERP("x", []byte(`<!doctype html><html><body><div id="rso">`+
		`<div data-snc="r0"><a href="https://example.com/a" data-ved="x"><h3>A result</h3></a></div>`+
		`</div></body></html>`))
	if err != nil {
		t.Fatalf("ParseSERP: %v", err)
	}
	if s.HasPagination || s.MaxOffset != 0 {
		t.Errorf("HasPagination=%v MaxOffset=%d, want false and 0", s.HasPagination, s.MaxOffset)
	}
}

func TestParseSERP_RelatedExcludesPaginationAndReadsThePhraseFromQ(t *testing.T) {
	// The pagination bar sits in the same container as the related-search
	// chips and answers to the same href prefix, so it must be told apart by
	// structure (role="navigation", a start parameter) rather than assumed
	// absent. And one chip's anchor text arrives out of visual order, so the
	// phrase must be read from the query parameter, not the anchor text.
	s := parseFixture(t, "serp_goto_ru.html")
	want := []string{"related one", "related two", "related three"}
	if len(s.Related) != len(want) {
		t.Fatalf("related=%d %v, want %d %v", len(s.Related), s.Related, len(want), want)
	}
	for i, w := range want {
		if s.Related[i] != w {
			t.Errorf("related %d=%q, want %q", i+1, s.Related[i], w)
		}
	}
}
