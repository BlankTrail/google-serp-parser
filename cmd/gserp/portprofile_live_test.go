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

// TestPortProfile_LiveWhatThePortIsOpenedWith compares the port this program
// used to open against the port anybody opening one by hand gets.
//
// Three things differed, and all three are this program's doing. It asked for
// the leak guard, which sets every port checking itself through the same
// upstream the work uses — fetching an address from a third-party site,
// resolving against a public resolver — and on a poor list those are what the
// port spends its first ten seconds failing at. And it said nothing about
// forcing IPv4 on the way out, or about injecting the exit subnet into DNS,
// both of which are on by default where a port is opened by hand.
//
// A reference client on the same list and the same service answers two to three
// hundred requests a minute where this program manages four, so the difference
// is here rather than in the addresses. Nothing is asserted; the two arms
// report and the numbers decide.
func TestPortProfile_LiveWhatThePortIsOpenedWith(t *testing.T) {
	path := os.Getenv(envLiveDB)
	if path == "" {
		t.Skipf("%s is not set", envLiveDB)
	}
	o := serveOptions{DB: path}
	saved, ok := o.saved(io.Discard)
	if !ok || saved.APIKey == "" || saved.Proxy.Kind == "" {
		t.Skip("no connection and list are saved")
	}

	was := blanktrail.DefaultPortSpec()
	was.LeakGuard = "warn"
	was.ForceIPv4Egress = false
	was.InjectECS = false

	for _, arm := range []struct {
		name string
		spec blanktrail.PortSpec
	}{
		{"as this program used to open one", was},
		{"as one opened by hand", blanktrail.DefaultPortSpec()},
	} {
		t.Run(arm.name, func(t *testing.T) { profileTrial(t, o, saved, arm.spec) })
	}
}

func profileTrial(t *testing.T, o serveOptions, saved settings.Settings, spec blanktrail.PortSpec) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()

	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		t.Fatalf("NewClient: %v", o.clean(err.Error()))
	}
	const ports, each = 10, 3
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
		Spec: spec, CA: report.CA,
		Channels:     []blanktrail.Channel{blanktrail.NewListChannel("list", rotor)},
		NoKeepAlives: true,
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

	attempt := &run.Attempt{Pool: pool, Tries: 1}
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
	t.Logf("%d/%d queries answered in %v — %d requests, %.0f%% of them failed, "+
		"%.1f answers a minute", answered, ports*each, took.Round(time.Second),
		sent, share, float64(answered)/took.Minutes())
}
