//go:build live

// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

// envPPTThreads, envPPTPorts and envPPTMinutes set the shape of the comparison.
const (
	envPPTThreads = "GSERP_LIVE_PPT_THREADS"
	envPPTPorts   = "GSERP_LIVE_PPT_PORTS"
	envPPTMinutes = "GSERP_LIVE_PPT_MINUTES"
)

// TestPortsPerThread_LiveWhetherASpareIdentityPaysForItself puts the two
// settings side by side and lets them run.
//
// The argument for more than one port per thread is now measured: an identity
// asked every five seconds answered around forty requests before it was
// challenged, where one asked every two answered twelve. A thread with three
// identities keeps that five-second gap on each of them while still asking
// something every second or two — so after the warming is paid for, three ports
// a thread should out-run one.
//
// It does not. That is the whole reason for this test: the two arms run at the
// same minute, on the same list, with the same number of threads, and every
// number that could explain the difference is reported beside the throughput —
// what the threads waited for, how long a request took, and how many of them
// were slow enough to be a challenge rather than an answer.
//
// Nothing is asserted. It reports, and the numbers say where to look.
func TestPortsPerThread_LiveWhetherASpareIdentityPaysForItself(t *testing.T) {
	db := os.Getenv(envLiveDB)
	if db == "" {
		t.Skipf("%s is not set", envLiveDB)
	}
	o := serveOptions{DB: db}
	saved, ok := o.saved(io.Discard)
	if !ok || saved.APIKey == "" {
		t.Skipf("no connection is saved beside the history %s names", envLiveDB)
	}

	threads := numberFrom(t, envPPTThreads, 5)
	spare := numberFrom(t, envPPTPorts, 3)
	minutes := numberFrom(t, envPPTMinutes, 12)
	run := time.Duration(minutes) * time.Minute

	ctx, cancel := context.WithTimeout(t.Context(), run+25*time.Minute)
	defer cancel()

	t.Logf("%d threads, %v each arm: one port a thread against %d, at the same minute",
		threads, run, spare)

	type reading struct {
		ports     int
		answered  int
		slow      int
		failed    int
		firstHalf int
		lastHalf  int
		waited    time.Duration
		spent     time.Duration
	}
	readings := make([]reading, 2)
	var arms sync.WaitGroup
	var say sync.Mutex

	for i, perThread := range []int{1, spare} {
		arms.Add(1)
		go func(i, perThread int) {
			defer arms.Done()
			pool, err := o.dial(ctx, saved, threads, perThread, blanktrail.DeviceDesktop, 0)
			if err != nil {
				say.Lock()
				t.Errorf("%d ports a thread: opening the identities: %v", perThread, o.clean(err.Error()))
				say.Unlock()
				return
			}
			defer func() { _ = pool.Close() }()

			r := reading{ports: perThread}
			began := time.Now()
			half := began.Add(run / 2)
			deadline := began.Add(run)

			var work sync.WaitGroup
			var count sync.Mutex
			for w := 0; w < threads; w++ {
				work.Add(1)
				go func(w int) {
					defer work.Done()
					for n := 0; time.Now().Before(deadline) && ctx.Err() == nil; n++ {
						asked := time.Now()
						lease, err := pool.Acquire(ctx)
						if err != nil {
							return
						}
						waited := time.Since(asked)
						sess := google.NewSession(lease.Client().Transport)
						sess.Client.Timeout = lease.Client().Timeout
						started := time.Now()
						_, err = sess.Search(ctx, google.Query{
							Text: phraseFor(n*threads + w), Country: "us", Language: "en"})
						took := time.Since(started)
						if err == nil {
							lease.Answered()
						} else {
							_ = lease.Reject(ctx)
						}
						lease.Release()

						count.Lock()
						r.waited += waited
						r.spent += took
						switch {
						case err != nil:
							r.failed++
						case took >= slowEnoughToBeAChallenge:
							r.slow++
						default:
							r.answered++
							if started.Before(half) {
								r.firstHalf++
							} else {
								r.lastHalf++
							}
						}
						count.Unlock()
					}
				}(w)
			}
			work.Wait()

			count.Lock()
			readings[i] = r
			count.Unlock()
		}(i, perThread)
	}
	arms.Wait()

	t.Log("")
	t.Log("ports  answered  a minute  first half  last half  slow  failed  mean wait  mean request")
	for _, r := range readings {
		if r.ports == 0 {
			continue
		}
		tries := r.answered + r.slow + r.failed
		perMinute := float64(r.answered) / run.Minutes()
		var wait, spent time.Duration
		if tries > 0 {
			wait = r.waited / time.Duration(tries)
			spent = r.spent / time.Duration(tries)
		}
		t.Logf("%-6d %-9d %-9.1f %-11d %-10d %-5d %-7d %-10v %v",
			r.ports, r.answered, perMinute, r.firstHalf, r.lastHalf, r.slow, r.failed,
			wait.Round(time.Millisecond), spent.Round(time.Millisecond))
	}
	t.Log("")
	t.Log("The last half is the one to read: the first pays for the cold identities.")
	t.Log("If the spare identities out-run one port a thread there and not overall,")
	t.Log("the cost is the warming. If they do not out-run it even there, something")
	t.Log("in the way they are handed out is wrong, and the wait and the request")
	t.Log("time say which.")
}

// numberFrom reads a whole number from the environment, or answers the default.
func numberFrom(t *testing.T, name string, fallback int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		t.Fatalf("%s: %q is not a number", name, raw)
	}
	return n
}
