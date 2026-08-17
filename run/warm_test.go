// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
)

// warmingPool is a pool of hot ports whose requests reach a stand-in Google,
// built the same way every other test in this package builds one: the ports are
// real numbers on this machine and something answers on each of them.
func warmingPool(t *testing.T, hot int) *blanktrail.Pool {
	t.Helper()
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	pool := poolFacing(t, o.addr(), hot).Pool
	// A standing set is one somebody declared: without this every port is a
	// job's own, and the warmer would find nothing of its own to warm.
	pool.KeepWarm()
	return pool
}

func TestWarmer_WarmsEveryStandingPortAndThenLeavesThemAlone(t *testing.T) {
	// The two halves of the arrangement. Every port is cold when it is opened
	// and every one of them is warmed at once, because that is the moment it is
	// most worth doing. Then nothing: a port warmed a moment ago is not idle,
	// and a warmer that went round again would be this program hammering its own
	// identities on a timer.
	pool := warmingPool(t, 3)

	var warmed atomic.Int64
	w := &Warmer{
		Pool:   pool,
		idle:   time.Hour,
		round:  time.Millisecond,
		warmed: func() { warmed.Add(1) },
	}

	// One round by hand rather than the loop, so this test is about what a round
	// does and not about how often rounds happen.
	w.oneRound(t.Context(), time.Hour)
	if got := warmed.Load(); got != 3 {
		t.Errorf("the first round warmed %d ports, want all three", got)
	}

	// And again: nothing is idle now, so nothing is warmed.
	w.oneRound(t.Context(), time.Hour)
	if got := warmed.Load(); got != 3 {
		t.Errorf("a second round warmed %d more ports, want none — they are all warm", got-3)
	}
}

func TestWarmer_LeavesAloneAPortSomethingElseIsUsing(t *testing.T) {
	// A port the run is working through is warm by definition, and warming it as
	// well would be this program competing with itself for its own identities —
	// on a machine where every port is busy, that is a job slowed down by its own
	// warmer.
	pool := warmingPool(t, 1)

	held, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	var warmed atomic.Int64
	w := &Warmer{Pool: pool, warmed: func() { warmed.Add(1) }}
	w.oneRound(t.Context(), 0)

	if got := warmed.Load(); got != 0 {
		t.Errorf("%d ports were warmed while the only one was leased out", got)
	}
}

func TestWarmer_StopsWhenItIsToldTo(t *testing.T) {
	// It runs for as long as the program does, so ending has to be the one thing
	// it does promptly: a warmer that outlived its context would go on making
	// requests through ports something else has already closed.
	pool := warmingPool(t, 2)
	ctx, stop := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Warmer{Pool: pool, idle: time.Hour, round: time.Hour}).Run(ctx)
	}()

	stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the warmer did not stop within ten seconds of being told to")
	}
}

func TestWarmer_AsksForOrdinaryThingsAndNotTheSameOneEveryTime(t *testing.T) {
	// A port warmed all afternoon must not be a port asking the same question
	// every fifteen minutes: that is a pattern, and a pattern is the one thing
	// keeping an identity quiet is meant to avoid.
	if len(warmingPhrases) < 5 {
		t.Errorf("there are %d phrases to warm with, which is a pattern", len(warmingPhrases))
	}
	seen := map[string]bool{}
	for _, phrase := range warmingPhrases {
		if seen[phrase] {
			t.Errorf("%q is in the list twice", phrase)
		}
		seen[phrase] = true
	}
}
