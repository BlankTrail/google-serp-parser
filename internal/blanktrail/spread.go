// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
)

// StoredProfile is one fingerprint the service holds, as its listing reports it.
type StoredProfile struct {
	Name    string
	Browser string
	Version string
}

// profilesPage is how the service answers a listing.
type profilesPage struct {
	Profiles []struct {
		Name      string `json:"name"`
		Browser   string `json:"browser"`
		Version   string `json:"version"`
		UserAgent string `json:"user_agent"`
	} `json:"profiles"`
	Total int `json:"total"`
}

// Profiles lists the fingerprints the service holds for one browser.
//
// The browser is required rather than optional, and the reason is in the
// service's own handler: without it the listing is ordered by name, the ten
// thousand Chrome profiles fill every page, and the other browsers are never
// reached at all.
func (c *Client) Profiles(ctx context.Context, browser string, limit, offset int) ([]StoredProfile, int, error) {
	if limit < 1 {
		limit = 100
	}
	path := fmt.Sprintf("/api/v1/profiles?browser=%s&limit=%d&offset=%d",
		strings.ToLower(strings.TrimSpace(browser)), limit, offset)
	var page profilesPage
	if err := c.doJSON(ctx, "GET", path, nil, &page); err != nil {
		return nil, 0, err
	}
	out := make([]StoredProfile, 0, len(page.Profiles))
	for _, p := range page.Profiles {
		out = append(out, StoredProfile{Name: p.Name, Browser: p.Browser, Version: p.Version})
	}
	return out, page.Total, nil
}

// NewestVersions is the newest n release numbers the service holds for a
// browser, newest first.
//
// It is the release number and not the whole version string because that is
// what a browser filter carries: the service reads "chrome_153" as the browser
// and the release, and a filter naming a build nobody has is a filter nothing
// matches.
func (c *Client) NewestVersions(ctx context.Context, browser string, n int) ([]int, error) {
	if n < 1 {
		n = 1
	}
	// One page, large. The listing is ordered by name rather than by version,
	// so there is no page that holds "the newest": what settles it is having
	// enough of them to sort.
	held, _, err := c.Profiles(ctx, browser, 1000, 0)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	for _, p := range held {
		if v, ok := releaseOf(p.Version); ok {
			seen[v] = true
		}
	}
	out := make([]int, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	if len(out) > n {
		out = out[:n]
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("blanktrail: the service holds no %s profile with a release number", browser)
	}
	return out, nil
}

// releaseOf reads the release number off a version string: the leading digits of
// "153", "153.0.7258.5" and "26.6" alike.
func releaseOf(version string) (int, bool) {
	version = strings.TrimSpace(version)
	end := 0
	for end < len(version) && version[end] >= '0' && version[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(version[:end])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// pair is one browser on one system, as a fingerprint that exists in the world.
type pair struct{ browser, os string }

// desktopPairs and mobilePairs are the combinations a job spreads over.
//
// They are written out rather than multiplied, because the product of every
// browser and every system contains combinations that do not exist: Safari does
// not ship on Windows, Edge does not ship on iOS, and an identity that never
// existed reads as itself. Safari is in the desktop set and paired with macOS
// alone, which is where it ships.
//
// The three browsers on iOS are deliberate and are one identity with three
// names. Measured on a live iPhone: Chrome, Safari and Firefox there answered
// with the same ClientHello and the same HTTP/2 settings — they are all the
// system engine — and what differs is the user agent. A phone fleet that is all
// Safari is a fleet nobody has.
var (
	desktopPairs = []pair{
		{"chrome", "windows"}, {"chrome", "macos"}, {"chrome", "linux"},
		{"firefox", "windows"}, {"firefox", "macos"}, {"firefox", "linux"},
		{"edge", "windows"}, {"edge", "macos"},
		{"safari", "macos"},
	}
	mobilePairs = []pair{
		{"chrome", "android"}, {"firefox", "android"}, {"edge", "android"},
		{"safari", "ios"}, {"chrome", "ios"}, {"firefox", "ios"},
	}
)

// pairsFor is the combinations one kind of result page is asked for.
func pairsFor(device string) []pair {
	if device == DeviceMobile {
		return mobilePairs
	}
	return desktopPairs
}

// Spread is the templates a pool opens its ports under when the job named no
// browser of its own: every browser this program knows on every system it ships
// on, each at the newest few releases the service holds.
//
// What it is for is that a run of three hundred identities that are all the
// latest Chrome on Windows is one identity three hundred times over. The
// versions come from the service rather than from a table here, because a table
// of release numbers is out of date the week after it is written and wrong in a
// way nothing reports: a filter naming a release the service does not hold is a
// filter that quietly falls back to something else.
//
// It is cut to the number of ports. A pool opens one port per template at least,
// and refuses to open at all when it has fewer ports than templates — so a job
// of two ports is given two of the combinations rather than a refusal, drawn at
// random so that two such jobs are not the same two.
func Spread(ctx context.Context, cl *Client, device string, releases, ports int) ([]NamedSpec, error) {
	if releases < 1 {
		releases = 1
	}
	pairs := pairsFor(device)

	// One listing per browser rather than one per combination: the versions a
	// browser has do not depend on the system it is asked for.
	versions := map[string][]int{}
	for _, p := range pairs {
		if _, done := versions[p.browser]; done {
			continue
		}
		held, err := cl.NewestVersions(ctx, p.browser, releases)
		if err != nil {
			return nil, err
		}
		versions[p.browser] = held
	}

	var out []NamedSpec
	for _, p := range pairs {
		for _, v := range versions[p.browser] {
			spec := DefaultPortSpec()
			spec.Browser = fmt.Sprintf("%s_%d", p.browser, v)
			spec.OS = p.os
			out = append(out, NamedSpec{
				Name: fmt.Sprintf("%s_%d_%s", p.browser, v, p.os),
				Spec: spec,
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("blanktrail: no browser and system this program knows is held for %q", device)
	}

	if ports > 0 && len(out) > ports {
		rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		out = out[:ports]
		// Back into a readable order, so a screen listing the templates of a
		// pool reads as a list rather than as the order the shuffle left.
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	}
	return out, nil
}
