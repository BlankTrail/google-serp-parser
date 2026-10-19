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
// program's own count of its attempts and solves, which costs nothing, and
// which each reading makes a better prediction of the service's by teaching it
// how many connections the service opens per request (Budget's amplification).
// The service is asked for what it opens on its own — retries, connections it
// keeps, the solver's — and what the asking costs depends on which listing
// answers.
//
// The light listing (/api/v1/ports/conns, see Client.PortLoads) is about 12.5 KB
// and 0.03 ms for 250 ports, so reading it every two seconds is cheap enough
// to do for as long as the profile carries any traffic: the stand re-measure
// (2026-10-18) showed that waiting for seventy per cent of the limit is too
// late, because the service counted 31-37 upstream connections while this
// program had 3-8 requests open, far below that threshold, the reading came
// every ten seconds, and the service's excess was seen runs of 11-16 s after
// it began. So on the light listing a profile is read at the fast rate
// whenever it has attempts in flight, or the last good reading found the
// service carrying anything on its ports (upstream connections or an excess
// over the prediction) or solving on them, which is the connections that
// outlive the requests; and at the slow rate only when it is idle in both
// senses.
//
// The whole listing (/api/v1/ports) is about 283 KB for 251 ports and 9-36 ms
// on the live service (2026-10-18), a megabyte at a thousand, and it is what a
// service from before the light one is read by. There the old rule stands: ten
// seconds while every limited profile counts below seventy per cent of its
// limit, where an extra connection or two cannot reach it; two seconds once any
// is at or above, where it can — and two seconds is how long the spec allows a
// count over the limit to go unseen.
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
	} else if r := b.Reading(); r.Limit != max(limit, 0) || r.Reserve != max(reserve, 0) {
		// Only a change is set: Set wakes every waiter, and opening another
		// pool of the profile is no reason to.
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
	// Which listing is read decides the rule, and it can change once, from
	// the light to the whole, when the first reading meets a 404. A registry
	// that has no client yet asks nothing, so it is judged by the stricter
	// (whole listing) rule that costs nothing to apply.
	light := bs.client != nil && bs.client.LightLoads()
	for _, b := range bs.byProfile {
		r := b.Reading()
		if r.Limit <= 0 {
			continue
		}
		if light {
			// The light listing is cheap enough to follow the traffic: fast
			// while the profile has anything in flight, and for as long as
			// the last good reading showed the service holding connections
			// on its ports or more than the prediction. The second is what
			// the 70% rule missed on the stand: connections that outlive
			// their requests and are counted by the service alone, and a
			// profile that has gone idle but still holds them is the one
			// whose count has yet to be seen to fall. So is a profile on whose
			// ports the service was solving at the last reading: each such
			// solve holds the reserve until a reading says it is over, and a
			// solve the ladder gave up on ends with nothing in this program to
			// see it end — read slowly, its reserve would be held for up to
			// ten seconds after the service let it go. (A solve holds
			// connections, so the reading's upstream is mostly above nought
			// too; the solve is asked about on its own because the two need
			// not land in the same instant.) A reading that failed stored
			// nought and unknown, so it keeps nothing fast. The attempts for
			// fresh sessions are attempts in flight like any (Budget): at the
			// start of a job they are all there is.
			if r.InFlight > 0 || r.Fresh > 0 || r.Observed > 0 || r.Excess > 0 || r.ServiceSolves > 0 {
				busy = true
			}
			continue
		}
		// Busy by what admission goes by: this program's count — k
		// connections per attempt, the prediction of the service's — plus the
		// excess the last reading found over it. A service that opened many
		// connections on its own is the case the fast rate is for, and the
		// prediction alone cannot see it.
		if r.InUse()*100 >= budgetBusyPercent*r.Limit {
			busy = true
		}
		// Busy, too, while a profile has attempts in flight and k has not yet
		// learned from ampWarmup readings: until then the prediction can be
		// short by the whole amplification (two on the stand: twenty attempts,
		// read as 67% of thirty and so slow, were forty connections), and ten
		// seconds of that is ten seconds over the limit. One reading is not
		// enough to end it: taken as the requests arrive, before the service
		// has dialled out for them, it would teach the 1 the budget starts at.
		// A service too old to report client connections never teaches k, and
		// a profile on it is read fast whenever it has attempts in flight —
		// the fresh sessions' among them, which are all a job starts with.
		if r.Samples < ampWarmup && (r.InFlight > 0 || r.Fresh > 0) {
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
// each its own sum: the connections, the solves the service is running, and
// the ports those solves are on. With no limit anywhere it asks nothing.
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
		var r profileLoad
		if err == nil {
			for _, port := range b.ports() {
				l := loads[port]
				r.upstream += l.Upstream
				r.client += l.Client
				if l.Solving > 0 {
					r.solving += l.Solving
					// The solves with a request still waiting on the port: no
					// more of them than the port has client connections.
					r.clientSolving += min(l.Solving, l.Client)
					// Each budget gets a set of its own, made here: the
					// budget keeps it until the next reading.
					if r.solvingPorts == nil {
						r.solvingPorts = map[int]int{}
					}
					r.solvingPorts[port] += l.Solving
				}
			}
		}
		// A count that cannot be read is not kept: nought and unknown, so a
		// number above the limit read before a restart of the service does
		// not hold the profile's searches and lookups for good. Nor is it
		// learned from: k stays what the last good readings made it. The
		// service's solves go with it, for the same reason.
		// holders is empty unless the count is over the limit.
		_, holders := b.observeProfile(r, err == nil)
		for _, h := range holders {
			h.overLimit()
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
