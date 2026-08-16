// SPDX-License-Identifier: MIT

package run

import "time"

// Estimate is what a job will cost, worked out before it is started.
type Estimate struct {
	Queries int
	Pages   int
	// Searches is every query taken to its full depth once, which is the work
	// the job is actually for. A walk that ends early because the page said the
	// results ran out costs less.
	Searches int
	// Warmups is the home-page visit a session pays before its first search. It
	// is one per port the work reaches rather than one per query, because the
	// session is kept for as long as the identity it belongs to, and a job with
	// fewer queries than ports never reaches the rest of them.
	Warmups int
	// Requests is what the two above add up to: what will actually leave the
	// machine in the best case. Report.Requests counts something else — the
	// identities a job took — so the two are not the same number and a run that
	// lands on it exactly has not confirmed this one.
	Requests int
	// MaxRequests is the ceiling: every query taken to its depth, the one page
	// each move to another identity makes it take again, and a home-page visit
	// for every identity it is carried to. A user sizing an address budget needs
	// this one, not the happy one.
	MaxRequests int

	Ports int
	// Cooldown is the gap the pool keeps between two requests on one port.
	Cooldown time.Duration
	// Floor is the shortest this can take. One query holds one identity however
	// deep it is taken, so a port carries one query per gap, and the busiest port
	// waits out one gap between each query and the next.
	//
	// It is a floor and nothing more. It counts no network time, no retries and
	// no walk that ends early, so the real run is longer. It is stated as the
	// minimum precisely so it cannot be read as a promise.
	Floor time.Duration
}

// Estimate works out what a job will cost before it is started. A user
// launching ten thousand queries has the right to know how many requests that
// is, and roughly how long, before pressing the button rather than an hour
// later.
//
// Throughput is bounded by the ports and the gap between two requests on one
// port, not by the thread count: a hundred threads over four ports is still
// four ports. Sizing the estimate by threads would tell a user that adding
// threads makes a job faster, which is the one thing it cannot do.
func (r *Runner) Estimate(j Job) Estimate {
	pages := j.Pages
	if pages < 1 {
		pages = 1
	}
	tries := j.Tries
	if tries < 1 {
		tries = defaultTries
	}

	est := Estimate{
		Queries:  len(j.Queries),
		Pages:    pages,
		Searches: len(j.Queries) * pages,
		Ports:    r.Pool.Size(),
		Cooldown: r.Pool.Cooldown(),
	}

	est.Warmups = min(est.Queries, est.Ports)
	est.Requests = est.Searches + est.Warmups

	// A query moved to another identity resumes at the page it stopped on, so
	// each move costs that page again rather than the depth again, and a fresh
	// identity pays the home-page visit once more. Charging every page to every
	// identity would quote several times the work.
	est.MaxRequests = est.Queries * (pages + 2*tries - 1)

	if est.Queries == 0 || est.Ports == 0 {
		return est
	}
	// The busiest port carries this many queries, and waits out the gap between
	// each of them and the next. The last query is not followed by a wait, so the
	// job is one gap shorter than it has queries to place on that port.
	perPort := (est.Queries + est.Ports - 1) / est.Ports
	est.Floor = time.Duration(perPort-1) * est.Cooldown
	return est
}
