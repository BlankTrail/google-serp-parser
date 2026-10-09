// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
)

func TestRunner_ReadsHiddenAddressesAgainAfterTheServiceRestarts(t *testing.T) {
	// The position check of 2026-10-09 on the English demo: the service was
	// restarted under it and every port it held was gone. The searching ports
	// opened again; the lookup ports never did, and all 150 threads stood for
	// good in a queue for a lookup port, the job "running" with nothing moving.
	// A lookup port the service no longer holds is a port to open again, not an
	// address that failed: the lookups after the restart have to be read.
	c := newCrowded(t, time.Millisecond)
	searching := poolFacing(t, c.addr(), 1)
	reading := poolFacing(t, c.addr(), 2, func(cfg *blanktrail.PoolConfig) {
		// What the lookup set is opened with, less the two minutes a
		// quarantined port waits: a test does not sit through them.
		cfg.WaitForIdentity = true
		cfg.MaxRetriesPerReq = 0
		cfg.RotateAfterFailures = 0
		cfg.MaxPortStrikes = 0
		cfg.MaxRevivals = 0
		cfg.ReviveAfter = time.Millisecond
	})
	r := &Runner{Pool: searching.Pool, Threads: 1,
		Addresses: func(context.Context) (*blanktrail.Pool, error) { return reading.Pool, nil }}
	before := eightHidden(c.URL)
	if got := r.ResolveLinks(t.Context(), &before, 4); got.Resolved != 8 {
		t.Fatalf("before the restart: resolved %d of 8: %v", got.Resolved, got.Errs)
	}

	reading.restart()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	after := eightHidden(c.URL)
	got := r.ResolveLinks(ctx, &after, 4)
	if ctx.Err() != nil {
		t.Fatalf("the lookups after the restart were still waiting for a port after 20 s (resolved %d of 8)", got.Resolved)
	}
	if got.Resolved != 8 {
		t.Errorf("after the restart: resolved %d of 8: %v", got.Resolved, got.Errs)
	}
	if open := len(reading.Fake.OpenPorts()); open == 0 {
		t.Error("the service holds no lookup port after the restart")
	}
}
