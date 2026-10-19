// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// The service's own solves (PortLoad.Solving) in the budget: counted as the
// larger of this program's solves and the service's, in the prediction, in
// k's sample and in the excess; pinning their ports against overLimit; gone
// with a failed reading; keeping the light listing's reading fast.

// readSolves hands a budget a good reading with the service solving on the
// given ports, one solve on each.
func readSolves(b *Budget, upstream, client int, ports ...int) {
	r := profileLoad{upstream: upstream, client: client, solving: len(ports)}
	for _, p := range ports {
		if r.solvingPorts == nil {
			r.solvingPorts = map[int]bool{}
		}
		r.solvingPorts[p] = true
	}
	b.observeProfile(r, true)
}

func TestBudget_HoldsTheReserveForASolveTheServiceRunsOnItsOwn(t *testing.T) {
	// Nothing in flight here and no solve of this program's: the ladder gave
	// up on two solves, and the service goes on with them, the solver's five
	// connections each on ports with no client. Ten upstream, two solves of
	// five: the prediction is the reading, and nothing is left as an excess.
	b := NewBudget(30, 5)
	readSolves(b, 10, 0, 20101, 20102)
	r := b.Reading()
	if r.Solves != 2 || r.ServiceSolves != 2 {
		t.Errorf("reads %+v, want two solves counted, both the service's", r)
	}
	if r.Excess != 0 || r.Computed() != 10 || r.InUse() != 10 {
		t.Errorf("reads %+v (computed %d, in use %d), want the ten held as two reserves and no excess", r, r.Computed(), r.InUse())
	}
}

func TestBudget_CountsTheLargerOfItsOwnSolvesAndTheServicesNotTheSum(t *testing.T) {
	// Two solves this program waits on, and the service reports the same two:
	// they are one pair of solves, and the reserve is held once for each.
	b := NewBudget(30, 5)
	setAmp(b, 1)
	held := take(t, b, ClassSearch, 3)
	held[0].Solving(20101)
	held[1].Solving(20102)
	readSolves(b, 11, 3, 20101, 20102)
	if r := b.Reading(); r.Solves != 2 || r.InUse() != 11 {
		t.Errorf("two own solves read as two by the service read %+v (in use %d), want 2 solves and 11 in use", r, r.InUse())
	}
	// The requests behind them give up; the service still solves. The count
	// stays at two until a reading says otherwise.
	held[0].Release()
	held[1].Release()
	if r := b.Reading(); r.Solves != 2 || r.InUse() != 11 {
		t.Errorf("after both requests gave up the budget reads %+v (in use %d), want the service's two solves still held", r, r.InUse())
	}
	// And more of this program's own than the service has reported yet: the
	// own count.
	more := take(t, b, ClassSearch, 3)
	for i, u := range more {
		u.Solving(20110 + i)
	}
	if r := b.Reading(); r.Solves != 3 {
		t.Errorf("three own solves against the service's two read %+v, want 3", r)
	}
}

func TestBudget_TakesTheServicesSolvesOutOfASample(t *testing.T) {
	// Ten requests from this program, none of them a solve, and two solves the
	// service runs on for requests that already gave up here — so on ports
	// with no client: 10 x 2 + 2 x 5 = 30 upstream. The solver's connections
	// come out of the upstream, but not out of the client connections, which
	// they have none of: (30 - 10) / 10 = 2. Counted as requests they would
	// teach k 30 / 10 = 3; taken out of both, (30 - 10) / (10 - 2) = 2.5.
	b := NewBudget(60, 5)
	readSolves(b, 30, 10, 20101, 20102)
	if k := ampOf(b); !near(k, 2) {
		t.Errorf("thirty on ten with the service solving twice on its own taught k %v, want 2", k)
	}
}

func TestBudget_ASolveTheServiceRunsDoesNotMakeAnAttemptCostK(t *testing.T) {
	// A limit of six with the reserve of five and k learned at two: the
	// service's one solve holds five, and the first search, with nothing of
	// this program's in flight, costs one and fits. Counted as k it would
	// wait until the service's solve ended, with nothing running here.
	b := NewBudget(6, 5)
	setAmp(b, 2)
	readSolves(b, 5, 0, 20101)
	// A context already done: Acquire hands over a place that fits at once and
	// gives up on one that would have to wait.
	done, cancel := context.WithCancel(t.Context())
	cancel()
	if u, err := b.Acquire(done, ClassSearch); err != nil || u == nil {
		t.Errorf("a first search beside the service's solve was refused (%v), want it let in at a cost of one", err)
	} else {
		u.Release()
	}
}

func TestBudget_KnowsThePortsTheServiceIsSolvingOn(t *testing.T) {
	b := NewBudget(30, 5)
	readSolves(b, 5, 0, 20101)
	if !b.solvingOn(20101) || b.solvingOn(20102) {
		t.Errorf("the service solving on 20101 reads solving on 20101 %v and on 20102 %v, want true and false",
			b.solvingOn(20101), b.solvingOn(20102))
	}
	// A reading that finds the solve over lets the port go.
	readSolves(b, 0, 0)
	if b.solvingOn(20101) {
		t.Error("a reading with no solve left 20101 marked as solving")
	}
}

func TestBudget_ForgetsTheServicesSolvesWhenAReadingFails(t *testing.T) {
	// A solve read before the service restarted is not one now: kept, it would
	// hold its reserve and pin its port for as long as the service is down.
	b := NewBudget(30, 5)
	readSolves(b, 10, 0, 20101, 20102)
	b.observeProfile(profileLoad{upstream: 10, solving: 2, solvingPorts: map[int]bool{20101: true}}, false)
	if r := b.Reading(); r.Solves != 0 || r.ServiceSolves != 0 || b.solvingOn(20101) {
		t.Errorf("after a failed reading the budget reads %+v (solving on 20101: %v), want no solve of the service's", r, b.solvingOn(20101))
	}
}

func TestBudget_KeepsNoSolveOfTheServicesWithoutALimit(t *testing.T) {
	// Nothing is counted on a profile with no limit, the service's solves no
	// more than the rest; and taking the limit off forgets the ones read.
	none := NewBudget(0, 5)
	readSolves(none, 10, 0, 20101, 20102)
	if r := none.Reading(); r.Solves != 0 || r.ServiceSolves != 0 || none.solvingOn(20101) {
		t.Errorf("a budget with no limit reads %+v after a reading of two solves, want nothing", r)
	}
	b := NewBudget(30, 5)
	readSolves(b, 10, 0, 20101, 20102)
	b.Set(0, 5)
	if r := b.Reading(); r.Solves != 0 || r.ServiceSolves != 0 || b.solvingOn(20101) {
		t.Errorf("after the limit was taken off the budget reads %+v (solving on 20101: %v), want nothing", r, b.solvingOn(20101))
	}
}

func TestBudgets_HandEachProfileTheServicesSolvesOnItsOwnPorts(t *testing.T) {
	// Two ports of the profile, solving one and two, and a stranger's port
	// solving four: the profile counts three, and knows its first two ports
	// and not the stranger's; a port of its own solving nothing is not pinned.
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 3)
	bs := NewBudgets(time.Hour, time.Hour)
	cfg.Budget = bs.For(cfg.Client, 1, 30, 5)
	mine, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer mine.Close()
	stranger, err := NewPool(t.Context(), testPoolConfig(t, fake, clock, 1, 1))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer stranger.Close()
	own, other := mine.portNumbers(), stranger.portNumbers()[0]
	fake.SetConns(own[0], 0, 5)
	fake.SetSolving(own[0], 1)
	fake.SetConns(own[1], 0, 10)
	fake.SetSolving(own[1], 2)
	fake.SetSolving(own[2], 0)
	fake.SetSolving(other, 4)
	bs.pollOnce(t.Context())
	b := cfg.Budget
	if r := b.Reading(); r.ServiceSolves != 3 || r.Solves != 3 || r.Excess != 0 {
		t.Errorf("the profile reads %+v, want the three solves on its own ports and no excess", r)
	}
	if !b.solvingOn(own[0]) || !b.solvingOn(own[1]) || b.solvingOn(own[2]) || b.solvingOn(other) {
		t.Errorf("solving on the profile's ports reads %v %v %v and on the stranger's %v, want true true false false",
			b.solvingOn(own[0]), b.solvingOn(own[1]), b.solvingOn(own[2]), b.solvingOn(other))
	}
	// A failed reading takes them all away.
	fake.FailNext("/api/v1/ports/conns", http.StatusServiceUnavailable, `{"error":"starting"}`)
	bs.pollOnce(t.Context())
	if r := b.Reading(); r.ServiceSolves != 0 || r.Solves != 0 || b.solvingOn(own[0]) {
		t.Errorf("after a failed reading the profile reads %+v, want no solve of the service's", r)
	}
}

func TestBudgets_LeaveAPortTheServiceIsSolvingOnAloneWhenTheProfileIsOver(t *testing.T) {
	// A lookup pool told the profile is over gives back its idle connections —
	// but not on a port where the service is still solving for a request that
	// gave up here: the port looks idle to this program, and the solver is
	// working through it.
	fake := fakebt.New(t)
	cfg := testPoolConfig(t, fake, newFakeClock(), 1, 2)
	bs := NewBudgets(time.Hour, time.Hour)
	cfg.Budget, cfg.BudgetClass = bs.For(cfg.Client, 1, 30, 5), ClassLookup
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	closed := map[int]int{}
	p.closeIdle = func(pt *poolPort) { closed[pt.num]++ }
	ports := p.portNumbers()
	fake.SetConns(ports[0], 0, 20)
	fake.SetSolving(ports[0], 1)
	fake.SetConns(ports[1], 1, 20)
	bs.pollOnce(t.Context())
	if closed[ports[0]] != 0 || closed[ports[1]] != 1 {
		t.Errorf("forty against thirty closed the solving port's connections %d times and the other's %d, want 0 and 1",
			closed[ports[0]], closed[ports[1]])
	}
}

func TestBudgets_OnTheLightListingKeepAskingEveryTwoSecondsWhileTheServiceSolves(t *testing.T) {
	// Nothing in flight, nothing upstream and no excess in the reading put in
	// by hand — only a solve the service reported. Its reserve is held until
	// a reading says it is over, so the profile is read at the fast rate.
	fake, clock, bs, b, _ := rateWorld(t)
	bs.pollOnce(t.Context())
	setSeen(b, 0, 0)
	b.mu.Lock()
	b.svcSolves, b.svcSolving = 1, map[int]bool{20101: true}
	b.mu.Unlock()
	clock.Advance(2 * time.Second)
	before := readsAsked(fake)
	bs.step(t.Context())
	if got := readsAsked(fake) - before; got != 1 {
		t.Errorf("an idle profile with the service solving once was read %d times after 2 s, want once (the fast rate)", got)
	}
}
