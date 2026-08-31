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

// TestGatewayLoad_LiveWhetherWidthIsWhatIsFailing asks one question: does the
// share of requests that fail depend on how many identities this machine is
// asking through at once?
//
// It matters because of what the list turned out to be. Fifteen thousand lines
// resolve to three hosts inside one /24, several thousand ports apiece — a
// backconnect gateway rather than fifteen thousand independent proxies. A
// gateway has a limit of its own, and if this program is past it then the
// failures are self-inflicted and the remedy is a narrower run, not a better
// list. If the share is flat, width is innocent and the addresses are simply
// this poor.
//
// Two arms, narrow then wide, back to back so the gateway and the target are in
// much the same mood for both. Nothing is asserted; the shares decide.
func TestGatewayLoad_LiveWhetherWidthIsWhatIsFailing(t *testing.T) {
	path := os.Getenv(envLiveDB)
	if path == "" {
		t.Skipf("%s is not set", envLiveDB)
	}
	o := serveOptions{DB: path}
	saved, ok := o.saved(io.Discard)
	if !ok || saved.APIKey == "" || saved.Proxy.Kind == "" {
		t.Skip("no connection and list are saved")
	}

	// The same number of requests either way, so the two arms differ in width
	// alone and not in how much they ask of anybody.
	const requests = 24
	for _, ports := range []int{3, 24} {
		t.Run(map[int]string{3: "three at a time", 24: "twenty-four at a time"}[ports],
			func(t *testing.T) { widthTrial(t, o, saved, ports, requests) })
	}
}

func widthTrial(t *testing.T, o serveOptions, saved settings.Settings, ports, requests int) {
	ctx, cancel := context.WithTimeout(t.Context(), 9*time.Minute)
	defer cancel()

	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		t.Fatalf("NewClient: %v", o.clean(err.Error()))
	}
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
	sent, failed := 0, 0
	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: 1, PortsPerThread: ports,
		Spec: blanktrail.DefaultPortSpec(), CA: report.CA,
		Channels:     []blanktrail.Channel{blanktrail.NewListChannel("list", rotor)},
		NoKeepAlives: true,
		Trace: func(tr blanktrail.RequestTrace) {
			mu.Lock()
			defer mu.Unlock()
			sent++
			if tr.Err != nil {
				failed++
			}
		},
	})
	if err != nil {
		t.Fatalf("opening %d ports: %v", ports, o.clean(err.Error()))
	}
	defer func() { _ = pool.Close() }()

	// One attempt per query, so a query that fails is one failure and not a
	// retry storm: what is being counted is how often an identity works, and
	// the retry would count the same address twice.
	attempt := &run.Attempt{Pool: pool, Tries: 1}
	queries := []string{"golang channels", "weather", "train times", "recipes"}

	var answered int
	work := make(chan int, requests)
	for i := range requests {
		work <- i
	}
	close(work)

	started := time.Now()
	var wg sync.WaitGroup
	for range ports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if ctx.Err() != nil {
					return
				}
				_, err := attempt.Search(ctx, google.Query{
					Text: queries[i%len(queries)], Country: "us", Language: "en",
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
		share = float64(failed) / float64(sent) * 100
	}
	t.Logf("%d identities at a time: %d/%d queries answered in %v — "+
		"%d requests through the gateway, %.0f%% of them failed, %.1f answers a minute",
		ports, answered, requests, took.Round(time.Second), sent, share,
		float64(answered)/took.Minutes())
}
