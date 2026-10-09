// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// theirs is the upstream fakebt.TakePort puts on a number somebody else opens.
const theirs = "somebody-else:1080"

// ownedPool is a pool of n ports whose client reads the clock the test holds.
func ownedPool(t *testing.T, fake *fakebt.Server, n int) (*Pool, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, n, 1)
	cfg.Channels = []Channel{NewListChannel("list", NewStaticRotor(sessionAddrs))}
	cfg.Cooldown = time.Millisecond
	cfg.Client.clock = clock.Now
	p, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, clock
}

// restartUnder restarts the service and lets somebody else open num, as the
// Xrumer pool opened 20013–20022 after the restart of 2026-10-09; the clock
// then moves past what the client last read of the service's list.
func restartUnder(fake *fakebt.Server, clock *fakeClock, num int) {
	fake.Restart()
	fake.TakePort(num)
	clock.Advance(ownFresh + time.Second)
}

func TestClient_AsksNothingOfANumberSomebodyElseOpenedAfterARestart(t *testing.T) {
	fake := fakebt.New(t)
	p, clock := ownedPool(t, fake, 1)
	ctx := context.Background()
	num := fake.OpenPorts()[0]
	if err := p.cl.Owns(ctx, num); err != nil {
		t.Fatalf("the port just opened is not ours: %v", err)
	}

	restartUnder(fake, clock, num)
	if err := p.cl.SetUpstream(ctx, num, "203.0.113.9:1080"); !errors.Is(err, ErrPortNotOurs) {
		t.Errorf("moving their port: %v, want ErrPortNotOurs", err)
	}
	if _, err := p.cl.RotateProfile(ctx, num); !errors.Is(err, ErrPortNotOurs) {
		t.Errorf("rotating their port's fingerprint: %v, want ErrPortNotOurs", err)
	}
	if err := p.cl.ClosePort(ctx, num); !errors.Is(err, ErrPortNotOurs) {
		t.Errorf("closing their port: %v, want ErrPortNotOurs", err)
	}
	if got := fake.UpstreamOf(num); got != theirs {
		t.Errorf("their port now goes to %q, want %q left alone", got, theirs)
	}
	if fake.RotateCount(num) != 0 {
		t.Error("their port's fingerprint was rotated")
	}
}

func TestPool_ReplacesAPortWhoseNumberSomebodyElseOpenedAfterARestart(t *testing.T) {
	fake := fakebt.New(t)
	p, clock := ownedPool(t, fake, 1)
	ctx := context.Background()
	num := fake.OpenPorts()[0]

	restartUnder(fake, clock, num)
	l, err := p.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("no port after the restart: %v", err)
	}
	defer l.Release()
	if l.Port() == num {
		t.Fatalf("handed out %d, which somebody else opened", num)
	}
	if !slices.Contains(fake.OpenPorts(), l.Port()) {
		t.Errorf("handed out %d and the service does not hold it", l.Port())
	}
	if got := fake.UpstreamOf(num); got != theirs {
		t.Errorf("their port now goes to %q, want %q left alone", got, theirs)
	}
	if got := p.Stats().Lost; got != 1 {
		t.Errorf("Lost = %d, want 1", got)
	}
	if p.Size() != 1 {
		t.Errorf("the pool holds %d ports, want the one it had", p.Size())
	}
}

func TestPool_WarmsNothingThroughANumberSomebodyElseOpened(t *testing.T) {
	// The demo parsers stood idle and warmed their standing ports; after the
	// restart those warming requests went through the Xrumer pool's ports.
	fake := fakebt.New(t)
	p, clock := ownedPool(t, fake, 1)
	p.KeepWarm()
	num := fake.OpenPorts()[0]

	restartUnder(fake, clock, num)
	for i := 0; i < 3; i++ {
		l, ok := p.AcquireIdleHot(0)
		if !ok {
			continue
		}
		if l.Port() == num {
			t.Fatalf("a warming request was handed %d, which somebody else opened", num)
		}
		l.Release()
		if p.Hot() != 1 {
			t.Errorf("%d standing ports, want the lost one replaced by a standing one", p.Hot())
		}
		return
	}
	t.Error("no standing port was handed out after the restart")
}

func TestPool_ClosesNothingOfSomebodyElseWhenItCloses(t *testing.T) {
	fake := fakebt.New(t)
	p, clock := ownedPool(t, fake, 1)
	num := fake.OpenPorts()[0]

	restartUnder(fake, clock, num)
	_ = p.Close()
	if !slices.Contains(fake.OpenPorts(), num) {
		t.Errorf("closing the pool closed %d, which somebody else had opened", num)
	}
}

func TestPool_OpensAgainANumberTheRestartLeftEmpty(t *testing.T) {
	fake := fakebt.New(t)
	p, clock := ownedPool(t, fake, 1)
	ctx := context.Background()
	num := fake.OpenPorts()[0]

	fake.Restart()
	clock.Advance(ownFresh + time.Second)
	l, err := p.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("no port after the restart: %v", err)
	}
	defer l.Release()
	if l.Port() != num || !slices.Contains(fake.OpenPorts(), num) {
		t.Errorf("handed out %d with the service holding %v, want %d opened again", l.Port(), fake.OpenPorts(), num)
	}
}

func TestPool_ReplacesAPortWhoseNumberWasTakenWhileItWasBeingOpenedAgain(t *testing.T) {
	// The restart left the number empty, and somebody else opened it between
	// the pool seeing that and opening it again.
	fake := fakebt.New(t)
	p, clock := ownedPool(t, fake, 1)
	ctx := context.Background()
	num := fake.OpenPorts()[0]

	fake.Restart()
	clock.Advance(ownFresh + time.Second)
	fake.FailNext("/api/v1/ports/open", 409, `{"error":"port already open"}`)
	l, err := p.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("no port after the restart: %v", err)
	}
	defer l.Release()
	if l.Port() == num {
		t.Fatalf("handed out %d, which the service would not open for this pool", num)
	}
	if got := p.Stats().Lost; got != 1 {
		t.Errorf("Lost = %d, want 1", got)
	}
	if got := p.Stats().Quarantines; got != 0 {
		t.Errorf("%d quarantines for a number somebody else took", got)
	}
}

func TestPool_KeepsAPortTheServiceRestoredWithItsSettingsAfterARestart(t *testing.T) {
	// The service of 8893 opens every port again when it restarts, as it was
	// and under a new creation time. Taken for somebody else's, each of the
	// parser's ports was left standing open and another opened beside it, on
	// every restart.
	fake := fakebt.New(t)
	fake.SetNoLabels(true) // a service older than labels: the settings and the restore decide
	fake.SetRestores(true)
	p, clock := ownedPool(t, fake, 1)
	ctx := context.Background()
	num := fake.OpenPorts()[0]

	fake.RestartRestoring()
	clock.Advance(ownFresh + time.Second)
	l, err := p.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("no port after the restart: %v", err)
	}
	defer l.Release()
	if l.Port() != num {
		t.Errorf("handed out %d, want the restored %d", l.Port(), num)
	}
	if got := p.Stats().Lost; got != 0 {
		t.Errorf("Lost = %d, want the restored port kept", got)
	}
	if err := p.cl.SetUpstream(ctx, num, "203.0.113.9:1080"); err != nil {
		t.Errorf("moving the restored port: %v", err)
	}
	if got := len(fake.OpenPorts()); got != 1 {
		t.Errorf("the service holds %d ports, want the one restored", got)
	}
}

func TestPool_TakesNoRestoredNumberWithOtherSettingsForItsOwn(t *testing.T) {
	// Restoring is what lets a new creation time stand for our own port, and
	// only where what the port was set to is still ours.
	fake := fakebt.New(t)
	fake.SetNoLabels(true) // a service older than labels: the settings and the restore decide
	fake.SetRestores(true)
	p, clock := ownedPool(t, fake, 1)
	ctx := context.Background()
	num := fake.OpenPorts()[0]

	fake.RestartRestoring()
	fake.TakePort(num)
	clock.Advance(ownFresh + time.Second)
	l, err := p.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("no port after the restart: %v", err)
	}
	defer l.Release()
	if l.Port() == num {
		t.Errorf("handed out %d, which carries somebody else's settings", num)
	}
	if got := fake.UpstreamOf(num); got != theirs {
		t.Errorf("their port now goes to %q, want %q left alone", got, theirs)
	}
}

func TestPool_TakesASameLookingNumberForSomebodyElsesWhereTheServiceRestoresNothing(t *testing.T) {
	// Set as ours is not enough by itself: the Xrumer pool's ports on 8893
	// are set exactly as a session port of ours. Only a service that restores
	// its ports gives a number back to whoever held it.
	fake := fakebt.New(t)
	fake.SetNoLabels(true) // a service older than labels: the settings and the restore decide
	p, clock := ownedPool(t, fake, 1)
	ctx := context.Background()
	num := fake.OpenPorts()[0]

	fake.RestartRestoring() // opened again as it was, by somebody, on a service that says it restores nothing
	clock.Advance(ownFresh + time.Second)
	l, err := p.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("no port after the restart: %v", err)
	}
	defer l.Release()
	if l.Port() == num {
		t.Errorf("handed out %d on a service that does not restore its ports", num)
	}
}

func TestPool_KeepsARestoredPortItHadMovedToAnotherAddress(t *testing.T) {
	// What a port is set to is what it was last seen set to, not what it was
	// opened with: a port is moved to other addresses all through a run.
	fake := fakebt.New(t)
	fake.SetNoLabels(true) // a service older than labels: the settings and the restore decide
	fake.SetRestores(true)
	p, clock := ownedPool(t, fake, 1)
	ctx := context.Background()
	num := fake.OpenPorts()[0]
	if err := p.cl.SetUpstream(ctx, num, "203.0.113.9:1080"); err != nil {
		t.Fatalf("SetUpstream: %v", err)
	}
	clock.Advance(ownFresh + time.Second)
	if err := p.cl.Owns(ctx, num); err != nil {
		t.Fatalf("the moved port is not ours: %v", err)
	}

	fake.RestartRestoring()
	clock.Advance(ownFresh + time.Second)
	if err := p.cl.Owns(ctx, num); err != nil {
		t.Errorf("the restored port, moved before the restart, was not taken for ours: %v", err)
	}
}

func TestClient_PutsItsRunsLabelOnEveryPortItOpens(t *testing.T) {
	fake := fakebt.New(t)
	ownedPool(t, fake, 2)
	ports := fake.OpenPorts()
	if len(ports) != 2 {
		t.Fatalf("%d ports open, want 2", len(ports))
	}
	a, b := fake.LabelOf(ports[0]), fake.LabelOf(ports[1])
	if a == "" || a != b {
		t.Errorf("the ports carry labels %q and %q, want one label of this run on both", a, b)
	}
	if other := lookupLabelOfAnotherRun(t, fake); other == a {
		t.Errorf("two runs put the same label %q on their ports", a)
	}
}

// lookupLabelOfAnotherRun opens a port through a second client, as a second run
// of the program would, and says what it was labelled.
func lookupLabelOfAnotherRun(t *testing.T, fake *fakebt.Server) string {
	t.Helper()
	before := fake.OpenPorts()
	ownedPool(t, fake, 1)
	for _, p := range fake.OpenPorts() {
		if !slices.Contains(before, p) {
			return fake.LabelOf(p)
		}
	}
	t.Fatal("the second run opened no port")
	return ""
}

func TestPool_KeepsARestoredPortThatCarriesItsLabelWhateverElseChanged(t *testing.T) {
	// The label is the run's own mark and the service gives it back with the
	// port: nothing else has to agree — not the restore setting, not what the
	// port was last seen set to (moved just before the restart).
	fake := fakebt.New(t)
	p, clock := ownedPool(t, fake, 1)
	ctx := context.Background()
	num := fake.OpenPorts()[0]
	if err := p.cl.SetUpstream(ctx, num, "203.0.113.9:1080"); err != nil {
		t.Fatalf("SetUpstream: %v", err)
	}
	fake.RestartRestoring() // before the move was seen, on a service that says it restores nothing
	clock.Advance(ownFresh + time.Second)
	if err := p.cl.Owns(ctx, num); err != nil {
		t.Errorf("the restored port carrying this run's label: %v", err)
	}
}

func TestPool_TakesANumberWithAnotherLabelForSomebodyElsesHoweverItIsSet(t *testing.T) {
	fake := fakebt.New(t)
	fake.SetRestores(true)
	p, clock := ownedPool(t, fake, 1)
	ctx := context.Background()
	num := fake.OpenPorts()[0]

	fake.RestartRestoring()
	fake.SetLabel(num, "xrumer-pool")
	clock.Advance(ownFresh + time.Second)
	if err := p.cl.Owns(ctx, num); !errors.Is(err, ErrPortNotOurs) {
		t.Errorf("a number set as ours with somebody else's label: %v, want ErrPortNotOurs", err)
	}
}

func TestPool_TakesAnUnlabelledNumberWhereItsLabelWasForSomebodyElses(t *testing.T) {
	// The service keeps labels: a port that carried ours and comes back with
	// none is not the port that carried it.
	fake := fakebt.New(t)
	fake.SetRestores(true)
	p, clock := ownedPool(t, fake, 1)
	ctx := context.Background()
	num := fake.OpenPorts()[0]

	fake.RestartRestoring()
	fake.SetLabel(num, "")
	clock.Advance(ownFresh + time.Second)
	if err := p.cl.Owns(ctx, num); !errors.Is(err, ErrPortNotOurs) {
		t.Errorf("an unlabelled number where ours was: %v, want ErrPortNotOurs", err)
	}
}
