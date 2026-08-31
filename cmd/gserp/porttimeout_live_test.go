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
	"github.com/blanktrail/google-serp-parser/internal/run"
	"github.com/blanktrail/google-serp-parser/internal/settings"
)

// TestPortTimeout_LiveWhatBoundingTheConnectCosts compares leaving the proxy's
// spans to it against naming them: a short wait to reach an address, and a
// generous one for the request that follows.
//
// The two are different waits and a poor list makes them opposite problems.
// Most addresses in one are dead, and a dead one is dead within a second or
// two, so a long wait to connect is a port held for nothing; most answers,
// meanwhile, come from a request that met a challenge and took one to three
// minutes to come back with a page, so a short wait for the request throws away
// work that was about to succeed.
//
// Nothing is asserted. Both arms run against the live service, and the answer
// rates decide.
func TestPortTimeout_LiveWhatBoundingTheConnectCosts(t *testing.T) {
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
		connect, request int
	}{
		{"left to the proxy", 0, 0},
		{"five seconds to connect, three minutes to answer", 5, 180},
	} {
		t.Run(arm.name, func(t *testing.T) {
			timeoutTrial(t, o, saved, arm.connect, arm.request)
		})
	}
}

func timeoutTrial(t *testing.T, o serveOptions, saved settings.Settings, connect, request int) {
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
	spec.ConnectTimeoutSeconds = connect
	spec.RequestTimeoutSeconds = request

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

	t.Logf("connect %ds, request %ds: %d/%d answered in %v",
		connect, request, answered, ports, time.Since(started).Round(time.Second))
	for what, n := range kinds {
		t.Logf("  %d x %s", n, what)
	}
}
