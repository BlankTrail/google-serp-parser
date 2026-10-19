// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// The slow start: an attempt for a session Google has never answered
// (FreshSession) is counted as a possible solve — the reserve — from admission
// until its answer comes without a check (it drops to k) or the service says
// it is solving (it stays the reserve, as a solve, counted once).

// takeFresh is n places for fresh sessions let in at once, failing the test
// rather than hanging.
func takeFresh(t *testing.T, b *Budget, n int) []*Unit {
	t.Helper()
	ctx, cancel := context.WithTimeout(FreshSession(t.Context()), 5*time.Second)
	defer cancel()
	var out []*Unit
	for i := 0; i < n; i++ {
		u, err := b.Acquire(ctx, ClassSearch)
		if err != nil {
			t.Fatalf("fresh place %d of %d was not given: %v", i+1, n, err)
		}
		out = append(out, u)
	}
	return out
}

// takeFreshOn is a place for a fresh session on each of the ports, let in at
// once, failing the test rather than hanging.
func takeFreshOn(t *testing.T, b *Budget, ports ...int) []*Unit {
	t.Helper()
	ctx, cancel := context.WithTimeout(FreshSession(t.Context()), 5*time.Second)
	defer cancel()
	var out []*Unit
	for _, port := range ports {
		u, err := b.AcquireOn(ctx, ClassSearch, port)
		if err != nil {
			t.Fatalf("a fresh place on %d was not given: %v", port, err)
		}
		out = append(out, u)
	}
	return out
}

// freshAdmitted asks the rule itself whether a fresh session's search would be
// let in now, so a waiter's luck in being scheduled decides nothing.
func freshAdmitted(b *Budget) bool { return freshAdmittedOn(b, 0) }

// freshAdmittedOn is freshAdmitted for a search through the given port.
func freshAdmittedOn(b *Budget, port int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.admitsLocked(ClassSearch, true, port)
}

func TestBudget_CountsAnAttemptForAFreshSessionAsTheReserve(t *testing.T) {
	// Thirty with a reserve of five: six fresh sessions start at once, and the
	// seventh waits — where counted as k (one, nothing learned yet) all twenty
	// of a job's threads would have gone.
	b := NewBudget(30, 5)
	takeFresh(t, b, 6)
	if r := b.Reading(); r.Fresh != 6 || r.InFlight != 0 || r.Solves != 0 || r.Reserves != 6 || r.InUse() != 30 {
		t.Errorf("six fresh attempts read %+v (in use %d), want six fresh, held as six reserves: 30", r, r.InUse())
	}
	waiting(FreshSession(t.Context()), t, b, ClassSearch, "a seventh fresh session")
}

func TestBudget_CountsAnAttemptForAnAdmittedSessionAsKAsBefore(t *testing.T) {
	b := NewBudget(30, 5)
	setAmp(b, 2)
	take(t, b, ClassSearch, 15)
	if r := b.Reading(); r.InFlight != 15 || r.Fresh != 0 || r.InUse() != 30 {
		t.Errorf("fifteen ordinary attempts at k 2 read %+v (in use %d), want 15 in flight, 30", r, r.InUse())
	}
	waiting(t.Context(), t, b, ClassSearch, "a sixteenth ordinary attempt")
}

func TestBudget_AFreshAttemptAnsweredWithoutACheckCostsKAndLetsAWaiterIn(t *testing.T) {
	b := NewBudget(30, 5)
	held := takeFresh(t, b, 6)
	got := waiting(FreshSession(t.Context()), t, b, ClassSearch, "a seventh fresh session")
	// One answered: 25 + 1 = 26, and a fresh session's five do not fit yet.
	held[0].notASolve()
	if r := b.Reading(); r.Fresh != 5 || r.InFlight != 1 || r.InUse() != 26 {
		t.Fatalf("after one answer the budget reads %+v (in use %d), want five fresh and one in flight: 26", r, r.InUse())
	}
	if freshAdmitted(b) {
		t.Fatal("a seventh fresh session would be let in at 26 of 30")
	}
	select {
	case <-got:
		t.Fatal("a seventh fresh session was let in at 26 of 30")
	default:
	}
	// Two answered: 20 + 2 = 22, and the seventh goes.
	held[1].notASolve()
	letIn(t, got, "the seventh fresh session")
	// Answered once is answered: a second word changes nothing, and release
	// gives back the k it now holds.
	held[0].notASolve()
	held[0].Release()
	if r := b.Reading(); r.Fresh != 5 || r.InFlight != 1 {
		t.Errorf("after the answered attempt was released the budget reads %+v, want five fresh and one in flight", r)
	}
}

func TestBudget_GivesBackAFreshPlaceReleasedBeforeItsAnswer(t *testing.T) {
	b := NewBudget(30, 5)
	held := takeFresh(t, b, 2)
	held[0].Release()
	held[0].notASolve()
	if r := b.Reading(); r.Fresh != 1 || r.InFlight != 0 || r.InUse() != 5 {
		t.Errorf("one of two fresh places released reads %+v (in use %d), want one fresh: 5", r, r.InUse())
	}
}

func TestBudget_AFreshAttemptThatTurnsIntoASolveIsCountedOnce(t *testing.T) {
	b := NewBudget(30, 5)
	held := takeFresh(t, b, 6)
	got := waiting(FreshSession(t.Context()), t, b, ClassSearch, "a seventh fresh session")
	held[0].Solving(20101)
	if r := b.Reading(); r.Fresh != 5 || r.Solves != 1 || r.InFlight != 0 || r.Reserves != 6 || r.InUse() != 30 {
		t.Fatalf("a fresh attempt turned solve reads %+v (in use %d), want five fresh and one solve: 30", r, r.InUse())
	}
	if !b.solvingOn(20101) {
		t.Error("the solve's port is not known as solving")
	}
	// A solve is not an answer: told so afterwards it stays a solve.
	held[0].notASolve()
	if r := b.Reading(); r.Solves != 1 || r.InFlight != 0 {
		t.Errorf("a solve told it was answered reads %+v, want it still one solve", r)
	}
	select {
	case <-got:
		t.Fatal("a seventh fresh session was let in beside six reserves")
	default:
	}
	held[0].Release()
	letIn(t, got, "the seventh fresh session once the solve ended")
	if r := b.Reading(); r.Solves != 0 || r.Fresh != 6 {
		t.Errorf("after the solve ended the budget reads %+v, want six fresh and no solve", r)
	}
}

func TestBudget_HoldsOneReserveForAFreshAttemptTheServiceSaysItIsSolving(t *testing.T) {
	// Six fresh sessions met four checks: the service is solving four on
	// their ports before the ladder knows of any. Four solves of the
	// service's on the ports of four of the six possible ones of this
	// program's are six reserves, not ten.
	b := NewBudget(30, 5)
	held := takeFreshOn(t, b, 20101, 20102, 20103, 20104, 20105, 20106)
	readSolves(b, 26, 6, 20101, 20102, 20103, 20104)
	if r := b.Reading(); r.Reserves != 6 || r.InUse() != 30 || r.Solves != 4 || r.Fresh != 6 {
		t.Errorf("six fresh and four of the service's solves read %+v (in use %d), want six reserves: 30", r, r.InUse())
	}
	// The ladder learns of them: still six.
	for i, u := range held[:4] {
		u.Solving(20101 + i)
	}
	if r := b.Reading(); r.Reserves != 6 || r.InUse() != 30 {
		t.Errorf("four of them turned solves read %+v (in use %d), want still six reserves: 30", r, r.InUse())
	}
	// And the other way: a solve the service runs with nobody here waiting on
	// it, and a fresh attempt put on its port, are one reserve — the attempt
	// rides the solve pinned there.
	c := NewBudget(30, 5)
	readSolves(c, 5, 0, 20201)
	takeFreshOn(t, c, 20201)
	if r := c.Reading(); r.Reserves != 1 || r.InUse() != 5 {
		t.Errorf("a fresh attempt on the port of a solve of the service's reads %+v (in use %d), want one reserve: 5", r, r.InUse())
	}
	// Fresh attempts on other ports are a reserve each: three reserves.
	takeFreshOn(t, c, 20202, 20203)
	if r := c.Reading(); r.Reserves != 3 || r.InUse() != 15 {
		t.Errorf("two more fresh on ports of their own read %+v (in use %d), want three reserves: 15", r, r.InUse())
	}
}

func TestBudget_HoldsAReserveForASolveLeftBehindBesideAFreshAttemptOnAnotherPort(t *testing.T) {
	// Thirty with a reserve of five, and the service solving on three ports
	// for requests that gave up here: fifteen held. Fresh sessions on other
	// ports are a reserve each on top: three fit and the fourth waits. Taken
	// as the larger of the two counts (v0.4.32), the three solves left behind
	// would have covered three fresh attempts for nothing and let six in.
	b := NewBudget(30, 5)
	readSolves(b, 15, 0, 20101, 20102, 20103)
	takeFreshOn(t, b, 20201, 20202, 20203)
	if r := b.Reading(); r.Reserves != 6 || r.InUse() != 30 || r.Solves != 3 {
		t.Errorf("three fresh beside three solves left behind read %+v (in use %d), want six reserves: 30", r, r.InUse())
	}
	if freshAdmittedOn(b, 20204) {
		t.Error("a fourth fresh attempt on a port of its own would be let in at 30 of 30")
	}
	// On the port of a solve left behind it rides that solve and fits.
	if !freshAdmittedOn(b, 20101) {
		t.Error("a fresh attempt on the port of a solve left behind would wait")
	}
	// A port not known has no solve to ride.
	if freshAdmitted(b) {
		t.Error("a fresh attempt on a port not known would be let in at 30 of 30")
	}
}

func TestBudget_MatchesTheServicesSolvesToItsOwnPortByPort(t *testing.T) {
	// Two solves of the service's on one port where this program has one
	// fresh attempt: one is the attempt's, the other a request's that gave up
	// there before a new session was put on the port. Two reserves.
	b := NewBudget(30, 5)
	held := takeFreshOn(t, b, 20101)
	b.observeProfile(profileLoad{upstream: 10, client: 1, solving: 2, clientSolving: 1, solvingPorts: map[int]int{20101: 2}}, true)
	if r := b.Reading(); r.Reserves != 2 || r.Solves != 2 || r.Fresh != 1 {
		t.Errorf("two solves on the port of one fresh attempt read %+v, want two reserves", r)
	}
	// The ladder hears of the attempt's solve: still two, counted once each.
	held[0].Solving(20101)
	if r := b.Reading(); r.Reserves != 2 || r.Solves != 2 || r.Fresh != 0 {
		t.Errorf("the attempt turned solve reads %+v, want still two reserves", r)
	}
	// The request gives up: the service's two are both left behind now.
	held[0].Release()
	if r := b.Reading(); r.Reserves != 2 || r.Solves != 2 {
		t.Errorf("after the request gave up the budget reads %+v, want the service's two held", r)
	}
}

func TestBudget_ASolveTheServiceReportsOnAFreshAttemptsPortIsLeftBehindOnceTheAttemptEnds(t *testing.T) {
	// A fresh attempt the service is solving for, and the request ends — given
	// up, or answered with its place handed on. The solve it was riding has
	// nobody waiting on it any more until a reading says it is over.
	for _, end := range []string{"released", "answered"} {
		b := NewBudget(30, 5)
		held := takeFreshOn(t, b, 20101, 20102)
		readSolves(b, 10, 2, 20101)
		if r := b.Reading(); r.Reserves != 2 {
			t.Fatalf("%s: two fresh, one being solved, read %+v, want two reserves", end, r)
		}
		if end == "released" {
			held[0].Release()
		} else {
			held[0].notASolve()
		}
		if r := b.Reading(); r.Reserves != 2 || r.Fresh != 1 {
			t.Errorf("%s: after the solved-for attempt ended the budget reads %+v, want the solve left behind and the other fresh: two reserves", end, r)
		}
		// The other fresh attempt's port has no solve on it.
		held[1].Release()
		if r := b.Reading(); r.Reserves != 1 {
			t.Errorf("%s: with only the solve left behind the budget reads %+v, want one reserve", end, r)
		}
	}
}

func TestLadder_TakesItsPlaceOnItsOwnPort(t *testing.T) {
	// Six with a reserve of five and the service solving on the ladder's port
	// with nobody waiting on it: a fresh request through that port rides the
	// solve and goes; through another it would wait.
	b := NewBudget(6, 5)
	readSolves(b, 5, 0, 20101)
	rt := &fakeRT{steps: []func() (*http.Response, error){respond(200, nil, "the page")}}
	l := &ladder{rt: rt, port: 20101, rem: &fakeRemedy{retries: 0}, budget: b, class: ClassSearch}
	ctx, cancel := context.WithTimeout(FreshSession(t.Context()), 5*time.Second)
	defer cancel()
	resp, err := l.RoundTrip(newReq(t, http.MethodGet, "").WithContext(ctx))
	if err != nil {
		t.Fatalf("a fresh request on the port of a solve left behind was not let in: %v", err)
	}
	_ = resp.Body.Close()
}

func TestBudget_LetsAFreshAttemptInOnTheReserveOfASolveTheServiceAlreadyRuns(t *testing.T) {
	// Six with a reserve of five, and the service solving once with nothing
	// of this program's in flight: five in use. A fresh attempt on that
	// solve's port rides it, so it adds no reserve of its own and goes; a
	// second one on the same port is a reserve more and waits, and so is one
	// on any other port.
	b := NewBudget(6, 5)
	readSolves(b, 5, 0, 20101)
	if !freshAdmittedOn(b, 20101) {
		t.Fatal("a fresh attempt on the port of the service's one solve would wait at 5 of 6")
	}
	if freshAdmittedOn(b, 20102) {
		t.Error("a fresh attempt on another port than the service's one solve would be let in at 5 of 6")
	}
	takeFreshOn(t, b, 20101)
	if freshAdmittedOn(b, 20101) {
		t.Error("a second fresh attempt on the port of the service's one solve would be let in at 5 of 6")
	}
}

func TestBudget_KeepsTheFreshAttemptsOutOfTheSample(t *testing.T) {
	// Ten fresh attempts on the ports, the service at two connections each and
	// solving none: k is two. Taken out as reserves (fifty of twenty upstream)
	// the sample would be below the floor and teach nothing, or one.
	b := NewBudget(100, 5)
	takeFresh(t, b, 10)
	b.observeLoad(20, 10, true)
	if r := b.Reading(); !near(r.Amplification, 2) || r.Samples != 1 {
		t.Errorf("a reading of ten fresh attempts at two each taught %+v, want k 2", r)
	}
}

func TestBudget_LetsAFreshAttemptIntoAnIdleSmallProfile(t *testing.T) {
	// Six with a reserve of five and k learned at two: a fresh session on an
	// idle profile goes (five), and another does not fit beside it.
	b := NewBudget(6, 5)
	setAmp(b, 2)
	takeFresh(t, b, 1)
	waiting(FreshSession(t.Context()), t, b, ClassSearch, "a second fresh session at 5 of 6")
	// A reserve below a learned k: the fresh attempt on an idle profile costs
	// one, as any first attempt does, or a limit of three would wait for good.
	c := NewBudget(3, 1)
	setAmp(c, 4)
	takeFresh(t, c, 1)
	if r := c.Reading(); r.Fresh != 1 || r.InUse() != 4 {
		t.Errorf("a fresh attempt with a reserve of one and k 4 reads %+v (in use %d), want it counted as k: 4", r, r.InUse())
	}
}

func TestBudget_CountsAFreshAttemptAsNoLessThanAnOrdinaryOne(t *testing.T) {
	// A reserve of one below a k of three: a fresh attempt is three, not one,
	// so ten fill thirty and the eleventh waits.
	b := NewBudget(30, 1)
	setAmp(b, 3)
	takeFresh(t, b, 10)
	if r := b.Reading(); r.InUse() != 30 || r.Computed() != 30 {
		t.Errorf("ten fresh attempts at k 3 with a reserve of one read %+v (in use %d), want 30", r, r.InUse())
	}
	waiting(FreshSession(t.Context()), t, b, ClassSearch, "an eleventh fresh session at k 3")
}

func TestBudget_ASecondAttemptBesideAFreshOneCostsK(t *testing.T) {
	// The budget is not empty with a fresh attempt in flight: an ordinary one
	// beside it costs k (two), not the one an idle profile's first costs.
	b := NewBudget(6, 5)
	setAmp(b, 2)
	takeFresh(t, b, 1)
	waiting(t.Context(), t, b, ClassSearch, "an ordinary attempt at 5 of 6 with k 2")
}

func TestBudget_SharesOutWhatTheFreshAttemptsLeaveToTheLookups(t *testing.T) {
	// Three fresh of thirty with a reserve of five: fifteen in use, and the
	// lookups keep one reserve aside — ten more at k 1.
	b := NewBudget(30, 5)
	setAmp(b, 1)
	takeFresh(t, b, 3)
	if got := b.Free(ClassLookup); got != 10 {
		t.Errorf("three fresh attempts of thirty leave %d lookups free, want 10", got)
	}
	// One fresh attempt and k 2 on seven: five in use, and an ordinary
	// attempt costs two — one search fits, no lookup.
	c := NewBudget(7, 5)
	setAmp(c, 2)
	takeFresh(t, c, 1)
	if got := c.Free(ClassSearch); got != 1 {
		t.Errorf("one fresh attempt of seven at k 2 leaves %d searches free, want 1", got)
	}
}

func TestBudget_IgnoresAFreshSessionWithoutALimit(t *testing.T) {
	b := NewBudget(0, 5)
	for _, u := range takeFresh(t, b, 50) {
		if u != nil {
			t.Fatal("a budget with no limit handed out a unit for a fresh session")
		}
	}
	if r := b.Reading(); r != (BudgetReading{Reserve: 5, Amplification: ampFloor}) {
		t.Errorf("a budget with no limit reads %+v after fresh sessions, want nothing counted", r)
	}
	if b.limited() || (*Budget)(nil).limited() {
		t.Error("a budget with no limit, or none at all, says it is limited")
	}
	if !NewBudget(30, 5).limited() {
		t.Error("a budget of thirty says it is not limited")
	}
}

func TestLadder_CountsAFreshSessionsAttemptAsTheReserveUntilItsAnswer(t *testing.T) {
	b := NewBudget(30, 5)
	var during BudgetReading
	rt := &fakeRT{steps: []func() (*http.Response, error){func() (*http.Response, error) {
		during = b.Reading()
		return respond(200, nil, "the page")()
	}}}
	l := &ladder{rt: rt, port: 20101, rem: &fakeRemedy{retries: 3}, budget: b, class: ClassSearch}
	resp, err := l.RoundTrip(newReq(t, http.MethodGet, "").WithContext(FreshSession(t.Context())))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if during.Fresh != 1 || during.InFlight != 0 {
		t.Errorf("while out the attempt read %+v, want one fresh", during)
	}
	if r := b.Reading(); r.Fresh != 0 || r.InFlight != 1 {
		t.Errorf("with its answer unread the budget reads %+v, want one ordinary attempt in flight", r)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	_ = resp.Body.Close()
	if r := b.Reading(); r.Fresh != 0 || r.InFlight != 0 {
		t.Errorf("after the answer was read the budget reads %+v, want nothing", r)
	}
}

func TestLadder_DropsAFreshAttemptToKOnARefusedAnswerItHandsOn(t *testing.T) {
	// Not every answer handed on is a page: a refusal the retries are spent on
	// is the far end's all the same, and no check is solved for it.
	b := NewBudget(30, 5)
	rt := &fakeRT{steps: []func() (*http.Response, error){respond(503, nil, "busy")}}
	l := &ladder{rt: rt, port: 20101, rem: &fakeRemedy{retries: 0}, budget: b, class: ClassSearch}
	resp, err := l.RoundTrip(newReq(t, http.MethodGet, "").WithContext(FreshSession(t.Context())))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if r := b.Reading(); r.Fresh != 0 || r.InFlight != 1 {
		t.Errorf("with the refusal unread the budget reads %+v, want one ordinary attempt in flight", r)
	}
}

func TestLadder_TurnsAFreshAttemptIntoOneSolveWhenTheSolverWorks(t *testing.T) {
	b := NewBudget(30, 5)
	var atWait BudgetReading
	rt := &fakeRT{steps: []func() (*http.Response, error){
		serviceSays(503, "solver_timeout"),
		respond(200, nil, "the page"),
	}}
	rem := &fakeRemedy{retries: 3}
	rem.atWait = func() { atWait = b.Reading() }
	l := &ladder{rt: rt, port: 20101, rem: rem, budget: b, class: ClassSearch}
	resp, err := l.RoundTrip(newReq(t, http.MethodGet, "").WithContext(FreshSession(t.Context())))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if atWait.Solves != 1 || atWait.Fresh != 0 || atWait.InFlight != 0 || atWait.Reserves != 1 {
		t.Errorf("while the solver worked the budget read %+v, want one solve and nothing else", atWait)
	}
	_ = resp.Body.Close()
	if r := b.Reading(); r.Solves != 0 || r.Fresh != 0 || r.InFlight != 0 {
		t.Errorf("after the request the budget reads %+v, want nothing", r)
	}
}

func TestBudgets_OnTheLightListingAskEveryTwoSecondsWhileAFreshSessionIsInFlight(t *testing.T) {
	// The start of a job is fresh sessions only: nothing else in flight.
	fake, clock, bs, b, _ := rateWorld(t)
	setAmp(b, 1)
	takeFresh(t, b, 1)
	if got := stepsFor(t, fake, clock, bs); got != 11 {
		t.Errorf("one fresh attempt on the light listing asked %d times in twenty seconds, want 11 (every 2 s)", got)
	}
}

func TestBudgets_AskFastWithFreshSessionsInFlightUntilKHasLearned(t *testing.T) {
	// The whole listing: one fresh attempt is five of thirty, far below
	// seventy per cent, but nothing has taught k yet.
	fake, clock, bs, b, _ := rateWorldOn(t, false)
	takeFresh(t, b, 1)
	if got := stepsFor(t, fake, clock, bs); got != 11 {
		t.Errorf("one fresh attempt with k unlearned asked %d times in twenty seconds, want 11 (every 2 s)", got)
	}
}
