//go:build live

// SPDX-License-Identifier: MIT

package run

// What a deep walk costs the identity walking it.
//
// A query taken to a hundred pages is a hundred requests through one identity
// with nothing between them: SearchFrom loops, and the only pause a run takes
// is the one between queries. The pause a reader sets on the form never applies
// inside a query, and a job of a hundred pages is almost entirely inside one.
//
// This asks what that costs. Three arms, the same depth, differing only in the
// gap between two pages: none, which is what the program does, two seconds and
// five. What is counted is where the walk starts being slow and where it stops
// being answered at all.
//
//	go test -tags live -run TestLivePaging -timeout 60m ./internal/run/ -v
//
// It holds six identities and spends up to six hundred requests.

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
	pagingDepth = 100
	pagingArms  = 2 // identities per arm
	pagingSlow  = 6 * time.Second
)

func TestLivePaging_WhatADeepWalkCostsTheIdentityWalkingIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	defer cancel()
	pool := livePool(ctx, t)

	type arm struct {
		gap   time.Duration
		walks [][]ask
	}
	arms := []*arm{
		{gap: 0},
		{gap: 2 * time.Second},
		{gap: 5 * time.Second},
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
}

// walkOneIdentity takes one query to depth through one identity, pausing gap
// between pages, and says what each page cost.
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

	// One phrase, walked deep, which is the shape of the job this is about.
	q := google.Query{Text: "купить кондиционер", Country: "ru", Language: "ru"}
	began := time.Now()
	var walk []ask
	for page := 1; page <= pagingDepth; page++ {
		if page > 1 && gap > 0 {
			if err := pool.Sleep(ctx, gap); err != nil {
				break
			}
		}
		at := time.Since(began)
		q.Page = page
		one := time.Now()
		serp, err := session.Search(ctx, q)
		walk = append(walk, ask{at: at, took: time.Since(one), results: len(serp.Results), err: err})
		if err != nil {
			// Three refusals in a row is a walk that is over.
			if n := len(walk); n >= 3 && walk[n-2].err != nil && walk[n-3].err != nil {
				break
			}
			continue
		}
		if len(serp.Results) == 0 {
			break
		}
	}
	return walk
}

// sayWalk writes down one walk: every page, and where it turned.
func sayWalk(t *testing.T, gap time.Duration, n int, walk []ask) {
	t.Helper()
	name := fmt.Sprintf("no pause between pages")
	if gap > 0 {
		name = fmt.Sprintf("%v between pages", gap)
	}
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

	logf(t, "MEASUREMENT %s, identity %d: %d pages asked, %d answered, %d past %v, %d refused, over %v",
		name, n, len(walk), answered, slow, pagingSlow, refused, walk[len(walk)-1].at.Round(time.Second))
	logf(t, "MEASUREMENT %s, identity %d: first slow page %d, first refused page %d, %.1fs of identity time per answer",
		name, n, firstSlow, firstRefusal, carried.Seconds()/float64(max(answered, 1)))
	logf(t, "MEASUREMENT %s, identity %d: seconds per page: %s", name, n, strings.Join(series, " "))
}
