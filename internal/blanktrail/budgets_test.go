// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// portsAsked counts the light readings of the service. The client's check that
// a port is its own reads the whole listing and must not disturb the counts.
func portsAsked(fake *fakebt.Server) int { return asked(fake, "/api/v1/ports/conns") }

func TestBudgets_HandEveryPoolOfAProfileOneBudget(t *testing.T) {
	// The searches, the lookups and the standing set of one profile spend one
	// tariff, so they wait in one line.
	bs := NewBudgets(time.Hour, time.Hour)
	a := bs.For(nil, 1, 30, 5)
	if b := bs.For(nil, 1, 40, 6); b != a {
		t.Fatal("two pools of one profile were given two budgets")
	}
	if r := a.Reading(); r.Limit != 40 || r.Reserve != 6 {
		t.Errorf("the budget reads %+v after the profile was opened again with 40 and 6", r)
	}
	if bs.For(nil, 2, 30, 5) == a {
		t.Error("two profiles share a budget")
	}
	var none *Budgets
	if none.For(nil, 1, 30, 5) != nil {
		t.Error("no registry gave a budget")
	}
}

func TestBudgets_CountWhatTheServiceSaysOfTheProfilesOwnPortsAndNoOneElses(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 2)
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
	own := mine.portNumbers()
	fake.SetConns(own[0], 4, 6)
	fake.SetConns(own[1], 1, 3)
	fake.SetConns(stranger.portNumbers()[0], 9, 40)

	bs.pollOnce(t.Context())
	if r := cfg.Budget.Reading(); r.Observed != 9 || !r.Known {
		t.Errorf("the profile reads %+v, want the nine its own two ports carry", r)
	}
}

func TestBudgets_AskTheServiceOnceForEveryProfile(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	bs := NewBudgets(time.Hour, time.Hour)
	for profile := int64(1); profile <= 2; profile++ {
		cfg := testPoolConfig(t, fake, clock, 1, 1)
		cfg.Budget = bs.For(cfg.Client, profile, 30, 5)
		p, err := NewPool(t.Context(), cfg)
		if err != nil {
			t.Fatalf("NewPool: %v", err)
		}
		t.Cleanup(func() { _ = p.Close() })
	}
	before := portsAsked(fake)
	bs.pollOnce(t.Context())
	if got := portsAsked(fake) - before; got != 1 {
		t.Errorf("one round asked the service %d times for two profiles, want once", got)
	}
}

func TestBudgets_LeaveTheServiceAloneWhereNoProfileHasALimit(t *testing.T) {
	fake := fakebt.New(t)
	cfg := testPoolConfig(t, fake, newFakeClock(), 1, 1)
	bs := NewBudgets(time.Hour, time.Hour)
	cfg.Budget = bs.For(cfg.Client, 1, 0, 5)
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	before := portsAsked(fake)
	bs.pollOnce(t.Context())
	if got := portsAsked(fake) - before; got != 0 {
		t.Errorf("a round with no limit anywhere asked the service %d times, want never", got)
	}
}

func TestBudgets_ForgetWhatTheServiceSaidWhenItCannotBeAsked(t *testing.T) {
	// A count that can no longer be read is not a count: kept, a number above
	// the limit read just before a restart of the service — or a key changed in
	// the settings — would hold every search of the profile for good.
	fake := fakebt.New(t)
	cfg := testPoolConfig(t, fake, newFakeClock(), 1, 1)
	bs := NewBudgets(time.Hour, time.Hour)
	cfg.Budget = bs.For(cfg.Client, 1, 30, 5)
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	fake.SetConns(p.portNumbers()[0], 1, 40)
	bs.pollOnce(t.Context())
	fake.FailNext("/api/v1/ports/conns", http.StatusServiceUnavailable, `{"error":"starting"}`)
	bs.pollOnce(t.Context())
	if r := cfg.Budget.Reading(); r.Observed != 0 || r.Known {
		t.Errorf("after a round the service did not answer the profile reads %+v, want nothing known", r)
	}
}

func TestBudgets_TellALookupPoolOfAProfileThatIsOver(t *testing.T) {
	fake := fakebt.New(t)
	cfg := testPoolConfig(t, fake, newFakeClock(), 1, 1)
	bs := NewBudgets(time.Hour, time.Hour)
	cfg.Budget, cfg.BudgetClass = bs.For(cfg.Client, 1, 30, 5), ClassLookup
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	closes := 0
	p.closeIdle = func(*poolPort) { closes++ }
	fake.SetConns(p.portNumbers()[0], 1, 30)
	bs.pollOnce(t.Context())
	if closes != 0 {
		t.Errorf("thirty against thirty closed connections %d times, want none", closes)
	}
	fake.SetConns(p.portNumbers()[0], 1, 31)
	bs.pollOnce(t.Context())
	if closes != 1 {
		t.Errorf("thirty-one against thirty closed connections %d times, want once", closes)
	}
}

// pollsOver counts the service's readings across steps two seconds apart for
// twenty seconds — eleven steps — with the clock in the test's hand.
func pollsOver(t *testing.T, held int) int {
	t.Helper()
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	bs := NewBudgets(10*time.Second, 2*time.Second)
	bs.now = clock.Now
	cfg.Budget = bs.For(cfg.Client, 1, 30, 5)
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	take(t, cfg.Budget, ClassSearch, held)
	before := portsAsked(fake)
	for i := 0; i <= 10; i++ {
		bs.step(t.Context())
		clock.Advance(2 * time.Second)
	}
	return portsAsked(fake) - before
}

func TestBudgets_AskEveryTenSecondsWhileEveryProfileHasRoom(t *testing.T) {
	// The listing is 283 KB for 251 ports and grows with every port
	// (2026-10-18). A profile well inside its limit is kept there by this
	// program's own count; the service is asked only to catch what it opens
	// on its own, and ten seconds is often enough for that.
	if got := pollsOver(t, 20); got != 3 {
		t.Errorf("twenty seconds at twenty of thirty asked the service %d times, want 3 (at 0, 10 and 20 s)", got)
	}
}

func TestBudgets_AskEveryTwoSecondsOnceAProfileIsAtSeventyPerCent(t *testing.T) {
	// Near the limit an over-count costs a breach, so the correction comes
	// five times as often — and only then.
	if got := pollsOver(t, 21); got != 11 {
		t.Errorf("twenty seconds at twenty-one of thirty asked the service %d times, want 11 (every 2 s)", got)
	}
}

func TestBudgets_NeverAskWhereNoProfileHasALimitWhateverTheTime(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	bs := NewBudgets(10*time.Second, 2*time.Second)
	bs.now = clock.Now
	cfg.Budget = bs.For(cfg.Client, 1, 0, 5)
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	before := portsAsked(fake)
	for i := 0; i < 30; i++ {
		bs.step(t.Context())
		clock.Advance(2 * time.Second)
	}
	if got := portsAsked(fake) - before; got != 0 {
		t.Errorf("a minute with no limit anywhere asked the service %d times, want never", got)
	}
}

func TestBudgets_RunAsksUntilItIsStopped(t *testing.T) {
	fake := fakebt.New(t)
	cfg := testPoolConfig(t, fake, newFakeClock(), 1, 1)
	bs := NewBudgets(5*time.Millisecond, 5*time.Millisecond)
	cfg.Budget = bs.For(cfg.Client, 1, 30, 5)
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	before := portsAsked(fake)
	go func() { bs.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for portsAsked(fake)-before < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := portsAsked(fake) - before; got < 2 {
		t.Fatalf("the service was asked %d times in two seconds, want a round every five milliseconds", got)
	}
	stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run went on after it was stopped")
	}
}

func TestBudgets_KeepTheRateOnTheWholeListingWhereTheLightOneIsNotThere(t *testing.T) {
	// A service from before /api/v1/ports/conns is read by the whole listing,
	// and the rate is the same: it is the dearer reading, not a reason to ask
	// more often. The 404 is met once and remembered.
	fake := fakebt.New(t)
	fake.SetNoConnsList(true)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	bs := NewBudgets(10*time.Second, 2*time.Second)
	bs.now = clock.Now
	cfg.Budget = bs.For(cfg.Client, 1, 30, 5)
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	take(t, cfg.Budget, ClassSearch, 20)
	whole, light := asked(fake, "/api/v1/ports"), portsAsked(fake)
	for i := 0; i <= 10; i++ {
		bs.step(t.Context())
		clock.Advance(2 * time.Second)
	}
	if got := asked(fake, "/api/v1/ports") - whole; got != 3 {
		t.Errorf("twenty seconds at twenty of thirty read the whole listing %d times, want 3", got)
	}
	if got := portsAsked(fake) - light; got != 1 {
		t.Errorf("the missing light listing was asked for %d times, want once and then remembered", got)
	}
}

func TestBudgets_StopTheOneGoroutineWhenStopped(t *testing.T) {
	// Run is the one goroutine per process; stopped, it must be gone.
	bs := NewBudgets(time.Hour, time.Millisecond)
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { bs.Run(ctx); close(done) }()
	stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run went on after it was stopped")
	}
	var none *Budgets
	none.Run(t.Context()) // a nil registry has nothing to run
}
