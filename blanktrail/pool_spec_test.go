// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// desktopMobile is the configuration the product's core scenario needs: one run
// measuring the same query on two device profiles.
func desktopMobile() []NamedSpec {
	desktop := DefaultPortSpec()
	desktop.OS = "windows"
	mobile := DefaultPortSpec()
	mobile.OS = "android"
	return []NamedSpec{
		{Name: "desktop", Spec: desktop},
		{Name: "mobile", Spec: mobile},
	}
}

func TestNewPool_OpensEachPortUnderItsAssignedTemplate(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 2, 3) // 6 ports
	cfg.Specs = desktopMobile()

	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	byOS := map[string]int{}
	for _, port := range fake.OpenPorts() {
		byOS[fake.ProfileOf(port).OS]++
	}
	if byOS["windows"] != 3 || byOS["android"] != 3 {
		t.Errorf("ports by OS = %v, want 3 windows and 3 android", byOS)
	}
}

func TestNewPool_WithoutSpecsStillUsesTheSingleTemplate(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 4)
	cfg.Spec.OS = "macos"

	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	for _, port := range fake.OpenPorts() {
		if got := fake.ProfileOf(port).OS; got != "macos" {
			t.Errorf("port %d opened with OS %q, want macos", port, got)
		}
	}
}

func TestNewPool_RefusesFewerPortsThanTemplates(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1) // one port, two templates
	cfg.Specs = desktopMobile()

	_, err := NewPool(context.Background(), cfg)
	if !errors.Is(err, ErrTooFewPorts) {
		t.Fatalf("NewPool err=%v, want ErrTooFewPorts", err)
	}
	if len(fake.OpenPorts()) != 0 {
		t.Errorf("ports left open after a refused pool: %v", fake.OpenPorts())
	}
}

func TestNewPool_DoesNotMutateTheCallersSpecSlice(t *testing.T) {
	// NewPool fills in defaults for templates that leave Browser empty. The
	// config is passed by value but the slice is not, so defaulting in place
	// would reach back into the caller's own data.
	fake := fakebt.New(t)
	clock := newFakeClock()
	specs := []NamedSpec{{Name: "a"}, {Name: "b"}}
	cfg := testPoolConfig(t, fake, clock, 1, 2)
	cfg.Specs = specs

	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	if specs[0].Spec.Browser != "" {
		t.Errorf("caller's specs[0].Spec.Browser=%q, want it untouched", specs[0].Spec.Browser)
	}
}

func TestPool_RenewalReopensAPortUnderItsOwnTemplate(t *testing.T) {
	// A renewed port must come back wearing the same device identity. Reopening
	// it from the pool-wide default would silently turn a mobile port into a
	// desktop one and corrupt the run's numbers from that point on.
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 2)
	cfg.Specs = desktopMobile()
	cfg.RenewAfterRequests = 1

	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	before := map[int]string{}
	for _, p := range fake.OpenPorts() {
		before[p] = fake.ProfileOf(p).OS
	}

	// Spend one request on every port, then acquire again to trigger renewal.
	for i := 0; i < 4; i++ {
		lease, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		lease.Release()
		clock.Advance(pool.Cooldown())
	}

	for port, wantOS := range before {
		if got := fake.ProfileOf(port).OS; got != wantOS {
			t.Errorf("port %d came back as %q after renewal, want %q", port, got, wantOS)
		}
	}
}

func TestPool_AcquireSpecOnlyHandsOutPortsOfThatTemplate(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 2, 3) // 6 ports, 3 of each
	cfg.Specs = desktopMobile()

	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	for i := 0; i < 6; i++ {
		lease, err := pool.AcquireSpec(context.Background(), "mobile")
		if err != nil {
			t.Fatalf("AcquireSpec %d: %v", i, err)
		}
		if got := fake.ProfileOf(lease.Port()).OS; got != "android" {
			t.Errorf("AcquireSpec(mobile) handed out port %d with OS %q", lease.Port(), got)
		}
		if got := lease.SpecName(); got != "mobile" {
			t.Errorf("lease.SpecName()=%q, want mobile", got)
		}
		lease.Release()
		clock.Advance(pool.Cooldown())
	}
}

func TestPool_AcquireSpecRejectsANameNoPortCarries(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 2)
	cfg.Specs = desktopMobile()

	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	// A typo must fail immediately and say so. Waiting for a port that can never
	// arrive would look exactly like a slow run.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = pool.AcquireSpec(ctx, "tablet")
	if !errors.Is(err, ErrUnknownSpec) {
		t.Fatalf("AcquireSpec(tablet) err=%v, want ErrUnknownSpec", err)
	}
}

func TestPool_AcquireStillHandsOutAnyPort(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 4)
	cfg.Specs = desktopMobile()

	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		lease, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		seen[lease.SpecName()] = true
		lease.Release()
		clock.Advance(pool.Cooldown())
	}
	if !seen["desktop"] || !seen["mobile"] {
		t.Errorf("Acquire only ever produced %v, want it to draw from both templates", seen)
	}
}

func TestPool_AcquireSpecReportsExhaustionPerTemplate(t *testing.T) {
	// Desktop ports being alive is no comfort to a caller that needs a mobile
	// one; exhaustion is answered per template, not pool-wide.
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 4)
	cfg.Specs = desktopMobile()

	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	pool.mu.Lock()
	for _, pt := range pool.ports {
		if pt.specName == "mobile" {
			pt.mu.Lock()
			pt.quarantined = true
			pt.mu.Unlock()
		}
	}
	pool.mu.Unlock()

	if _, err := pool.AcquireSpec(context.Background(), "mobile"); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("AcquireSpec(mobile) err=%v, want ErrPoolExhausted", err)
	}
	lease, err := pool.AcquireSpec(context.Background(), "desktop")
	if err != nil {
		t.Fatalf("AcquireSpec(desktop) failed while desktop ports are healthy: %v", err)
	}
	lease.Release()
}

func TestPool_StatsBreakDownByTemplate(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 2, 4) // 8 ports, 4 of each
	cfg.Specs = desktopMobile()

	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	st := pool.Stats()
	if st.Ports != 8 {
		t.Fatalf("Stats().Ports=%d, want 8", st.Ports)
	}
	if got := st.Specs["desktop"].Ports; got != 4 {
		t.Errorf("desktop ports=%d, want 4", got)
	}
	if got := st.Specs["mobile"].Ports; got != 4 {
		t.Errorf("mobile ports=%d, want 4", got)
	}

	// Quarantine two mobile ports; only the mobile line may move.
	pool.mu.Lock()
	quarantined := 0
	for _, pt := range pool.ports {
		if pt.specName == "mobile" && quarantined < 2 {
			pt.mu.Lock()
			pt.quarantined = true
			pt.mu.Unlock()
			quarantined++
		}
	}
	pool.mu.Unlock()

	st = pool.Stats()
	if got := st.Specs["mobile"].Quarantined; got != 2 {
		t.Errorf("mobile quarantined=%d, want 2", got)
	}
	if got := st.Specs["mobile"].Available; got != 2 {
		t.Errorf("mobile available=%d, want 2", got)
	}
	if got := st.Specs["desktop"].Quarantined; got != 0 {
		t.Errorf("desktop quarantined=%d, want 0 — quarantine must not leak across templates", got)
	}
	if st.Quarantined != 2 {
		t.Errorf("pool-wide quarantined=%d, want 2", st.Quarantined)
	}
}

func TestPool_StatsHasNoSpecMapWhenThereAreNoTemplates(t *testing.T) {
	fake := fakebt.New(t)
	clock := newFakeClock()
	pool, err := NewPool(context.Background(), testPoolConfig(t, fake, clock, 1, 2))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	if got := pool.Stats().Specs; len(got) != 0 {
		t.Errorf("Stats().Specs=%v, want it empty for an unnamed pool", got)
	}
}
