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

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
)

// ErrNoIdentityLeft is returned when a query was refused by every identity it
// was allowed to try.
var ErrNoIdentityLeft = errors.New("run: every identity refused this query")

// defaultTries is how many identities one query is taken to before it is given
// up on. Three gets past a refusal aimed at the identity, and is cheap enough
// that a query which is genuinely unanswerable does not eat a job's budget.
const defaultTries = 3

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
	if err == nil {
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
