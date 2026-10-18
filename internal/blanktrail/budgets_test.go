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
	if r := cfg.Budget.Reading(); r.Amplification != 2 || !r.Sampled {
		t.Errorf("the profile reads %+v, want k 2 from its own two ports", r)
	}
	// A reading that fails leaves k as the good ones made it.
	fake.FailNext("/api/v1/ports/conns", http.StatusServiceUnavailable, `{"error":"starting"}`)
	bs.pollOnce(t.Context())
	if r := cfg.Budget.Reading(); r.Amplification != 2 || r.Known {
		t.Errorf("after a failed reading the profile reads %+v, want k still 2 and the count unknown", r)
	}
}

func TestBudgets_AskFastWhileAProfileHasAttemptsInFlightAndHasNotBeenSampled(t *testing.T) {
	// The job starts just after a reading found the profile idle. Twenty of
	// thirty is below seventy per cent at the k of 1 nothing has taught yet, but
	// on the stand those twenty were forty connections: the next reading comes
	// at the fast rate, and once it has been taken the rate is the usual one.
	fake, clock, bs, b, _ := rateWorld(t)
	bs.pollOnce(t.Context())
	take(t, b, ClassSearch, 20)
	before := portsAsked(fake)
	for range 2 { // at 0 and 2 s
		bs.step(t.Context())
		clock.Advance(2 * time.Second)
	}
	if got := portsAsked(fake) - before; got != 1 {
		t.Errorf("an unsampled profile with twenty in flight was read %d times in the first two seconds, want once (at 2 s)", got)
	}
	for range 4 { // at 4, 6, 8 and 10 s
		bs.step(t.Context())
		clock.Advance(2 * time.Second)
	}
	if got := portsAsked(fake) - before; got != 1 {
		t.Errorf("a sampled profile at twenty of thirty was read %d times by 10 s, want only the one at 2 s (next at 12 s)", got)
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

// rateWorld is a registry on a clock in the test's hand, one pool of profile 1
// with a limit of thirty, and the fake behind them.
func rateWorld(t *testing.T) (*fakebt.Server, *fakeClock, *Budgets, *Budget, *Pool) {
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
	return fake, clock, bs, cfg.Budget, p
}

// stepsFor runs eleven steps two seconds apart and counts the light readings.
func stepsFor(t *testing.T, fake *fakebt.Server, clock *fakeClock, bs *Budgets) int {
	t.Helper()
	before := portsAsked(fake)
	for i := 0; i <= 10; i++ {
		bs.step(t.Context())
		clock.Advance(2 * time.Second)
	}
	return portsAsked(fake) - before
}

func TestBudgets_LeaveOutAProfileWithoutALimitWhenDecidingTheRate(t *testing.T) {
	// A profile whose limit was taken off may still hold what it held; its
	// count against a limit of nought is not "busy".
	fake, clock, bs, b, _ := rateWorld(t)
	take(t, b, ClassSearch, 20)
	other := bs.For(nil, 2, 30, 5)
	take(t, other, ClassSearch, 25)
	bs.For(nil, 2, 0, 5)
	if got := stepsFor(t, fake, clock, bs); got != 3 {
		t.Errorf("an unlimited profile at 25 beside a limited one at 20 of 30 asked %d times in twenty seconds, want 3", got)
	}
}

func TestBudgets_GoFastWhenAnyOneProfileIsBusyAndTheOthersAreNot(t *testing.T) {
	fake, clock, bs, b, _ := rateWorld(t)
	take(t, b, ClassSearch, 21)
	bs.For(nil, 2, 30, 5)
	if got := stepsFor(t, fake, clock, bs); got != 11 {
		t.Errorf("one profile at 21 of 30 and one idle asked %d times in twenty seconds, want 11", got)
	}
}

func TestBudgets_GoFastOnTheAmplifiedCount(t *testing.T) {
	// Eleven attempts at two connections each are twenty-two of thirty: busy,
	// though eleven requests alone would be a third of the limit.
	fake, clock, bs, b, _ := rateWorld(t)
	setAmp(b, 2)
	take(t, b, ClassSearch, 11)
	if got := stepsFor(t, fake, clock, bs); got != 11 {
		t.Errorf("eleven attempts at k 2 against thirty asked %d times in twenty seconds, want 11 (every 2 s)", got)
	}
}

func TestBudgets_GoFastWhenTheServiceCountsFarMoreThanThisProgramDoes(t *testing.T) {
	// The service opens connections on its own; this program's count of five
	// cannot see them, the reading of twenty-eight can.
	fake, clock, bs, b, p := rateWorld(t)
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
