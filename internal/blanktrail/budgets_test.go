// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// portsAsked counts the light readings of the service. The client's check that
// a port is its own reads the whole listing and must not disturb the counts.
func portsAsked(fake *fakebt.Server) int { return asked(fake, "/api/v1/ports/conns") }

// readsAsked counts the readings of the service on either listing, light or
// whole: a rate test cares how often the service is read, and which listing
// answers is the test's setup. It is taken as a difference across the steps,
// which never check a port's owner, so the whole listing is not disturbed.
func readsAsked(fake *fakebt.Server) int { return portsAsked(fake) + asked(fake, "/api/v1/ports") }

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

func TestBudgets_LearnTheAmplificationFromTheProfilesOwnPorts(t *testing.T) {
	// The service's connections to the proxy over this program's to the ports,
	// summed over the profile's ports alone: (6+4) over (4+1) is two, and the
	// stranger's forty on one would make it anything but.
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
	fake.SetConns(own[1], 1, 4)
	fake.SetConns(stranger.portNumbers()[0], 1, 40)
	bs.pollOnce(t.Context())
	if r := cfg.Budget.Reading(); r.Amplification != 2 || r.Samples != 1 {
		t.Errorf("the profile reads %+v, want k 2 from its own two ports", r)
	}
	// A reading that fails leaves k as the good ones made it.
	fake.FailNext("/api/v1/ports/conns", http.StatusServiceUnavailable, `{"error":"starting"}`)
	bs.pollOnce(t.Context())
	if r := cfg.Budget.Reading(); r.Amplification != 2 || r.Known {
		t.Errorf("after a failed reading the profile reads %+v, want k still 2 and the count unknown", r)
	}
}

func TestBudgets_AskFastUntilTheAmplificationHasLearnedFromTwoReadings(t *testing.T) {
	// This is the rule of the whole listing: the light one is read fast with
	// anything in flight, and needs no warm-up rule. The job starts just after a reading found the profile idle. Five of
	// thirty is far below seventy per cent, but nothing has taught k yet: the
	// first reading catches the five requests on the ports before the service
	// has dialled out for them and teaches nothing, and the profile is read
	// fast until two readings of two per request have taught k; then, at ten
	// of thirty, at the usual slow rate.
	fake, clock, bs, b, p := rateWorldOn(t, false)
	port := p.portNumbers()[0]
	bs.pollOnce(t.Context())
	take(t, b, ClassSearch, 5)
	fake.SetConns(port, 5, 0)
	before := readsAsked(fake)
	at := func(steps int) {
		for range steps {
			bs.step(t.Context())
			clock.Advance(2 * time.Second)
		}
	}
	at(2) // 0 s (not due), 2 s (nothing upstream yet)
	if r := b.Reading(); r.Samples != 0 || readsAsked(fake)-before != 1 {
		t.Fatalf("after the reading at 2 s the budget reads %+v with %d readings, want one reading and no sample",
			r, readsAsked(fake)-before)
	}
	fake.SetConns(port, 5, 10)
	at(5) // 4 s and 6 s teach two; 8, 10 and 12 s are slow
	if r := b.Reading(); !near(r.Amplification, 2) || r.Samples != 2 {
		t.Errorf("after the readings at 4 and 6 s the budget reads %+v, want k 2 from two samples", r)
	}
	if got := readsAsked(fake) - before; got != 3 {
		t.Errorf("the profile was read %d times by 12 s, want 3 (2, 4 and 6 s; the next at 16 s)", got)
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
	// The 70% rule is the rule of the whole listing (see Budgets.step).
	fake.SetNoConnsList(true)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	cfg.Client.noConnsList.Store(true)
	bs := NewBudgets(10*time.Second, 2*time.Second)
	bs.now = clock.Now
	cfg.Budget = bs.For(cfg.Client, 1, 30, 5)
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	// k learned already (at 1), so the rate goes by the count alone.
	setAmp(cfg.Budget, 1)
	take(t, cfg.Budget, ClassSearch, held)
	before := readsAsked(fake)
	for i := 0; i <= 10; i++ {
		bs.step(t.Context())
		clock.Advance(2 * time.Second)
	}
	return readsAsked(fake) - before
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
	setAmp(cfg.Budget, 1)
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

// rateWorld is a registry on a clock in the test's hand, one pool of profile 1
// with a limit of thirty, and the fake behind them.
func rateWorld(t *testing.T) (*fakebt.Server, *fakeClock, *Budgets, *Budget, *Pool) {
	t.Helper()
	return rateWorldOn(t, true)
}

// rateWorldOn is rateWorld on the light listing, or, with light false, on a
// service that has none and a client that has met its 404 already, so that the
// rules of the whole listing are tested by readings that are all of one kind.
func rateWorldOn(t *testing.T, light bool) (*fakebt.Server, *fakeClock, *Budgets, *Budget, *Pool) {
	t.Helper()
	fake := fakebt.New(t)
	fake.SetNoConnsList(!light)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	if !light {
		cfg.Client.noConnsList.Store(true)
	}
	bs := NewBudgets(10*time.Second, 2*time.Second)
	bs.now = clock.Now
	cfg.Budget = bs.For(cfg.Client, 1, 30, 5)
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return fake, clock, bs, cfg.Budget, p
}

// stepsFor runs eleven steps two seconds apart and counts the readings.
func stepsFor(t *testing.T, fake *fakebt.Server, clock *fakeClock, bs *Budgets) int {
	t.Helper()
	before := readsAsked(fake)
	for i := 0; i <= 10; i++ {
		bs.step(t.Context())
		clock.Advance(2 * time.Second)
	}
	return readsAsked(fake) - before
}

func TestBudgets_LeaveOutAProfileWithoutALimitWhenDecidingTheRate(t *testing.T) {
	// A profile whose limit was taken off may still hold what it held; its
	// count against a limit of nought is not "busy".
	fake, clock, bs, b, _ := rateWorldOn(t, false)
	setAmp(b, 1)
	take(t, b, ClassSearch, 20)
	other := bs.For(nil, 2, 30, 5)
	take(t, other, ClassSearch, 25)
	bs.For(nil, 2, 0, 5)
	if got := stepsFor(t, fake, clock, bs); got != 3 {
		t.Errorf("an unlimited profile at 25 beside a limited one at 20 of 30 asked %d times in twenty seconds, want 3", got)
	}
}

func TestBudgets_GoFastWhenAnyOneProfileIsBusyAndTheOthersAreNot(t *testing.T) {
	fake, clock, bs, b, _ := rateWorldOn(t, false)
	take(t, b, ClassSearch, 21)
	bs.For(nil, 2, 30, 5)
	if got := stepsFor(t, fake, clock, bs); got != 11 {
		t.Errorf("one profile at 21 of 30 and one idle asked %d times in twenty seconds, want 11", got)
	}
}

func TestBudgets_GoFastOnTheAmplifiedCount(t *testing.T) {
	// Eleven attempts at two connections each are twenty-two of thirty: busy,
	// though eleven requests alone would be a third of the limit.
	fake, clock, bs, b, _ := rateWorldOn(t, false)
	setAmp(b, 2)
	take(t, b, ClassSearch, 11)
	if got := stepsFor(t, fake, clock, bs); got != 11 {
		t.Errorf("eleven attempts at k 2 against thirty asked %d times in twenty seconds, want 11 (every 2 s)", got)
	}
}

func TestBudgets_GoFastWhenTheServiceCountsFarMoreThanThisProgramDoes(t *testing.T) {
	// The service opens connections on its own; this program's count of five
	// cannot see them, the reading of twenty-eight can.
	fake, clock, bs, b, p := rateWorldOn(t, false)
	take(t, b, ClassSearch, 5)
	fake.SetConns(p.portNumbers()[0], 1, 28)
	bs.pollOnce(t.Context())
	if r := b.Reading(); r.Observed != 28 || !r.Known {
		t.Fatalf("the budget reads %+v, want 28 known", r)
	}
	// The reading just taken counts as the last: eleven steps see readings at
	// 2, 4, ... 20 s at the fast rate and at 10 and 20 s at the slow one.
	if got := stepsFor(t, fake, clock, bs); got != 10 {
		t.Errorf("computed 5 and observed 28 of 30 asked %d times in twenty seconds, want 10", got)
	}
}

func TestBudgets_CountAFailedReadingAsAnAttempt(t *testing.T) {
	// A service that is down is not asked on every tick.
	fake, clock, bs, _, _ := rateWorld(t)
	fake.SetDown(true)
	if got := stepsFor(t, fake, clock, bs); got != 3 {
		t.Errorf("a service that answers nothing was asked %d times in twenty seconds, want 3", got)
	}
}

// logCapture is the default logger for a test: every record, as text.
type logCapture struct {
	mu   sync.Mutex
	recs []string
	lvls []slog.Level
}

func (l *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (l *logCapture) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logCapture) WithGroup(string) slog.Handler            { return l }
func (l *logCapture) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool { sb.WriteString(" " + a.String()); return true })
	l.mu.Lock()
	l.recs, l.lvls = append(l.recs, sb.String()), append(l.lvls, r.Level)
	l.mu.Unlock()
	return nil
}

func captureLog(t *testing.T) *logCapture {
	t.Helper()
	l := &logCapture{}
	old := slog.Default()
	slog.SetDefault(slog.New(l))
	t.Cleanup(func() { slog.SetDefault(old) })
	return l
}

func TestBudgets_LogOncePerFailureStreakAndNeverTheError(t *testing.T) {
	logs := captureLog(t)
	fake, _, bs, _, _ := rateWorld(t)
	fake.SetDown(true)
	bs.pollOnce(t.Context())
	bs.pollOnce(t.Context())
	if len(logs.recs) != 1 || logs.lvls[0] != slog.LevelWarn {
		t.Fatalf("two failures logged %v at %v, want one warning", logs.recs, logs.lvls)
	}
	fake.SetDown(false)
	bs.pollOnce(t.Context())
	bs.pollOnce(t.Context())
	if len(logs.recs) != 2 || logs.lvls[1] != slog.LevelInfo {
		t.Fatalf("a recovery logged %v at %v, want one info after the warning", logs.recs, logs.lvls)
	}
	for _, r := range logs.recs {
		if strings.Contains(r, "starting") || strings.Contains(r, fake.URL()) || strings.Contains(r, "http") {
			t.Errorf("a log line carries the error or an address: %q", r)
		}
	}
}

func TestBudgets_KeepTheLastReadingWhenStoppedMidReading(t *testing.T) {
	logs := captureLog(t)
	fake, _, bs, b, p := rateWorld(t)
	fake.SetConns(p.portNumbers()[0], 1, 7)
	bs.pollOnce(t.Context())
	ctx, stop := context.WithCancel(t.Context())
	stop()
	bs.pollOnce(ctx)
	if r := b.Reading(); r.Observed != 7 || !r.Known {
		t.Errorf("a reading cut short by a stop left the budget at %+v, want the 7 it had", r)
	}
	if len(logs.recs) != 0 {
		t.Errorf("a stop logged %v, want nothing", logs.recs)
	}
}

func TestBudgets_IgnoreAReadingForAProfileWhoseLimitWasTakenOff(t *testing.T) {
	b := NewBudget(30, 5)
	b.Set(0, 5)
	if over, holders := b.observe(99, true); over || holders != nil {
		t.Errorf("a budget without a limit was over: %v %v", over, holders)
	}
	if r := b.Reading(); r.Observed != 0 || r.Known {
		t.Errorf("a budget without a limit stored a reading: %+v", r)
	}
}

func TestBudgets_LeaveTheWaitingAloneWhenAProfileIsOpenedAgainUnchanged(t *testing.T) {
	bs := NewBudgets(time.Hour, time.Hour)
	b := bs.For(nil, 1, 30, 5)
	woken := func() <-chan struct{} { b.mu.Lock(); defer b.mu.Unlock(); return b.changed }
	before := woken()
	bs.For(nil, 1, 30, 5)
	if woken() != before {
		t.Error("opening a pool with the same limit and reserve woke the waiting")
	}
	bs.For(nil, 1, 31, 5)
	if woken() == before {
		t.Error("a changed limit did not wake the waiting")
	}
	before = woken()
	bs.For(nil, 1, 31, 6)
	if woken() == before {
		t.Error("a changed reserve did not wake the waiting")
	}
}

// setSeen puts a reading into a budget by hand: the upstream connections the
// service was found carrying and the excess over the prediction. A real
// reading cannot make an excess without upstream connections under it, so the
// two rules that read them apart are proven one at a time this way.
func setSeen(b *Budget, observed int, excess float64) {
	b.mu.Lock()
	b.observed, b.known, b.excess = observed, true, excess
	b.mu.Unlock()
}

func TestBudgets_OnTheLightListingAskEveryTwoSecondsWhileAnythingIsInFlight(t *testing.T) {
	// Three requests of thirty with k learned at one are ten per cent of the
	// limit: the 70% rule would read every ten seconds, and on the stand the
	// service counted thirty-odd while this program had three to eight open.
	// The light listing is cheap, so anything in flight is read fast.
	fake, clock, bs, b, _ := rateWorld(t)
	setAmp(b, 1)
	take(t, b, ClassSearch, 3)
	if got := stepsFor(t, fake, clock, bs); got != 11 {
		t.Errorf("three attempts of thirty on the light listing asked %d times in twenty seconds, want 11 (every 2 s)", got)
	}
}

func TestBudgets_OnTheLightListingAskEveryTenSecondsWhileIdleAndQuiet(t *testing.T) {
	// Nothing in flight and a last reading that found nothing on the ports:
	// nothing for a correction to catch, and the slow rate keeps the cost of
	// an idle profile at one cheap request in ten seconds.
	fake, clock, bs, b, _ := rateWorld(t)
	bs.pollOnce(t.Context())
	if r := b.Reading(); !r.Known || r.Observed != 0 || r.Excess != 0 {
		t.Fatalf("the idle profile reads %+v, want a known nought", r)
	}
	if got := stepsFor(t, fake, clock, bs); got != 2 {
		t.Errorf("an idle quiet profile on the light listing asked %d times in twenty seconds, want 2 (at 10 and 20 s)", got)
	}
}

// dueAfterTwoSeconds stamps a reading, puts a reading by hand into the budget,
// lets two seconds pass and says whether one step then asks the service: a
// profile at the fast rate does, at the slow one it waits for ten.
func dueAfterTwoSeconds(t *testing.T, fake *fakebt.Server, clock *fakeClock, bs *Budgets, b *Budget, observed int, excess float64) bool {
	t.Helper()
	bs.pollOnce(t.Context())
	setSeen(b, observed, excess)
	clock.Advance(2 * time.Second)
	before := readsAsked(fake)
	bs.step(t.Context())
	return readsAsked(fake)-before == 1
}

func TestBudgets_OnTheLightListingKeepAskingEveryTwoSecondsWhileTheServiceStillCarriesUpstream(t *testing.T) {
	// Nothing in flight, but the last reading found connections to the proxy
	// the service holds on its own (and no excess over the prediction, as
	// when a solve's reserve covers them): they are what outlives the
	// requests, and the profile is read fast until they are seen to be gone.
	fake, clock, bs, b, _ := rateWorld(t)
	if !dueAfterTwoSeconds(t, fake, clock, bs, b, 5, 0) {
		t.Error("an idle profile with 5 upstream seen was not read after 2 s, want the fast rate")
	}
}

func TestBudgets_OnTheLightListingKeepAskingEveryTwoSecondsWhileTheLastReadingHadAnExcess(t *testing.T) {
	fake, clock, bs, b, _ := rateWorld(t)
	if !dueAfterTwoSeconds(t, fake, clock, bs, b, 0, 3) {
		t.Error("an idle profile with an excess of 3 seen was not read after 2 s, want the fast rate")
	}
}

func TestBudgets_OnTheLightListingKeepTheSlowRateForAnIdleQuietProfileAfterTwoSeconds(t *testing.T) {
	// The other side of the two above, with the same clock: nothing seen and
	// nothing in flight is not read after two seconds.
	fake, clock, bs, b, _ := rateWorld(t)
	if dueAfterTwoSeconds(t, fake, clock, bs, b, 0, 0) {
		t.Error("an idle quiet profile was read after 2 s, want the slow rate")
	}
}

func TestBudgets_OnTheLightListingSlowDownOnceTheServiceHasLetGo(t *testing.T) {
	// The fast rate follows what the service reports and ends with it: a
	// reading that finds the ports empty puts an idle profile back to ten
	// seconds, and the service is seen to have let go.
	fake, clock, bs, b, p := rateWorld(t)
	fake.SetConns(p.portNumbers()[0], 1, 5)
	bs.pollOnce(t.Context())
	fake.SetConns(p.portNumbers()[0], 0, 0)
	bs.step(t.Context())
	clock.Advance(2 * time.Second)
	bs.step(t.Context()) // the fast reading that finds nothing
	if r := b.Reading(); r.Observed != 0 || r.Excess != 0 {
		t.Fatalf("the profile reads %+v, want the ports empty", r)
	}
	if got := stepsFor(t, fake, clock, bs); got != 2 {
		t.Errorf("an idle profile whose ports emptied asked %d times in twenty seconds, want the slow rate (2)", got)
	}
}

func TestBudgets_OnTheWholeListingKeepTheSeventyPerCentRuleWhateverIsInFlight(t *testing.T) {
	// Three attempts of thirty, k learned: far below seventy per cent. The
	// whole listing is dear, so the light listing's rule of reading with
	// anything in flight does not apply: ten seconds.
	fake, clock, bs, b, _ := rateWorldOn(t, false)
	setAmp(b, 1)
	take(t, b, ClassSearch, 3)
	setSeen(b, 5, 2)
	if got := stepsFor(t, fake, clock, bs); got != 3 {
		t.Errorf("three attempts of thirty on the whole listing asked %d times in twenty seconds, want 3", got)
	}
}

func TestBudgets_AfterThe404TheRuleChangesToTheWholeListings(t *testing.T) {
	// The pace follows the client's own flag, which flips on the first 404:
	// in flight is fast while the light listing is believed to be there, and
	// the 70% rule takes over once it is known not to be.
	fake, clock, bs, b, _ := rateWorldOn(t, true)
	fake.SetNoConnsList(true)
	setAmp(b, 1)
	take(t, b, ClassSearch, 3)
	bs.step(t.Context()) // the first reading meets the 404
	clock.Advance(2 * time.Second)
	if got := stepsFor(t, fake, clock, bs); got != 2 {
		t.Errorf("after the 404, three attempts of thirty asked %d times in twenty seconds, want 2 (at 10 and 20 s)", got)
	}
}
