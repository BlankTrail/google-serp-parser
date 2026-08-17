//go:build live

// SPDX-License-Identifier: MIT

package blanktrail_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
)

// TestLiveRaise_SaysHowLongAPoolTakesToComeUp measures the gap an operator
// notices between pressing start and the first query going out.
//
// The queue itself has no delay in it: a job queued wakes the worker at once.
// Everything between that and the first request is this — the check, and then
// every port opened one at a time.
//
// Run it deliberately:
//
//	BLANKTRAIL_API_KEY=... BLANKTRAIL_URL=... go test -tags live ./blanktrail/ -run TestLiveRaise -v
func TestLiveRaise_SaysHowLongAPoolTakesToComeUp(t *testing.T) {
	key, control := os.Getenv("BLANKTRAIL_API_KEY"), os.Getenv("BLANKTRAIL_URL")
	if key == "" || control == "" {
		t.Skip("BLANKTRAIL_API_KEY and BLANKTRAIL_URL are not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		t.Fatalf("control client: %v", err)
	}

	// The check first, on its own, because it runs before every raise and its
	// cost is somebody's waiting time as much as the ports are.
	started := time.Now()
	report := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{Ports: 1})
	t.Logf("MEASUREMENT check: %v", time.Since(started).Round(time.Millisecond))
	if !report.OK() {
		for _, f := range report.Findings {
			t.Logf("[%s] %s — %s", f.Severity, f.Title, f.Detail)
		}
		t.Fatal("preflight failed; fix the findings above")
	}

	// Sizes an operator actually types. The last is the one from the report that
	// prompted this: ten threads of ten ports is a hundred ports, opened one at a
	// time before a single query goes out.
	for _, size := range []struct{ threads, ports int }{{1, 1}, {2, 3}, {4, 5}, {10, 10}} {
		started := time.Now()
		pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
			Client: client, Threads: size.threads, PortsPerThread: size.ports,
			Spec: blanktrail.DefaultPortSpec(), CA: report.CA,
		})
		took := time.Since(started)
		if err != nil {
			t.Errorf("MEASUREMENT raise %dx%d: failed after %v: %v",
				size.threads, size.ports, took.Round(time.Millisecond), err)
			continue
		}
		opened := size.threads * size.ports
		t.Logf("MEASUREMENT raise %dx%d = %d ports: %v, which is %v a port",
			size.threads, size.ports, opened, took.Round(time.Millisecond),
			(took / time.Duration(opened)).Round(time.Millisecond))
		if err := pool.Close(); err != nil {
			t.Errorf("closing the pool of %d: %v", opened, err)
		}
	}
}

// TestLiveRotor_SaysHowLongAListTakesToLoad measures the other thing that
// stands between pressing start and the first query: the addresses.
//
// It is loaded once when the pool is raised, and a list of fifteen thousand is
// an ordinary size for one.
func TestLiveRotor_SaysHowLongAListTakesToLoad(t *testing.T) {
	at := os.Getenv("GSERP_PROXY_LIST")
	if at == "" {
		t.Skip("GSERP_PROXY_LIST is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	started := time.Now()
	rotor, err := blanktrail.NewRotor(ctx, blanktrail.Source{
		Kind: "url", Location: at, DefaultScheme: "socks5",
	})
	took := time.Since(started)
	if err != nil {
		t.Fatalf("MEASUREMENT list: failed after %v: %v", took.Round(time.Millisecond), err)
	}
	t.Logf("MEASUREMENT list: %d addresses in %v", rotor.Len(), took.Round(time.Millisecond))
}

// TestLiveRaise_SaysHowLongAPoolThroughAListTakes measures the raise an
// operator actually runs.
//
// The measurement above opens ports with no upstream: the proxy dials out
// directly. With a list of addresses each port is opened against one of them,
// and whether the service reaches that address while the port is being opened —
// or later, on the first request through it — is the difference between a job
// that starts at once and one that starts a minute in. It is not written down
// anywhere, so it is measured.
func TestLiveRaise_SaysHowLongAPoolThroughAListTakes(t *testing.T) {
	key, control := os.Getenv("BLANKTRAIL_API_KEY"), os.Getenv("BLANKTRAIL_URL")
	list := os.Getenv("GSERP_PROXY_LIST")
	if key == "" || control == "" || list == "" {
		t.Skip("BLANKTRAIL_API_KEY, BLANKTRAIL_URL and GSERP_PROXY_LIST are not all set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		t.Fatalf("control client: %v", err)
	}
	report := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{Ports: 1})
	if !report.OK() {
		t.Fatal("preflight failed")
	}

	started := time.Now()
	rotor, err := blanktrail.NewRotor(ctx, blanktrail.Source{
		Kind: "url", Location: list, DefaultScheme: "socks5",
	})
	if err != nil {
		t.Fatalf("loading the list: %v", err)
	}
	t.Logf("MEASUREMENT list: %d addresses in %v", rotor.Len(),
		time.Since(started).Round(time.Millisecond))

	for _, size := range []struct{ threads, ports int }{{1, 1}, {4, 5}, {10, 10}} {
		started := time.Now()
		pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
			Client: client, Threads: size.threads, PortsPerThread: size.ports,
			Spec:     blanktrail.DefaultPortSpec(),
			CA:       report.CA,
			Channels: []blanktrail.Channel{blanktrail.NewListChannel("list", rotor)},
		})
		took := time.Since(started)
		if err != nil {
			t.Errorf("MEASUREMENT raise through a list %dx%d: failed after %v: %v",
				size.threads, size.ports, took.Round(time.Millisecond), err)
			continue
		}
		opened := size.threads * size.ports
		t.Logf("MEASUREMENT raise through a list %dx%d = %d ports: %v, which is %v a port",
			size.threads, size.ports, opened, took.Round(time.Millisecond),
			(took / time.Duration(opened)).Round(time.Millisecond))

		// And the first request through it, which is the other half of the wait:
		// if the ports came up quickly, this is where the minute went.
		lease, err := pool.Acquire(ctx)
		if err != nil {
			t.Errorf("leasing a port of %d: %v", opened, err)
			_ = pool.Close()
			continue
		}
		asked := time.Now()
		res, err := lease.Client().Get("https://www.google.com/")
		if err != nil {
			t.Logf("MEASUREMENT first request through %d ports: failed after %v: %v",
				opened, time.Since(asked).Round(time.Millisecond), err)
		} else {
			_ = res.Body.Close()
			t.Logf("MEASUREMENT first request through %d ports: %d in %v",
				opened, res.StatusCode, time.Since(asked).Round(time.Millisecond))
		}
		lease.Release()
		if err := pool.Close(); err != nil {
			t.Errorf("closing the pool of %d: %v", opened, err)
		}
	}
}
