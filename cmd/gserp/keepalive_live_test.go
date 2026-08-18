//go:build live

// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
	"github.com/blanktrail/google-serp-parser/run"
	"github.com/blanktrail/google-serp-parser/settings"
)

// TestKeepAlive_LiveWhatReusingAConnectionCosts compares a pool that keeps
// connections alive between requests against one that opens a fresh connection
// for each.
//
// A trace of a live run put the difference at the centre of everything: a
// request that opened its own connection answered in four to seven seconds,
// while a request handed a pooled one waited thirty-eight seconds, a minute,
// two minutes for the first byte — with the whole of that time spent after the
// request had been sent. Reuse is worth a handshake; it is not worth two
// minutes, and every session this program makes sends its second request down
// the connection its first one opened.
//
// Nothing is asserted. Both arms make the same searches through the same list.
func TestKeepAlive_LiveWhatReusingAConnectionCosts(t *testing.T) {
	path := os.Getenv(envLiveDB)
	if path == "" {
		t.Skipf("%s is not set", envLiveDB)
	}
	o := serveOptions{DB: path}
	saved, ok := o.saved(io.Discard)
	if !ok || saved.APIKey == "" || saved.Proxy.Kind == "" {
		t.Skip("no connection and list are saved")
	}

	for _, keep := range []bool{true, false} {
		name := "keeping connections alive (what this program does)"
		if !keep {
			name = "a fresh connection for every request"
		}
		t.Run(name, func(t *testing.T) { keepAliveTrial(t, o, saved, keep) })
	}
}

func keepAliveTrial(t *testing.T, o serveOptions, saved settings.Settings, keepAlive bool) {
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
	var reusedSlow, freshFast []time.Duration
	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: 1, PortsPerThread: ports,
		Spec: blanktrail.DefaultPortSpec(), CA: report.CA,
		Channels:     []blanktrail.Channel{blanktrail.NewListChannel("list", rotor)},
		NoKeepAlives: !keepAlive,
		Trace: func(tr blanktrail.RequestTrace) {
			mu.Lock()
			defer mu.Unlock()
			if tr.Reused {
				reusedSlow = append(reusedSlow, tr.Total)
				return
			}
			freshFast = append(freshFast, tr.Total)
		},
	})
	if err != nil {
		t.Fatalf("opening %d ports: %v", ports, o.clean(err.Error()))
	}
	defer func() { _ = pool.Close() }()

	attempt := &run.Attempt{Pool: pool, Tries: 1}
	var answered, failed int
	var wg sync.WaitGroup
	started := time.Now()
	for i := range ports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range each {
				_, err := attempt.Search(ctx, google.Query{
					Text:    []string{"golang channels", "weather", "train times"}[(i+n)%3],
					Country: "us", Language: "en",
				})
				mu.Lock()
				if err != nil {
					failed++
				} else {
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
	t.Logf("%d answered, %d failed, in %v — %.1f answers a minute",
		answered, failed, took.Round(time.Second), float64(answered)/took.Minutes())
	report3 := func(label string, ds []time.Duration) {
		if len(ds) == 0 {
			t.Logf("  %s: none", label)
			return
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		var sum time.Duration
		for _, d := range ds {
			sum += d
		}
		t.Logf("  %s: %d requests, median %v, worst %v, %v in all",
			label, len(ds), ds[len(ds)/2].Round(time.Millisecond),
			ds[len(ds)-1].Round(time.Millisecond), sum.Round(time.Second))
	}
	report3("on a connection that was already open", reusedSlow)
	report3("on a connection of its own", freshFast)
}
