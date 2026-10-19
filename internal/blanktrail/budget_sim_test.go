// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// simResult is what a simulated run of searchers against a limited profile saw.
type simResult struct {
	peak     int           // the most connections to the proxy at any tick
	longest  time.Duration // the longest stretch above the limit
	over     time.Duration // all the time above the limit
	lastOver time.Duration // when the profile was last above the limit
	served   int           // requests answered
	amp      float64       // k at the end
}

// simulateStand runs threads searchers through a profile with the given limit
// and reserve, against a fake service that opens perRequest connections to the
// proxy for every request on the profile's port, for span of simulated time.
//
// It models the stand's run of 2026-10-19 (task 11): every thread of the job
// sends its next request as soon as its last is answered, so the job keeps one
// request per thread in flight or waiting for a place; the service carries
// each to the proxy on two connections (18-19 client against 37-43 upstream
// there); the registry reads the service on its own schedule (Budgets.step on
// a clock in the test's hand, 2 s fast and 10 s slow, the job starting just
// after a reading had found the profile idle); a request lasts one to three
// seconds. Time moves in ticks of a quarter second; within a tick, first the
// registry takes its reading if one is due (of what the port carried during
// the tick before), then answered requests give their places back and the
// threads send again, and the connections standing after that are what the
// tariff counts for the tick.
//
// Everything is deterministic but for the goroutines that wait in Acquire, and
// those are made deterministic by settling: after every change the test waits
// until each thread is either holding a place or counted as waiting while the
// budget admits no search, so no admission is left to happen behind its back.
func simulateStand(t *testing.T, limit, reserve, threads, perRequest int, span time.Duration) simResult {
	t.Helper()
	const tick = 250 * time.Millisecond
	fake := fakebt.New(t)
	clock := newFakeClock()
	cfg := testPoolConfig(t, fake, clock, 1, 1)
	bs := NewBudgets(BudgetPollSlow, BudgetPollFast)
	bs.now = clock.Now
	b := bs.For(cfg.Client, 1, limit, reserve)
	cfg.Budget = b
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	port := p.portNumbers()[0]
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	type thread struct {
		got  chan *Unit
		unit *Unit
		ends time.Time
		sent int
	}
	ts := make([]thread, threads)
	// One to three seconds, spread so the threads do not move in step.
	lasts := func(i, n int) time.Duration { return time.Duration(4+(i*7+n*3)%9) * tick }
	send := func(i int) {
		ch := make(chan *Unit, 1)
		ts[i].got, ts[i].unit = ch, nil
		go func() {
			if u, err := b.Acquire(ctx, ClassSearch); err == nil {
				ch <- u
			}
		}()
	}
	collect := func() (pending int) {
		for i := range ts {
			th := &ts[i]
			if th.unit != nil {
				continue
			}
			select {
			case u := <-th.got:
				th.unit, th.ends = u, clock.Now().Add(lasts(i, th.sent))
				th.sent++
			default:
				pending++
			}
		}
		return pending
	}
	settle := func() {
		spinUntil(t, "every thread to hold a place or settle to wait", func() bool {
			pending := collect()
			b.mu.Lock()
			w, admits := b.searchWaiting, b.admitsLocked(ClassSearch, false)
			b.mu.Unlock()
			return w == pending && (w == 0 || !admits)
		})
	}

	// The reading just before the job: nothing on the port.
	bs.pollOnce(ctx)
	for i := range ts {
		send(i)
	}
	settle()
	var res simResult
	var run time.Duration
	for at := time.Duration(0); at < span; at += tick {
		n := b.Reading().InFlight
		fake.SetConns(port, n, perRequest*n)
		bs.step(ctx)
		settle()
		for i := range ts {
			if th := &ts[i]; th.unit != nil && !clock.Now().Before(th.ends) {
				th.unit.Release()
				res.served++
				send(i)
			}
		}
		settle()
		up := perRequest * b.Reading().InFlight
		res.peak = max(res.peak, up)
		if up > limit {
			run += tick
			res.over += tick
			res.lastOver = at
			res.longest = max(res.longest, run)
		} else {
			run = 0
		}
		clock.Advance(tick)
	}
	res.amp = b.Reading().Amplification
	cancel()
	for i := range ts {
		ts[i].unit.Release()
	}
	return res
}

// TestBudget_HoldsAServiceThatOpensTwoConnectionsPerRequestToItsLimit is the
// stand's failing run (limit 30, reserve 5, twenty threads, the service at two
// connections per request; 43 at the peak and up to 43 s above the limit) in
// the test's hand.
//
// What can and cannot be held: until the service has been read once with the
// job's requests on its ports nothing is known of the amplification, the
// twenty requests counted as twenty are forty, and the parser cannot take back
// a request it has sent. So the job's first moments are over the limit, for at
// most one fast poll period (the first reading, which a profile whose k has not
// been learned asks for at the fast rate) plus the time the requests already
// sent take to be answered — three seconds at the longest here. That reading teaches k; from
// then on the prediction holds the threads to fifteen requests, thirty
// connections, and the profile is never over again. The searches keep going
// throughout at the pace the limit allows: the limit costs a quarter of the
// threads, and a reading that agrees with the prediction costs nothing.
func TestBudget_HoldsAServiceThatOpensTwoConnectionsPerRequestToItsLimit(t *testing.T) {
	const longestRequest = 3 * time.Second
	res := simulateStand(t, 30, 5, 20, 2, 2*time.Minute)
	t.Logf("peak %d, longest over %v, over %v in all, last over at %v, %d served, k %.3f",
		res.peak, res.longest, res.over, res.lastOver, res.served, res.amp)
	if res.longest > BudgetPollFast+longestRequest {
		t.Errorf("the profile was over its limit for %v at a stretch, want at most a fast poll period and the longest request (%v)",
			res.longest, BudgetPollFast+longestRequest)
	}
	if res.over != res.longest {
		t.Errorf("the profile was over its limit for %v in all and %v at the longest stretch, want one stretch only, at the start", res.over, res.longest)
	}
	if res.lastOver >= BudgetPollFast+longestRequest {
		t.Errorf("the profile was over its limit %v into the job, want never once the first reading taught the amplification", res.lastOver)
	}
	if res.amp < 1.99 || res.amp > 2.01 {
		t.Errorf("the budget learned an amplification of %.3f from a service that opens two connections per request, want 2", res.amp)
	}
	// Fifteen requests at once (thirty connections at two each) of two
	// seconds on average for two minutes is an ideal of about 900. Measured
	// (2026-10-19): 911 with the excess carried between readings (R5) — the
	// twenty of the first two seconds make up for the waits — against 658
	// when the raw reading was carried and every reading near the limit
	// stopped the searches until the next one (R4). Far fewer would be a
	// budget that holds the job rather than its connections.
	if res.served < 870 {
		t.Errorf("%d requests were answered in two minutes, want at least 870 of an ideal of about 900", res.served)
	}
}
