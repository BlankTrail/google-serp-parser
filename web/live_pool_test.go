//go:build live

// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/store"
)

// What a pool per job costs and what it buys, measured rather than argued.
//
// The arrangement was chosen knowing the price: reaching an identity that
// answers has been measured at anything from half a minute to nine, and that is
// now paid by every job instead of by the first one. These are the numbers
// behind that sentence.

func TestLiveIsolation_CostsAWarmUpForEachJobAndSaysHowMuch(t *testing.T) {
	// The same work split into three jobs against the same work as one job.
	// Nothing here is a threshold — three probes are three probes — and the
	// difference is what an operator trades for jobs that share nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Minute)
	defer cancel()

	base, _ := liveServer(ctx, t)
	cl := browser()

	split := time.Now()
	for i := range 3 {
		form := liveForm()
		form.Set("name", fmt.Sprintf("one of three, %d", i+1))
		form.Set("queries", liveQueries[i])
		form.Set("pages", "1")
		res := submit(t, cl, base+newAt, form)
		if res.StatusCode != http.StatusSeeOther {
			fatalf(t, "starting job %d came back %d:\n%s", i+1, res.StatusCode, body(t, res))
		}
		at := base + jobPath(jobIDIn(t, res))
		waitUntilLive(t, ctx, fmt.Sprintf("job %d of three to finish", i+1), func() bool {
			return !jobPageAt(t, cl, at).watching
		})
		logf(t, "MEASUREMENT isolation: job %d of three was done %v in",
			i+1, time.Since(split).Round(time.Second))
	}
	apart := time.Since(split)

	together := time.Now()
	form := liveForm()
	form.Set("name", "all three in one")
	form.Set("queries", strings.Join(liveQueries[:3], "\n"))
	form.Set("pages", "1")
	res := submit(t, cl, base+newAt, form)
	if res.StatusCode != http.StatusSeeOther {
		fatalf(t, "starting the whole list came back %d:\n%s", res.StatusCode, body(t, res))
	}
	at := base + jobPath(jobIDIn(t, res))
	waitUntilLive(t, ctx, "the whole list to finish", func() bool {
		return !jobPageAt(t, cl, at).watching
	})
	whole := time.Since(together)

	logf(t, "MEASUREMENT isolation: three phrases as three jobs took %v, and as one job %v",
		apart.Round(time.Second), whole.Round(time.Second))
	logf(t, "MEASUREMENT isolation: a pool per job cost %v over these three, which is %.1f times the one job",
		(apart - whole).Round(time.Second), apart.Seconds()/max(whole.Seconds(), 1))
}

func TestLiveRaise_LeavesAJobWhosePoolWouldNotGoUpWhereItCanBeCarriedOn(t *testing.T) {
	// A pool that will not come up is the ordinary way this fails now: a key
	// that has been revoked, a service that is not running. It must not cost the
	// list. The job stays where it is and carrying it on after the connection is
	// repaired is all it takes.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	liveEnv(t)

	st := testStore(t)
	sup := NewSupervisor(st, func(context.Context, int, int, string, time.Duration) (*blanktrail.Pool, error) {
		return nil, errors.New("the control service refused this connection")
	}, livePorts, liveThreads)
	t.Cleanup(func() { _ = sup.Close() })

	id, err := sup.Enqueue(store.JobSpec{Name: "no pool for it", Pages: 1}, liveQueries[:2])
	if err != nil {
		fatalf(t, "Enqueue: %v", err)
	}
	waitUntilLive(t, ctx, "the job to have been let go of", func() bool {
		_, running := sup.Running()
		return !running && len(sup.Queued()) == 0
	})

	sum, err := st.Progress(ctx, id)
	if err != nil {
		fatalf(t, "Progress: %v", err)
	}
	logf(t, "MEASUREMENT raise: a job whose pool would not go up reads back as done=%d failed=%d left=%d finished=%v",
		sum.Done, sum.Failed, sum.Pending, sum.Finished)
	if sum.Finished {
		errorf(t, "a job that never ran is stamped finished, so nothing can carry it on")
	}
	if sum.Pending != 2 {
		errorf(t, "%d queries left, want both of them still there to be taken up", sum.Pending)
	}
}

// waitUntilLive waits for something a live run is waiting on, and says what it
// was waiting for when it runs out of patience.
//
// The gap is five seconds because everything waited for here takes minutes: a
// tighter loop would ask a hundred times for nothing, and the answers all come
// from a database or a page this test is already reading.
func waitUntilLive(t *testing.T, ctx context.Context, what string, done func() bool) {
	t.Helper()
	for {
		if done() {
			return
		}
		select {
		case <-ctx.Done():
			fatalf(t, "ran out of time waiting for %s", what)
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// jobIDIn is the job a redirect points at.
func jobIDIn(t *testing.T, res *http.Response) int64 {
	t.Helper()
	where := res.Header.Get("Location")
	_, tail, ok := strings.Cut(where, "/job/")
	if !ok {
		fatalf(t, "the answer sent the browser to %q, which names no job", where)
	}
	id, err := strconv.ParseInt(tail, 10, 64)
	if err != nil {
		fatalf(t, "the answer sent the browser to %q, whose job is not a number", where)
	}
	return id
}

func TestLiveStart_SaysWhereTheTimeGoesBetweenTheButtonAndTheFirstAnswer(t *testing.T) {
	// An operator presses start and watches a screen of noughts. This says which
	// part of the wait is what, so the answer is a number rather than a guess.
	//
	// The pieces measured elsewhere: the connection check is about three seconds
	// and runs before every raise, a list of fifteen thousand addresses loads in
	// about one, and a hundred ports open in under a tenth. What is left is the
	// first query itself, and that is the identity waking up.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	base, st := liveServer(ctx, t)
	cl := browser()

	form := liveForm()
	form.Set("name", "how long until the first answer")
	form.Set("queries", strings.Join(liveQueries[:4], "\n"))
	form.Set("pages", "1")

	pressed := time.Now()
	res := submit(t, cl, base+newAt, form)
	if res.StatusCode != http.StatusSeeOther {
		fatalf(t, "starting the job came back %d:\n%s", res.StatusCode, body(t, res))
	}
	id := jobIDIn(t, res)
	logf(t, "MEASUREMENT start: the form answered %v after the press",
		time.Since(pressed).Round(time.Millisecond))

	// The first settled query, read from the history rather than from the screen:
	// the moment is written down beside the query, so this is when it happened
	// and not when somebody looked.
	waitUntilLive(t, ctx, "the first query to settle", func() bool {
		sum, err := st.Progress(ctx, id)
		return err == nil && sum.Done+sum.Failed > 0
	})
	pace, err := st.Pace(ctx, id)
	if err != nil {
		fatalf(t, "Pace: %v", err)
	}
	logf(t, "MEASUREMENT start: the first query settled %v after the press",
		time.Since(pressed).Round(time.Second))

	waitUntilLive(t, ctx, "the job to finish", func() bool {
		sum, err := st.Progress(ctx, id)
		return err == nil && sum.Finished
	})
	logf(t, "MEASUREMENT start: all four settled %v after the press, at %.1f a minute",
		time.Since(pressed).Round(time.Second), pace.PerMinute())
}
