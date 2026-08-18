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

// TestPortTimeout_LiveWhatTheProxySideBudgetCosts compares the per-port request
// budget this program sends the control service against a much larger one.
//
// PortSpec.TimeoutSeconds is what the proxy allows one request through a port,
// and this program has always sent 30. A reference client run against the same
// list and the same service, differing chiefly in sending 300, produced answers
// at 248 a minute with dozens of solver processes busy, where this program
// produced single figures with almost none — and a solved challenge was
// measured, in this same repository, taking one to three minutes. Thirty
// seconds is less than that, so the question is whether the shorter budget is
// cutting challenge solves off before they finish.
//
// Nothing is asserted. Both arms run against the live service, and the answer
// rates decide.
func TestPortTimeout_LiveWhatTheProxySideBudgetCosts(t *testing.T) {
	path := os.Getenv(envLiveDB)
	if path == "" {
		t.Skipf("%s is not set", envLiveDB)
	}
	o := serveOptions{DB: path}
	saved, ok := o.saved(io.Discard)
	if !ok || saved.APIKey == "" || saved.Proxy.Kind == "" {
		t.Skip("no connection and list are saved")
	}

	for _, budget := range []int{30, 300} {
		t.Run(map[int]string{30: "thirty seconds (what this program sends)",
			300: "three hundred seconds"}[budget], func(t *testing.T) {
			timeoutTrial(t, o, saved, budget)
		})
	}
}

func timeoutTrial(t *testing.T, o serveOptions, saved settings.Settings, budget int) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()

	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		t.Fatalf("NewClient: %v", o.clean(err.Error()))
	}
	const ports = 8
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

	spec := blanktrail.DefaultPortSpec()
	spec.TimeoutSeconds = budget

	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: 1, PortsPerThread: ports,
		Spec: spec, CA: report.CA,
		Channels: []blanktrail.Channel{blanktrail.NewListChannel("list", rotor)},
	})
	if err != nil {
		t.Fatalf("opening %d ports: %v", ports, o.clean(err.Error()))
	}
	defer func() { _ = pool.Close() }()

	attempt := &run.Attempt{Pool: pool, Tries: 1}
	var mu sync.Mutex
	answered := 0
	kinds := map[string]int{}
	started := time.Now()

	var wg sync.WaitGroup
	for range ports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			at := time.Now()
			serp, err := attempt.Search(ctx, google.Query{Text: "golang channels", Country: "us", Language: "en"})
			took := time.Since(at)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				kinds[kindOf(o.clean(err.Error()))]++
				t.Logf("  failed after %v", took.Round(time.Millisecond))
				return
			}
			answered++
			t.Logf("  answered in %v, %d results", took.Round(time.Millisecond), len(serp.Results))
		}()
	}
	wg.Wait()

	t.Logf("port budget %ds: %d/%d answered in %v",
		budget, answered, ports, time.Since(started).Round(time.Second))
	for what, n := range kinds {
		t.Logf("  %d x %s", n, what)
	}
}
