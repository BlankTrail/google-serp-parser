// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ErrNotRedirect is returned when a redirector answers with something other
// than a redirect. That normally means a challenge page: treating its address
// as the destination would record Google itself as the ranking site.
var ErrNotRedirect = errors.New("google: redirector did not answer with a redirect")

// ErrRedirectedIntoGoogle is returned when the redirect the redirector answered
// with stays on Google.
//
// It is the same fault as ErrNotRedirect wearing a redirect's clothes: the
// identity has been walled and sent to a challenge, or the link has been handed
// on to another redirector, and either way the header does not hold the address
// of the result. It is told apart because it says something ErrNotRedirect does
// not — this identity is being refused — and the caller acts on that by moving
// to another one rather than by blaming the link.
var ErrRedirectedIntoGoogle = errors.New("google: the redirect stays on Google, so it is not the address")

// classifyLink decides what a result's href is and, where the page allows it,
// what it points at.
//
// Three forms are in the wild and which one arrives is Google's choice: two
// captures of the same query, same session, minutes apart, returned different
// forms. All three are therefore supported and the form is decided per link.
//
// The encrypted form yields no destination. Its payload is a protobuf
// envelope wrapping ciphertext under a per-page key — checked against double
// URL-decoding, base64 in four alphabets, gzip, flate and protobuf unwrapping
// at every offset, with no plaintext anywhere. Only the network can answer it;
// see Resolver.
func classifyLink(href string) (dest string, form LinkForm, ok bool) {
	href = strings.TrimSpace(href)
	if href == "" {
		return "", "", false
	}

	switch {
	case strings.HasPrefix(href, "/goto"):
		return "", LinkEncrypted, true

	case strings.HasPrefix(href, "/url"):
		u, err := url.Parse(href)
		if err != nil {
			return "", "", false
		}
		// Read the destination with the URL parser: it arrives percent-encoded
		// and may itself contain a query string, which any hand-rolled split
		// would truncate.
		for _, key := range []string{"q", "url"} {
			if v := u.Query().Get(key); v != "" {
				if d, err := url.Parse(v); err == nil && d.Host != "" && !isGoogleHost(d.Hostname()) {
					return v, LinkRedirect, true
				}
			}
		}
		return "", "", false

	case strings.HasPrefix(href, "http://"), strings.HasPrefix(href, "https://"):
		u, err := url.Parse(href)
		if err != nil || u.Host == "" || isGoogleHost(u.Hostname()) {
			return "", "", false
		}
		return href, LinkDirect, true
	}

	// Anything else is navigation within the result page — another page of
	// results, a settings link, an anchor.
	return "", "", false
}

// isGoogleHost reports whether a host is one of Google's own properties. They
// appear among the results and are not organic entries.
//
// The test is structural: "google" must be the label immediately left of the
// public suffix. A substring test would be wrong in a way that costs results —
// a legitimate site such as google.example.com would be dropped, and every
// position below it would shift up to fill the gap.
func isGoogleHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	labels := strings.Split(host, ".")
	// A one-label suffix covers google.com and google.de; a two-label suffix
	// covers google.co.uk and google.com.br, but only for the second-level
	// labels that are actually used as suffixes — otherwise google.myshop.com
	// would match the same shape.
	secondLevel := map[string]bool{"co": true, "com": true, "net": true, "org": true, "ac": true}
	if i := len(labels) - 2; i >= 0 && labels[i] == "google" {
		return true
	}
	if i := len(labels) - 3; i >= 0 && labels[i] == "google" && secondLevel[labels[len(labels)-2]] {
		return true
	}
	return false
}

// Resolver turns an encrypted result link into the address it points at.
//
// It costs one request per link, and that is the whole cost: the redirect is
// not followed, so the destination site is never contacted and no body is
// read. The work is also independent of the session that captured the page —
// measured with links resolved hours later, from a different address, with no
// cookies — so it can be deferred, retried on its own, and carried out by a
// different client from the one that captured the page.
type Resolver struct {
	// Client carries the request. It must not follow redirects; NewResolver
	// builds one that does not.
	Client *http.Client
}

// NewResolver wraps a transport in a client configured for this job.
func NewResolver(rt http.RoundTripper) *Resolver {
	return &Resolver{Client: &http.Client{
		Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// Resolve returns the address behind one redirector link.
func (r *Resolver) Resolve(ctx context.Context, link string) (string, error) {
	client := r.Client
	if client == nil {
		client = &http.Client{}
	}
	// A caller-supplied client may follow redirects by default; stop it here
	// so Resolve behaves the same however it was constructed.
	local := *client
	local.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return "", fmt.Errorf("google: build resolve request: %w", err)
	}
	resp, err := local.Do(req)
	if err != nil {
		return "", fmt.Errorf("google: resolve: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", fmt.Errorf("%w: HTTP %d", ErrNotRedirect, resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("%w: no Location header", ErrNotRedirect)
	}
	u, err := url.Parse(loc)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("google: resolve returned an unusable Location %q", loc)
	}
	if isGoogleHost(u.Hostname()) {
		// Only the host is quoted. A challenge address carries the query it
		// refused inside it, and an error goes to logs and screens.
		return "", fmt.Errorf("%w: %s", ErrRedirectedIntoGoogle, u.Hostname())
	}
	return loc, nil
}
