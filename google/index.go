// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// ErrNoTarget is returned when an index check is asked about nothing.
var ErrNoTarget = errors.New("google: index target is empty")

// indexSample is how many results a status carries alongside its verdict.
// Enough for a reader to see what was found, and no more: the verdict is the
// answer, and the page behind it is what ListIndexed exists to fetch.
const indexSample = 3

// IndexStatus is what Google knows about one address or site.
//
// Hits is counted from the results, not read from the page: no captured or
// live page has ever stated a result total, so a count taken from the page
// would be a number this program invented.
type IndexStatus struct {
	Target  string
	Indexed bool
	Hits    int
	Sample  []Result
}

// CheckIndexed answers whether Google holds a page or a site.
//
// One page of one query settles it. The question is presence, not rank, so the
// first result that is the target is the answer.
//
// Only results that are the target count. A site: query is a request rather
// than a guarantee, and Google mixes in results from elsewhere often enough
// that a page holding nothing but those would otherwise answer "indexed" — a
// verdict about a target Google never mentioned. It is the same fault
// ListIndexed below already filters for, and it is worse here, because a
// listing is read as a listing and a verdict is read as an answer.
//
// The rule for what counts is the one FindPosition uses, and for its reasons: a
// target with a path is the exact address and nothing else of the site, and a
// bare host is any page of the site. So "is example.com/page indexed" is not
// answered by example.com/other, and "is example.com indexed" is answered by
// any page of it.
//
// The cost is carried in the same place FindPosition carries it. Under the link
// form that gives a host and no address, a target with a path cannot be
// recognised at all and this answers "not indexed": the results carry nothing
// to compare the address against. That answer is wrong for a page that is held,
// and it is the direction to be wrong in — the other one credits a site with a
// page on evidence that named somebody else. A caller who needs the address on
// every result resolves them first; see ResolveAll.
//
// The caller's Query supplies the axes — country, language, device — and its
// text is replaced by the operator.
//
// A target is reduced to a host and a path, because that is as far as the
// operator reaches. Asking about an address with a query string therefore asks
// about its path, and a positive answer establishes the path rather than the
// exact address the caller named — see siteQuery.
//
// A page carrying no results answers "not indexed" and returns no error:
// Google saying it found nothing is a successful capture, and for a target
// that is genuinely absent it is the correct and ordinary answer. A response
// that could not be read returns an error and no verdict. Everything here
// rests on those two staying apart — a check that reported them alike would
// answer every unreadable response with "not indexed", which is a wrong answer
// a caller has no way to disbelieve.
func CheckIndexed(ctx context.Context, s Searcher, q Query, target string) (IndexStatus, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return IndexStatus{}, ErrNoTarget
	}
	q.Text = siteQuery(target)
	q.Page = 1

	serp, err := s.Search(ctx, q)
	if err != nil {
		return IndexStatus{Target: target}, err
	}

	wantURL, wantHost := splitSite(target)
	st := IndexStatus{Target: target}
	for _, r := range serp.Results {
		if !isTarget(r, wantURL, wantHost) {
			continue
		}
		st.Hits++
		// The sample is evidence for the verdict, so it holds what the verdict
		// was reached on. Filled from the head of the page instead it would show
		// the reader somebody else's pages under the sentence saying their page
		// is held.
		if len(st.Sample) < indexSample {
			st.Sample = append(st.Sample, r)
		}
	}
	st.Indexed = st.Hits > 0
	return st, nil
}

// isTarget reports whether one result is the thing that was asked about.
//
// The two branches are the split splitSite makes and they are not
// interchangeable. A question about a page answered by the host would report
// example.com/page as held because example.com/other came back, which is the
// fallback the ranking engine already refused: it credits a page with a
// presence its site has and it does not.
func isTarget(r Result, wantURL, wantHost string) bool {
	if wantURL != "" {
		// A result under the encrypted link form carries no address, so it
		// cannot answer a question asked about one.
		return r.Resolved() && sameURL(r.URL, wantURL)
	}
	return hostBelongsTo(r.Host, wantHost)
}

// ListIndexed walks a site: query and returns the results that are actually on
// the site.
//
// The filter is not paranoia: a site: query is a request rather than a
// guarantee, and Google mixes in results from elsewhere often enough that
// reporting them as the site's indexed pages would be wrong.
//
// A page reported on more than one result page is returned once. The parser
// already drops a result linked twice within a page, and a listing whose
// length is read as a count of what is indexed would be inflated by counting
// the repeat again.
//
// A failure returns the results already gathered alongside the error, for the
// same reason the walk does: pages taken are pages paid for.
//
// Under the encrypted link form these results carry an exact host but no
// address. Callers wanting addresses must resolve them — see ResolveAll.
func ListIndexed(ctx context.Context, s Searcher, q Query, site string, pages int) ([]Result, error) {
	site = strings.TrimSpace(site)
	if site == "" {
		return nil, ErrNoTarget
	}
	q.Text = siteQuery(site)
	want := normaliseSite(site)

	serps, err := SearchDepth(ctx, s, q, pages)
	var out []Result
	seen := map[string]bool{}
	for _, serp := range serps {
		for _, r := range serp.Results {
			if !hostBelongsTo(r.Host, want) {
				continue
			}
			// Repeats are told apart by the same key the page parser uses, so
			// the two agree on what one result is. A result carrying neither a
			// link nor a title is kept: nothing identifies it, and dropping it
			// would shorten the listing on no evidence at all.
			key := r.Link
			if key == "" {
				key = r.Title
			}
			if key != "" {
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			out = append(out, r)
		}
	}
	return out, err
}

// siteQuery renders the operator for an address or a site, dropping the scheme
// and a leading www that Google does not want inside it.
//
// A query string and a fragment are dropped with them, and that is a decision
// rather than an oversight: the operator matches a host and a path, so there
// is nothing to be gained by carrying the rest and a malformed query to be had
// by trying. The cost is stated where callers read it — a check on an address
// with a query string is a check on its path, and answers the broader
// question. There is no narrower one to ask.
func siteQuery(target string) string {
	raw := strings.TrimSpace(target)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "site:" + normaliseSite(target)
	}
	out := normaliseSite(u.Hostname())
	if p := strings.TrimRight(u.EscapedPath(), "/"); p != "" {
		out += p
	}
	return "site:" + out
}
