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
	"github.com/blanktrail/google-serp-parser/internal/settings"
)

// TestAfterFailure_LiveWhetherAPortIsSpentOrTheAddressWas answers the one
// question a trace could not: when a request through a port fails and the next
// one through the same port fails too, which of the two is at fault — the port,
// or the address behind it?
//
// It matters because the two have opposite remedies. A port that is spent after
// one failure is something to give back and open again, and it would be the
// proxy's business. An address that is simply dead is something to rotate away
// from at once — and this program does not: it repeats through the same one
// twice before its third failure moves the port elsewhere, which a live trace
// showed answering nothing at all in seventy-two attempts.
//
// So each port here is asked three times over, and between the tries it is told
// to change address or left as it is. If the request after an explicit rotation
// succeeds about as often as a first request does, the port was never the
// problem. If it fails as reliably as a plain repeat, the port is.
func TestAfterFailure_LiveWhetherAPortIsSpentOrTheAddressWas(t *testing.T) {
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
	const ports, rounds = 12, 4
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

	// The ladder is taken out of the picture: one try per request, no rotation
	// of its own, so every rotation below is one this test asked for and every
	// failure is one request's own.
	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: 1, PortsPerThread: ports,
		Spec: blanktrail.DefaultPortSpec(), CA: report.CA,
		Channels:            []blanktrail.Channel{blanktrail.NewListChannel("list", rotor)},
		NoKeepAlives:        true,
		MaxRetriesPerReq:    1,
		RotateAfterFailures: 1000, // never on its own
		Cooldown:            time.Second,
	})
	if err != nil {
		t.Fatalf("opening %d ports: %v", ports, o.clean(err.Error()))
	}
	defer func() { _ = pool.Close() }()

	type tally struct{ tried, answered int }
	var mu sync.Mutex
	first := tally{}         // the first request through a port
	repeated := tally{}      // a request after a failure, same address
	afterRotation := tally{} // a request after a failure and an explicit rotation

	ask := func(lease *blanktrail.Lease, text string) bool {
		sess := google.NewSession(lease.Client().Transport)
		sess.Client.Timeout = lease.Client().Timeout
		_, err := sess.Search(ctx, google.Query{Text: text, Country: "us", Language: "en"})
		return err == nil
	}

	var wg sync.WaitGroup
	for i := range ports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Half the ports repeat through the same address after a failure and
			// half rotate first, so both are measured against the same list at the
			// same moment rather than one after the other.
			rotates := i%2 == 0

			lease, err := pool.Acquire(ctx)
			if err != nil {
				return
			}
			defer lease.Release()

			for round := range rounds {
				if ctx.Err() != nil {
					return
				}
				ok := ask(lease, []string{"golang channels", "weather", "train times", "recipes"}[round])

				mu.Lock()
				switch {
				case round == 0:
					first.tried++
					if ok {
						first.answered++
					}
				case rotates:
					afterRotation.tried++
					if ok {
						afterRotation.answered++
					}
				default:
					repeated.tried++
					if ok {
						repeated.answered++
					}
				}
				mu.Unlock()

				if ok {
					// A port that answered has proved itself; what this test is
					// about is what follows a failure.
					return
				}
				if rotates {
					if err := pool.RotateEgressFor(ctx, lease.Port()); err != nil {
						return
					}
				}
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
		t.Logf("%-38s %2d/%2d answered (%.0f%%)", label, x.answered, x.tried, share)
	}
	show("first request through a port", first)
	show("again, same address", repeated)
	show("again, after rotating the address", afterRotation)
}

var _ = settings.Settings{}
