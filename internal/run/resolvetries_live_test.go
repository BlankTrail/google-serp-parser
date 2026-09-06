//go:build live

// SPDX-License-Identifier: MIT

package run

// How many identities it takes before a hidden address is read.
//
// The lookups give up on a page after a fixed number of identities bring
// nothing back, and pages keep arriving with holes in them: 3 280 results
// captured by one build, 332 of them written with no address, and 84 of 367
// pages carrying at least one hole. The number that rule is built on was never
// measured — it was borrowed from a page-level observation — and this measures
// it: one link, one identity at a time, until the address comes back or the
// allowance is spent.
//
//	go test -tags live -run TestLiveResolveTries -timeout 30m ./internal/run/ -v
//
// It spends up to a few dozen identities and one request each.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/google"
)

// resolveCeiling is how many identities this measurement is willing to spend on
// one link. It is not the program's rule — it is the window the rule has to be
// chosen inside, and it has to be wider than any rule worth having.
const resolveCeiling = 30

func TestLiveResolveTries_HowManyIdentitiesUntilAnAddressIsRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	pool := livePool(ctx, t)

	// The same region the run that showed the holes was pointed at.
	query := google.Query{Text: "купить кондиционер", Country: "ru", Language: "ru"}
	var serps []google.SERP
	for try := 1; try <= 12; try++ {
		lease, err := pool.Acquire(ctx)
		if err != nil {
			fatalf(t, "acquiring an identity to capture with: %v", err)
		}
		cl := lease.Client()
		session := google.NewSession(cl.Transport)
		session.Client.Timeout = cl.Timeout
		got, err := google.SearchDepth(ctx, session, query, 3)
		if err != nil {
			logf(t, "capture attempt %d did not get through: %v", try, err)
			_ = lease.Reject(ctx)
			lease.Release()
			continue
		}
		lease.Release()
		serps = got
		break
	}
	if len(serps) == 0 {
		t.Fatal("no identity carried the capture")
	}

	origin := serps[0].Origin
	var hidden []google.Result
	var results int
	for _, serp := range serps {
		for _, one := range serp.Results {
			results++
			if !one.Resolved() && one.Link != "" {
				hidden = append(hidden, one)
			}
		}
	}
	logf(t, "MEASUREMENT capture: %d pages, %d results, %d hiding the address",
		len(serps), results, len(hidden))
	if len(hidden) < 6 {
		t.Skip("this capture hid too few addresses to measure how many identities one takes")
	}

	// One link at a time, one identity per attempt, so what is counted is what
	// the rule is about: how many identities a link is carried to before its
	// address comes back.
	took := map[int]int{}
	var lost, attempts int
	kinds := map[string]int{}
	for i := range hidden {
		one := hidden[i]
		read := 0
		for try := 1; try <= resolveCeiling; try++ {
			lease, err := pool.Acquire(ctx)
			if err != nil {
				fatalf(t, "acquiring an identity for a lookup: %v", err)
			}
			cl := lease.Client()
			resolver := google.NewResolver(cl.Transport)
			resolver.Client.Timeout = cl.Timeout
			copyOf := []google.Result{one}
			rep := resolver.ResolveResults(ctx, origin, copyOf, 1)
			attempts++
			if rep.Resolved == 0 {
				kinds[kindOfResolveFailure(rep.Errs)]++
				_ = lease.Reject(ctx)
			}
			lease.Release()
			if rep.Resolved == 1 {
				read = try
				break
			}
		}
		if read == 0 {
			lost++
			continue
		}
		took[read]++
	}

	var tries []int
	for n := range took {
		tries = append(tries, n)
	}
	sort.Ints(tries)
	var said []string
	var read, weighted int
	for _, n := range tries {
		said = append(said, fmt.Sprintf("%d identities×%d links", n, took[n]))
		read += took[n]
		weighted += n * took[n]
	}
	logf(t, "MEASUREMENT identities per address: %s; %d never came, over %d attempts",
		strings.Join(said, ", "), lost, attempts)
	if read > 0 {
		logf(t, "MEASUREMENT mean identities per address read: %.2f; one attempt in %d succeeded",
			float64(weighted)/float64(read), attempts/max(read, 1))
	}

	// What the failures were. If they are the far end answering something other
	// than a redirect, patience is the wrong fix and no number of identities
	// will help; if they are the address dying, patience is the whole fix.
	var why []string
	for kind, n := range kinds {
		why = append(why, fmt.Sprintf("%s×%d", kind, n))
	}
	sort.Strings(why)
	logf(t, "MEASUREMENT what the failures were: %s", strings.Join(why, ", "))

	// The share of links that would still be lost under a rule of n identities,
	// read straight off the distribution rather than modelled.
	for _, rule := range []int{1, 2, 3, 5, 8, 10, 15, 20, 30} {
		var kept int
		for _, n := range tries {
			if n <= rule {
				kept += took[n]
			}
		}
		logf(t, "MEASUREMENT a rule of %2d identities reads %d of %d links (%.0f%%)",
			rule, kept, len(hidden), 100*float64(kept)/float64(len(hidden)))
	}
}
