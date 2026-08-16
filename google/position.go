// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// Match says how a site was recognised in the results.
type Match string

const (
	// MatchHost is a match on the result's host — the ordinary case.
	MatchHost Match = "host"
	// MatchURL is a match on the exact address, possible only where the page
	// carried one.
	MatchURL Match = "url"
	// MatchNone is no match within the depth scanned.
	MatchNone Match = "none"
)

// ErrNoSite is returned when a lookup is asked for an empty site.
var ErrNoSite = errors.New("google: site is empty")

// Position is where a site stands for one query.
//
// Scanned is how many results were actually examined, and it is not decoration:
// "not in the top 30" and "not in the top 100" are different answers, and Found
// alone cannot tell them apart. It is also the only honest account of the depth
// reached, because a walk ends where the page's own pagination bar ends rather
// than where the caller asked it to.
type Position struct {
	Query   string
	Site    string
	Rank    int
	Page    int
	Found   bool
	How     Match
	Result  Result
	Scanned int
}

// FindPosition walks up to pages of results and reports where site stands.
//
// Matching is by host by default, and that is a measurement rather than a
// preference: about a third of results arrive under a link form that carries no
// address at all, only an exact host. A lookup that insisted on the address
// would silently fail to find a site that is plainly there.
//
// When site names a page — anything with a path — the exact address is compared
// instead, and only results whose address is known can match. That is the
// honest behaviour: the question "is this page ranking" cannot be answered from
// a host, and answering it with the site's own other page would report a
// position the named page does not hold.
func FindPosition(ctx context.Context, s Searcher, q Query, site string, pages int) (Position, error) {
	site = strings.TrimSpace(site)
	if site == "" {
		return Position{}, ErrNoSite
	}

	pos := Position{Query: q.Text, Site: site, How: MatchNone}
	wantURL, wantHost := splitSite(site)

	// The walk stops at the page that answers the question. A site is usually
	// found on the first page, and taking the rest anyway would spend a request
	// each to confirm something already known.
	rank := 0
	err := SearchUntil(ctx, s, q, pages, func(pageNo int, serp SERP) bool {
		for _, r := range serp.Results {
			rank++
			pos.Scanned = rank
			if wantURL != "" {
				// A result under the encrypted link form carries no address, so
				// it cannot answer a question asked about one.
				if r.Resolved() && sameURL(r.URL, wantURL) {
					pos.Found, pos.How, pos.Rank, pos.Page, pos.Result = true, MatchURL, rank, pageNo, r
					return true
				}
				continue
			}
			if hostBelongsTo(r.Host, wantHost) {
				pos.Found, pos.How, pos.Rank, pos.Page, pos.Result = true, MatchHost, rank, pageNo, r
				return true
			}
		}
		return false
	})
	return pos, err
}

// splitSite reads what the caller asked for. A path means a page and is
// compared by address; anything else is a site and is compared by host.
func splitSite(site string) (wantURL, wantHost string) {
	raw := site
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", normaliseSite(site)
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		return u.String(), normaliseSite(u.Hostname())
	}
	return "", normaliseSite(u.Hostname())
}

// normaliseSite reduces a site to the host it means: lower case, no scheme, no
// path, no leading www, no trailing dot.
func normaliseSite(site string) string {
	site = strings.ToLower(strings.TrimSpace(site))
	if i := strings.Index(site, "://"); i >= 0 {
		site = site[i+3:]
	}
	if i := strings.IndexAny(site, "/?#"); i >= 0 {
		site = site[:i]
	}
	site = strings.TrimSuffix(site, ".")
	return strings.TrimPrefix(site, "www.")
}

// hostBelongsTo reports whether a result's host is the wanted site or a
// subdomain of it.
//
// The suffix is compared label by label. A plain string suffix would count
// notexample.com as example.com — a different site, and reporting it would
// credit the site with a position it does not hold.
func hostBelongsTo(host, want string) bool {
	host = normaliseSite(host)
	if host == "" || want == "" {
		return false
	}
	if host == want {
		return true
	}
	return strings.HasSuffix(host, "."+want)
}

// sameURL compares two addresses ignoring the scheme, a leading www and a
// trailing slash — differences Google renders freely and a caller never means.
func sameURL(a, b string) bool {
	return canonicalURL(a) == canonicalURL(b)
}

func canonicalURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimSpace(raw))
	}
	host := normaliseSite(u.Hostname())
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	if q := u.RawQuery; q != "" {
		return host + path + "?" + q
	}
	return host + path
}
