// SPDX-License-Identifier: MIT

package google

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// resultContainer is how far up from a link the parser climbs to find that
// result's own box. Class names are useless here — Google randomises them —
// so the climb stops at the nearest ancestor carrying one of Google's own
// structural attributes.
const resultContainer = "div[data-snc], div[data-snf], div[jscontroller], li, div"

// ParseSERP turns a result page into typed data. It classifies the body
// first: a page that is not results has nothing to parse, and saying so is
// more useful than returning an empty SERP that reads like a real zero.
func ParseSERP(query string, body []byte) (SERP, error) {
	if class, err := Classify(200, "", body); err != nil {
		return SERP{}, fmt.Errorf("google: parse %q: %w (class %s)", query, err, class)
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return SERP{}, fmt.Errorf("google: parse %q: %w", query, err)
	}

	s := SERP{Query: query}
	s.Results = parseOrganic(doc)
	s.Ads = parseAds(doc)
	s.Related = parseRelated(doc)
	return s, nil
}

// parseOrganic reads the organic results in page order.
func parseOrganic(doc *goquery.Document) []Result {
	var out []Result
	seen := map[string]bool{}

	doc.Find("a[data-ved]").Each(func(_ int, link *goquery.Selection) {
		if isAd(link) {
			return
		}
		title, _ := firstHeading(link)
		if title == "" {
			return
		}
		href, _ := link.Attr("href")
		dest, form, ok := classifyLink(href)
		if !ok {
			return
		}

		box := link.Closest(resultContainer)
		host, path, hasCite := citeHost(box)
		if !hasCite && dest != "" {
			// The direct and redirect forms carry the address, so the host is
			// known even on a layout that renders no cite.
			if u, err := url.Parse(dest); err == nil {
				host = u.Hostname()
			}
		}

		// One result is linked more than once — title, breadcrumb, sitelinks.
		// The first occurrence is the ranking one.
		key := href
		if key == "" {
			key = title
		}
		if seen[key] {
			return
		}
		seen[key] = true

		out = append(out, Result{
			Position:    len(out) + 1,
			Title:       title,
			Snippet:     snippetOf(box),
			Host:        host,
			DisplayPath: path,
			URL:         dest,
			Link:        href,
			Form:        form,
		})
	})
	return out
}

// snippetOf reads a result's description: the longest own-text run inside its
// box, which is the shape a snippet has regardless of how the box is built.
func snippetOf(box *goquery.Selection) string {
	best := ""
	box.Find("div, span").Each(func(_ int, s *goquery.Selection) {
		if t := ownText(s); len(t) > len(best) {
			best = t
		}
	})
	return best
}

// parseAds reads the three paid placements. They are separate products with
// separate markup, so each is read on its own terms rather than through one
// selector that would fit none of them well.
func parseAds(doc *goquery.Document) []Ad {
	var out []Ad
	for _, block := range []struct {
		selector  string
		placement Placement
	}{
		{"#tads [data-text-ad]", PlacementTop},
		{"#bottomads [data-text-ad]", PlacementBottom},
	} {
		doc.Find(block.selector).Each(func(_ int, unit *goquery.Selection) {
			link := unit.Find("a[href]").First()
			title, _ := firstHeading(unit)
			ad := Ad{
				Position:  len(out) + 1,
				Placement: block.placement,
				Title:     title,
				Snippet:   snippetOf(unit),
			}
			// data-pcu carries the advertiser's address in plain text even
			// when the click URL is encrypted. It may list a tracker after a
			// comma; the advertiser is first.
			if pcu, ok := link.Attr("data-pcu"); ok {
				first, _, _ := strings.Cut(pcu, ",")
				if u, err := url.Parse(strings.TrimSpace(first)); err == nil && u.Host != "" {
					ad.URL = u.String()
					ad.Host = u.Hostname()
				}
			}
			out = append(out, ad)
		})
	}

	// The right-hand block is product listings: no data-text-ad, and its
	// units are click links rather than headed text ads.
	doc.Find("#rhs a[href*='aclk']").Each(func(_ int, link *goquery.Selection) {
		title, _ := firstHeading(link)
		if title == "" {
			title = strings.Join(strings.Fields(link.Text()), " ")
		}
		if title == "" {
			return
		}
		out = append(out, Ad{
			Position:  len(out) + 1,
			Placement: PlacementProduct,
			Title:     title,
		})
	})
	return out
}

// parseRelated reads the "related searches" phrases. They are links back into
// search rather than out of it, which is exactly what identifies them.
func parseRelated(doc *goquery.Document) []string {
	var out []string
	seen := map[string]bool{}
	doc.Find("#botstuff a[href^='/search'], #bres a[href^='/search']").Each(func(_ int, s *goquery.Selection) {
		phrase := strings.Join(strings.Fields(s.Text()), " ")
		if phrase == "" || seen[phrase] {
			return
		}
		seen[phrase] = true
		out = append(out, phrase)
	})
	return out
}
