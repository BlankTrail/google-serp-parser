//go:build live

// SPDX-License-Identifier: MIT

package run

// What the gap between two pages is worth.
//
// A thread now pauses between every request on one identity — the pause a
// reader sets on the form, applied ninety-nine places it never used to be —
// and while it pauses on one identity it works the others. So the pause costs
// nothing in dead time and everything in how hard one identity is pushed, and
// the question is what number to start the form on.
//
// An earlier run of this asked it at a hundred pages a query and never found
// out: every identity stopped between eleven and twenty-three pages because
// the query's results ran out, and the depth was never reached to be paced. So
// the walk here is what a real one is — a query to the depth it actually has,
// then the next query on the same identity, and the next — because that is
// what the pause governs: requests through one identity, whatever query they
// belong to.
//
// Four arms, the same phrases, differing only in the gap. What is counted is
// answered pages a minute per identity, which is what a run's speed is: a gap
// too short buys challenges, a gap too long buys nothing, and both cost pages.
//
//	go test -tags live -run TestLivePaging -timeout 90m ./internal/run/ -v
//
// It holds eight identities and spends up to four hundred and eighty requests.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

const (
	// pagingDepth is how deep one query is taken. Thirty, because that is what
	// a query has: measured by hand in a browser, no phrase went past twenty to
	// thirty pages before Google stopped offering more.
	pagingDepth = 30
	// pagingBudget is how many requests one identity spends, across as many
	// queries as it takes. It is the sample size per identity, and it is more
	// than one query's depth on purpose: an identity in a real run carries
	// query after query, and the pause is between requests rather than between
	// queries.
	pagingBudget = 60
	pagingArms   = 2 // identities per arm
	// pagingSlow is how long an answer takes when the identity was sent to a
	// challenge and the solver got it through. Anything at or past it is a
	// challenge rather than a page.
	pagingSlow = 6 * time.Second
)

// pagingGaps are the arms: what the program did before the round was built,
// the number the form offers, and two either side of it.
var pagingGaps = []time.Duration{0, 2 * time.Second, 5 * time.Second, 10 * time.Second}

func TestLivePaging_WhatTheGapBetweenTwoPagesIsWorth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Minute)
	defer cancel()
	pool := livePool(ctx, t)

	// Room for the identities that will be held for the whole run and for the
	// ones a warm-up rejects. Without it the last arm queues behind the others
	// for a port and measures the wait rather than the gap.
	if err := pool.Grow(ctx, len(pagingGaps)*pagingArms); err != nil {
		t.Logf("the pool could not be widened for the arms, which may slow the warm-up: %v", err)
	}

	type arm struct {
		gap   time.Duration
		walks [][]ask
	}
	arms := make([]*arm, 0, len(pagingGaps))
	for _, gap := range pagingGaps {
		arms = append(arms, &arm{gap: gap})
	}

	// All arms at the same minute, or the slowest one measures a different hour
	// of Google's day and reads as the arm it is not.
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, a := range arms {
		a.walks = make([][]ask, pagingArms)
		for i := 0; i < pagingArms; i++ {
			wg.Add(1)
			go func(a *arm, i int) {
				defer wg.Done()
				walk := walkOneIdentity(ctx, t, pool, a.gap)
				mu.Lock()
				a.walks[i] = walk
				mu.Unlock()
			}(a, i)
		}
	}
	wg.Wait()

	for _, a := range arms {
		for i, walk := range a.walks {
			sayWalk(t, a.gap, i+1, walk)
		}
	}
	logf(t, "MEASUREMENT %s", strings.Repeat("-", 60))
	for _, a := range arms {
		sayArm(t, a.gap, a.walks)
	}
}

// walkOneIdentity spends one identity's budget on queries taken to depth,
// pausing gap between every request, and says what each one cost.
func walkOneIdentity(ctx context.Context, t *testing.T, pool *blanktrail.Pool, gap time.Duration) []ask {
	var lease *blanktrail.Lease
	var session *google.Session
	for try := 1; try <= 20; try++ {
		l, err := pool.Acquire(ctx)
		if err != nil {
			return nil
		}
		cl := l.Client()
		s := google.NewSession(cl.Transport)
		s.Client.Timeout = cl.Timeout
		if _, err := s.Search(ctx, google.Query{Text: phrases[0], Country: "ru", Language: "ru"}); err != nil {
			_ = l.Reject(ctx)
			l.Release()
			continue
		}
		lease, session = l, s
		break
	}
	if lease == nil {
		return nil
	}
	defer lease.Release()

	began := time.Now()
	var walk []ask
	// Query after query, each to the depth it has. The warm-up above already
	// asked the first phrase, so this starts on the second.
	for i := 1; len(walk) < pagingBudget; i++ {
		q := google.Query{Text: phrases[i%len(phrases)], Country: "ru", Language: "ru"}
		for page := 1; page <= pagingDepth && len(walk) < pagingBudget; page++ {
			if gap > 0 {
				if err := pool.Sleep(ctx, gap); err != nil {
					return walk
				}
			}
			at := time.Since(began)
			q.Page = page
			one := time.Now()
			serp, err := session.Search(ctx, q)
			walk = append(walk, ask{at: at, took: time.Since(one), results: len(serp.Results), err: err})
			if err != nil {
				// Three refusals in a row is an identity that is finished, and
				// going on would measure a dead port rather than a gap.
				if n := len(walk); n >= 3 && walk[n-2].err != nil && walk[n-3].err != nil {
					return walk
				}
				continue
			}
			// Where the walk of this query ends, by the same rule the run uses.
			if google.LastPage(page, serp) {
				break
			}
		}
	}
	return walk
}

// sayWalk writes down one walk: every page, and where it turned.
func sayWalk(t *testing.T, gap time.Duration, n int, walk []ask) {
	t.Helper()
	name := pagingArmName(gap)
	if len(walk) == 0 {
		logf(t, "MEASUREMENT %s, identity %d: no identity carried the first page", name, n)
		return
	}

	var answered, slow, refused int
	firstSlow, firstRefusal := -1, -1
	var series []string
	var carried time.Duration
	for i, one := range walk {
		carried += one.took
		switch {
		case one.err != nil:
			refused++
			if firstRefusal < 0 {
				firstRefusal = i + 1
			}
			series = append(series, "×")
		case one.took >= pagingSlow:
			answered++
			slow++
			if firstSlow < 0 {
				firstSlow = i + 1
			}
			series = append(series, fmt.Sprintf("C%.0f", one.took.Seconds()))
		default:
			answered++
			series = append(series, fmt.Sprintf("%.1f", one.took.Seconds()))
		}
	}

	logf(t, "MEASUREMENT %s, identity %d: %d asked, %d answered, %d past %v, %d refused, over %v",
		name, n, len(walk), answered, slow, pagingSlow, refused, walk[len(walk)-1].at.Round(time.Second))
	logf(t, "MEASUREMENT %s, identity %d: first slow page %d, first refused page %d, %.1fs of identity time per answer",
		name, n, firstSlow, firstRefusal, carried.Seconds()/float64(max(answered, 1)))
	logf(t, "MEASUREMENT %s, identity %d: seconds per page: %s", name, n, strings.Join(series, " "))
}

// sayArm is the arm as one line: what the gap bought and what it cost.
//
// Pages a minute per identity is the number the choice turns on. A gap that is
// too short is paid for in challenges, which are answers that took six seconds
// instead of one, and in refusals, which are pages nobody got; a gap that is
// too long is paid for in waiting. Both come out here as the same unit.
func sayArm(t *testing.T, gap time.Duration, walks [][]ask) {
	t.Helper()
	var asked, answered, slow, refused, withResults int
	var wall time.Duration
	live := 0
	for _, walk := range walks {
		if len(walk) == 0 {
			continue
		}
		live++
		wall += walk[len(walk)-1].at + walk[len(walk)-1].took
		for _, one := range walk {
			asked++
			switch {
			case one.err != nil:
				refused++
			default:
				answered++
				if one.took >= pagingSlow {
					slow++
				}
				if one.results > 0 {
					withResults++
				}
			}
		}
	}
	if live == 0 || wall == 0 {
		logf(t, "MEASUREMENT %s: no identity carried anything", pagingArmName(gap))
		return
	}
	// Per identity: the wall clock is summed over the identities, so dividing
	// the pages by it is already per identity.
	perMinute := float64(withResults) / wall.Minutes()
	logf(t, "MEASUREMENT %s: %d identities, %d asked, %d answered (%d of them past %v), %d refused, %d pages with results",
		pagingArmName(gap), live, asked, answered, slow, pagingSlow, refused, withResults)
	logf(t, "MEASUREMENT %s: %.1f pages a minute per identity, %.0f%% of requests refused, %.0f%% of answers challenged",
		pagingArmName(gap), perMinute,
		100*float64(refused)/float64(max(asked, 1)),
		100*float64(slow)/float64(max(answered, 1)))
}

func pagingArmName(gap time.Duration) string {
	if gap == 0 {
		return "no pause between pages"
	}
	return fmt.Sprintf("%v between pages", gap)
}
