// SPDX-License-Identifier: MIT

package run

import (
	"context"

	"github.com/blanktrail/google-serp-parser/google"
)

// ResolveLinks fills in the addresses of the results that arrived without one,
// over a whole report, and says what it managed.
//
// This runs after the job rather than during it. Doing it as the walk goes
// doubles the requests exactly when it matters most to finish the capture,
// while the address behind such a link is measured to be readable later, from
// an unrelated address, with no cookies. What it cannot do is predict the work:
// the share of results arriving without an address is not a constant — two runs
// measured 19 of 54 and none of about 60 — so this counts what it finds instead
// of estimating it.
//
// What gets looked up is decided by what was captured, not by how the query
// ended. A walk that failed on a later page still hands back the pages it took,
// and those results are as worth completing as any other; a page nobody
// captured costs nothing here, because there is nothing on it to look up.
func (r *Runner) ResolveLinks(ctx context.Context, rep *Report, workers int) google.ResolveReport {
	var total google.ResolveReport
	for i := range rep.Results {
		pages := rep.Results[i].Pages
		for p := range pages {
			if err := ctx.Err(); err != nil {
				// One account of why the rest was left, rather than the same
				// one against every page still to come.
				total.Errs = append(total.Errs, err)
				return total
			}
			one := r.resolvePage(ctx, &pages[p], workers)
			total.Attempted += one.Attempted
			total.Resolved += one.Resolved
			total.Failed += one.Failed
			total.Errs = append(total.Errs, one.Errs...)
		}
	}
	return total
}

// resolvePage looks up the addresses missing from one page, through one
// identity.
//
// One identity for the page rather than one per link: the address behind a link
// does not depend on who captured the page, so a separate identity per link
// buys nothing and spends a whole page's worth of the pool to get it.
func (r *Runner) resolvePage(ctx context.Context, serp *google.SERP, workers int) google.ResolveReport {
	if !needsResolving(serp) {
		return google.ResolveReport{}
	}
	lease, err := r.Pool.Acquire(ctx)
	if err != nil {
		return google.ResolveReport{Errs: []error{err}}
	}
	defer lease.Release()

	client := lease.Client()
	resolver := google.NewResolver(client.Transport)
	// The pool was told how long one request may take. A resolver built on the
	// transport alone would drop that bound, and a call with no deadline of its
	// own would then wait on an unreachable identity for as long as it took.
	resolver.Client.Timeout = client.Timeout

	return resolver.ResolveAll(ctx, serp, workers)
}

// needsResolving reports whether a page holds any result whose address is still
// unknown. Asking first is what keeps a page that states its addresses from
// costing an identity to discover there was nothing to do.
func needsResolving(serp *google.SERP) bool {
	for _, res := range serp.Results {
		if res.Link != "" && !res.Resolved() {
			return true
		}
	}
	return false
}
