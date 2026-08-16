// SPDX-License-Identifier: MIT

package google

import (
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

// adContainers are the blocks a paid placement can live in. Ads carry the
// same heading markup as organic results, so anything inside these is
// excluded from the organic set — without that, every ranking is inflated by
// the number of ads sitting above it.
const adContainers = "#tads, #bottomads, #rhs, [data-text-ad]"

// isAd reports whether a link sits inside a paid placement.
func isAd(s *goquery.Selection) bool {
	return s.Closest(adContainers).Length() > 0
}

// ownText returns the text a node holds directly, ignoring text inside its
// child elements. A snippet is the container's own prose; descendant text
// would drag the title and the breadcrumb into it.
func ownText(s *goquery.Selection) string {
	var b strings.Builder
	for _, n := range s.Nodes {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				b.WriteString(c.Data)
			}
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// firstHeading returns a link's title and reports which layout supplied it.
//
// Two layouts are in the wild and a page uses one or the other throughout.
// The common one titles results with an h3. The card layout has no h3 and no
// cite anywhere on the page and titles results with role="heading". Both
// signals are semantic; neither is a CSS class, which is the point — Google
// randomises class names between builds.
func firstHeading(s *goquery.Selection) (text string, viaRole bool) {
	if h := s.Find("h3").First(); h.Length() > 0 {
		if t := strings.Join(strings.Fields(h.Text()), " "); t != "" {
			return t, false
		}
	}
	if h := s.Find("[role=heading]").First(); h.Length() > 0 {
		if t := strings.Join(strings.Fields(h.Text()), " "); t != "" {
			return t, true
		}
	}
	return "", false
}

// citeHost reads the displayed address out of a result container.
//
// The cite element is Google's own rendering of where a result points, and
// its host is exact. Its path is not: it is a prettified breadcrumb that
// drops and reorders segments, so it is returned separately and never
// assembled back into a URL.
//
// The slot is also reused for rich-snippet metadata — about one in twenty
// carries a line such as "20+ comments · 2 years ago" — so a candidate is
// accepted only if it parses as an absolute URL.
func citeHost(container *goquery.Selection) (host, path string, ok bool) {
	container.Find("cite").EachWithBreak(func(_ int, c *goquery.Selection) bool {
		text := strings.Join(strings.Fields(c.Text()), " ")
		head, rest, _ := strings.Cut(text, " ")
		u, err := url.Parse(head)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return true // keep looking; this cite is metadata, not an address
		}
		host = u.Hostname()
		path = strings.TrimSpace(strings.TrimPrefix(rest, "› "))
		ok = true
		return false
	})
	return host, path, ok
}
