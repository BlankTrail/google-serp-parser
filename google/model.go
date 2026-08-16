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
	Link        string
	Form        LinkForm
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

// Ad is one paid placement. Unlike an organic result its destination is in
// the page in plain text even when the click URL is encrypted, so URL is
// populated without any extra request.
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
	Query         string
	Results       []Result
	Ads           []Ad
	Related       []string
	PeopleAlsoAsk []string

	// TotalResults is Google's own estimate. HasTotal separates a real zero
	// from a page that did not state one — every captured page so far has
	// carried an empty #result-stats, so absence is the common case.
	TotalResults int64
	HasTotal     bool
}
