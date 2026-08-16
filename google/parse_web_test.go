// SPDX-License-Identifier: MIT

package google

import (
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
	// Unlike organic results, an ad's destination is in the page even when
	// the click URL is encrypted — data-pcu holds it. No resolution needed.
	s := parseFixture(t, "serp_ads_us.html")
	if len(s.Ads) == 0 {
		t.Fatal("no ads parsed from the ads fixture")
	}
	var withURL int
	for _, a := range s.Ads {
		if a.URL != "" {
			withURL++
		}
	}
	if withURL == 0 {
		t.Error("no ad carried a destination; data-pcu was not read")
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
