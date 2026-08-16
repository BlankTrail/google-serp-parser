// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"sync"
)

// ScreenConfig is how a list is checked.
type ScreenConfig struct {
	// Client is the control-API client. Required.
	Client *Client
	// Workers is how many addresses are checked at once. Non-positive means one.
	Workers int
	// Checks are the probes to run, named as TestEgress names them. Empty is
	// passed through to TestEgress, which chooses.
	Checks []string
	// Progress, when set, is called once per address with how many are done and
	// how many there are. It is called from several goroutines at once, so an
	// implementation that touches shared state has to guard it.
	Progress func(done, total int)
}

// ScreenResult is one address and why it was rejected.
type ScreenResult struct {
	Upstream Upstream
	OK       bool
	// Detail is the reason the check gave, or the error that stopped it. It is
	// kept because a user handed a count of failures has nothing to act on,
	// while a user handed the reasons can tell one dead address from a whole
	// range their network cannot reach.
	Detail string
}

// ScreenReport is the outcome of one pass over a list.
type ScreenReport struct {
	// Good are the addresses that answered, in the order they were given. The
	// pool's layout is a pure function of its configuration, which is what makes
	// two runs comparable; returning these in completion order would put a
	// different set in front of it every time and take that away.
	Good []Upstream
	Bad  []ScreenResult
}

// Screen checks every address in a list and sorts it into what works and what
// does not.
//
// A bought list is expected to hold dead addresses, addresses the target
// refuses and addresses that terminate TLS themselves. That is the normal state
// of such a list rather than a fault, so a failure never ends the pass: every
// address is checked and every verdict is reported. Checking one costs about a
// second, while the same address failing an hour into a job costs the job.
//
// Cancelling returns what was already established instead of discarding the
// pass.
func Screen(ctx context.Context, cfg ScreenConfig, ups []Upstream) ScreenReport {
	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	}
	if workers > len(ups) {
		workers = len(ups)
	}

	// Each index is handed to exactly one worker, so each element has exactly
	// one writer and the verdicts need no lock. The slice header never changes.
	verdicts := make([]ScreenResult, len(ups))
	checked := make([]bool, len(ups))

	var mu sync.Mutex
	done := 0

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				res, err := cfg.Client.TestEgress(ctx, Egress{Upstream: ups[i].URL()}, cfg.Checks...)
				verdicts[i] = verdictOf(ups[i], res, err)
				checked[i] = true

				mu.Lock()
				done++
				at := done
				mu.Unlock()
				if cfg.Progress != nil {
					cfg.Progress(at, len(ups))
				}
			}
		}()
	}

sending:
	for i := range ups {
		// A select picks at random among the cases that are ready, so on its own it
		// would keep handing out addresses after a cancellation whenever a worker
		// happens to be waiting. Asking first is what makes cancelling stop the pass
		// rather than leak a few more checks past it.
		if ctx.Err() != nil {
			break sending
		}
		select {
		case <-ctx.Done():
			break sending
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()

	var rep ScreenReport
	for i := range ups {
		switch {
		case !checked[i]:
			// Never reached. Reporting it as bad would condemn an address on no
			// evidence, which is the same mistake as approving one on none.
		case verdicts[i].OK:
			rep.Good = append(rep.Good, ups[i])
		default:
			rep.Bad = append(rep.Bad, verdicts[i])
		}
	}
	return rep
}

// verdictOf reduces one probe to a verdict.
//
// An address passes only when at least one check ran and every check that ran
// said so. A check that was skipped states nothing about the address, and
// treating silence as approval is how a dead address reaches a port.
func verdictOf(u Upstream, res map[string]CheckResult, err error) ScreenResult {
	if err != nil {
		return ScreenResult{Upstream: u, Detail: err.Error()}
	}
	ran := 0
	for name, r := range res {
		if r.Skipped {
			continue
		}
		ran++
		if !r.OK {
			detail := r.Detail
			if detail == "" {
				detail = name + " failed"
			}
			return ScreenResult{Upstream: u, Detail: detail}
		}
	}
	if ran == 0 {
		return ScreenResult{Upstream: u, Detail: "no check ran"}
	}
	return ScreenResult{Upstream: u, OK: true}
}
