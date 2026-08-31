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

// TestReopen_LiveWhetherChangingTheAddressIsEnough compares the two ways of
// moving a port to another address.
//
// This program changes the upstream on a port that stays open, which keeps
// everything else the port is: the profile it wears, and the cookie jar the
// proxy holds for it. A reference client on the same list closes the port and
// opens it again on the new address, so nothing of the old identity survives —
// and it answers two to three hundred requests a minute where this one manages
// ten.
//
// The difference matters if what Google objected to was the session rather than
// the address. Changing only the exit then walks the same visitor in through a
// different door, which is not less suspicious but more.
//
// Each port here is failed on purpose and then moved both ways in turn, so the
// two are measured against the same list at the same minute. Nothing is
// asserted; the two shares report.
func TestReopen_LiveWhetherChangingTheAddressIsEnough(t *testing.T) {
	path := os.Getenv(envLiveDB)
	if path == "" {
		t.Skipf("%s is not set", envLiveDB)
	}
	o := serveOptions{DB: path}
	saved, ok := o.saved(io.Discard)
	if !ok || saved.APIKey == "" || saved.Proxy.Kind == "" {
		t.Skip("no connection and list are saved")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 14*time.Minute)
	defer cancel()

	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		t.Fatalf("NewClient: %v", o.clean(err.Error()))
	}
	const ports, rounds = 12, 6
	report, err := o.checked(ctx, client, ports)
	if err != nil {
		t.Fatalf("checked: %v", o.clean(err.Error()))
	}

	ups, _, err := blanktrail.Source{
		Kind: saved.Proxy.Kind, Location: saved.Proxy.Location, DefaultScheme: listScheme,
	}.Load(ctx)
	if err != nil {
		t.Fatalf("loading the list: %v", o.clean(err.Error()))
	}

	rotor, err := blanktrail.NewRotor(ctx, blanktrail.Source{
		Kind: saved.Proxy.Kind, Location: saved.Proxy.Location, DefaultScheme: listScheme,
	})
	if err != nil {
		t.Fatalf("NewRotor: %v", o.clean(err.Error()))
	}
	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: 1, PortsPerThread: ports,
		Spec: blanktrail.DefaultPortSpec(), CA: report.CA,
		Channels:            []blanktrail.Channel{blanktrail.NewListChannel("list", rotor)},
		NoKeepAlives:        true,
		MaxRetriesPerReq:    1,
		AddressesPerRequest: 1,
		RotateAfterFailures: 1000,
		Cooldown:            time.Second,
	})
	if err != nil {
		t.Fatalf("opening %d ports: %v", ports, o.clean(err.Error()))
	}
	defer func() { _ = pool.Close() }()

	type tally struct{ tried, answered int }
	var mu sync.Mutex
	changed, reopened := tally{}, tally{}
	var next int

	// The addresses are handed out from one place so the two arms draw from the
	// same stretch of the list rather than one taking the good half.
	takeAddress := func() blanktrail.Upstream {
		mu.Lock()
		defer mu.Unlock()
		u := ups[next%len(ups)]
		next++
		return u
	}

	phrases := []string{"golang channels", "weather", "train times", "recipes",
		"dictionary", "news"}

	var wg sync.WaitGroup
	for i := range ports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := pool.Acquire(ctx)
			if err != nil {
				return
			}
			defer lease.Release()
			port := lease.Port()

			ask := func(text string) bool {
				sess := google.NewSession(lease.Client().Transport)
				sess.Client.Timeout = lease.Client().Timeout
				_, err := sess.Search(ctx, google.Query{Text: text, Country: "us", Language: "en"})
				return err == nil
			}

			// Half the ports move by changing the address on a port that stays
			// open, half by closing it and opening it again on the new one.
			reopens := i%2 == 1

			for round := range rounds {
				if ctx.Err() != nil {
					return
				}
				u := takeAddress()

				var moved bool
				switch {
				case reopens:
					_ = client.ClosePort(ctx, port)
					if _, err := client.OpenPort(ctx, port, blanktrail.DefaultPortSpec(),
						blanktrail.Egress{Upstream: u.URL()}); err == nil {
						moved = true
					}
				default:
					if err := client.SetUpstream(ctx, port, u.URL()); err == nil {
						moved = true
					}
				}
				if !moved {
					continue
				}

				ok := ask(phrases[round%len(phrases)])
				mu.Lock()
				if reopens {
					reopened.tried++
					if ok {
						reopened.answered++
					}
				} else {
					changed.tried++
					if ok {
						changed.answered++
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	show := func(label string, x tally) {
		share := 0.0
		if x.tried > 0 {
			share = float64(x.answered) / float64(x.tried) * 100
		}
		t.Logf("%-46s %2d/%2d answered (%.0f%%)", label, x.answered, x.tried, share)
	}
	show("address changed on a port that stayed open", changed)
	show("port closed and opened again on the address", reopened)
}
