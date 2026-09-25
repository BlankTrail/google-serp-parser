// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

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
// One address to an attempt, and a fresh identity for every attempt — though
// not always an identity to itself: where the run has ports for the lookups
// alone, several share a port at once (see lanes). That is the whole of the
// arrangement, and it is what the measurement leaves standing: a
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
		held, err := r.portFor(ctx, pool)
		if err != nil {
			if ctx.Err() != nil {
				// The pool was fine; this worker was stopped because another
				// found it starved, or because the caller went. Say nothing.
				return total, nil
			}
			return total, err
		}

		client := held.lease.Client()
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
			held.refuse(ctx)
		}
		held.leave(ctx)
	}
	return total, nil
}

// portFor is a port to read one link through.
//
// The lookups of a run that has ports of its own for them share those ports,
// lookupsAtOnce to a port. A run that reads them through its searching ports
// takes one each, as it always did: a searching port carries a session, and
// what goes out through it at the same moment as its own search is what that
// session is seen doing.
func (r *Runner) portFor(ctx context.Context, pool *blanktrail.Pool) (*lane, error) {
	if r.Addresses == nil {
		lease, err := pool.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		return &lane{lease: lease, users: 1}, nil
	}
	// Asking for the set again is what lets it widen while its ports are all
	// carrying lookups: a lookup that shares a port stands in no queue, and a
	// set that waited for one to widen would put ten on one address while the
	// ports it is allowed stood unopened.
	return r.lookupLanes().take(ctx, pool, func(ctx context.Context) { _, _ = r.addresses(ctx) })
}

// lookupLanes is the run's register of shared lookup ports, made the first
// time a lookup asks for one.
func (r *Runner) lookupLanes() *lanes {
	r.lanesOnce.Do(func() { r.lanes = &lanes{} })
	return r.lanes
}

// lookupsAtOnce is how many lookups one port of the set they have to themselves
// carries at the same moment.
//
// One was the rule while each lookup took a port for itself, and it was the
// ceiling a job with addresses ran into. A hundred ports reading one link each
// read about seven thousand a minute — 0.86 s a link on average — while a
// hundred threads searching at full speed wanted about nine thousand four
// hundred, nine to a page. The queue of finished queries waiting for their
// addresses filled, and the threads stood in it: over the pair of runs of
// 2026-09-25, 59 of 100 threads were asking Google with the lookups and 94
// without them, 821 pages a minute against 1054, and the captchas a thousand
// pages cost were the same in both.
//
// Ten is the user's number, checked on the live list before it was taken: five
// ports reading 1, 2, 5 and 10 links at once read 75, 164, 421 and 901 a minute
// a port, the median lookup 332, 439, 407 and 489 ms, and not one answer of all
// 800 was Google refusing — no /sorry, no 429 (2026-09-26). The service holds
// no limit of its own on how many connections a port carries unless a port is
// opened with one, and a request in flight survives the port being moved to
// another address.
const lookupsAtOnce = 10

// renewAfterLookups and renewAfterTime are how often a lookup port is moved to
// another address and given another fingerprint when nothing has failed on it:
// the user's rule for these ports, in place of any rest — after a failure, and
// periodically by the number of lookups or by time.
//
// Five hundred is about a minute of a busy port's work at the pace of the runs
// measured here, and five minutes bounds a port on a quiet one. Neither is a
// limit Google was seen to have — no lookup through the lookup ports was ever
// refused, at up to 901 a minute a port — so they are where to start and what
// to change when a measurement says otherwise.
const (
	renewAfterLookups = 500
	renewAfterTime    = 5 * time.Minute
)

// lanes shares the ports the hidden addresses are read through.
//
// A lookup joins a port already carrying lookups, the least crowded one with
// room, and takes another port only when every one in use is full: a port in
// use is a port whose road to Google is open. Spread over every port the set
// held instead, the lookups at the end of job 19 of 2026-09-26 came to a lookup
// or two a minute a port, every one of them paid the whole road again, and
// each took 2.7 s where it had taken half a second.
//
// Nothing rests. A port one lookup came back empty from takes no more, and once
// the lookups on it are done it is moved to another address and given another
// fingerprint before it is given back — and so is a port that has carried
// renewAfterLookups lookups or stood on its address for renewAfterTime.
type lanes struct {
	mu   sync.Mutex
	open []*lane
	// used is each port's lookups and when its address and fingerprint were
	// last changed, across the lanes it has been, keyed by pool and port.
	used map[portOf]*portUse
	// now is a clock seam for tests; nil is the wall clock.
	now func() time.Time
}

// portOf names one port of one pool.
type portOf struct {
	pool *blanktrail.Pool
	num  int
}

// portUse is what a port has carried since it was last renewed.
type portUse struct {
	lookups int
	since   time.Time
}

func (ls *lanes) clock() time.Time {
	if ls.now != nil {
		return ls.now()
	}
	return time.Now()
}

// dueLocked says whether a port has carried enough, or stood long enough, to be
// renewed. The register's lock is held.
func (ls *lanes) dueLocked(l *lane) bool {
	u := ls.used[portOf{l.pool, l.lease.Port()}]
	return u != nil && (u.lookups >= renewAfterLookups || ls.clock().Sub(u.since) >= renewAfterTime)
}

// countLocked counts one more lookup on the lane's port. The register's lock is
// held.
func (ls *lanes) countLocked(l *lane) {
	if ls.used == nil {
		ls.used = map[portOf]*portUse{}
	}
	key := portOf{l.pool, l.lease.Port()}
	u := ls.used[key]
	if u == nil {
		u = &portUse{since: ls.clock()}
		ls.used[key] = u
	}
	u.lookups++
}

// lane is one port and the lookups on it.
type lane struct {
	// from is the register the lane is shared through, and nil for a port
	// taken for one lookup alone.
	from  *lanes
	pool  *blanktrail.Pool
	lease *blanktrail.Lease
	// users and shut are the register's, read and written under its lock.
	users int
	shut  bool
	// blaming keeps two lookups from refusing the same address at once: a
	// refusal may move the port, which is a call to the service.
	blaming sync.Mutex
}

// take is a place on a port for one lookup.
func (ls *lanes) take(ctx context.Context, pool *blanktrail.Pool, widen func(context.Context)) (*lane, error) {
	for {
		if l := ls.join(pool); l != nil {
			return l, nil
		}
		lease, err := pool.TryAcquire(ctx)
		if err == nil {
			return ls.opened(pool, lease), nil
		}
		if !errors.Is(err, blanktrail.ErrPoolExhausted) {
			return nil, err
		}
		// Every port is carrying all it may: the set is too narrow. The lookup
		// queues for a port — but only for a moment before it looks again: a
		// place on a shared port comes free long before the whole port does,
		// and a lookup that waited for the port could wait for as long as
		// others kept joining it.
		widen(ctx)
		wait, cancel := context.WithTimeout(ctx, lookupsLookAgain)
		lease, err = pool.Acquire(wait)
		cancel()
		if err == nil {
			return ls.opened(pool, lease), nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
	}
}

// lookupsLookAgain is how long a lookup that found every port full queues for
// one before it looks for a place on a shared one again.
const lookupsLookAgain = 50 * time.Millisecond

// opened registers a port just taken, with its first lookup on it.
func (ls *lanes) opened(pool *blanktrail.Pool, lease *blanktrail.Lease) *lane {
	l := &lane{from: ls, pool: pool, lease: lease, users: 1}
	ls.mu.Lock()
	ls.open = append(ls.open, l)
	ls.countLocked(l)
	ls.mu.Unlock()
	return l
}

// join puts one more lookup on the least crowded port of the pool with room,
// leaving alone a port that is to be renewed once its lookups are done.
func (ls *lanes) join(pool *blanktrail.Pool) *lane {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	var best *lane
	for _, l := range ls.open {
		if l.pool != pool || l.shut || l.users >= lookupsAtOnce || ls.dueLocked(l) {
			continue
		}
		if best == nil || l.users < best.users {
			best = l
		}
	}
	if best != nil {
		best.users++
		ls.countLocked(best)
	}
	return best
}

// refuse says nothing came back through the lane's address: the address is
// blamed, as a lookup on a port of its own blamed it, and no further lookup
// joins the lane.
func (l *lane) refuse(ctx context.Context) {
	if l.from != nil {
		l.from.mu.Lock()
		l.shut = true
		l.from.mu.Unlock()
	}
	l.blaming.Lock()
	_ = l.lease.Reject(ctx)
	l.blaming.Unlock()
}

// leave ends one lookup's place on the lane. The last one out gives the port
// back — renewed first, where one of its lookups came back empty or it has
// carried or stood enough.
func (l *lane) leave(ctx context.Context) {
	ls := l.from
	if ls == nil {
		l.lease.Release()
		return
	}
	ls.mu.Lock()
	l.users--
	last := l.users == 0
	renew := false
	if last {
		ls.open = slices.DeleteFunc(ls.open, func(o *lane) bool { return o == l })
		if renew = l.shut || ls.dueLocked(l); renew {
			ls.used[portOf{l.pool, l.lease.Port()}] = &portUse{since: ls.clock()}
		}
	}
	ls.mu.Unlock()
	if !last {
		return
	}
	if renew {
		// A port that could not be moved stays where it is: the pool marks
		// what it could not do, and the next failure asks again.
		_ = l.lease.Renew(ctx)
	}
	l.lease.Release()
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
