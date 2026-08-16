// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
)

// usQueries builds a numbered list, so a result can be traced back to the query
// that produced it.
func usQueries(n int) []google.Query {
	qs := make([]google.Query, n)
	for i := range qs {
		qs[i] = usQuery(fmt.Sprintf("q%02d", i))
	}
	return qs
}

func TestRunner_ReportsTheAnswersInTheOrderTheQueriesWereGiven(t *testing.T) {
	// A user who handed over a list expects the answers against it. The order
	// the threads finish in is the order of luck with the addresses, and lining
	// a report up with that puts every answer beside the wrong query.
	o := newOrigin(t, func(r *http.Request, _ int) string {
		return serpBody(r.URL.Query().Get("q") + ".test")
	})
	f := poolFacing(t, o.addr(), 4)

	queries := usQueries(12)
	r := &Runner{Pool: f.Pool, Threads: 4}
	rep := r.Run(context.Background(), Job{Queries: queries, Pages: 1})

	if rep.Done != len(queries) {
		t.Fatalf("Done=%d, want %d (failed %d, untried %d)", rep.Done, len(queries), rep.Failed, rep.Untried)
	}
	for i, q := range queries {
		res := rep.Results[i]
		if res.Query.Text != q.Text {
			t.Fatalf("Results[%d] stands against %q, want %q", i, res.Query.Text, q.Text)
		}
		if len(res.Pages) != 1 || len(res.Pages[0].Results) != 1 {
			t.Fatalf("Results[%d] carries no page to check", i)
		}
		if got, want := res.Pages[0].Results[0].Host, q.Text+".test"; got != want {
			t.Errorf("Results[%d] holds the answer to %q", i, strings.TrimSuffix(got, ".test"))
		}
	}
}

func TestRunner_LeavesAQueryNobodyReachedNeitherDoneNorFailed(t *testing.T) {
	// Counting untried work as failed tells a reader it was attempted and lost.
	// They then go looking for a fault in queries that were never sent.
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	queries := usQueries(10)
	r := &Runner{Pool: f.Pool, Threads: 2}
	rep := r.Run(ctx, Job{Queries: queries, Pages: 1, Tries: 1})

	if rep.Untried != len(queries) {
		t.Errorf("Untried=%d, want %d - nothing was sent after the caller had gone", rep.Untried, len(queries))
	}
	if rep.Failed != 0 || rep.Done != 0 {
		t.Errorf("Done=%d Failed=%d, want none of either", rep.Done, rep.Failed)
	}
	if rep.Done+rep.Failed+rep.Untried != len(queries) {
		t.Errorf("Done=%d Failed=%d Untried=%d, want them to add up to %d",
			rep.Done, rep.Failed, rep.Untried, len(queries))
	}
	for i, res := range rep.Results {
		if !res.Attempted && res.Err != nil {
			t.Errorf("Results[%d] was never attempted but carries an error", i)
		}
		if res.Query.Text != queries[i].Text {
			t.Errorf("Results[%d] stands against %q, want %q", i, res.Query.Text, queries[i].Text)
		}
	}
	if got := o.searches.Load(); got != 0 {
		t.Errorf("%d searches, want none", got)
	}
}

func TestRunner_CountsAQueryCutOffMidFlightAsAttempted(t *testing.T) {
	// A query that was in the air when the caller cancelled did produce nothing,
	// and it says why. The queries behind it were never sent, and claiming
	// otherwise would put a fault on work that never happened.
	started := make(chan struct{})
	var once sync.Once
	o := newOrigin(t, func(r *http.Request, _ int) string {
		once.Do(func() { close(started) })
		<-r.Context().Done()
		return shellBody
	})
	f := poolFacing(t, o.addr(), 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	queries := usQueries(3)
	r := &Runner{Pool: f.Pool, Threads: 1}
	rep := r.Run(ctx, Job{Queries: queries, Pages: 1, Tries: 1})

	if !rep.Results[0].Attempted {
		t.Error("the query that was in the air is reported as never sent")
	}
	if rep.Results[0].Err == nil {
		t.Error("the query that was cut off is reported as done")
	}
	if rep.Untried < 1 {
		t.Errorf("Untried=%d, want the queries behind it to be left untried", rep.Untried)
	}
	if rep.Done != 0 {
		t.Errorf("Done=%d, want none", rep.Done)
	}
	if rep.Done+rep.Failed+rep.Untried != len(queries) {
		t.Errorf("Done=%d Failed=%d Untried=%d, want them to add up to %d",
			rep.Done, rep.Failed, rep.Untried, len(queries))
	}
}

func TestRunner_PausesBetweenTheRequestsOfOneThread(t *testing.T) {
	// The pool derives the pause but does not apply it: it cannot know when the
	// caller is about to ask again. The runner is that caller, and without this
	// the delay a user configured does nothing at all.
	const pace = 90 * time.Second
	var paused atomic.Int64

	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2, func(c *blanktrail.PoolConfig) {
		c.DelayMin, c.DelayMax = pace, pace
		c.Sleep = func(ctx context.Context, d time.Duration) error {
			if d == pace {
				paused.Add(1)
			}
			return ctx.Err()
		}
	})

	queries := usQueries(4)
	r := &Runner{Pool: f.Pool, Threads: 1}
	rep := r.Run(context.Background(), Job{Queries: queries, Pages: 1})

	if rep.Done != len(queries) {
		t.Fatalf("Done=%d, want %d", rep.Done, len(queries))
	}
	if got, want := paused.Load(), int64(len(queries)-1); got != want {
		t.Errorf("%d pauses over %d queries on one thread, want %d - one between each pair",
			got, len(queries), want)
	}
}

func TestRunner_GivesUpOnWorkNobodyIsLeftToTakeWhenACancellationLandsInAPause(t *testing.T) {
	// A thread cancelled while it is pacing itself stops without taking the
	// query it was about to. Nothing else is coming to take it either, so a job
	// that only watched its threads would wait on a hand that has already gone.
	const pace = 90 * time.Second
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	paused := make(chan struct{})
	var once sync.Once
	f := poolFacing(t, o.addr(), 1, func(c *blanktrail.PoolConfig) {
		c.DelayMin, c.DelayMax = pace, pace
		c.Sleep = func(sctx context.Context, d time.Duration) error {
			if d == pace {
				once.Do(func() { close(paused) })
				<-sctx.Done()
			}
			return sctx.Err()
		}
	})
	go func() {
		<-paused
		cancel()
	}()

	queries := usQueries(3)
	r := &Runner{Pool: f.Pool, Threads: 1}

	// A job that waits on a thread that has gone never returns at all, so the
	// report is collected with a deadline. It is there only to make that
	// outcome visible.
	reports := make(chan Report, 1)
	go func() { reports <- r.Run(ctx, Job{Queries: queries, Pages: 1}) }()

	select {
	case rep := <-reports:
		if rep.Done != 1 {
			t.Errorf("Done=%d, want the 1 query that got through before the pause", rep.Done)
		}
		if rep.Untried != 2 {
			t.Errorf("Untried=%d, want 2 - neither the query left in the pause nor the one behind it was sent",
				rep.Untried)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the job never gave up on work no thread was left to take")
	}
}

func TestRunner_TakesANonPositiveThreadAndPageCountAsOne(t *testing.T) {
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2)

	// A thread count that is not clamped shows up as a job that never comes
	// back, so this one is bounded: it has to end by finishing the query, and
	// the deadline is there only to make the other outcome visible.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r := &Runner{Pool: f.Pool, Threads: 0}
	rep := r.Run(ctx, Job{Queries: usQueries(1)})

	if rep.Done != 1 {
		t.Fatalf("Done=%d, want 1 - a job with no threads to run on hands out work nobody takes (%v)",
			rep.Done, rep.Results[0].Err)
	}
	if n := len(rep.Results[0].Pages); n != 1 {
		t.Errorf("collected %d pages, want the one page a job that named no depth is worth", n)
	}
}

func TestRunner_FinishesAJobThatHasNoQueries(t *testing.T) {
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2)

	r := &Runner{Pool: f.Pool, Threads: 4}
	rep := r.Run(context.Background(), Job{Pages: 1})

	if len(rep.Results) != 0 || rep.Done+rep.Failed+rep.Untried != 0 {
		t.Errorf("an empty job reported %d results (done %d, failed %d, untried %d)",
			len(rep.Results), rep.Done, rep.Failed, rep.Untried)
	}
}

func TestRunner_RecordsWhyAQueryFailedRatherThanOnlyThatItDid(t *testing.T) {
	// "Failed" on its own sends a reader to the query. The reason is what tells
	// them whether the query is unanswerable or the answers were unusable.
	o := newOrigin(t, func(*http.Request, int) string { return shellBody })
	f := poolFacing(t, o.addr(), 3)

	r := &Runner{Pool: f.Pool, Threads: 1}
	rep := r.Run(context.Background(), Job{Queries: usQueries(1), Pages: 1, Tries: 2})

	if rep.Failed != 1 {
		t.Fatalf("Failed=%d, want 1", rep.Failed)
	}
	if !rep.Results[0].Attempted {
		t.Error("a query that was sent is reported as never attempted")
	}
	if rep.Results[0].Err == nil {
		t.Fatal("a failed query carries no reason")
	}
	if !strings.Contains(rep.Results[0].Err.Error(), string(google.ClassShell)) {
		t.Errorf("the recorded reason %q does not say what came back", rep.Results[0].Err)
	}
}

func TestRunner_AsksForEveryPageTheJobRequestedOnOneIdentity(t *testing.T) {
	o := newOrigin(t, func(*http.Request, int) string { return serpBodyWithBar("example.com") })
	// Three ports, so a walk that changed identity between pages has somewhere to
	// change to and this test can see it happen.
	f := poolFacing(t, o.addr(), 3)

	r := &Runner{Pool: f.Pool, Threads: 1}
	rep := r.Run(context.Background(), Job{Queries: usQueries(1), Pages: 3})

	if rep.Done != 1 {
		t.Fatalf("Done=%d, want 1 (failed %d: %v)", rep.Done, rep.Failed, rep.Results[0].Err)
	}
	if n := len(rep.Results[0].Pages); n != 3 {
		t.Errorf("collected %d pages, want the 3 the job asked for", n)
	}
	if got := o.searches.Load(); got != 3 {
		t.Errorf("%d searches, want 3", got)
	}
	// One identity for the three pages: it visits the front page once, and the
	// job takes one identity rather than one per page.
	if got := o.homes.Load(); got != 1 {
		t.Errorf("%d visits to the front page, want 1 - the walk changed identity between pages", got)
	}
	if got := f.portsUsed(); got != 1 {
		t.Errorf("%d identities carried the walk, want 1", got)
	}
	if rep.Requests != 1 {
		t.Errorf("Requests=%d, want the 1 identity an unrefused walk costs", rep.Requests)
	}
}
