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
	// and they are warmed as fast as the bound below allows, because that is the
	// moment it is most worth doing. Then nothing: a port warmed a moment ago is
	// not idle, and a warmer that went round again would be this program
	// hammering its own identities on a timer.
	pool := warmingPool(t, 4)

	var warmed atomic.Int64
	w := &Warmer{
		Pool:   pool,
		idle:   time.Hour,
		round:  time.Millisecond,
		warmed: func() { warmed.Add(1) },
	}

	// Rounds by hand rather than the loop, so this test is about what a round
	// does and not about how often rounds happen. Two of them, because a round
	// holds at most half the set at once and four ports are two rounds' worth.
	w.oneRound(t.Context(), time.Hour)
	if got := warmed.Load(); got != 2 {
		t.Errorf("the first round warmed %d of four ports, want the half it may hold", got)
	}
	w.oneRound(t.Context(), time.Hour)
	if got := warmed.Load(); got != 4 {
		t.Errorf("two rounds warmed %d of four ports, want all of them", got)
	}

	// And again: nothing is idle now, so nothing is warmed.
	w.oneRound(t.Context(), time.Hour)
	if got := warmed.Load(); got != 4 {
		t.Errorf("a third round warmed %d more ports, want none — they are all warm", got-4)
	}
}

func TestWarmer_WarmsSeveralAtOnceRatherThanOneAfterAnother(t *testing.T) {
	// The defect this replaced: one identity at a time, each costing the one to
	// three minutes a cold identity's first request costs, so a set of twelve
	// took a quarter of an hour to become worth anything — and a job started
	// inside that window ran on identities the operator had been told were warm.
	//
	// The stand-in Google holds every request until all of them have arrived, so
	// a round that warms one at a time cannot finish this test at all.
	pool := warmingPool(t, 4)

	together := make(chan struct{})
	var arrived atomic.Int64
	w := &Warmer{Pool: pool, idle: time.Hour, round: time.Millisecond, warmAtOnce: 4,
		warmed: func() {}}
	w.beforeSearch = func() {
		if arrived.Add(1) == 4 {
			close(together)
		}
		select {
		case <-together:
		case <-time.After(5 * time.Second):
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.oneRound(t.Context(), time.Hour)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("only %d of four warmings were in flight at once, so they are being "+
			"made one after another", arrived.Load())
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
