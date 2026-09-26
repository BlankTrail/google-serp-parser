// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

func TestSolverQueue_ReadsWhatTheSolverHasInHand(t *testing.T) {
	// The three numbers the service reports, and the one that matters is the
	// queue. Running is bounded by the tariff and a run keeping every process
	// busy is a run using what it paid for; queued is requests that have
	// already been made and are holding identities while they wait.
	f := fakebt.New(t)
	f.SetSolverQueue(fakebt.SolverQueue{Queued: 4, Running: 10, MaxLen: 256})
	cl, err := NewClient(f.URL(), f.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	got, err := cl.SolverQueue(context.Background())
	if err != nil {
		t.Fatalf("SolverQueue: %v", err)
	}
	if want := (SolverQueue{Queued: 4, Running: 10, Ceiling: 256}); got != want {
		t.Errorf("the solver queue reads %+v, want %+v", got, want)
	}
}

// heldFor drives a brake through one reading and says how long it would hold.
func heldFor(t *testing.T, queued int) time.Duration {
	t.Helper()
	b := NewBrake(func(context.Context) (SolverQueue, error) {
		return SolverQueue{Queued: queued, Running: 10, Ceiling: 256}, nil
	})
	var waited time.Duration
	b.sleep = func(_ context.Context, d time.Duration) error {
		waited = d
		return nil
	}
	// The middle of each request's spread, and readings enough for the pause
	// to have stepped all the way to what the queue asks for.
	b.rand = func() float64 { return 0.5 }
	at := time.Unix(0, 0)
	b.now = func() time.Time { return at }
	for i := 0; i < 20; i++ {
		at = at.Add(brakeReadEvery)
		if err := b.Hold(context.Background()); err != nil {
			t.Fatalf("Hold: %v", err)
		}
	}
	return waited
}

func TestBrake_SlowsARunOnlyOnceTheSolverIsBehind(t *testing.T) {
	// The line is two waiting requests. One waiting request is a run using
	// every process it has with one in hand, which is what full speed looks
	// like; two is the beginning of the avalanche this exists to stop — every
	// request that meets a challenge joins the queue, holds its identity while
	// it waits, and the threads behind it go on making more.
	for _, c := range []struct {
		queued int
		want   time.Duration
	}{
		{0, 0},
		{1, 0},
		{2, brakePerItem},
		{3, 2 * brakePerItem},
		{5, 4 * brakePerItem},
		// Past the ceiling the run is not going to be rescued by waiting
		// longer, and a pause of minutes reads as a program that has stopped.
		{100, brakeCeiling},
	} {
		if got := heldFor(t, c.queued); got != c.want {
			t.Errorf("a queue of %d holds a request for %v, want %v", c.queued, got, c.want)
		}
	}
}

func TestBrake_LetsTheRunThroughWhenTheQueueCannotBeRead(t *testing.T) {
	// The service being unreachable is about to be reported by the request
	// itself, loudly. A run that stopped because its own instrument failed
	// would be a run stopped by the thing watching it.
	b := NewBrake(func(context.Context) (SolverQueue, error) {
		return SolverQueue{}, errors.New("the service is not answering")
	})
	held := false
	b.sleep = func(context.Context, time.Duration) error {
		held = true
		return nil
	}
	if err := b.Hold(context.Background()); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if held {
		t.Error("a request was held on a reading that failed")
	}
	if got := b.Holding(); got != 0 {
		t.Errorf("the brake reports it is holding %v after a failed reading", got)
	}
}

func TestBrake_WithNothingToAskHoldsNobody(t *testing.T) {
	// A machine with no connection to the service has a brake that holds
	// nothing, rather than a nil one every caller has to check for.
	b := NewBrake(nil)
	b.sleep = func(context.Context, time.Duration) error {
		t.Error("a request was held by a brake with nothing to read")
		return nil
	}
	if err := b.Hold(context.Background()); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	// And so does no brake at all, which is what a run built without one has.
	var none *Brake
	if err := none.Hold(context.Background()); err != nil {
		t.Fatalf("Hold on no brake: %v", err)
	}
	if got := none.Queue(); got != (SolverQueue{}) {
		t.Errorf("no brake reports a queue of %+v", got)
	}
}

func TestBrake_ReadsTheQueueOnATimerRatherThanOnEveryRequest(t *testing.T) {
	// Every thread of a run passes through here. A brake that read the service
	// once per request would put a hundred calls a second behind a run of a
	// hundred threads, to learn the same number a hundred times.
	asked := 0
	b := NewBrake(func(context.Context) (SolverQueue, error) {
		asked++
		return SolverQueue{Queued: 3}, nil
	})
	b.sleep = func(context.Context, time.Duration) error { return nil }
	at := time.Unix(0, 0)
	b.now = func() time.Time { return at }

	for i := 0; i < 50; i++ {
		if err := b.Hold(context.Background()); err != nil {
			t.Fatalf("Hold: %v", err)
		}
	}
	if asked != 1 {
		t.Errorf("the service was asked %d times for fifty requests in one instant, want 1", asked)
	}
	// And it is read again once the reading is old enough, so a queue that has
	// drained is noticed rather than braked against forever.
	at = at.Add(brakeReadEvery)
	if err := b.Hold(context.Background()); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if asked != 2 {
		t.Errorf("the service was asked %d times after the reading went stale, want 2", asked)
	}
}

func TestBrake_LetsGoAsTheQueueDrains(t *testing.T) {
	// The way out of an avalanche is a run that slows until it is under the
	// line and then speeds up again — gradually, because a brake that came off
	// all at once would hand the solver the same pile it just cleared.
	queued := 6
	b := NewBrake(func(context.Context) (SolverQueue, error) {
		return SolverQueue{Queued: queued}, nil
	})
	b.sleep = func(context.Context, time.Duration) error { return nil }
	at := time.Unix(0, 0)
	b.now = func() time.Time { return at }
	for i := 0; i < 10; i++ {
		at = at.Add(brakeReadEvery)
		if err := b.Hold(context.Background()); err != nil {
			t.Fatalf("Hold: %v", err)
		}
	}

	var seen []time.Duration
	for _, q := range []int{6, 4, 2, 1, 0, 0} {
		queued = q
		at = at.Add(brakeReadEvery)
		if err := b.Hold(context.Background()); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		seen = append(seen, b.Holding())
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] > seen[i-1] {
			t.Errorf("the brake tightened as the queue drained: %v", seen)
			break
		}
	}
	if seen[len(seen)-1] != 0 {
		t.Errorf("the brake is still holding %v on an empty queue", seen[len(seen)-1])
	}
	if seen[0] == 0 {
		t.Error("the brake held nothing on a queue of six")
	}
}

func TestBrake_TightensAndEasesAStepAReadingRatherThanAtOnce(t *testing.T) {
	// Jumping straight to what each reading asked for, the brake swung on five
	// hundred threads: every thread held thirty seconds, the queue drained to
	// nothing, every thread left at once, and the queue climbed again.
	queued := 100
	b := NewBrake(func(context.Context) (SolverQueue, error) { return SolverQueue{Queued: queued}, nil })
	b.sleep = func(context.Context, time.Duration) error { return nil }
	at := time.Unix(0, 0)
	b.now = func() time.Time { return at }
	read := func() time.Duration {
		at = at.Add(brakeReadEvery)
		if err := b.Hold(context.Background()); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		return b.Holding()
	}
	if got := read(); got != 4*time.Second {
		t.Errorf("the first reading of a deep queue holds %v, want one step of four seconds", got)
	}
	for i := 0; i < 10; i++ {
		read()
	}
	if got := b.Holding(); got != brakeCeiling {
		t.Fatalf("after a run of readings the brake holds %v, want the ceiling", got)
	}
	queued = 0
	if got := read(); got != brakeCeiling-4*time.Second {
		t.Errorf("the first reading of an empty queue holds %v, want the ceiling less one step", got)
	}
}

func TestBrake_SpreadsTheHoldsSoTheThreadsDoNotLeaveTogether(t *testing.T) {
	// Each request draws its own share of the pause, from half of it to half
	// again: threads held together leave one by one.
	b := NewBrake(func(context.Context) (SolverQueue, error) { return SolverQueue{Queued: 3}, nil })
	var waited []time.Duration
	b.sleep = func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }
	draws := []float64{0, 0.999}
	b.rand = func() float64 { d := draws[0]; draws = draws[1:]; return d }
	for i := 0; i < 2; i++ {
		if err := b.Hold(context.Background()); err != nil {
			t.Fatalf("Hold: %v", err)
		}
	}
	held := b.Holding()
	if waited[0] != held/2 || waited[1] < held*149/100 {
		t.Errorf("two requests under a pause of %v waited %v, want from half of it to half again", held, waited)
	}
}
