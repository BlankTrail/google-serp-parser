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
)

// TestTraceCost_LiveWhereARequestActuallyWaits reports, request by request,
// where each one was when it stopped: at the tunnel, at the handshake, or with
// the request sent and nothing coming back.
//
// A total alone cannot tell those apart, and they are three different faults —
// a proxy that will not carry the connection, one that carries it and then goes
// silent, and a target thinking about an answer. This is the measurement that
// says which of them a slow run is made of.
func TestTraceCost_LiveWhereARequestActuallyWaits(t *testing.T) {
	path := os.Getenv(envLiveDB)
	if path == "" {
		t.Skipf("%s is not set", envLiveDB)
	}
	o := serveOptions{DB: path}
	saved, ok := o.saved(io.Discard)
	if !ok || saved.APIKey == "" || saved.Proxy.Kind == "" {
		t.Skip("no connection and list are saved")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		t.Fatalf("NewClient: %v", o.clean(err.Error()))
	}
	const ports = 10
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
	var traces []blanktrail.RequestTrace
	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: 1, PortsPerThread: ports,
		Spec: blanktrail.DefaultPortSpec(), CA: report.CA,
		Channels: []blanktrail.Channel{blanktrail.NewListChannel("list", rotor)},
		Trace: func(tr blanktrail.RequestTrace) {
			mu.Lock()
			traces = append(traces, tr)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("opening %d ports: %v", ports, o.clean(err.Error()))
	}
	defer func() { _ = pool.Close() }()

	attempt := &run.Attempt{Pool: pool, Tries: 1}
	var wg sync.WaitGroup
	for i := range ports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = attempt.Search(ctx, google.Query{
				Text:    []string{"golang channels", "weather", "train times"}[i%3],
				Country: "us", Language: "en",
			})
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	t.Logf("%d requests traced", len(traces))

	// Where each one stopped. A request that never connected, one that connected
	// and never finished the handshake, one that sent and heard nothing, and one
	// that got an answer are four different lines of the same table.
	stages := map[string]int{}
	var stuckAfterSending, waited time.Duration
	for _, tr := range traces {
		var where string
		switch {
		case tr.Connect == 0:
			where = "never opened the tunnel"
		case tr.TLS == 0:
			where = "opened the tunnel, never finished the handshake"
		case tr.Wrote == 0:
			where = "shook hands, never sent the request"
		case tr.FirstByte == 0:
			where = "sent the request, nothing came back"
		case tr.Err != nil:
			where = "answered, then failed"
		default:
			where = "answered"
		}
		stages[where]++
		waited += tr.Total
		if tr.FirstByte == 0 && tr.Wrote > 0 {
			stuckAfterSending += tr.Total - tr.Wrote
		}
		t.Logf("  port %d try %d reused=%v: connect=%v tls=%v sent=%v firstByte=%v total=%v — %s%v",
			tr.Port, tr.Attempt, tr.Reused,
			tr.Connect.Round(time.Millisecond), tr.TLS.Round(time.Millisecond),
			tr.Wrote.Round(time.Millisecond), tr.FirstByte.Round(time.Millisecond),
			tr.Total.Round(time.Millisecond), where, errText(o, tr))
	}

	type pair struct {
		what string
		n    int
	}
	var sorted []pair
	for what, n := range stages {
		sorted = append(sorted, pair{what, n})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].n > sorted[j].n })
	t.Log("where the requests stopped:")
	for _, p := range sorted {
		t.Logf("  %3d x %s", p.n, p.what)
	}
	t.Logf("time in requests: %v in all, %v of it after the request had been sent "+
		"with nothing coming back", waited.Round(time.Second), stuckAfterSending.Round(time.Second))
}

func errText(o serveOptions, tr blanktrail.RequestTrace) string {
	if tr.Err == nil {
		if tr.Status != 0 {
			return ""
		}
		return ""
	}
	return " (" + kindOf(o.clean(tr.Err.Error())) + ")"
}
