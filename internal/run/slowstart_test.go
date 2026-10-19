// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
)

// The slow start from the run's side: a request for a session Google has not
// answered in this job, where it stands now, goes out marked
// (blanktrail.FreshSession), and the profile's budget counts its attempt as a
// possible solve; a session answered in this job there goes out as before.

// budgetWatch is an origin that writes down the profile's budget as each search
// arrives, while the request is in flight.
type budgetWatch struct {
	*cookieOrigin
	mu   sync.Mutex
	seen []blanktrail.BudgetReading
}

func watchBudget(t *testing.T, b *blanktrail.Budget) *budgetWatch {
	t.Helper()
	w := &budgetWatch{}
	w.cookieOrigin = newCookieOrigin(t, func(int) string {
		r := b.Reading()
		w.mu.Lock()
		w.seen = append(w.seen, r)
		w.mu.Unlock()
		return serpBody("example.com")
	})
	return w
}

func (w *budgetWatch) readings() []blanktrail.BudgetReading {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]blanktrail.BudgetReading(nil), w.seen...)
}

func TestAttempt_CountsAFreshSessionsSearchAsAPossibleSolveInTheBudget(t *testing.T) {
	// The first search makes a session and goes out as a fresh one's; the
	// second takes the same session, which Google has answered now, and goes
	// out as an ordinary attempt.
	b := blanktrail.NewBudget(30, 5)
	o := watchBudget(t, b)
	f := poolFacing(t, o.addr(), 1, inSessions, func(c *blanktrail.PoolConfig) { c.Budget = b })
	a := &Attempt{Pool: f.Pool, Keeper: sessions.NewKeeper(sessions.NewMemory()), Want: searchDesktop}
	for i := 0; i < 2; i++ {
		if _, err := a.Search(context.Background(), usQuery("x")); err != nil {
			t.Fatalf("Search %d: %v", i, err)
		}
	}
	seen := o.readings()
	if len(seen) != 2 {
		t.Fatalf("the origin saw %d searches, want 2", len(seen))
	}
	if seen[0].Fresh != 1 || seen[0].InFlight != 0 {
		t.Errorf("a new session's search went out with the budget at %+v, want one fresh attempt", seen[0])
	}
	if seen[1].Fresh != 0 || seen[1].InFlight != 1 {
		t.Errorf("an answered session's search went out with the budget at %+v, want one ordinary attempt", seen[1])
	}
	if r := b.Reading(); r.Fresh != 0 || r.InFlight != 0 {
		t.Errorf("after both searches the budget reads %+v, want nothing", r)
	}
}

func TestBoundSearcher_MarksASessionsRequestsUntilItIsAnsweredInThisJobWhereItStands(t *testing.T) {
	// Both doors a walk's page goes out by — the query's first page and a page
	// at the address the one before it carried. One Attempt is one job.
	b := blanktrail.NewBudget(30, 5)
	o := watchBudget(t, b)
	f := poolFacing(t, o.addr(), 1, inSessions, func(c *blanktrail.PoolConfig) { c.Budget = b })
	keeper := sessions.NewKeeper(sessions.NewMemory())
	ctx := context.Background()
	lease, err := f.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()
	held, err := keeper.Take(ctx, leasePort{lease}, searchDesktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	const next = "https://www.google.com/search?q=x&start=10"
	ask := func(search boundSearcher, at bool) {
		t.Helper()
		if at {
			_, err = search.SearchAt(ctx, usQuery("x"), next)
		} else {
			_, err = search.Search(ctx, usQuery("x"))
		}
		if err != nil {
			t.Fatalf("asking: %v", err)
		}
	}
	// The first job: a new session's first request is fresh, its second —
	// answered in this job, where it stands — is not.
	job := boundSearcher{attempt: &Attempt{Pool: f.Pool, Keeper: keeper, Want: searchDesktop}, lease: lease, held: held}
	ask(job, true)
	ask(job, false)
	if err := held.Save(ctx, leasePort{lease}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// The next job: the history knows the session (Admitted), but this job has
	// not seen it answered, and Google checks a session that rested between
	// jobs again on its first request.
	if !held.Admitted() {
		t.Fatal("the session was not written down as answered")
	}
	again := boundSearcher{attempt: &Attempt{Pool: f.Pool, Keeper: keeper, Want: searchDesktop}, lease: lease, held: held}
	ask(again, false)
	ask(again, true)
	// Moved on to another address in the middle of a page: a stranger there,
	// though answered in this job elsewhere; and answered there, known again.
	if err := held.MoveOn(ctx, leasePort{lease}); err != nil {
		t.Fatalf("MoveOn: %v", err)
	}
	ask(again, false)
	ask(again, true)
	seen := o.readings()
	if len(seen) != 6 {
		t.Fatalf("the origin saw %d searches, want 6", len(seen))
	}
	for i, want := range []bool{true, false, true, false, true, false} {
		if fresh := seen[i].Fresh == 1 && seen[i].InFlight == 0; fresh != want {
			t.Errorf("search %d went out with the budget at %+v, want counted as fresh: %v", i+1, seen[i], want)
		}
	}
}

func TestRunner_CountsASessionThatRestedBetweenJobsAsAPossibleSolveOnItsFirstRequest(t *testing.T) {
	// The sessions a job on a kept history starts with have all been answered
	// before — in an earlier job — and every one is checked again on its first
	// request. Two jobs on one history: the first makes a session; the second
	// takes it up again, counted fresh on its first request and as an
	// ordinary one on its second.
	b := blanktrail.NewBudget(30, 5)
	o := watchBudget(t, b)
	f := poolFacing(t, o.addr(), 1, inSessions, func(c *blanktrail.PoolConfig) { c.Budget = b })
	h := sessions.NewMemory()
	keeper := sessions.NewKeeper(h)
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: keeper, Want: searchDesktop}
	ctx := context.Background()
	if rep := r.Run(ctx, Job{Queries: []google.Query{usQuery("one")}, Pages: 1}); rep.Results[0].Err != nil {
		t.Fatalf("the first job: %v", rep.Results[0].Err)
	}
	rep := r.Run(ctx, Job{Queries: []google.Query{usQuery("two"), usQuery("three")}, Pages: 1})
	for i, res := range rep.Results {
		if res.Err != nil {
			t.Fatalf("the second job's query %d: %v", i+1, res.Err)
		}
	}
	if all, _ := h.Sessions(ctx, "desktop", time.Time{}); len(all) != 1 {
		t.Fatalf("the two jobs made %d sessions, want the one taken up again", len(all))
	}
	seen := o.readings()
	if len(seen) != 3 {
		t.Fatalf("the origin saw %d searches, want 3", len(seen))
	}
	for i, want := range []bool{true, true, false} {
		if fresh := seen[i].Fresh == 1 && seen[i].InFlight == 0; fresh != want {
			t.Errorf("search %d went out with the budget at %+v, want counted as fresh: %v", i+1, seen[i], want)
		}
	}
}

func TestRunner_CountsASessionMovedToAnotherAddressAsAPossibleSolveThere(t *testing.T) {
	// Answered in this job, then refused by the address (a shell) and taken off
	// it: its next request goes out from an address it was never answered on.
	b := blanktrail.NewBudget(30, 5)
	var mu sync.Mutex
	var seen []blanktrail.BudgetReading
	o := newDeepOrigin(t, 1)
	o.shells = func(n int) bool {
		mu.Lock()
		seen = append(seen, b.Reading())
		mu.Unlock()
		return n == 2
	}
	f := poolFacing(t, o.addr(), 1, inSessions, func(c *blanktrail.PoolConfig) { c.Budget = b })
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(sessions.NewMemory()), Want: searchDesktop}
	r.Run(context.Background(), Job{
		Queries: []google.Query{usQuery("one"), usQuery("two"), usQuery("three"), usQuery("four")},
		Pages:   1, Tries: 1})
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 4 {
		t.Fatalf("the origin saw %d searches, want 4", len(seen))
	}
	for i, want := range []bool{true, false, true, false} {
		if fresh := seen[i].Fresh == 1 && seen[i].InFlight == 0; fresh != want {
			t.Errorf("search %d went out with the budget at %+v, want counted as fresh: %v", i+1, seen[i], want)
		}
	}
}

func TestBoundSearcher_KeepsMarkingASessionGoogleRefused(t *testing.T) {
	// A shell is no answer: the session has not been let in where it stands,
	// and its next request may meet the check again.
	b := blanktrail.NewBudget(30, 5)
	var mu sync.Mutex
	var seen []blanktrail.BudgetReading
	o := newDeepOrigin(t, 1)
	o.shells = func(n int) bool {
		mu.Lock()
		seen = append(seen, b.Reading())
		mu.Unlock()
		return n == 1
	}
	f := poolFacing(t, o.addr(), 1, inSessions, func(c *blanktrail.PoolConfig) { c.Budget = b })
	keeper := sessions.NewKeeper(sessions.NewMemory())
	ctx := context.Background()
	lease, err := f.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()
	held, err := keeper.Take(ctx, leasePort{lease}, searchDesktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	search := boundSearcher{attempt: &Attempt{Pool: f.Pool, Keeper: keeper, Want: searchDesktop}, lease: lease, held: held}
	if _, err := search.Search(ctx, usQuery("x")); err == nil {
		t.Fatal("a shell came back as a page")
	}
	for i := 0; i < 2; i++ {
		if _, err := search.Search(ctx, usQuery("x")); err != nil {
			t.Fatalf("Search %d: %v", i+2, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("the origin saw %d searches, want 3", len(seen))
	}
	for i, want := range []bool{true, true, false} {
		if fresh := seen[i].Fresh == 1 && seen[i].InFlight == 0; fresh != want {
			t.Errorf("search %d went out with the budget at %+v, want counted as fresh: %v", i+1, seen[i], want)
		}
	}
}

func TestAttempt_WritesDownNoAnswersWithoutALimit(t *testing.T) {
	o := newCookieOrigin(t, func(int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 1, inSessions, func(c *blanktrail.PoolConfig) { c.Budget = blanktrail.NewBudget(0, 5) })
	a := &Attempt{Pool: f.Pool, Keeper: sessions.NewKeeper(sessions.NewMemory()), Want: searchDesktop}
	if _, err := a.Search(context.Background(), usQuery("x")); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(a.answered.by) != 0 {
		t.Errorf("a profile with no limit wrote down %d answered sessions, want none", len(a.answered.by))
	}
}

func TestAskingAs_LeavesTheContextAloneWithoutALimitOrASessionToTellApart(t *testing.T) {
	// No limit: the context goes out as it came, with nothing made for it —
	// not even for a session Google has never seen.
	o := newCookieOrigin(t, nil)
	for _, b := range []*blanktrail.Budget{nil, blanktrail.NewBudget(0, 5)} {
		f := poolFacing(t, o.addr(), 1, inSessions, func(c *blanktrail.PoolConfig) { c.Budget = b })
		keeper := sessions.NewKeeper(sessions.NewMemory())
		ctx := context.Background()
		lease, err := f.Pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		held, err := keeper.Take(ctx, leasePort{lease}, searchDesktop)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		if got := (&Attempt{Pool: f.Pool}).askingAs(ctx, lease, held); got != ctx {
			t.Errorf("with budget %v a fresh session's context was replaced", b)
		}
		held.PutBack()
		lease.Release()
	}
	// A limit, but no session: the port's own identity is not told apart.
	f := poolFacing(t, o.addr(), 1, func(c *blanktrail.PoolConfig) { c.Budget = blanktrail.NewBudget(30, 5) })
	lease, err := f.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()
	if ctx := context.Background(); (&Attempt{Pool: f.Pool}).askingAs(ctx, lease, nil) != ctx {
		t.Error("a request with no held session was marked as a fresh session's")
	}
}

func TestAttempt_ASharedRecordCarriesAnswersFromOneAttemptToTheNext(t *testing.T) {
	// The /search interface makes an Attempt for every request. Sharing one
	// record, the second request on a session the first one got answered is
	// an ordinary one; an attempt with a record of its own still takes it for
	// a stranger.
	o := newCookieOrigin(t, nil)
	f := poolFacing(t, o.addr(), 1, inSessions, func(c *blanktrail.PoolConfig) { c.Budget = blanktrail.NewBudget(30, 5) })
	keeper := sessions.NewKeeper(sessions.NewMemory())
	ctx := context.Background()
	lease, err := f.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()
	held, err := keeper.Take(ctx, leasePort{lease}, searchDesktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	defer held.PutBack()

	shared := &Answered{}
	(&Attempt{Pool: f.Pool, Answered: shared}).answeredHere(lease, held, nil)
	if markedFresh((&Attempt{Pool: f.Pool, Answered: shared}).askingAs(ctx, lease, held)) {
		t.Error("a session answered through a shared record was counted as fresh by the next attempt sharing it")
	}
	if !markedFresh((&Attempt{Pool: f.Pool}).askingAs(ctx, lease, held)) {
		t.Error("an attempt with a record of its own took a session it never saw answered for an ordinary one")
	}
}

// markedFresh is whether a budget takes a request under ctx for a fresh
// session's: the mark read the way the budget reads it.
func markedFresh(ctx context.Context) bool {
	b := blanktrail.NewBudget(30, 5)
	u, err := b.Acquire(ctx, blanktrail.ClassSearch)
	if err != nil {
		return false
	}
	defer u.Release()
	return b.Reading().Fresh == 1
}

func TestAnswered_ARecordWithABoundForgetsAnAnswerOlderThanIt(t *testing.T) {
	// The /search interface has no job to end its record: a session not
	// answered again within For is a stranger once more, and the forgotten
	// answers are swept out rather than kept for as long as the program runs.
	now := time.Unix(1_000_000, 0)
	r := &Answered{For: 10 * time.Minute, now: func() time.Time { return now }}
	r.put(1, "exit-a")
	now = now.Add(9 * time.Minute)
	if exit, ok := r.at(1); !ok || exit != "exit-a" {
		t.Errorf("an answer nine minutes old on a ten-minute record reads %q %v, want exit-a true", exit, ok)
	}
	now = now.Add(2 * time.Minute)
	if _, ok := r.at(1); ok {
		t.Error("an answer eleven minutes old on a ten-minute record is still remembered")
	}
	r.put(2, "exit-b")
	if _, kept := r.by[1]; kept {
		t.Error("a forgotten answer was not swept out when the next was written down")
	}
	if _, ok := r.at(2); !ok {
		t.Error("the answer just written down is not remembered")
	}
	// Every answer starts the bound again: a session that keeps being
	// answered is never forgotten.
	r.put(3, "exit-c")
	now = now.Add(9 * time.Minute)
	r.put(3, "exit-c")
	now = now.Add(6 * time.Minute)
	if _, ok := r.at(3); !ok {
		t.Error("a session answered again six minutes ago was forgotten fifteen minutes after its first answer")
	}
	// No bound: a job's record forgets nothing.
	j := &Answered{now: func() time.Time { return now }}
	j.put(1, "exit-a")
	now = now.Add(24 * time.Hour)
	if _, ok := j.at(1); !ok {
		t.Error("a record with no bound forgot an answer")
	}
}

func TestBoundSearcher_CountsTheAttemptsAfterTheLadderMovedARequestAsAFreshSessions(t *testing.T) {
	// A session never answered before this job keeps no address of its own
	// (sessions.Keeper pins only an admitted one), so the ladder may carry its
	// request elsewhere. Answered on page one, its page two goes out as an
	// ordinary request; the address dies under it, the ladder walks away, and
	// the attempt that reaches Google from the new address is a stranger's.
	b := blanktrail.NewBudget(30, 5)
	o := watchBudget(t, b)
	// Every answer closes its connection, so page two dials the dead address
	// afresh rather than riding the tunnel page one left open.
	o.Config.SetKeepAlivesEnabled(false)
	f := poolFacing(t, o.addr(), 1, inSessions, func(c *blanktrail.PoolConfig) {
		c.Budget = b
		c.AddressesPerRequest = 5
	})
	keeper := sessions.NewKeeper(sessions.NewMemory())
	ctx := context.Background()
	lease, err := f.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()
	held, err := keeper.Take(ctx, leasePort{lease}, searchDesktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	search := boundSearcher{attempt: &Attempt{Pool: f.Pool, Keeper: keeper, Want: searchDesktop}, lease: lease, held: held}
	if _, err := search.Search(ctx, usQuery("x")); err != nil {
		t.Fatalf("page one: %v", err)
	}
	first := f.Fake.UpstreamOf(lease.Port())
	f.kill(first)
	if _, err := search.SearchAt(ctx, usQuery("x"), "https://www.google.com/search?q=x&start=10"); err != nil {
		t.Fatalf("page two: %v", err)
	}
	if moved := f.Fake.UpstreamOf(lease.Port()); moved == first {
		t.Fatalf("the ladder did not move the request off the dead address: stood %v carried %d seen %d", f.stoodOn(lease.Port()), f.carriedBy(lease.Port()), len(o.readings()))
	}
	seen := o.readings()
	if len(seen) != 2 {
		t.Fatalf("the origin saw %d searches, want 2", len(seen))
	}
	if seen[1].Fresh != 1 || seen[1].InFlight != 0 {
		t.Errorf("page two reached Google from a new address with the budget at %+v, want one fresh attempt", seen[1])
	}
}
