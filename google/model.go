// SPDX-License-Identifier: MIT

package google

// LinkForm is how a result page expressed its outgoing links. Which one
// arrives is Google's choice, not the caller's, and it has been observed to
// differ between two captures of the same query minutes apart.
type LinkForm string

const (
	// LinkDirect is a plain href to the destination.
	LinkDirect LinkForm = "direct"
	// LinkRedirect is the classic /url?q= redirector, with the destination
	// readable in the query string.
	LinkRedirect LinkForm = "redirect"
	// LinkEncrypted is /goto?url= carrying ciphertext. The destination is not
	// in the page at all and can only be had by asking for it — see Resolver.
	LinkEncrypted LinkForm = "encrypted"
)

// Result is one organic result.
//
// Host is always exact. URL is not always known: under LinkEncrypted the page
// carries no address, only the host and a prettified path, and filling URL
// then costs one request per result. Consumers must branch on Resolved rather
// than treating an empty URL as an error.
type Result struct {
	Position    int
	Title       string
	Snippet     string
	Host        string
	DisplayPath string
	URL         string

	// Link is the href exactly as the page carried it, and it is not always a
	// URL. Under LinkDirect and LinkRedirect it is absolute. Under
	// LinkEncrypted it is origin-relative — "/goto?url=…" — because that is
	// all the markup holds, and handing it straight to Resolver.Resolve fails
	// with an empty protocol scheme. Join it to the origin the page came from,
	// SERP.Origin, before resolving it.
	Link string

	Form LinkForm
}

// Resolved reports whether this result carries the exact destination address.
func (r Result) Resolved() bool { return r.URL != "" }

// Placement is where on the page an ad sat. The three are separate products
// with separate markup, not three renderings of one thing.
type Placement string

const (
	// PlacementTop is the text ad block above the organic results.
	PlacementTop Placement = "top"
	// PlacementBottom is the text ad block below them.
	PlacementBottom Placement = "bottom"
	// PlacementProduct is the product listing block beside them.
	PlacementProduct Placement = "product"
)

// Ad is one paid placement.
//
// The two text placements carry the advertiser's address in the page in plain
// text even when the click URL is encrypted, so URL is populated without any
// extra request — unlike an organic result. PlacementProduct does not: the
// product listing block carries a title, and this parser reads nothing else
// from it, so URL and Host are empty there. Do not read an empty URL on a
// product listing as "the page did not say"; it means this parser did not
// look.
type Ad struct {
	Position  int
	Placement Placement
	Title     string
	Host      string
	Snippet   string
	URL       string
}

// SERP is one parsed result page.
//
// Ad presence is not reproducible: two captures of the same query in one
// session, ten seconds apart, returned six ads and none. Treat Ads as what
// was on the page at that moment, never as a complete account of who
// advertises on the query.
type SERP struct {
	Query string

	// Origin is the scheme and host the page was served from, which is what an
	// origin-relative Result.Link has to be joined to before it can be
	// fetched. Session.Search fills it in from where the response actually
	// landed. ParseSERP leaves it empty: it is handed a body and never learns
	// where that body came from.
	Origin string

	Results []Result
	Ads     []Ad
	Related []string

	// PeopleAlsoAsk is not populated yet — no code reads that block. It is
	// declared so the field's name and type are settled before anything
	// depends on them.
	PeopleAlsoAsk []string

	// TotalResults is Google's own estimate and HasTotal says whether it was
	// read. Neither is populated yet: nothing here reads #result-stats. So
	// HasTotal false currently means "this library did not look", not "Google
	// stated nothing" — and on every page measured so far, 21 captured and one
	// live, that element was empty in any case.
	TotalResults int64
	HasTotal     bool
}
