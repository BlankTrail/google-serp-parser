// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// The ladder's own move: a request for a kept session (KeptSession) that the
// ladder carries to another address in the middle of it takes its places
// after the move as a fresh session's — the session is a stranger there.

// readingAt is a step of a fake round trip that writes down the budget as the
// attempt goes out, and then answers as next does.
func readingAt(b *Budget, into *[]BudgetReading, next func() (*http.Response, error)) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		*into = append(*into, b.Reading())
		return next()
	}
}

// movedRequest runs one GET through a ladder whose first attempt meets what
// first gives and whose second is a page, and returns the budget as each
// attempt went out.
func movedRequest(ctx context.Context, t *testing.T, rem *fakeRemedy, first func() (*http.Response, error)) []BudgetReading {
	t.Helper()
	b := NewBudget(30, 5)
	var seen []BudgetReading
	rt := &fakeRT{steps: []func() (*http.Response, error){
		readingAt(b, &seen, first),
		readingAt(b, &seen, respond(200, nil, "the page")),
	}}
	l := &ladder{rt: rt, port: 20101, rem: rem, budget: b, class: ClassSearch}
	resp, err := l.RoundTrip(newReq(t, http.MethodGet, "").WithContext(ctx))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	if len(seen) != 2 {
		t.Fatalf("the ladder made %d attempts, want 2", len(seen))
	}
	return seen
}

// asFresh says how an attempt went out: as a fresh session's or not.
func asFresh(r BudgetReading) bool { return r.Fresh == 1 && r.InFlight == 0 }

func TestLadder_CountsAKeptSessionsAttemptsAfterItMovedTheRequestAsAFreshSessions(t *testing.T) {
	// Each of the three ways the ladder leaves an address in the middle of a
	// request: a connection that never arrived, the service saying it could
	// not reach the address, and an answer counted against the port.
	ways := map[string]struct {
		first func() (*http.Response, error)
		rem   *fakeRemedy
	}{
		"never arrived":   {failWith(errors.New("read: connection reset")), &fakeRemedy{retries: 3}},
		"unreachable":     {serviceSays(523, "upstream_unreachable"), &fakeRemedy{retries: 3}},
		"counted refusal": {respond(503, nil, "busy"), &fakeRemedy{retries: 3, rotateOnNth: 1}},
	}
	for name, way := range ways {
		seen := movedRequest(KeptSession(t.Context()), t, way.rem, way.first)
		if way.rem.rotations != 1 {
			t.Fatalf("%s: the ladder moved the request %d times, want once", name, way.rem.rotations)
		}
		if asFresh(seen[0]) || !asFresh(seen[1]) {
			t.Errorf("%s: a kept session's attempts went out as fresh: %v then %v; want false, then true after the move",
				name, asFresh(seen[0]), asFresh(seen[1]))
		}
	}
}

func TestLadder_CountsARequestWithNoSessionAsOrdinaryWhereverItGoes(t *testing.T) {
	// No mark: a port's own identity or a lookup. Moved, it is what it was.
	rem := &fakeRemedy{retries: 3}
	seen := movedRequest(t.Context(), t, rem, failWith(errors.New("read: connection reset")))
	if rem.rotations != 1 || asFresh(seen[0]) || asFresh(seen[1]) {
		t.Errorf("an unmarked request moved %d times went out as fresh: %v then %v, want never",
			rem.rotations, asFresh(seen[0]), asFresh(seen[1]))
	}
}

func TestLadder_KeepsAKeptSessionOrdinaryWhereTheRequestWasNotMoved(t *testing.T) {
	// Asked again on the same address — a refusal not counted against the
	// port, or a move that failed — the session is where it was answered.
	for name, rem := range map[string]*fakeRemedy{
		"asked again":  {retries: 3},
		"move refused": {retries: 3, rotateErr: ErrRenewUnsupported},
	} {
		first := respond(503, nil, "busy")
		if name == "move refused" {
			first = failWith(errors.New("read: connection reset"))
		}
		seen := movedRequest(KeptSession(t.Context()), t, rem, first)
		if asFresh(seen[0]) || asFresh(seen[1]) {
			t.Errorf("%s: a kept session's attempts went out as fresh: %v then %v, want neither", name, asFresh(seen[0]), asFresh(seen[1]))
		}
	}
}

func TestLadder_KeepsAFreshSessionFreshBeforeAndAfterAMove(t *testing.T) {
	rem := &fakeRemedy{retries: 3}
	seen := movedRequest(FreshSession(t.Context()), t, rem, failWith(errors.New("read: connection reset")))
	if !asFresh(seen[0]) || !asFresh(seen[1]) {
		t.Errorf("a fresh session's attempts went out as fresh: %v then %v, want both", asFresh(seen[0]), asFresh(seen[1]))
	}
}

func TestBudget_TakesAKeptSessionForAnOrdinaryAttemptUntilMoved(t *testing.T) {
	b := NewBudget(30, 5)
	ctx := KeptSession(t.Context())
	u, err := b.AcquireOn(ctx, ClassSearch, 20101)
	if err != nil {
		t.Fatalf("AcquireOn: %v", err)
	}
	if r := b.Reading(); r.Fresh != 0 || r.InFlight != 1 {
		t.Errorf("a kept session's attempt reads %+v, want one ordinary", r)
	}
	u.Release()
	if !freshSession(FreshSession(t.Context())) || freshSession(ctx) || freshSession(t.Context()) {
		t.Error("the marks read back wrong: fresh, kept and none")
	}
}
