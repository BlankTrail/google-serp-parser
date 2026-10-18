// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"math"
	"sync"
)

// Class is what a place in the budget is wanted for.
type Class int

const (
	// ClassSearch is a search request, a result page the job is after.
	ClassSearch Class = iota
	// ClassLookup is everything that is not the job's own search: the
	// suggestions and the /goto lookups. It gives way to searches and to solves.
	ClassLookup
)

// Budget is how many connections to the proxy a profile may have open at once,
// shared by every pool of the profile in this process. It exists because a
// measured job (twenty threads, up to a hundred and twenty connections to one
// proxy at the peak) went over a tariff that counts connections, and the
// service then cut connections, solves in progress among them. A solve is
// what a search cannot go on without, so it keeps its room (the reserve) for as
// long as it lasts, searches come before lookups, and a lookup goes only while
// a solve met right now would still fit beside it.
//
// Two counts decide. This program's own is free and always at hand; the
// service's (read now and then from its listing) also sees what the service
// opened on its own: retries, kept connections, the solver's own. "In use" is
// the larger of the two, and both classes are admitted by it.
//
// The own count is a prediction of the service's, not a count of requests. The
// first version counted one connection per attempt and let searches go by that
// alone, on the grounds that the service's reading is seconds old; measured on
// the stand (2026-10-19: limit 30, reserve 5, twenty threads) the service
// opened about two connections to the proxy for every request it was sent,
// twenty searches never reached the twenty-five places they were counted
// against, and the profile sat at 37-43 connections for up to 43 seconds at a
// time while the reading — which only held the lookups — said so every two
// seconds. So the budget learns from each reading how many connections the
// service opens per request on this profile's ports (the amplification, k),
// counts every attempt as k, and holds searches as well as lookups on the
// larger of that prediction and the last reading. Between two readings the
// prediction keeps the count inside the limit; a reading that shows more than
// was predicted holds everything new until a later one shows room.
//
// A limit of nought is how every profile ran before there was a limit: nothing
// waits and nothing is counted.
type Budget struct {
	mu       sync.Mutex
	limit    int
	reserve  int
	inFlight int
	solves   int
	observed int
	known    bool
	// amp is k, the connections to the proxy the service opens for each
	// connection this program makes to a port of the profile, learned from
	// the readings (see learnLocked); 1 until a reading teaches otherwise.
	amp float64
	// learned is whether amp has had a sample yet; the first is taken as it
	// is (see learnLocked).
	learned bool
	// sampled is whether a reading has been taken while this profile had
	// attempts in flight, which is when amp can first be learned. Until then
	// the registry reads the service at the fast rate (see Budgets.step).
	sampled bool
	// searchWaiting is how many searches wait for room; while any does, no
	// lookup is let in. lookupWaiting only tells the tests when a waiter has
	// settled to wait.
	searchWaiting int
	lookupWaiting int
	solving       map[int]int
	holders       map[budgetHolder]struct{}
	// changed is closed and replaced whenever anything a waiter decides on
	// moves, so every waiter looks again; a sync.Cond cannot be left on a
	// cancelled context.
	changed chan struct{}
}

// budgetHolder is a pool as the budget sees it: the ports it holds, and a way
// to tell it that the service counts more than the limit allows.
type budgetHolder interface {
	portNumbers() []int
	overLimit()
}

// NewBudget makes a budget of limit connections, reserve of which a solve in
// progress holds. A limit of nought or less is no limit.
func NewBudget(limit, reserve int) *Budget {
	b := &Budget{amp: ampFloor, solving: map[int]int{}, holders: map[budgetHolder]struct{}{}, changed: make(chan struct{})}
	b.Set(limit, reserve)
	return b
}

// The amplification's learning.
//
// A reading's sample is the profile's connections to the proxy over its
// connections to the ports. It is smoothed (an exponentially weighted mean,
// each reading weighing ampAlpha) because one reading is one instant: a solve
// caught in it has the solver's three to five connections on a single request,
// a reading taken as a burst of requests arrives has them counted before the
// service has dialled out for them. Three tenths lets a change in the service's
// ways be followed within half a dozen readings — twelve seconds at the fast
// rate — while one odd reading moves k by less than a third of its oddness.
//
// The first sample is not smoothed but taken as it is. The 1 that k starts at
// is a guess, not a reading, and mixed in at seven tenths it would take those
// half-dozen readings to wash out: a simulation of the stand (two connections
// per request, limit 30, twenty threads; budget_sim_test.go) went over the
// limit again and again for the first fourteen seconds that way, and only once,
// in the stretch before the first reading, with the first sample taken whole.
//
// The floor is one: the service cannot carry a request to the proxy on less than
// one connection, and a sample below one (connections to the port kept alive
// between requests, with nothing upstream) would let the budget count less than
// it sends. The ceiling is four: past it the reading is a solve or a burst and
// not the service's way with requests, and a k that high already holds a limit
// of thirty to seven searches. Each sample is clamped before it is mixed in, so
// one later reading of forty connections on one request moves k as far as a
// reading of four would, not to the ceiling at once.
const (
	ampAlpha   = 0.3
	ampFloor   = 1.0
	ampCeiling = 4.0
	// budgetSlack absorbs the rounding of k times a count, so a sum that is the
	// limit in exact arithmetic is not refused for being a hair above it.
	budgetSlack = 1e-9
)

// Set changes the limit and the reserve of a budget in use, and lets the
// waiting look again: a raised limit is room they are waiting for.
func (b *Budget) Set(limit, reserve int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.limit, b.reserve = max(limit, 0), max(reserve, 0)
	if b.limit == 0 {
		// With no limit the service is not read, so what was learned from it
		// would only grow stale; a limit set again starts from scratch.
		b.observed, b.known = 0, false
		b.amp, b.learned, b.sampled = ampFloor, false, false
	}
	b.wakeLocked()
	b.mu.Unlock()
}

func (b *Budget) wakeLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// computedLocked is this program's own count, as a prediction of the
// service's: k connections for every attempt in flight, and the reserve for
// every solve.
//
// The count is kept in fractions rather than rounding each attempt up to a
// whole number of connections. Rounded per attempt, a k of 1.1 would count
// every request as two and halve the searches for a tenth more connections;
// in fractions twenty attempts at 1.1 are twenty-two, which is what the
// service will open. Whole numbers are what is shown (BudgetReading.Computed
// rounds the sum up, once).
//
// A solve counts the reserve and not the reserve times k: the reserve is the
// user's own figure for what a solve opens to the proxy (the solver's three to
// five connections, measured), already counted in the service's terms. That
// is also why a reading taken during a solve can push a sample high (the
// solver's connections over one request) and why samples are clamped.
func (b *Budget) computedLocked() float64 {
	return b.amp*float64(b.inFlight) + float64(b.reserve*b.solves)
}

// inUseLocked is the prediction corrected by the service's last reading.
func (b *Budget) inUseLocked() float64 {
	return max(b.computedLocked(), float64(b.observed))
}

// admitsLocked is whether one more attempt of the class fits now: in use, plus
// the k connections the attempt will cost, within the limit — and for a lookup
// with the room for one more solve left over, and no search waiting.
//
// A search answers to the service's reading as well. That reading can be up to
// two seconds old at the rate a busy profile is read, and a search held on it
// may stand still until the next one shows room; but a search let in past it
// is a connection the tariff counts and the service may cut, a solve's among
// them, and the stand showed the own count alone letting a profile run a third
// over its limit for most of a minute.
func (b *Budget) admitsLocked(c Class) bool {
	if b.limit <= 0 {
		return true
	}
	need := b.inUseLocked() + b.amp
	if c == ClassLookup {
		return b.searchWaiting == 0 && need+float64(b.reserve) <= float64(b.limit)+budgetSlack
	}
	return need <= float64(b.limit)+budgetSlack
}

// Acquire waits for a place of the given class and never refuses one: it
// returns only when it has the place, or when ctx is done (then the error is
// ctx's and nothing is held). Without a budget, or with a limit of nought, it
// returns at once with no unit, and a nil unit is safe to use.
func (b *Budget) Acquire(ctx context.Context, c Class) (*Unit, error) {
	if b == nil {
		return nil, nil
	}
	b.mu.Lock()
	if b.limit <= 0 {
		b.mu.Unlock()
		return nil, nil
	}
	waiting := false
	for !b.admitsLocked(c) {
		if !waiting {
			waiting = true
			b.countWaiterLocked(c, 1)
		}
		ch := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			b.mu.Lock()
			b.countWaiterLocked(c, -1)
			// A search that stops waiting may be the last thing the lookups
			// were standing behind.
			b.wakeLocked()
			b.mu.Unlock()
			return nil, ctx.Err()
		case <-ch:
		}
		b.mu.Lock()
	}
	if waiting {
		b.countWaiterLocked(c, -1)
	}
	b.inFlight++
	b.wakeLocked()
	b.mu.Unlock()
	return &Unit{b: b}, nil
}

func (b *Budget) countWaiterLocked(c Class, d int) {
	if c == ClassSearch {
		b.searchWaiting += d
	} else {
		b.lookupWaiting += d
	}
}

// Unit is one place held in a budget. Release gives it back, once.
type Unit struct {
	b        *Budget
	port     int
	solving  bool
	released bool
}

// Solving turns the place into a solve on the given port: from now on it holds
// the reserve instead of one connection, until it is released. The service
// says when a challenge is being solved; the budget only counts it.
func (u *Unit) Solving(port int) {
	if u == nil {
		return
	}
	b := u.b
	b.mu.Lock()
	if !u.released && !u.solving {
		u.solving, u.port = true, port
		b.inFlight--
		b.solves++
		b.solving[port]++
		// With a reserve below k a solve holds less than the attempt it
		// replaces, and that is room for the waiting.
		b.wakeLocked()
	}
	b.mu.Unlock()
}

// Release gives the place back. Calling it again does nothing.
func (u *Unit) Release() {
	if u == nil {
		return
	}
	b := u.b
	b.mu.Lock()
	if !u.released {
		u.released = true
		if u.solving {
			b.solves--
			if b.solving[u.port]--; b.solving[u.port] <= 0 {
				delete(b.solving, u.port)
			}
		} else {
			b.inFlight--
		}
		b.wakeLocked()
	}
	b.mu.Unlock()
}

// waitsOnASolve reports whether the unit is a solve in progress.
func (u *Unit) waitsOnASolve() bool {
	if u == nil {
		return false
	}
	u.b.mu.Lock()
	defer u.b.mu.Unlock()
	return u.solving && !u.released
}

// Free is how many more places of the class may go now, by the same measure
// admission uses; with no limit there is no end.
func (b *Budget) Free(c Class) int {
	if b == nil {
		return math.MaxInt
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		return math.MaxInt
	}
	room := float64(b.limit) - b.inUseLocked()
	if c == ClassLookup {
		room -= float64(b.reserve)
	}
	// Each place costs k, so the room is shared out k at a time.
	return max(int(math.Floor(room/b.amp+budgetSlack)), 0)
}

// BudgetReading is a budget's figures at one moment.
type BudgetReading struct {
	Limit, Reserve, InFlight, Solves, Observed int
	// Known is whether the service's count has been read.
	Known bool
	// Amplification is k: the connections to the proxy the service opens for
	// each connection to a port of the profile, as learned from the readings;
	// 1 until one has taught otherwise. Every attempt in flight counts k.
	Amplification float64
	// Sampled is whether a reading has been taken while the profile had
	// attempts in flight, the first chance to learn the amplification.
	Sampled bool
}

// Computed is this program's own count, as a prediction of the service's:
// k connections for every attempt in flight, rounded up once, plus the reserve
// of each solve. An amplification below one (a reading put together by hand)
// counts as one.
func (r BudgetReading) Computed() int {
	k := max(r.Amplification, ampFloor)
	return int(math.Ceil(k*float64(r.InFlight)-budgetSlack)) + r.Reserve*r.Solves
}

// InUse is the larger of this program's count and the service's.
func (r BudgetReading) InUse() int { return max(r.Computed(), r.Observed) }

// Reading copies the budget's figures; a nil budget reads as zeros.
func (b *Budget) Reading() BudgetReading {
	if b == nil {
		return BudgetReading{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return BudgetReading{
		Limit: b.limit, Reserve: b.reserve, InFlight: b.inFlight,
		Solves: b.solves, Observed: b.observed, Known: b.known,
		Amplification: b.amp, Sampled: b.sampled,
	}
}

// join and leave tell the budget which pools draw on it.
func (b *Budget) join(h budgetHolder) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.holders[h] = struct{}{}
	b.mu.Unlock()
}

func (b *Budget) leave(h budgetHolder) {
	if b == nil {
		return
	}
	b.mu.Lock()
	delete(b.holders, h)
	b.mu.Unlock()
}

func (b *Budget) holdersLocked() []budgetHolder {
	out := make([]budgetHolder, 0, len(b.holders))
	for h := range b.holders {
		out = append(out, h)
	}
	return out
}

// ports lists the ports of every pool in the budget. The pools are asked after
// the budget's lock is let go: a pool takes its own lock to answer, and the
// order of locks is the budgets' first, then this one, never a pool's under it.
func (b *Budget) ports() []int {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	hs := b.holdersLocked()
	b.mu.Unlock()
	var out []int
	for _, h := range hs {
		out = append(out, h.portNumbers()...)
	}
	return out
}

// solvingOn reports whether a solve in progress is on the port.
func (b *Budget) solvingOn(port int) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.solving[port] > 0
}

// observe records the service's count of connections to the proxy alone, with
// nothing to learn the amplification from. See observeLoad.
func (b *Budget) observe(n int, known bool) (over bool, holders []budgetHolder) {
	return b.observeLoad(n, 0, known)
}

// observeLoad records a reading of the profile's ports — upstream connections
// to the proxy, client connections from this program — learns the
// amplification from it, and lets the waiting look again. over is whether the
// count is above the limit; then holders are the pools to tell.
//
// Every reading wakes the waiting, whatever it says: a lower count is room, and
// so is a lower k, which shrinks what every attempt in flight is counted as. A
// higher one needs no wake, but telling the two apart buys nothing — a waiter
// woken for nothing looks once and sleeps again.
func (b *Budget) observeLoad(upstream, client int, known bool) (over bool, holders []budgetHolder) {
	if b == nil {
		return false, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// A reading that lands after the limit was taken off (between a caller
	// collecting the limited budgets and observing) is not stored: Set(0)
	// cleared the count, and an old number would come back with it.
	if b.limit <= 0 {
		return false, nil
	}
	b.observed, b.known = upstream, known
	if known && (client > 0 || b.inFlight > 0) {
		b.sampled = true
	}
	if known && client > 0 {
		b.learnLocked(float64(upstream) / float64(client))
	}
	b.wakeLocked()
	over = b.limit > 0 && known && upstream > b.limit
	if over {
		holders = b.holdersLocked()
	}
	return over, holders
}

// learnLocked mixes one reading's sample of the amplification into k, or
// takes it as k if it is the first (see ampAlpha). Samples are clamped to
// [ampFloor, ampCeiling], which keeps k there too: it starts at the floor and
// is only ever a mean of clamped samples. A
// reading that failed, or one with no connection from this program on the
// profile's ports (nothing to divide by: the upstream count then is the
// service's leftovers, not its way with requests), is never a sample; the
// caller leaves k as it was.
func (b *Budget) learnLocked(sample float64) {
	sample = min(max(sample, ampFloor), ampCeiling)
	if !b.learned {
		b.amp, b.learned = sample, true
		return
	}
	b.amp += ampAlpha * (sample - b.amp)
}
