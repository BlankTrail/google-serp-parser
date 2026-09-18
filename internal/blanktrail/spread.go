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

// Worn is the identity a job asked its ports to wear. Anything left empty is
// every value this program knows for it, so the zero Worn is the whole matrix
// and a fully named one is a single identity.
//
// Release is a number rather than a version string because that is what a
// browser filter carries — the service reads "chrome_153" as the browser and
// the release — and nought is the newest the service holds.
type Worn struct {
	Browser string
	OS      string
	Release int
}

// String is the identity written out, for the one place it is said aloud: the
// refusal when nothing in the matrix matches it.
func (w Worn) String() string {
	var parts []string
	if w.Browser != "" {
		name := w.Browser
		if w.Release > 0 {
			name = fmt.Sprintf("%s %d", name, w.Release)
		}
		parts = append(parts, name)
	}
	if w.OS != "" {
		parts = append(parts, w.OS)
	}
	if len(parts) == 0 {
		return "every browser and system"
	}
	return strings.Join(parts, " on ")
}

// narrow is the pairs this identity leaves standing. Naming neither side
// leaves all of them, which is what makes the zero value the whole matrix.
func (w Worn) narrow(pairs []pair) []pair {
	browser, os := strings.ToLower(strings.TrimSpace(w.Browser)), strings.ToLower(strings.TrimSpace(w.OS))
	if browser == "" && os == "" {
		return pairs
	}
	var out []pair
	for _, p := range pairs {
		if (browser == "" || p.browser == browser) && (os == "" || p.os == os) {
			out = append(out, p)
		}
	}
	return out
}

// ReleasesPerBrowser is how many releases of one browser a spread that names no
// version draws on.
//
// Ten, and it is a number about how a fleet looks rather than about this
// program: real traffic to Google from a network of any size is not all on the
// build that shipped this week — it is this one and the nine before it, in
// whatever proportion people update — and a run whose every port is the newest
// release is a run that is one identity however many ports it has. Further back
// than ten is a fleet that never updates, which reads as its own kind of
// strange.
const ReleasesPerBrowser = 10

// Spread is the templates a pool opens its ports under: every browser this program knows on every system it ships
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
func Spread(ctx context.Context, cl *Client, device string, want Worn, releases, ports int) ([]NamedSpec, error) {
	if releases < 1 {
		releases = 1
	}
	pairs := want.narrow(pairsFor(device))
	if len(pairs) == 0 {
		return nil, fmt.Errorf("blanktrail: %s is not a browser and system a %s port is opened as", want, device)
	}

	// One listing per browser rather than one per combination: the versions a
	// browser has do not depend on the system it is asked for.
	//
	// A named release is taken as it stands and nothing is listed at all. It is
	// the caller saying which build, and a build the service does not hold is
	// theirs to be told about by the port that will not open, rather than
	// quietly replaced here with one it does.
	versions := map[string][]int{}
	for _, p := range pairs {
		if _, done := versions[p.browser]; done {
			continue
		}
		if want.Release > 0 {
			versions[p.browser] = []int{want.Release}
			continue
		}
		// A service that will not say what it holds is not a reason to refuse
		// the job. What is lost is the spread over releases, and what is left is
		// a browser named without one — which the service reads as the newest it
		// has, so the run goes out on a real fingerprint either way. The reasons
		// a listing fails are the service being unreachable or not having that
		// endpoint, and the first of the two is about to be reported by the
		// ports refusing to open, loudly, through the same connection.
		held, err := cl.NewestVersions(ctx, p.browser, releases)
		if err != nil {
			held = nil
		}
		versions[p.browser] = held
	}

	var out []NamedSpec
	for _, p := range pairs {
		held := versions[p.browser]
		if len(held) == 0 {
			// Nothing known about this browser's releases, so it is asked for by
			// name alone.
			out = append(out, template(p.browser, p.browser, p.os))
			continue
		}
		for _, v := range held {
			out = append(out, template(fmt.Sprintf("%s_%d", p.browser, v),
				fmt.Sprintf("%s_%d", p.browser, v), p.os))
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

// Browsers is every browser this program knows a fingerprint for, in the order
// a chooser offers them.
//
// It is read off the same table Spread walks rather than written out again,
// because a screen offering a browser the matrix has never heard of offers a
// job that opens no port at all.
func Browsers() []string { return namesIn(func(p pair) string { return p.browser }) }

// Systems is every system this program knows, desktop ones first.
//
// Both kinds are in one list because the form that offers it also offers the
// kind of result page, and the two are separate boxes: a reader who picks
// Android and leaves the page on desktop is told so by Ships, which is a better
// answer than a list that silently rearranged itself.
func Systems() []string { return namesIn(func(p pair) string { return p.os }) }

// namesIn is every distinct value one side of the pairs table takes, desktop
// before mobile, in the order the tables are written.
func namesIn(of func(pair) string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range append(append([]pair{}, desktopPairs...), mobilePairs...) {
		if name := of(p); !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// Ships says whether what a job named is something Spread would open a port
// for: a browser on a system, on the kind of result page asked for.
//
// Naming neither is the whole spread and is always allowed. Naming one of the
// two is allowed where anything at all is opened under it — Android on a
// desktop job is not, and neither is Safari on a job that never reaches macOS
// or iOS. Naming both is allowed only for a pair that exists in the world,
// which is the point: an identity that never existed reads as itself.
func Ships(device, browser, os string) bool {
	browser, os = strings.ToLower(strings.TrimSpace(browser)), strings.ToLower(strings.TrimSpace(os))
	if browser == "" && os == "" {
		return true
	}
	for _, p := range pairsFor(device) {
		if (browser == "" || p.browser == browser) && (os == "" || p.os == os) {
			return true
		}
	}
	return false
}

// template is one port of a spread: what to call it, and the browser filter and
// system it is opened under.
func template(name, browser, os string) NamedSpec {
	spec := DefaultPortSpec()
	spec.Browser = browser
	spec.OS = os
	return NamedSpec{Name: name + "_" + os, Spec: spec}
}
