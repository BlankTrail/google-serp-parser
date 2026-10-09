// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
)

// pausesOf runs the job and counts the gaps its threads left between two of
// their own queries.
func pausesOf(t *testing.T, r *Runner, j Job) int {
	t.Helper()
	var mu sync.Mutex
	pauses := 0
	r.Watch = func(s Step) {
		if s.Stage == StagePause {
			mu.Lock()
			pauses++
			mu.Unlock()
		}
	}
	rep := r.Run(context.Background(), j)
	for i, q := range rep.Results {
		if q.Err != nil {
			t.Fatalf("query %d: %v", i, q.Err)
		}
	}
	return pauses
}

func positionJob() Job {
	return Job{Kind: Position, Target: "example.com", Pages: 1,
		Queries: []google.Query{usQuery("a"), usQuery("b"), usQuery("c"), usQuery("d")}}
}

// A position or index check in a run that keeps sessions does not pause its
// threads between queries. The pause a pool is paced at is the session's rest,
// and the keeper already holds every session to it; a thread that slept it too
// asked once every half minute whatever sessions stood rested. Measured on the
// position checks of 2026-10-09: 150 threads, a rest of 30 to 60 seconds, about
// 230 queries a minute, where a parse job on the same sessions and threads read
// 1 600 pages a minute — its threads take another session instead of waiting.
func TestRunner_APositionCheckOnKeptSessionsDoesNotPauseItsThreads(t *testing.T) {
	o := newCookieOrigin(t, func(int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2, inSessions)
	f.Pool.PaceAt(30 * time.Second)
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}}

	if n := pausesOf(t, r, positionJob()); n != 0 {
		t.Errorf("the thread paused %d times between queries; the keeper holds the sessions' rest", n)
	}
}

// Without kept sessions the thread is what paces the port it asks through, and
// the pause stays.
func TestRunner_APositionCheckWithoutKeptSessionsStillPauses(t *testing.T) {
	o := newCookieOrigin(t, func(int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2)
	f.Pool.PaceAt(30 * time.Second)
	r := &Runner{Pool: f.Pool, Threads: 1}

	if n := pausesOf(t, r, positionJob()); n != 3 {
		t.Errorf("the thread paused %d times between four queries, want 3", n)
	}
}
