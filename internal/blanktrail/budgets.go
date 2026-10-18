// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// How often the service is asked what the ports of the profiles with a limit
// are carrying, and when the faster of the two applies.
//
// The reading is a correction and not the count: what a profile holds is this
// program's own count of its attempts and solves, which costs nothing. The
// service is asked only for what it opens on its own — retries, connections it
// keeps, the solver's — and the asking is not free: the whole listing
// (/api/v1/ports) is about 283 KB for 251 ports and 9-36 ms on the live
// service (2026-10-18), a megabyte at a thousand. The light listing
// (/api/v1/ports/conns, see Client.PortLoads) is tried first, but a reading is
// still a request to the service and is kept rare all the same. Ten seconds
// while every limited profile counts below seventy per cent of its limit, where
// an extra connection or two cannot reach it; two seconds once any is at or
// above, where it can — and two seconds is how long the spec allows a count
// over the limit to go unseen.
const (
	BudgetPollSlow    = 10 * time.Second
	BudgetPollFast    = 2 * time.Second
	budgetBusyPercent = 70

	// budgetPollTimeout bounds one reading, so a service that hangs cannot
	// hold the one goroutine that asks for longer than a few intervals.
	budgetPollTimeout = 5 * time.Second
)

// Budgets is every profile's budget in this process, and the one reading of
// the service they share. One goroutine (Run) asks for all of them: a request
// per profile would multiply a cost that is already the largest this program
// puts on the service's control API.
type Budgets struct {
	slow, fast time.Duration
	// now is a clock seam for tests.
	now       func() time.Time
	mu        sync.Mutex
	client    *Client
	byProfile map[int64]*Budget
	// polledAt is when the service was last asked; zero is never.
	polledAt time.Time
	// failing is whether the last reading failed, so a service that is down
	// for an hour is logged once and not once per tick.
	failing bool
}

// NewBudgets makes an empty registry that reads the service every slow while
// every limited profile has room, and every fast once any is busy.
func NewBudgets(slow, fast time.Duration) *Budgets {
	return &Budgets{slow: slow, fast: fast, now: time.Now, byProfile: map[int64]*Budget{}}
}

// For is the budget of a profile: the same one for every pool of the profile
// in this process, since the searches, the lookups and the standing set of one
// profile spend one tariff. A limit changed on the screen takes effect from the
// next time any pool of the profile is opened. A nil registry gives no budget.
func (bs *Budgets) For(cl *Client, profile int64, limit, reserve int) *Budget {
	if bs == nil {
		return nil
	}
	bs.mu.Lock()
	defer bs.mu.Unlock()
	b, ok := bs.byProfile[profile]
	if !ok {
		b = NewBudget(limit, reserve)
		bs.byProfile[profile] = b
	} else {
		b.Set(limit, reserve)
	}
	// Every client in the process looks at the same service.
	if cl != nil {
		bs.client = cl
	}
	return b
}

// Run asks the service on schedule until ctx is done. It is the one goroutine
// per process that does, and it returns when ctx is cancelled.
func (bs *Budgets) Run(ctx context.Context) {
	if bs == nil {
		return
	}
	tick := time.NewTicker(bs.fast)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			bs.step(ctx)
		}
	}
}

// step asks the service if a reading is due: once the interval of the current
// rate has passed since the last. Deciding is a look at this process's own
// counts and costs nothing; whether there is any limit to read for is left to
// pollOnce, which then asks nothing.
func (bs *Budgets) step(ctx context.Context) {
	bs.mu.Lock()
	busy := false
	for _, b := range bs.byProfile {
		r := b.Reading()
		if r.Limit > 0 && r.Computed()*100 >= budgetBusyPercent*r.Limit {
			busy = true
		}
	}
	every := bs.slow
	if busy {
		every = bs.fast
	}
	due := bs.polledAt.IsZero() || bs.now().Sub(bs.polledAt) >= every
	bs.mu.Unlock()
	if due {
		bs.pollOnce(ctx)
	}
}

// pollOnce reads the service once for every profile with a limit and hands
// each its own sum. With no limit anywhere it asks nothing.
func (bs *Budgets) pollOnce(ctx context.Context) {
	bs.mu.Lock()
	cl := bs.client
	var limited []*Budget
	for _, b := range bs.byProfile {
		if b.Reading().Limit > 0 {
			limited = append(limited, b)
		}
	}
	if cl != nil && len(limited) > 0 {
		// Stamped before the request so a failed reading counts as an
		// attempt and is not repeated on every tick.
		bs.polledAt = bs.now()
	}
	bs.mu.Unlock()
	if cl == nil || len(limited) == 0 {
		return
	}
	ask, cancel := context.WithTimeout(ctx, budgetPollTimeout)
	loads, err := cl.PortLoads(ask)
	cancel()
	if ctx.Err() != nil {
		return
	}
	bs.noteOutcome(err)
	for _, b := range limited {
		sum := 0
		if err == nil {
			for _, port := range b.ports() {
				sum += loads[port].Upstream
			}
		}
		// A count that cannot be read is not kept: nought and unknown, so a
		// number above the limit read before a restart of the service does
		// not hold the profile's lookups for good.
		over, holders := b.observe(sum, err == nil)
		if over {
			for _, h := range holders {
				h.overLimit()
			}
		}
	}
}

// noteOutcome logs the first failure of a streak and the recovery after it.
// The error itself is left out: it can carry the service's address.
func (bs *Budgets) noteOutcome(err error) {
	bs.mu.Lock()
	was := bs.failing
	bs.failing = err != nil
	bs.mu.Unlock()
	switch {
	case err != nil && !was:
		slog.Warn("blanktrail: the connection counts of the service cannot be read; limits go by this program's own count")
	case err == nil && was:
		slog.Info("blanktrail: the connection counts of the service are readable again")
	}
}
