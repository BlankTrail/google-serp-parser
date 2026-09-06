// SPDX-License-Identifier: MIT

// Package run turns the engines into work that finishes: it takes a list of
// queries, spreads them over threads, and carries a refused query to another
// identity instead of losing it.
package run

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

// ErrNoIdentityLeft is returned when a query was refused by every identity it
// was allowed to try.
var ErrNoIdentityLeft = errors.New("run: every identity refused this query")

// defaultTries is how many identities one query is taken to before it is given
// up on, when the job itself named none.
//
// It was three, chosen as "enough to get past a refusal aimed at the identity,
// and cheap enough that a query which is genuinely unanswerable does not eat a
// job's budget". The first live run to measure it disagreed: 77% of requests
// refused, and two answers out of ten queries. On a poor list three tries lose
// the job, and what the operator sees is not "the list is poor" but "the parser
// does not work".
//
// Thirty is only as safe as the speed at which a dead identity is recognised. A
// port the control service reports as open and which never answers costs the
// whole request deadline each time it is tried, so thirty tries against thirty
// of those is thirty deadlines on one query — which is why the number a port is
// asked for is checked against this machine before the port is opened.
const defaultTries = 30

// Attempt is one search that survives a refused identity.
//
// It implements google.Searcher, which is the whole point of its shape. The
// engines take that interface, so SearchUntil, FindPosition, CheckIndexed and
// ListIndexed gain the retry without a line of them changing, and none of them
// learns that a retry exists.
//
// The retry goes to a different identity rather than repeating on the same one.
// A refusal answers the identity that sent the request, not the query, so
// asking again from the same one spends a try to be told the same thing.
type Attempt struct {
	// Pool leases the identities. Required.
	Pool *blanktrail.Pool
	// Mobile says the ports this attempt leases are phones, which changes one
	// header the session sets: what a browser will accept. It is not a choice
	// made here — the pool was opened as phones or as desktops — it is that same
	// choice, carried to the one place that has to act on it.
	Mobile bool

	// Asking, when set, is told the address of each search this attempt makes,
	// as it goes out. It is how a screen says what the run is doing this second;
	// on several threads it is told each thread's address in turn and whoever
	// holds it decides what to keep.
	Asking func(url string)

	// Captured, when set, is told about each page that comes back, as it comes
	// back. It is said here rather than where the pages are gathered because
	// every kind of job asks through this one place: a walk, a position check
	// and an index check all reach the same searcher, and a hook per engine
	// would be three that drift apart.
	Captured func(page google.SERP)

	// SpecName asks for a port opened under a named template, so a run that
	// wants mobile results is not quietly answered from a desktop one. Empty
	// takes any port.
	SpecName string
	// Tries is how many identities one query may be taken to. Non-positive
	// means defaultTries.
	Tries int

	mu       sync.Mutex
	sessions map[int]heldSession
}

// heldSession is one port's search session, tagged with the identity it was
// opened under.
type heldSession struct {
	identity string
	session  *google.Session
}

var _ google.Searcher = (*Attempt)(nil)

// Search runs one query, moving to another identity for as long as the answers
// are unusable.
func (a *Attempt) Search(ctx context.Context, q google.Query) (google.SERP, error) {
	tries := a.Tries
	if tries < 1 {
		tries = defaultTries
	}

	var last error
	for i := 0; i < tries; i++ {
		if err := ctx.Err(); err != nil {
			return google.SERP{}, err
		}
		serp, err := a.once(ctx, q)
		if err == nil {
			return serp, nil
		}
		if ctx.Err() != nil {
			// The caller's own context ended the search. A deadline that came
			// from the pool's request bound did not: that one is this identity
			// failing to answer, and the query is still worth taking to
			// another.
			return google.SERP{}, err
		}
		if errors.Is(err, blanktrail.ErrPoolExhausted) {
			// There is no other identity to move to. Saying so beats reporting
			// the last refusal, which would send a reader to their query list
			// when the answer is in their address list.
			return google.SERP{}, err
		}
		last = err
	}
	return google.SERP{}, fmt.Errorf("%w after %d: %w", ErrNoIdentityLeft, tries, last)
}

// Walk takes one query to a depth, holding a single identity for the whole of
// it and moving to another only when an answer is refused.
//
// A visitor paging through results does not change address between page one and
// page two, and does not arrive at page three with nothing behind them. Search
// takes an identity per call, so a walk built out of it presents a different
// visitor for every page: it throws away the referrer chain the session has
// built and pays a fresh visit to the front page for each page taken.
//
// A refused page moves the walk on and it resumes at that page rather than at
// the first. The pages already captured are captured, and taking them again
// spends requests to learn what is already known. What the new identity then
// looks like — its first search landing on a later page — is measured by the
// live run rather than settled here.
//
// A failure returns the pages already taken alongside the error, for the same
// reason: a position found on page one is still a position.
func (a *Attempt) Walk(ctx context.Context, q google.Query, pages int) ([]google.SERP, error) {
	if pages < 1 {
		pages = 1
	}
	tries := a.Tries
	if tries < 1 {
		tries = defaultTries
	}

	var out []google.SERP
	var last error
	next := 1

	for i := 0; i < tries; i++ {
		taken, err := a.walkOnce(ctx, q, next, pages)
		out = append(out, taken...)
		next += len(taken)
		if err == nil {
			// The walk ran to its end. That end is not always the depth asked
			// for: the results can run out first, and a walk stopped by the page
			// itself is finished, not interrupted.
			return out, nil
		}
		if ctx.Err() != nil {
			// The caller's own context ended the walk. A deadline that came from
			// the pool's request bound did not: that one is this identity failing
			// to answer, and the rest of the walk is still worth taking to
			// another.
			return out, err
		}
		if errors.Is(err, blanktrail.ErrPoolExhausted) {
			// There is no other identity to move to. Saying so beats reporting
			// the last refusal, which would send a reader to their query list
			// when the answer is in their address list.
			return out, err
		}
		last = err
	}
	return out, fmt.Errorf("%w after %d: %w", ErrNoIdentityLeft, tries, last)
}

// walkOnce takes as much of from..to as one identity manages and returns the
// pages it captured, so a refusal deeper in leaves the earlier ones standing.
func (a *Attempt) walkOnce(ctx context.Context, q google.Query, from, to int) ([]google.SERP, error) {
	lease, err := a.lease(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	var out []google.SERP
	err = google.SearchFrom(ctx, boundSearcher{attempt: a, lease: lease}, q, from, to,
		func(_ int, serp google.SERP) bool {
			// This identity has brought back a page, which is what makes it warm:
			// the next request through it costs seconds where the first cost
			// minutes. It is said here rather than at the end of the walk because
			// a walk refused on page seven has still proved the identity on six.
			lease.Answered()
			out = append(out, serp)
			return false
		})
	if err == nil {
		return out, nil
	}
	if _, classified := google.ClassOf(err); classified {
		// Only a response that was read and judged counts as a refusal. A
		// request that never completed was already accounted for below.
		if rejErr := lease.Reject(ctx); rejErr != nil {
			return out, fmt.Errorf("%w (reporting it also failed: %v)", err, rejErr)
		}
	}
	return out, err
}

// boundSearcher asks one held identity and nothing else. It carries no retry of
// its own: moving to another identity is Walk's decision, taken once per refusal
// rather than once per page.
type boundSearcher struct {
	attempt *Attempt
	lease   *blanktrail.Lease
}

func (b boundSearcher) Search(ctx context.Context, q google.Query) (google.SERP, error) {
	serp, err := b.attempt.sessionFor(b.lease).Search(ctx, q)
	b.attempt.caught(serp, err)
	return serp, err
}

// caught tells whoever is watching that a page came back. A refused request
// brought no page and is not one: what this counts is what the run has, not
// what it asked for.
func (a *Attempt) caught(serp google.SERP, err error) {
	if err != nil || a.Captured == nil {
		return
	}
	a.Captured(serp)
}

// once leases one identity, asks it, and hands the answer back.
//
// An unusable answer is reported to the pool before the lease is released: it
// arrived with a successful status, so nothing below this layer can see that it
// was a refusal, and an identity that is not told about it is handed out again
// unchanged.
func (a *Attempt) once(ctx context.Context, q google.Query) (google.SERP, error) {
	lease, err := a.lease(ctx)
	if err != nil {
		return google.SERP{}, err
	}
	defer lease.Release()

	serp, err := a.sessionFor(lease).Search(ctx, q)
	a.caught(serp, err)
	if err == nil {
		// The identity answered, which is what makes it warm. The pool offers a
		// warm identity before a cold one, and this is the only place that can
		// tell it: a challenge comes back as a successful request.
		lease.Answered()
		return serp, nil
	}
	if _, classified := google.ClassOf(err); classified {
		// Only a response that was read and judged counts as a refusal. A
		// request that never completed was already accounted for below.
		if rejErr := lease.Reject(ctx); rejErr != nil {
			return google.SERP{}, fmt.Errorf("%w (reporting it also failed: %v)", err, rejErr)
		}
	}
	return google.SERP{}, err
}

// sessionFor returns the session belonging to this lease's identity, opening
// one the first time that identity is seen.
//
// The session outlives a single query on purpose. Its first search visits the
// front page so the search is a navigation from somewhere, and a session built
// fresh for every query would pay that visit every time — two requests where
// one was needed, and the referrer chain thrown away after each. Keeping it is
// what makes a second query on the same port cost what a second query should.
//
// It is dropped the moment the identity changes. Lease.Session names that
// identity and changes with it, so a stale session is never handed to a port
// that is no longer the one it was built for.
//
// The map is guarded; the sessions in it are not, and do not need to be. A
// session is only ever used by whoever holds the lease on its port, and the
// pool leases a port to one caller at a time, so no two goroutines can reach
// the same session. Keying by port number rather than by identity also bounds
// the map at the size of the pool instead of growing with every change.
func (a *Attempt) sessionFor(l *blanktrail.Lease) *google.Session {
	identity, port := l.Session(), l.Port()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sessions == nil {
		a.sessions = map[int]heldSession{}
	}
	if held, ok := a.sessions[port]; ok && held.identity == identity {
		return held.session
	}

	client := l.Client()
	s := google.NewSession(client.Transport)
	s.Mobile = a.Mobile
	s.Asking = a.Asking
	// The pool was told how long one request may take. A session built on the
	// transport alone would drop that bound, and a call with no deadline of its
	// own would then wait on an unreachable identity for as long as it took.
	s.Client.Timeout = client.Timeout

	a.sessions[port] = heldSession{identity: identity, session: s}
	return s
}

func (a *Attempt) lease(ctx context.Context) (*blanktrail.Lease, error) {
	if a.SpecName == "" {
		return a.Pool.Acquire(ctx)
	}
	return a.Pool.AcquireSpec(ctx, a.SpecName)
}
