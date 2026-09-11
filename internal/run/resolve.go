// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"sync"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
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

// spot names one result of one page: the address missing from it, and where to
// write that address once it is had.
type spot struct{ page, at int }

// resolveQuery reads every address one query's pages are missing.
//
// One address, one identity, and a fresh one for every attempt. That is the
// whole of the arrangement, and it is what the measurement leaves standing: a
// hidden address is read out of a Location header, and reading it needs nothing
// a session provides. Measured on the live list, the same links through three
// kinds of port — as a search runs, with the challenge solver switched off, and
// with neither solver nor cookie jar — read 9 of 9, 9 of 9 and 11 of 11, at
// 2.33, 2.33 and 2.27 attempts each. Every failure in all three was the address
// dropping the connection. Nothing else came into it.
//
// So there is no page to work at and no identity to keep warm. There is a list
// of addresses to read, each of which wants a live proxy and nothing else; they
// go out several at a time, and one that will not come back is carried to
// another identity until it does or the allowance is spent. What that leaves is
// a step with no session in it at all, which is why it can be given ports of
// its own — see Runner.Addresses.
//
// This is where a job that writes as it goes has to do it. The comment above
// says the lookups belong after the capture rather than during it, and they
// still do — but "after the job" is too late for a run whose results reach the
// history one query at a time: a row already written has nowhere to put an
// address discovered afterwards, and there is no path that goes back for it.
// One query is where the two meet. The walk of that query's pages is over
// before any of this runs.
func (r *Runner) resolveQuery(ctx context.Context, res *QueryResult, workers int) google.ResolveReport {
	todo := whatIsMissing(res)
	if len(todo) == 0 {
		// Asking first is what keeps a query whose pages state their addresses
		// — which is most regions — from costing an identity to discover there
		// was nothing to do.
		return google.ResolveReport{}
	}
	if workers < 1 {
		workers = 1
	}

	// Asked here and not per attempt: this is the first place that knows an
	// address really has to be read, and one ask a query is what lets whoever
	// holds these ports widen the set as the lookups queue for one.
	pool, err := r.addresses(ctx)
	if err != nil {
		return google.ResolveReport{Errs: []error{err}}
	}

	// A pool with nothing to give is the one failure that stops the rest: it
	// says nothing about any address, and asking again cannot change it.
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	var mu sync.Mutex
	var total google.ResolveReport
	var starved error

	jobs := make(chan spot)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for one := range jobs {
				got, err := r.readAddress(ctx, pool, res, one)
				mu.Lock()
				gather(&total, got)
				if err != nil && starved == nil {
					starved = err
					stop()
				}
				mu.Unlock()
			}
		}()
	}
	for _, one := range todo {
		sent := false
		select {
		case <-ctx.Done():
			// Either the caller has gone or the pool has nothing left. Both are
			// answered once, below, rather than once per address still to come.
		case jobs <- one:
			sent = true
		}
		if !sent {
			break
		}
	}
	close(jobs)
	wg.Wait()

	if starved != nil {
		total.Errs = append(total.Errs, starved)
	}
	return total
}

// readAddress reads one address, carrying it to another identity for as long as
// it is allowed. The error it returns is the pool having no identity to give,
// which is not something about this address.
func (r *Runner) readAddress(ctx context.Context, pool *blanktrail.Pool, res *QueryResult, one spot) (google.ResolveReport, error) {
	var total google.ResolveReport
	page := &res.Pages[one.page]
	// The one result, in place: what is written into it is written into the
	// page, and no other worker holds this index.
	mine := page.Results[one.at : one.at+1]

	for try := 0; try < resolveTries && !mine[0].Resolved(); try++ {
		if ctx.Err() != nil {
			return total, nil
		}
		lease, err := pool.Acquire(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// The pool was fine; this worker was stopped because another
				// found it starved, or because the caller went. Say nothing.
				return total, nil
			}
			return total, err
		}

		client := lease.Client()
		resolver := google.NewResolver(client.Transport)
		// The pool was told how long one request may take. A resolver built on
		// the transport alone would drop that bound, and a call with no
		// deadline of its own would then wait on an unreachable identity for as
		// long as it took.
		resolver.Client.Timeout = client.Timeout

		got := resolver.ResolveResults(ctx, page.Origin, mine, 1)
		gather(&total, got)
		if got.Resolved == 0 && got.Failed > 0 {
			// Nothing came back through this address and something was asked of
			// it, so it is the address rather than the link: it is refused,
			// which is what puts it out of the rotation and hands the next
			// attempt a different one. A lookup answered with a redirect that
			// stayed on Google is the same verdict from the other direction —
			// this identity is being sent to a challenge — and nothing below
			// this layer can see it, because the request succeeded.
			_ = lease.Reject(ctx)
		}
		lease.Release()
	}
	return total, nil
}

// addresses is the pool the lookups go through.
//
// It is a pool of its own where the caller has one to give, and the run's own
// where nobody does. What that pool is opened as, and when, belongs to whoever
// opens it; what it is for is settled here — strangers, one per request, with
// nothing kept between them.
func (r *Runner) addresses(ctx context.Context) (*blanktrail.Pool, error) {
	if r.Addresses == nil {
		return r.Pool, nil
	}
	return r.Addresses(ctx)
}

// whatIsMissing lists every result of every page whose address is still
// unknown, in the order they were captured.
func whatIsMissing(res *QueryResult) []spot {
	var out []spot
	for p := range res.Pages {
		for i, one := range res.Pages[p].Results {
			if one.Link != "" && !one.Resolved() {
				out = append(out, spot{page: p, at: i})
			}
		}
	}
	return out
}

// resolveTries is how many identities one missing address is carried to before
// it is given up on.
//
// It is defaultTries, and for the reason defaultTries is thirty rather than for
// a reason of its own: what a lookup meets is what a search meets. Measured
// against the live fifteen-thousand-address list, one link at a time, one
// identity per attempt — 29 hidden addresses, 114 attempts to read all 29, and
// every single failure the address dropping the connection rather than the far
// end answering something else. On that distribution a rule of three identities
// reads 55% of the addresses, five reads 76%, eight reads 93%, and fifteen
// reads every one of them. Three was the rule, and 55% is what a hole in the
// middle of a report was.
const resolveTries = defaultTries

// gather adds one account to the running total, so a caller looking at many
// addresses ends with one account of the whole rather than a slice of them.
func gather(total *google.ResolveReport, one google.ResolveReport) {
	total.Attempted += one.Attempted
	total.Resolved += one.Resolved
	total.Failed += one.Failed
	total.Errs = append(total.Errs, one.Errs...)
}

// resolveWorkers is how many of one query's missing addresses are read at once.
//
// Four, and the number is about the run rather than about the pool: every
// thread reads its own query's addresses, so a job of a hundred threads already
// has four hundred lookups in the air — more than the demand measured on a run
// doing three hundred pages a minute. Raising it here would buy queueing at the
// pool rather than speed.
const resolveWorkers = 4

// firstOf is the one error worth putting against a step. A step carries one,
// and a query whose every lookup failed failed for one reason.
func firstOf(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}
