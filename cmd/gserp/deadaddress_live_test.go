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

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
	"github.com/blanktrail/google-serp-parser/run"
	"github.com/blanktrail/google-serp-parser/settings"
)

// TestDeadAddress_LiveWhatRetryingTheSameOneCosts compares what this program
// does with an address that did not carry a request against what a reference
// client does.
//
// This program retries the same port up to four times, backing off between
// them, and only moves the port to another address after three failures have
// piled up. So one dead address is asked four times before anything changes,
// and the port keeps it for three of those rounds. A reference client on the
// same list rotates on the failure and goes on, and it reports the same share
// of transport errors this program sees — 247 in 655 attempts — while
// answering two hundred requests a minute against this program's four.
//
// If the difference is the hammering, then rotating at the first failure and
// not repeating it should show up as answers per minute here. Nothing is
// asserted; the arms report.
func TestDeadAddress_LiveWhatRetryingTheSameOneCosts(t *testing.T) {
	path := os.Getenv(envLiveDB)
	if path == "" {
		t.Skipf("%s is not set", envLiveDB)
	}
	o := serveOptions{DB: path}
	saved, ok := o.saved(io.Discard)
	if !ok || saved.APIKey == "" || saved.Proxy.Kind == "" {
		t.Skip("no connection and list are saved")
	}

	for _, arm := range []struct {
		name             string
		retries, rotates int
	}{
		{"four tries, rotate after three failures", 4, 3},
		{"four tries, rotate at the first failure", 4, 1},
	} {
		t.Run(arm.name, func(t *testing.T) {
			deadAddressTrial(t, o, saved, arm.retries, arm.rotates)
		})
	}
}

func deadAddressTrial(t *testing.T, o serveOptions, saved settings.Settings, retries, rotates int) {
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()

	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		t.Fatalf("NewClient: %v", o.clean(err.Error()))
	}
	const ports, each = 10, 6
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

	var mu sync.Mutex
	sent, failedRequests := 0, 0
	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: 1, PortsPerThread: ports,
		Spec: blanktrail.DefaultPortSpec(), CA: report.CA,
		Channels:            []blanktrail.Channel{blanktrail.NewListChannel("list", rotor)},
		NoKeepAlives:        true,
		MaxRetriesPerReq:    retries,
		RotateAfterFailures: rotates,
		Trace: func(tr blanktrail.RequestTrace) {
			mu.Lock()
			defer mu.Unlock()
			sent++
			if tr.Err != nil {
				failedRequests++
			}
		},
	})
	if err != nil {
		t.Fatalf("opening %d ports: %v", ports, o.clean(err.Error()))
	}
	defer func() { _ = pool.Close() }()

	// Several identities per query, as a run has: what is being compared is how
	// quickly a query reaches an address that works, not whether the first one
	// happened to.
	attempt := &run.Attempt{Pool: pool, Tries: 5}
	queries := []string{"golang channels", "weather", "train times"}
	var answered int

	started := time.Now()
	var wg sync.WaitGroup
	for i := range ports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range each {
				if ctx.Err() != nil {
					return
				}
				_, err := attempt.Search(ctx, google.Query{
					Text: queries[(i+n)%len(queries)], Country: "us", Language: "en",
				})
				mu.Lock()
				if err == nil {
					answered++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	took := time.Since(started)

	mu.Lock()
	defer mu.Unlock()
	share := 0.0
	if sent > 0 {
		share = float64(failedRequests) / float64(sent) * 100
	}
	t.Logf("retries=%d rotate-after=%d: %d/%d queries answered in %v — "+
		"%d requests, %.0f%% failed, %.1f answers a minute",
		retries, rotates, answered, ports*each, took.Round(time.Second),
		sent, share, float64(answered)/took.Minutes())
}
