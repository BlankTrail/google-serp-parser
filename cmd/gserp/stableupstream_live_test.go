//go:build live

// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

// TestStableUpstream_LiveWhetherAnAddressThatWorksKeepsWorking is the control
// on the rotation rule.
//
// Leaving a dead address at once is only the right remedy if the address is
// what was wrong. So this takes the other side of it: rotate until a request
// finally answers, and then, without rotating again, ask ten more times through
// that same address. If those ten answer, an address is either good or bad and
// the pool's job is to find a good one quickly. If they do not, an address that
// works is a coincidence rather than a state, and no rotation policy can hold a
// run together.
//
// Nothing is asserted. It reports what the ten did.
func TestStableUpstream_LiveWhetherAnAddressThatWorksKeepsWorking(t *testing.T) {
	path := os.Getenv(envLiveDB)
	if path == "" {
		t.Skipf("%s is not set", envLiveDB)
	}
	o := serveOptions{DB: path}
	saved, ok := o.saved(io.Discard)
	if !ok || saved.APIKey == "" || saved.Proxy.Kind == "" {
		t.Skip("no connection and list are saved")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()

	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		t.Fatalf("NewClient: %v", o.clean(err.Error()))
	}
	const ports, hunt, after = 6, 25, 10
	report, err := o.checked(ctx, client, ports)
	if err != nil {
		t.Fatalf("checked: %v", o.clean(err.Error()))
	}
	rotor, err := blanktrail.NewRotor(ctx, blanktrail.Source{
		Kind: saved.Proxy.Kind, Location: saved.Proxy.Location, DefaultScheme: listScheme,
	})
	if err != nil {
		t.Fatalf("NewRotor: %v", o.clean(err.Error()))
	}

	// Every rotation below is one this test asked for.
	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: 1, PortsPerThread: ports,
		Spec: blanktrail.DefaultPortSpec(), CA: report.CA,
		Channels:            []blanktrail.Channel{blanktrail.NewListChannel("list", rotor)},
		NoKeepAlives:        true,
		MaxRetriesPerReq:    1,
		RotateAfterFailures: 1000,
		Cooldown:            2 * time.Second,
	})
	if err != nil {
		t.Fatalf("opening %d ports: %v", ports, o.clean(err.Error()))
	}
	defer func() { _ = pool.Close() }()

	phrases := []string{
		"golang channels", "weather", "train times", "recipes", "dictionary",
		"news", "maps", "calculator", "translate", "opening hours", "football results",
	}

	var mu sync.Mutex
	var found, streaks int
	var kept [][]bool

	var wg sync.WaitGroup
	for range ports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := pool.Acquire(ctx)
			if err != nil {
				return
			}
			defer lease.Release()

			ask := func(text string) bool {
				sess := google.NewSession(lease.Client().Transport)
				sess.Client.Timeout = lease.Client().Timeout
				_, err := sess.Search(ctx, google.Query{Text: text, Country: "us", Language: "en"})
				return err == nil
			}

			// Rotate until one answers. That is the pool's whole job on a list
			// like this, and the point here is what happens afterwards.
			var landed bool
			for try := 0; try < hunt && !landed; try++ {
				if ctx.Err() != nil {
					return
				}
				if ask(phrases[try%len(phrases)]) {
					landed = true
					break
				}
				if err := pool.RotateEgressFor(ctx, lease.Port()); err != nil {
					return
				}
			}
			if !landed {
				return
			}

			// Ten more through the same address, no rotation whatever happens.
			run := make([]bool, 0, after)
			for n := range after {
				if ctx.Err() != nil {
					break
				}
				run = append(run, ask(phrases[(n+3)%len(phrases)]))
			}

			mu.Lock()
			found++
			kept = append(kept, run)
			ok := 0
			for _, r := range run {
				if r {
					ok++
				}
			}
			if ok == len(run) && len(run) == after {
				streaks++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	t.Logf("%d of %d ports reached an address that answered", found, ports)
	total, good := 0, 0
	for i, run := range kept {
		line := ""
		ok := 0
		for _, r := range run {
			if r {
				line += "."
				ok++
				continue
			}
			line += "x"
		}
		total += len(run)
		good += ok
		t.Logf("  address %d: %s  (%d/%d)", i+1, line, ok, len(run))
	}
	if total > 0 {
		t.Logf("after landing on one that answers: %d/%d answered (%.0f%%), "+
			"%d of %d addresses answered all ten",
			good, total, float64(good)/float64(total)*100, streaks, len(kept))
	}
}
