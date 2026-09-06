// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"errors"

	"github.com/blanktrail/google-serp-parser/internal/google"
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
		if err := ctx.Err(); err != nil {
			// One account of why the rest was left, rather than the same one
			// against every query still to come.
			total.Errs = append(total.Errs, err)
			return total
		}
		gather(&total, r.resolveQuery(ctx, &rep.Results[i], workers))
	}
	return total
}

// resolveQuery fills in the addresses missing from one query's pages.
//
// This is where a job that writes as it goes has to do it. The comment above
// says the lookups belong after the capture rather than during it, and they
// still do — but "after the job" is too late for a run whose results reach the
// history one query at a time: a row already written has nowhere to put an
// address discovered afterwards, and there is no path that goes back for it.
// One query is where the two meet. The walk of that query's pages is over
// before any of this runs.
func (r *Runner) resolveQuery(ctx context.Context, res *QueryResult, workers int) google.ResolveReport {
	var total google.ResolveReport
	for p := range res.Pages {
		if err := ctx.Err(); err != nil {
			total.Errs = append(total.Errs, err)
			return total
		}
		gather(&total, r.resolvePage(ctx, &res.Pages[p], workers))
	}
	return total
}

// resolvePage looks up the addresses missing from one page, carrying what is
// left to another identity for as long as there is one left to try.
//
// One identity at a time for the whole page rather than one per link: the
// address behind a link does not depend on who captured the page, so a separate
// identity per link buys nothing and spends a whole page's worth of the pool to
// get it. A round asks only for what is still missing, so the cost falls away
// as the page fills: a page down to its last address costs one request a round.
//
// What it stops on is the important part, and it was twice wrong. It was three
// identities however well they were doing, which left gaps in the middle of a
// page; then it was three in a row bringing nothing, which left fewer. Both
// were three, and three is the number the measurement below has no time for.
func (r *Runner) resolvePage(ctx context.Context, serp *google.SERP, workers int) google.ResolveReport {
	var total google.ResolveReport
	for tried := 0; tried < resolveTries && needsResolving(serp); tried++ {
		if err := ctx.Err(); err != nil {
			total.Errs = append(total.Errs, err)
			return total
		}
		one, err := r.resolveOnce(ctx, serp, workers)
		gather(&total, one)
		if err != nil {
			// No identity was handed out, so nothing was asked and nothing is
			// known about what is left. The pool is closed or the caller has
			// gone — neither is answered by asking again.
			total.Errs = append(total.Errs, err)
			return total
		}
	}
	return total
}

// resolveTries is how many identities one page's missing addresses are carried
// to before what is still missing is given up on.
//
// It is defaultTries, and for the reason defaultTries is thirty rather than for
// a reason of its own: what a lookup meets is what a search meets. Measured
// against the live fifteen-thousand-address list this is pointed at, one link
// at a time, one identity per attempt — 29 hidden addresses, 114 attempts to
// read all 29, and every single failure the address dropping the connection
// rather than the far end answering something else. On that distribution a rule
// of three identities reads 55% of the addresses, five reads 76%, eight reads
// 93%, and fifteen reads every one of them. Three was the rule, and 55% is what
// a hole in the middle of a report was.
//
// Nothing is spent on a page that does not need it. The step asks whether there
// is anything to look up before it takes an identity, so the region that states
// its addresses — which is most of them — costs no requests at all.
const resolveTries = defaultTries

// resolveOnce looks up what is missing from one page, through one identity, and
// puts that identity away when it carried nothing. The error it returns is the
// pool having no identity to give, which is not something about this page.
func (r *Runner) resolveOnce(ctx context.Context, serp *google.SERP, workers int) (google.ResolveReport, error) {
	lease, err := r.Pool.Acquire(ctx)
	if err != nil {
		return google.ResolveReport{}, err
	}
	defer lease.Release()

	client := lease.Client()
	resolver := google.NewResolver(client.Transport)
	// The pool was told how long one request may take. A resolver built on the
	// transport alone would drop that bound, and a call with no deadline of its
	// own would then wait on an unreachable identity for as long as it took.
	resolver.Client.Timeout = client.Timeout

	rep := resolver.ResolveAll(ctx, serp, workers)
	switch {
	case rep.Resolved == 0 && rep.Failed > 0:
		// Nothing came back through this address and something was asked of it,
		// so it is the address rather than the links: it is refused, which is
		// what puts it out of the rotation and hands the next round a different
		// one. A round that read even one is left alone — a single dead link is
		// not a dead address.
		_ = lease.Reject(ctx)
	case walled(rep.Errs):
		// A lookup that came back pointing into Google is this identity being
		// refused, whatever the rest of the round managed. Nothing below this
		// layer can see it — the request succeeded and the answer was a proper
		// redirect — so it is said here, and the links that met it are carried
		// to another identity rather than counted against the page.
		_ = lease.Reject(ctx)
	}
	return rep, nil
}

// walled reports whether any lookup of a round was answered with a redirect
// that stayed on Google.
func walled(errs []error) bool {
	for _, err := range errs {
		if errors.Is(err, google.ErrRedirectedIntoGoogle) {
			return true
		}
	}
	return false
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

// gather adds one page's account to the running total, so a caller looking at
// many pages ends with one account of the whole rather than a slice of them.
func gather(total *google.ResolveReport, one google.ResolveReport) {
	total.Attempted += one.Attempted
	total.Resolved += one.Resolved
	total.Failed += one.Failed
	total.Errs = append(total.Errs, one.Errs...)
}

// resolveWorkers is how many of one page's links are looked up at a time.
//
// They go through one identity, and the pool holds that identity to its own
// per-address limit whatever this says, so a larger number here buys queueing
// rather than speed. Four is what a page of ten hidden addresses clears in
// three rounds without asking the pool for a second port.
const resolveWorkers = 4

// firstOf is the one error worth putting against a step. A step carries one,
// and a page whose every lookup failed failed for one reason.
func firstOf(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}
