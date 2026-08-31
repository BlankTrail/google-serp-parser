//go:build live

// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/run"
)

// TestAttemptCost_LiveWhereTheMinutesGo reports what one attempt through one
// identity costs on this machine's list of addresses, and how many of them
// bring back nothing.
//
// It is the measurement behind "the job is slow": a run at a hundred threads
// that settles nine queries a minute is spending eleven minutes per query
// somewhere, and the only two candidates are requests that fail slowly and
// requests that are refused. Which of the two it is decides what to change —
// a deadline, or the way an identity is chosen — and guessing between them has
// already cost a day.
//
// Nothing is asserted. It reports, and the numbers decide.
func TestAttemptCost_LiveWhereTheMinutesGo(t *testing.T) {
	path := os.Getenv(envLiveDB)
	if path == "" {
		t.Skipf("%s is not set", envLiveDB)
	}
	o := serveOptions{DB: path}
	saved, ok := o.saved(io.Discard)
	if !ok || saved.APIKey == "" {
		t.Skipf("no connection is saved beside the history %s names", envLiveDB)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()

	const ports, threads, each = 12, 6, 5
	pool, err := o.dial(ctx, saved, 1, ports, blanktrail.DeviceDesktop, 0)
	if err != nil {
		t.Fatalf("opening %d identities: %v", ports, o.clean(err.Error()))
	}
	defer func() { _ = pool.Close() }()
	t.Logf("%d identities open, cooldown %v", pool.Stats().Ports, pool.Cooldown())

	// The same thing the run does: one attempt, which leases an identity, asks,
	// and hands a refusal back to the pool. Retrying across identities is the
	// run's own and is left out — what is being timed is one attempt.
	attempt := &run.Attempt{Pool: pool, Tries: 1}

	type outcome struct {
		took time.Duration
		what string
	}
	var mu sync.Mutex
	var got []outcome

	var wg sync.WaitGroup
	for w := range threads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if ctx.Err() != nil {
					return
				}
				q := google.Query{
					Text:     fmt.Sprintf("golang channels %d %d", w, i),
					Country:  "us",
					Language: "en",
				}
				started := time.Now()
				serp, err := attempt.Search(ctx, q)
				took := time.Since(started)
				what := fmt.Sprintf("answered, %d results", len(serp.Results))
				if err != nil {
					what = kindOf(o.clean(err.Error()))
				}
				mu.Lock()
				got = append(got, outcome{took, what})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(got) == 0 {
		t.Fatal("nothing was attempted")
	}
	sort.Slice(got, func(i, j int) bool { return got[i].took < got[j].took })

	kinds := map[string]int{}
	var answered, total time.Duration
	answers := 0
	for _, g := range got {
		kinds[g.what]++
		total += g.took
		if strings.HasPrefix(g.what, "answered") {
			answered += g.took
			answers++
		}
	}
	t.Logf("%d attempts, %d answered", len(got), answers)
	t.Logf("time in attempts: %v in all, %v of it in the ones that answered",
		total.Round(time.Second), answered.Round(time.Second))
	for _, at := range []float64{0.5, 0.9, 1} {
		i := int(float64(len(got)-1) * at)
		t.Logf("  %3.0f%% of attempts took up to %v — %s",
			at*100, got[i].took.Round(time.Millisecond), got[i].what)
	}
	type pair struct {
		what string
		n    int
	}
	var sorted []pair
	for what, n := range kinds {
		sorted = append(sorted, pair{what, n})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].n > sorted[j].n })
	for _, p := range sorted {
		t.Logf("  %3d × %s", p.n, p.what)
	}

	st := pool.Stats()
	t.Logf("the pool made %d requests, %d answers were handed back as unusable, "+
		"%d egress rotations, %d quarantined, %d warm",
		st.Requests, st.Rejections, st.EgressRotations, st.Quarantined, st.Warm)
}

// kindOf turns one failure into the kind of failure it is, so that twenty of
// them group rather than reading as twenty faults.
func kindOf(text string) string {
	for _, mark := range []string{
		"context deadline exceeded", "forcibly closed", "EOF",
		"connection was refused", "no such host", "timeout",
		"unexpected EOF", "reset by peer",
	} {
		if strings.Contains(text, mark) {
			return "failed: " + mark
		}
	}
	if i := strings.Index(text, "google: "); i >= 0 {
		rest := text[i:]
		if j := strings.Index(rest, ":"); j > 0 {
			if k := strings.Index(rest[j+1:], ":"); k > 0 {
				return "failed: " + rest[:j+1+k]
			}
		}
		return "failed: " + rest
	}
	if len(text) > 90 {
		text = text[:90] + "…"
	}
	return "failed: " + text
}
