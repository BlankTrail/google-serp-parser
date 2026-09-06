// SPDX-License-Identifier: MIT

package run

import (
	"context"

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
// left to another identity for as long as that keeps working.
//
// One identity at a time for the whole page rather than one per link: the
// address behind a link does not depend on who captured the page, so a separate
// identity per link buys nothing and spends a whole page's worth of the pool to
// get it.
//
// What it stops on is the important part. It used to be three identities,
// however well they were doing, and that left gaps in the middle of a page:
// each round read some of what was left, three rounds ran out, and the rest
// were written with no address at all. A page that arrived is a page whose
// addresses can be had, so the rule is now about progress rather than about
// effort — it goes on while rounds keep bringing something back, and gives up
// only when several in a row bring nothing.
func (r *Runner) resolvePage(ctx context.Context, serp *google.SERP, workers int) google.ResolveReport {
	var total google.ResolveReport
	var quiet int
	for try := 0; try < resolveRounds && quiet < resolveStall && needsResolving(serp); try++ {
		if err := ctx.Err(); err != nil {
			total.Errs = append(total.Errs, err)
			return total
		}
		one := r.resolveOnce(ctx, serp, workers)
		if one.Resolved > 0 {
			quiet = 0
		} else {
			quiet++
		}
		gather(&total, one)
	}
	return total
}

// resolveStall is how many identities in a row may bring nothing back before
// what is left is given up on.
//
// One was not enough, and the reason is the list rather than the lookup. A
// search is carried to a fresh identity when the address it went through is
// dead — that is what Tries is for — and the lookups had no such thing: one
// address, one attempt, and every link on that page lost together when it was a
// dead one. Measured live on a fifteen-thousand-address list, three pages of a
// region that hides its addresses: 5 of 14 read through the identity that
// captured the page and 6 of 15 through another, and every single failure was
// the same one — the address dropped the connection. Not Google refusing, not a
// challenge: the proxy.
//
// Three of them in a row, for the reason the search takes three by default: it
// is the number past which a list this bad is the operator's problem rather
// than something to spend more requests on. The difference from counting rounds
// is the whole of the fix: a page whose links come back a few at a time is
// worked at until they are all had, and only a page nothing can be had from at
// all is let go.
const resolveStall = 3

// resolveRounds is the ceiling on the identities one page may be carried to,
// however well they are doing.
//
// It is not a budget, it is a stop. A page holds ten links at most and a round
// that brings even one back keeps the walk going, so ten rounds would already
// be the worst case worth planning for; twenty is that with room, and it is
// there so that a page which somehow answers one link a round forever cannot
// hold a thread of the run for the rest of the night.
const resolveRounds = 20

// resolveOnce looks up what is missing from one page, through one identity, and
// puts that identity away when it carried nothing.
func (r *Runner) resolveOnce(ctx context.Context, serp *google.SERP, workers int) google.ResolveReport {
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

	rep := resolver.ResolveAll(ctx, serp, workers)
	if rep.Resolved == 0 && rep.Failed > 0 {
		// Nothing came back through this address and something was asked of it,
		// so it is the address rather than the links: it is refused, which is
		// what puts it out of the rotation and hands the next round a different
		// one. A round that read even one is left alone — a single dead link is
		// not a dead address.
		_ = lease.Reject(ctx)
	}
	return rep
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
