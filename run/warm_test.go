// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
		Pool:    pool,
		idle:    time.Hour,
		round:   time.Millisecond,
		spacing: time.Millisecond,
		warmed:  func() { warmed.Add(1) },
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
	w := &Warmer{Pool: pool, idle: time.Hour, round: time.Millisecond, spacing: time.Millisecond, warmAtOnce: 4,
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
		(&Warmer{Pool: pool, idle: time.Hour, round: time.Hour, spacing: time.Millisecond}).Run(ctx)
	}()

	stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the warmer did not stop within ten seconds of being told to")
	}
}

func TestWarmer_AsksForOrdinaryThingsAndNoWordTwice(t *testing.T) {
	// The two lists a phrase is built from. A word in one of them twice is a word
	// that comes up twice as often as the rest, which is the beginning of the
	// pattern this arrangement exists to avoid.
	for _, list := range []struct {
		what  string
		words []string
	}{
		{"subjects", warmingSubjects},
		{"tails", warmingAsks},
	} {
		if len(list.words) < 5 {
			t.Errorf("there are %d %s to build a phrase from, which is a pattern",
				len(list.words), list.what)
		}
		seen := map[string]bool{}
		for _, word := range list.words {
			if seen[word] {
				t.Errorf("%q is in the %s twice", word, list.what)
			}
			seen[word] = true
		}
	}
}

func TestWarmer_AsksForNoLocaleSoTheAddressItselfDecidesIt(t *testing.T) {
	// A warming request is not the work. What it is for is that this identity has
	// been to Google once; the locale of the results is a parameter of each
	// request rather than a property of the identity, so a job asks for whatever
	// locale it wants afterwards and the identity stays warm. Asking for none
	// lets Google answer as it would answer whoever is behind this address, which
	// is what an ordinary visitor gets — and a machine whose every identity, on
	// addresses all over the world, warmed itself with the same country in the
	// query is a machine describing itself.
	var mu sync.Mutex
	var asked []url.Values
	o := newOrigin(t, func(r *http.Request, _ int) string {
		mu.Lock()
		asked = append(asked, r.URL.Query())
		mu.Unlock()
		return serpBody("example.com")
	})
	pool := poolFacing(t, o.addr(), 1).Pool
	pool.KeepWarm()

	w := &Warmer{Pool: pool, idle: time.Hour, round: time.Millisecond, spacing: time.Millisecond, warmed: func() {}}
	w.oneRound(t.Context(), time.Hour)

	mu.Lock()
	defer mu.Unlock()
	if len(asked) == 0 {
		t.Fatal("nothing was asked, so this test measures nothing")
	}
	for _, q := range asked {
		if got := q.Get("gl"); got != "" {
			t.Errorf("a warming request named the country %q, and it should name none", got)
		}
		if got := q.Get("hl"); got != "" {
			t.Errorf("a warming request named the language %q, and it should name none", got)
		}
		if q.Get("q") == "" {
			t.Error("a warming request asked for nothing at all")
		}
	}
}

func TestWarmingPhrase_IsNotTheSameHandfulOfQuestionsAllDay(t *testing.T) {
	// One warming per identity per quarter of an hour is four an hour, so a fixed
	// handful is a handful an identity works through by lunchtime and then starts
	// again — and every identity on the machine works through the same one. Built
	// from two lists, the same amount of writing gives a few hundred phrases.
	seen := map[string]int{}
	for range 400 {
		phrase := warmingPhrase()
		if strings.TrimSpace(phrase) != phrase || phrase == "" {
			t.Fatalf("a warming phrase is %q, which is not something anybody types", phrase)
		}
		seen[phrase]++
	}
	// Four hundred draws from a few hundred phrases; anything near the ten this
	// replaced is the old list under a new name.
	if len(seen) < 100 {
		t.Errorf("four hundred warmings produced %d different phrases, and a machine "+
			"asking that few different questions is a machine with a habit", len(seen))
	}
}

func TestWarmer_StartsOneWarmingASecondRatherThanAllOfThemAtOnce(t *testing.T) {
	// How many may be in flight and how fast they may be started are different
	// questions. A machine keeping sixty identities that warmed thirty of them in
	// the same instant would be making thirty searches in one second, which is
	// not something a person does — so the round is spread out, one start at a
	// time, and the identities warm across the minute instead of in a burst.
	pool := warmingPool(t, 4)

	var mu sync.Mutex
	var at []time.Time
	gap := 40 * time.Millisecond
	w := &Warmer{Pool: pool, idle: time.Hour, round: time.Hour,
		spacing: gap, warmAtOnce: 4, warmed: func() {}}
	w.beforeSearch = func() {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
	}

	started := time.Now()
	w.oneRound(t.Context(), time.Hour)
	took := time.Since(started)

	mu.Lock()
	defer mu.Unlock()
	if len(at) != 4 {
		t.Fatalf("%d of four identities were warmed", len(at))
	}
	// Three gaps between four starts. Measured on the round rather than between
	// each pair, because what is being checked is that the starts are spread at
	// all: a round that fired them together would be over in the time one
	// request takes.
	if want := 3 * gap; took < want {
		t.Errorf("four warmings were started inside %v, and spread one per %v they "+
			"cannot be started in less than %v", took.Round(time.Millisecond), gap, want)
	}
}

func TestWarmer_StandsAsideWhileAJobIsRunning(t *testing.T) {
	// A pool with identities in hand is a pool a job is running on, and a job
	// warms every identity it touches by working through it. Warming alongside
	// it is not help: the two take leases from the same set, so every identity
	// the warmer holds is one the job is queueing for, and the warming request
	// pays the same challenge the job's own request would have paid for a phrase
	// somebody actually asked for.
	pool := warmingPool(t, 4)

	held, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	var warmed atomic.Int64
	w := &Warmer{Pool: pool, warmed: func() { warmed.Add(1) }}
	w.oneRound(t.Context(), 0)
	if got := warmed.Load(); got != 0 {
		t.Errorf("%d identities were warmed while a job held one of the four", got)
	}

	// And it takes the set up again the moment the job lets go.
	held.Release()
	w.oneRound(t.Context(), 0)
	if got := warmed.Load(); got == 0 {
		t.Error("nothing was warmed after the job let go of the identity it held")
	}
}
