// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
)

// 🔴 Job 18 of 2026-10-08: an index check of a thousand addresses had 999
// answered in seven minutes, and the last query stood for a quarter of an hour
// with every other thread idle — two of its requests held by the far end the
// whole 450 seconds a request may take, and twenty-eight tries still to go. A
// parse job had a cure for exactly that end of a job (twin.go); the kinds that
// ask once per query did not. The same hold here, through sessions as on the
// demo: once the queue is out, a twin asks the held query again beside it.
func TestRunner_AnIndexJobStartsItsLastQueryAgainBesideARequestThatHasWaitedTooLong(t *testing.T) {
	const hold = 10 * time.Second
	o := newDeepOrigin(t, 1)
	o.pick = heldOnce("site:example.com/page", 1, hold)
	f := poolFacing(t, o.addr(), 2, inSessions)
	r := &Runner{Pool: f.Pool, Threads: 2, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}, TwinAfter: 200 * time.Millisecond}

	began := time.Now()
	rep := r.Run(context.Background(), Job{Kind: Index,
		Queries: []google.Query{usQuery("example.com/other"), usQuery("example.com/page")}})
	if took := time.Since(began); took >= hold/2 {
		t.Errorf("the job took %v, want it settled by a twin well before the %v the held request waits", took, hold)
	}
	if rep.Done != 2 || rep.Failed != 0 {
		t.Fatalf("done %d, failed %d (%v), want both addresses answered", rep.Done, rep.Failed, rep.Results[1].Err)
	}
	if got := rep.Results[1].Pages; len(got) != 1 || len(got[0].Results) != 1 {
		t.Errorf("the held address settled with %+v, want the twin's verdict: the address held", got)
	}
}

// The same end of a job for a position check, which goes through the same loop.
func TestRunner_APositionJobStartsItsLastQueryAgainBesideARequestThatHasWaitedTooLong(t *testing.T) {
	const hold = 10 * time.Second
	o := newDeepOrigin(t, 1)
	o.pick = heldOnce("held", 1, hold)
	f := poolFacing(t, o.addr(), 2)
	r := &Runner{Pool: f.Pool, Threads: 2, TwinAfter: 200 * time.Millisecond}

	began := time.Now()
	rep := r.Run(context.Background(), Job{Kind: Position, Target: "example.com",
		Queries: []google.Query{usQuery("quick"), usQuery("held")}, Pages: 1})
	if took := time.Since(began); took >= hold/2 {
		t.Errorf("the job took %v, want it settled by a twin well before %v", took, hold)
	}
	if rep.Done != 2 || rep.Failed != 0 {
		t.Fatalf("done %d, failed %d (%v), want both phrases answered", rep.Done, rep.Failed, rep.Results[1].Err)
	}
}

func TestFlying_HedgesTheOldestRequestOnceAndOnlyPastTheWait(t *testing.T) {
	var fl flying
	now := time.Now()
	fl.start(0, now.Add(-3*time.Second), func() {})
	fl.start(1, now.Add(-5*time.Second), func() {})
	fl.start(2, now.Add(-time.Second/2), func() {})

	at, ok := fl.hedge(now, 2*time.Second)
	if !ok || at != 1 {
		t.Fatalf("hedged %d (%v), want query 1, the one waiting longest", at, ok)
	}
	if at, ok = fl.hedge(now, 2*time.Second); !ok || at != 0 {
		t.Fatalf("hedged %d (%v) next, want query 0: query 1 has its twin already", at, ok)
	}
	if at, ok := fl.hedge(now, 2*time.Second); ok {
		t.Errorf("hedged %d, want nothing: query 2 has waited less than the wait", at)
	}
}

func TestFlying_AFailureDoesNotSettleWhileTheOtherIsStillAsking(t *testing.T) {
	// A refusal one of the two met is no answer for the other, which may yet
	// come back with the page; the one left settles the query either way.
	var fl flying
	cancelled := 0
	fl.start(0, time.Now().Add(-time.Minute), func() { cancelled++ })
	if _, ok := fl.hedge(time.Now(), time.Second); !ok {
		t.Fatal("no twin started")
	}
	fl.joined(0, func() { cancelled++ })

	if fl.finish(0, errors.New("refused")) {
		t.Fatal("the first failure settled the query while its twin was still asking")
	}
	if !fl.finish(0, nil) {
		t.Fatal("the answer that came back did not settle the query")
	}
	if fl.finish(0, nil) {
		t.Error("the query settled twice")
	}
	if cancelled != 2 {
		t.Errorf("%d requests called off once the query settled, want both", cancelled)
	}
	if n := fl.open(); n != 0 {
		t.Errorf("%d queries still open, want none", n)
	}
}
