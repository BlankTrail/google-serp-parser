// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
)

// heldOnce holds the first search for one query's page for the given time, the
// way the service holds a request while a check on it is being solved, and
// answers every other search at once.
func heldOnce(q string, page int, hold time.Duration) func(string, int) (time.Duration, bool) {
	var held atomic.Bool
	return func(asked string, at int) (time.Duration, bool) {
		if asked == q && at == page && held.CompareAndSwap(false, true) {
			return hold, false
		}
		return 0, false
	}
}

func TestRunner_StartsTheLastQueryAgainBesideARequestThatHasWaitedTooLong(t *testing.T) {
	// The end of the speed test of 2026-09-28: the last queries waited on
	// checks the service took five minutes to solve, with every other thread
	// idle. Two threads, a short query and a long one whose second page is held
	// far longer than the run should take: once the short one is done, a twin
	// of the long one walks it again through another session and settles it.
	const hold = 10 * time.Second
	o := newDeepOrigin(t, 2)
	o.depthOf = func(q string) int {
		if q == "short" {
			return 1
		}
		return 2
	}
	o.pick = heldOnce("long", 2, hold)
	f := poolFacing(t, o.addr(), 2, inSessions)
	r := &Runner{Pool: f.Pool, Threads: 2, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}, TwinAfter: 200 * time.Millisecond}

	began := time.Now()
	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("short"), usQuery("long")}, Pages: 2})
	took := time.Since(began)
	if took >= hold/2 {
		t.Errorf("the job took %v, want it done by a twin well before the %v the held request waits", took, hold)
	}
	// Two pages, the twin's: not the walk's first page with the twin's two
	// after it, and not the walk's one page alone.
	if got := rep.Results[1]; got.Err != nil || len(got.Pages) != 2 {
		t.Errorf("the long query settled with %d pages and %v, want the twin's two and no error", len(got.Pages), got.Err)
	}
	if rep.Done != 2 || rep.Failed != 0 {
		t.Errorf("the report says %d done and %d failed, want both queries done", rep.Done, rep.Failed)
	}
}

func TestRunner_ATwinGoogleRefusesLeavesTheQueryToTheWalkItWasStartedBeside(t *testing.T) {
	// A twin that fails settles nothing: the walk it was started beside may yet
	// come back with the pages, and here it does, a second and a half late.
	o := newDeepOrigin(t, 2)
	o.depthOf = func(q string) int {
		if q == "short" {
			return 1
		}
		return 2
	}
	held := heldOnce("long", 2, 1500*time.Millisecond)
	var firsts atomic.Int32
	o.pick = func(q string, page int) (time.Duration, bool) {
		// The walk's own first page is answered; the twin's, the second time
		// the first page is asked for, is refused.
		if q == "long" && page == 1 && firsts.Add(1) > 1 {
			return 0, true
		}
		return held(q, page)
	}
	f := poolFacing(t, o.addr(), 2, inSessions)
	r := &Runner{Pool: f.Pool, Threads: 2, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}, TwinAfter: 200 * time.Millisecond}

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("short"), usQuery("long")}, Pages: 2, Tries: 1})
	if firsts.Load() < 2 {
		t.Fatalf("the long query's first page was asked for %d times, want a twin to have asked it again", firsts.Load())
	}
	if got := rep.Results[1]; got.Err != nil || len(got.Pages) != 2 {
		t.Errorf("the long query settled with %d pages and %v, want the walk's two and no error", len(got.Pages), got.Err)
	}
}

func TestRunner_ATwinThatLosesTakesItsPagesWithIt(t *testing.T) {
	// The walk comes back first: the twin has a page of its own by then and is
	// held on its second, and the query is the walk's two pages — not the walk's
	// two with the twin's first among them — with the twin's request called off
	// rather than waited for.
	const twinHold = 10 * time.Second
	o := newDeepOrigin(t, 2)
	o.depthOf = func(q string) int {
		if q == "short" {
			return 1
		}
		return 2
	}
	var seconds atomic.Int32
	o.pick = func(q string, page int) (time.Duration, bool) {
		if q != "long" || page != 2 {
			return 0, false
		}
		if seconds.Add(1) == 1 {
			return 1200 * time.Millisecond, false
		}
		return twinHold, false
	}
	f := poolFacing(t, o.addr(), 2, inSessions)
	r := &Runner{Pool: f.Pool, Threads: 2, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}, TwinAfter: 200 * time.Millisecond}

	began := time.Now()
	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("short"), usQuery("long")}, Pages: 2})
	took := time.Since(began)
	if seconds.Load() < 2 {
		t.Fatalf("the long query's second page was asked for %d times, want the twin to have reached it", seconds.Load())
	}
	if got := rep.Results[1]; got.Err != nil || len(got.Pages) != 2 {
		t.Errorf("the long query settled with %d pages and %v, want the walk's two and no error", len(got.Pages), got.Err)
	}
	if took >= twinHold/2 {
		t.Errorf("the job took %v, want the losing twin's request called off rather than waited for", took)
	}
}

func TestCrew_StartsATwinOnlyOnceTheQueueIsDrainedAndAfterAMinute(t *testing.T) {
	now := time.Now()
	c := &crew{r: &Runner{}, walks: newWalks()}
	one := c.walks.begin(0, usQuery("one"), now)
	c.walks.give(1, one)
	c.walks.asking(one, now.Add(-50*time.Second), func() {})

	if twin := c.twin(true, now); twin != nil {
		t.Error("a twin was started for a request fifty seconds out, want none before the minute")
	}
	later := now.Add(20 * time.Second)
	if twin := c.twin(false, later); twin != nil {
		t.Error("a twin was started before the queue was drained")
	}
	if twin := c.twin(true, later); twin == nil {
		t.Error("no twin for a request seventy seconds out at the end of the job")
	}
}

func TestWalks_HedgesTheRequestThatHasWaitedLongestAndOnlyOnce(t *testing.T) {
	w := newWalks()
	now := time.Now()
	long := w.begin(0, usQuery("long"), now)
	short := w.begin(1, usQuery("short"), now)
	quiet := w.begin(2, usQuery("quiet"), now)
	next := w.begin(3, usQuery("next"), now)
	w.give(1, long)
	w.give(2, short)
	w.give(3, quiet)
	w.give(4, next)
	w.asking(long, now.Add(-90*time.Second), func() {})
	w.asking(short, now.Add(-10*time.Second), func() {})
	w.asking(next, now.Add(-70*time.Second), func() {})

	twin := w.hedge(now, time.Minute)
	if twin == nil || twin.at != long.at || !twin.twin {
		t.Fatalf("hedged %+v, want a twin of the request that has waited ninety seconds", twin)
	}
	if got := w.open(); got != 5 {
		t.Errorf("%d walks open with the twin, want five", got)
	}
	if again := w.hedge(now, time.Minute); again == nil || again.at != next.at {
		t.Fatalf("the second hedge was %+v, want the request seventy seconds out", again)
	}
	if again := w.hedge(now, time.Minute); again != nil {
		t.Errorf("hedged query %d again, want no second twin and none for a request under the minute", again.at)
	}
	if none := w.hedge(now.Add(time.Hour), time.Minute); none != nil && none.at == quiet.at {
		t.Error("hedged a walk with no request out")
	}
}

func TestWalks_DropCallsOffTheLosersRequestAndTakesItOutOnce(t *testing.T) {
	w := newWalks()
	one := w.begin(0, usQuery("one"), time.Now())
	w.give(7, one)
	var called atomic.Bool
	w.asking(one, time.Now(), func() { called.Store(true) })

	w.drop(one)
	w.drop(one)
	if !called.Load() {
		t.Error("the dropped walk's request was not called off")
	}
	if got := w.open(); got != 0 {
		t.Errorf("%d walks open after the drop, want none", got)
	}
	if _, ok := w.of(7); ok {
		t.Error("the dropped walk is still under its session")
	}
	if _, took := w.give(8, one); took {
		t.Error("a dropped walk was given to a session")
	}
	w.waitFor(one)
	if _, ok := w.resume(); ok {
		t.Error("a dropped walk came back among those waiting")
	}
}

func TestCrew_ATwinThatNeverReachedGoogleLeavesTheQueryAsTheWalkHasIt(t *testing.T) {
	// A twin whose tries all went on the road ends having asked nothing. The
	// walk beside it is still carrying the query, which stays attempted: left
	// unattempted, a query the walk goes on to finish is reported as never
	// tried.
	var mu sync.Mutex
	c := &crew{r: &Runner{}, walks: newWalks(), results: make([]QueryResult, 1), mu: &mu}
	c.results[0].Attempted = true
	now := time.Now()
	one := c.walks.begin(0, usQuery("one"), now)
	c.walks.give(1, one)
	c.walks.asking(one, now.Add(-2*time.Minute), func() {})
	twin := c.twin(true, now)
	c.walks.give(2, twin)

	c.never(twin, 2)
	if !c.results[0].Attempted {
		t.Error("the query was left unattempted by a twin that never reached Google")
	}
	if _, ok := c.walks.of(1); !ok {
		t.Error("the walk the twin was started beside is no longer carrying the query")
	}
	if got := c.walks.open(); got != 1 {
		t.Errorf("%d walks open after the twin left, want the walk alone", got)
	}
}
