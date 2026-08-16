// SPDX-License-Identifier: MIT

package google

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

// resultBox returns the element that holds one whole result — title, cite and
// description together.
//
// The climb is staged rather than one comma-separated group: Closest returns
// the NEAREST ancestor matching any branch, so a group ending in a bare div
// always stops at the innermost wrapper, which carries the title and the cite
// but not the description — measured on 9 of 9 results checked across three
// real pages.
func resultBox(link *goquery.Selection) *goquery.Selection {
	for _, sel := range []string{"div[data-snc]", "div[data-snf]", "div[jscontroller]"} {
		if box := link.Closest(sel); box.Length() > 0 {
			return box
		}
	}
	return link.Closest("li, div")
}

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

		box := resultBox(link)
		host, path, hasCite := citeHost(box)
		if !hasCite && dest != "" {
			// The direct and redirect forms carry the address, so the host is
			// known even on a layout that renders no cite.
			if u, err := url.Parse(dest); err == nil {
				host = u.Hostname()
			}
		}
		if host != "" && isGoogleHost(host) {
			// classifyLink already excludes Google's own properties for the
			// direct and redirect forms, checked against the address itself.
			// Under the encrypted form there is no address yet — the cite is
			// the only place a Google host can show up — so it needs the same
			// filter, or the same page yields a different result set purely
			// because of which link form Google happened to answer with.
			return
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

// snippetOf reads a result's description: everything in its box except the
// title, the displayed address, and what the page draws for the eye rather
// than writes for the reader.
//
// It cannot use ownText: Google wraps the query's own terms in em and b
// inside the snippet, and taking only direct text children would drop them,
// leaving a sentence with a hole where the match was.
func snippetOf(box *goquery.Selection) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "h3", "cite", "style", "script":
				return
			}
			if outsideTheDescription(n) {
				return
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteString(" ")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	for _, n := range box.Nodes {
		walk(n)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// outsideTheDescription reports whether an element's own attributes say its
// text is not part of a result's description. Every test here is an attribute
// on the element itself — no CSS class, no pattern matching.
//
//   - data-snhf marks a result's header field: its title, its byline and its
//     cite. Google renders that block TWICE inside one result box — once
//     inside the result's own anchor, once beside it for a hover animation —
//     which is why the site name arrived doubled in every snippet: measured on
//     a real page as "Хабр Хабр 7 дек. 2022 г. — …". Skipping the header
//     field drops both copies and leaves the description behind.
//   - role="heading" is the card layout's title, which has no h3 to skip.
//   - aria-hidden="true" is the page saying this text is not read to anyone:
//     favicons, thumbnails, and the duration drawn over a video card.
//   - an inline visibility:hidden or display:none is the same statement made
//     in CSS. Looking for it with strings.Contains over one attribute's value
//     is neither a class selector nor a regular expression.
//
// A layout that carries no data-snhf is left alone: the card layout has no
// duplicated header at all. A few video and rich results do carry the
// duplicate without the attribute, and their byline is still doubled — there
// the second copy is hidden by a stylesheet class, which this parser
// deliberately cannot see. Measured over the corpus: 134 of 138 results come
// out clean, the remaining 4 are those.
func outsideTheDescription(n *html.Node) bool {
	for _, a := range n.Attr {
		switch a.Key {
		case "data-snhf":
			return true
		case "role":
			if a.Val == "heading" {
				return true
			}
		case "aria-hidden":
			if a.Val == "true" {
				return true
			}
		case "style":
			v := strings.ReplaceAll(strings.ToLower(a.Val), " ", "")
			if strings.Contains(v, "visibility:hidden") || strings.Contains(v, "display:none") {
				return true
			}
		}
	}
	return false
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
			// The address is taken from whichever anchor in the unit carries
			// data-pcu, not from the first anchor: measured, one bottom
			// placement in the corpus leads with a sitelink that has no
			// data-pcu while another anchor in the same unit does, and reading
			// only the first left that ad with no destination at all.
			link := unit.Find("a[data-pcu]").First()
			if link.Length() == 0 {
				link = unit.Find("a[href]").First()
			}
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

	// The right-hand block is product listings: no data-text-ad, and its units
	// are click links rather than headed text ads. Only the title is read, and
	// Ad.URL stays empty here — see Ad. The click address does carry an adurl
	// parameter, but it was present and empty on all 27 such links in the
	// corpus, so there is no advertiser address in the page to take.
	//
	// On those same pages this branch produces nothing: every #rhs click link
	// there is an empty placeholder carrying no text. A product listing block
	// with readable titles reaches this parser in the shape the fixture models
	// and has not been seen in a capture since.
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
// search rather than out of it, which is exactly what identifies them — but
// the pagination bar lives in the same containers and answers to the same
// selector, so it must be told apart rather than assumed absent.
func parseRelated(doc *goquery.Document) []string {
	var out []string
	seen := map[string]bool{}
	doc.Find("#botstuff a[href^='/search'], #bres a[href^='/search']").Each(func(_ int, s *goquery.Selection) {
		if s.Closest("[role=navigation]").Length() > 0 {
			return // the pagination bar, not a suggestion
		}
		href, _ := s.Attr("href")
		u, err := url.Parse(href)
		if err != nil {
			return
		}
		q := u.Query()
		// An offset or a vertical switch is navigation within this search,
		// not a different search being suggested.
		if q.Get("q") == "" || q.Has("start") || q.Has("tbm") || q.Has("udm") || q.Has("tbs") {
			return
		}
		// The phrase comes from the query parameter, not the anchor text:
		// measured with the anchor's text nodes out of reading order.
		phrase := strings.Join(strings.Fields(q.Get("q")), " ")
		if phrase == "" || seen[phrase] {
			return
		}
		seen[phrase] = true
		out = append(out, phrase)
	})
	return out
}
