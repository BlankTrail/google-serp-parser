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
// the own count plus the excess the last reading found over it, and both
// classes are admitted by it.
//
// The excess and not the raw reading is what is kept, because a reading is a
// snapshot that goes stale while the own count moves on. Kept raw (the first
// fix, R4) a reading of twenty-nine went on counting as twenty-nine for the
// two seconds to the next one, however many requests were answered meanwhile,
// and every search stood still for that time at every reading near the limit:
// the simulation of the stand answered about 660 requests in two minutes of an
// ideal 900. What the reading adds that the own count cannot know is how far
// the service was above the prediction at that instant — connections the
// solver opened, retries, kept connections — and that is the part carried
// forward until the next reading measures it again (ruling R5).
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
// counts every attempt as k, and holds searches as well as lookups on that
// prediction plus the excess. Between two readings the prediction keeps the
// count inside the limit; a reading that shows more than was predicted holds
// that much more room until a later one shows the excess gone.
//
// The solves are counted from both sides too. This program knows of a solve
// from the request that met the challenge, and stops knowing when that request
// gives up; the service goes on solving, with the solver's three to five
// connections to the proxy on a port that no longer has a client. The third
// re-measure on the stand (2026-10-19) found two thirds of the connections over
// the limit on ports with no client, solves the ladder had given up on among
// them. Read as an excess they held the room all the same, but they also went
// into k's sample as connections a request had opened, and they did not keep
// the lookup pools from closing the idle connections of a port the service was
// solving on. So a reading also brings the service's own count of its solves
// and the ports they are on (PortLoad.Solving), and the budget counts the
// larger of its own solves and the service's: in the prediction, in k's
// sample, and in the excess, so a solve nobody waits for any more is held as
// the reserve it is.
//
// A limit of nought is how every profile ran before there was a limit: nothing
// waits and nothing is counted.
type Budget struct {
	mu       sync.Mutex
	limit    int
	reserve  int
	inFlight int
	solves   int
	// observed is the service's last reading as it was read, for the job page
	// and for telling the pools the profile is over; admission does not use
	// it but excess.
	observed int
	known    bool
	// excess is how far the last good reading was above the own count at the
	// moment it was taken, priced with the k that reading taught; nought when
	// the reading agreed with the prediction, failed, or was never taken.
	excess float64
	// amp is k, the connections to the proxy the service opens for each
	// connection this program makes to a port of the profile, learned from
	// the readings (see learnLocked); 1 until a reading teaches otherwise.
	amp float64
	// samples is how many readings amp has learned from. Until two have, the
	// registry reads the service at the fast rate while the profile has
	// attempts in flight (see Budgets.step and ampWarmup).
	samples int
	// svcSolves is how many solves the last good reading found the service
	// running on the profile's ports, and svcSolving the ports they were on;
	// nought and empty when the reading failed, was never taken, or came from a
	// service too old to say. They stay as the reading left them until the
	// next, like the excess: a solve the service runs on its own ends when the
	// service says so, and nothing in this program sees it end.
	svcSolves  int
	svcSolving map[int]bool
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
// connections to the ports, with the solves taken out of both: a solve is one
// connection to a port and, on the proxy side, the solver's own three to five,
// which the budget already counts as the reserve. Left in, a reading with two
// solves among twenty requests would teach a k that counted the solver's
// connections a second time on every request. So the sample is (upstream −
// reserve × solves) / (client − solves), the divisor kept at one or more.
//
// Samples are smoothed (an exponentially weighted mean, each reading weighing
// ampAlpha) because one reading is one instant: a reading taken as a burst of
// requests arrives has them counted before the service has dialled out for
// them, one taken as many end has the service's connections still closing.
// Three tenths lets a change in the service's ways be followed within half a
// dozen readings — twelve seconds at the fast rate — while one odd reading
// moves k by less than a third of its oddness.
//
// The first samples are not smoothed against the 1 that k starts at: that is a
// guess, not a reading, and mixed in at seven tenths it would take those
// half-dozen readings to wash out; a simulation of the stand (two connections
// per request, limit 30, twenty threads; budget_sim_test.go) went over the
// limit again and again for the first fourteen seconds that way. The first
// sample is taken whole and the second makes k the plain mean of the two, so
// one badly timed reading weighs half and not all. A first sample below the
// floor is not taken at all: it is a reading of requests the service has not
// dialled out for yet (client connections and nothing upstream), and taken it
// would teach the k of 1 that the budget already assumes and end the fast
// reading meant to learn the real one (ampWarmup).
//
// The floor is one: the service cannot carry a request to the proxy on less than
// one connection, and a sample below one (connections to the port kept alive
// between requests, with nothing upstream) would let the budget count less than
// it sends. The ceiling is four: past it the reading is a burst and not the
// service's way with requests, and a k that high already holds a limit of
// thirty to seven searches. Each sample is clamped before it is mixed in, so
// one later reading of forty connections on one request moves k as far as a
// reading of four would, not to the ceiling at once.
const (
	ampAlpha   = 0.3
	ampFloor   = 1.0
	ampCeiling = 4.0
	// ampWarmup is how many samples k must have learned from before the
	// registry stops reading a profile with attempts in flight at the fast
	// rate for that reason alone.
	ampWarmup = 2
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
		b.observed, b.known, b.excess = 0, false, 0
		b.amp, b.samples = ampFloor, 0
		b.svcSolves, b.svcSolving = 0, nil
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
// five connections, measured), already counted in the service's terms; for the
// same reason the solves are taken out of a reading's sample (learnLocked).
//
// The solves are the counted ones (solvesLocked): this program's own, or the
// service's at the last reading where it ran more.
func (b *Budget) computedLocked() float64 {
	return b.amp*float64(b.inFlight) + float64(b.reserve*b.solvesLocked())
}

// solvesLocked is the solves the budget counts: the larger of the ones this
// program has seen begin and not end, and the ones the service said at the
// last good reading it was running on the profile's ports.
//
// The larger and not the sum, because the two are mostly the same solves: a
// solve this program waits on is one the service is running, and summed every
// such solve would hold the reserve twice. Where the service runs more, the
// rest are solves whose requests have gone — the ladder gave up on them — and
// the service still holds the solver's connections for them. Where this
// program has more, the service has not yet reported a solve that has just
// begun, or a reading is older than it. Which ports the solves are on is not
// compared: a reading is a snapshot and the own count moves on, and the count
// is what admission needs.
func (b *Budget) solvesLocked() int {
	return max(b.solves, b.svcSolves)
}

// inUseLocked is the prediction plus what the last reading found the service
// carrying beyond it. The prediction moves with every attempt let in or given
// back; the excess stays as the reading left it until the next reading.
func (b *Budget) inUseLocked() float64 {
	return b.computedLocked() + b.excess
}

// admitsLocked is whether one more attempt of the class fits now: in use, plus
// the k connections the attempt will cost, within the limit — and for a lookup
// with the room for one more solve left over, and no search waiting.
//
// A search answers to the service's reading as well, through the excess. A
// search let in past what the service opened on its own is a connection the
// tariff counts and the service may cut, a solve's among them, and the stand
// showed the own count alone letting a profile run a third over its limit for
// most of a minute. Only the excess is held against it, not the reading as it
// was: a reading the prediction agreed with holds nothing once requests it saw
// have been answered.
//
// On an empty budget — nothing in flight, nothing being solved by this
// program — the attempt costs one, not k. k is learned up to four, so a small
// limit the form lets through (seven with the reserve of five, k two) would
// otherwise refuse every lookup with nothing running at all; no reading would
// then ever teach k down again, and the job would stand still with no word of
// why. What the service
// carries beyond the prediction still holds it: those are connections the
// tariff counts, and the service lets them go by itself. So do the solves the
// service runs on its own (svcSolves): they hold their reserve in the
// prediction, but they are not this program's attempts and do not make an
// attempt cost k — they are the service's leftovers like the excess, and
// counted as an attempt's company they would stall a small limit the same way.
func (b *Budget) admitsLocked(c Class) bool {
	if b.limit <= 0 {
		return true
	}
	need := b.inUseLocked() + b.attemptCostLocked()
	if c == ClassLookup {
		return b.searchWaiting == 0 && need+float64(b.reserve) <= float64(b.limit)+budgetSlack
	}
	return need <= float64(b.limit)+budgetSlack
}

// attemptCostLocked is what one more attempt is counted as: k, or one on an
// empty budget (see admitsLocked).
func (b *Budget) attemptCostLocked() float64 {
	if b.inFlight == 0 && b.solves == 0 {
		return 1
	}
	return b.amp
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
	// Each place costs k, so the room is shared out k at a time — but the
	// first on an empty budget costs one (admitsLocked), so a set grown to
	// what is free is not held at nought on an idle profile the first fits.
	free := max(int(math.Floor(room/b.amp+budgetSlack)), 0)
	if free == 0 && room+budgetSlack >= b.attemptCostLocked() {
		free = 1
	}
	return free
}

// BudgetReading is a budget's figures at one moment.
type BudgetReading struct {
	// Solves is the solves counted: this program's own, or the service's at
	// the last reading where it ran more (see Budget.solvesLocked).
	// ServiceSolves is the service's alone, as the last good reading found it.
	Limit, Reserve, InFlight, Solves, ServiceSolves, Observed int
	// Known is whether the service's count has been read.
	Known bool
	// Amplification is k: the connections to the proxy the service opens for
	// each connection to a port of the profile, as learned from the readings;
	// 1 until one has taught otherwise. Every attempt in flight counts k.
	Amplification float64
	// Samples is how many readings the amplification has learned from.
	Samples int
	// Excess is how far the last good reading was above this program's count
	// when it was taken: what the service carries that the prediction does not
	// see, held against admission until the next reading.
	Excess float64
}

// Computed is this program's own count, as a prediction of the service's:
// k connections for every attempt in flight, rounded up once, plus the reserve
// of each solve. An amplification below one (a reading put together by hand)
// counts as one.
func (r BudgetReading) Computed() int {
	k := max(r.Amplification, ampFloor)
	return int(math.Ceil(k*float64(r.InFlight)-budgetSlack)) + r.Reserve*r.Solves
}

// InUse is what admission goes by: this program's count plus the excess the
// last reading found over it, rounded up once.
func (r BudgetReading) InUse() int {
	k := max(r.Amplification, ampFloor)
	return int(math.Ceil(k*float64(r.InFlight)+r.Excess-budgetSlack)) + r.Reserve*r.Solves
}

// Reading copies the budget's figures; a nil budget reads as zeros.
func (b *Budget) Reading() BudgetReading {
	if b == nil {
		return BudgetReading{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return BudgetReading{
		Limit: b.limit, Reserve: b.reserve, InFlight: b.inFlight,
		Solves: b.solvesLocked(), ServiceSolves: b.svcSolves, Observed: b.observed, Known: b.known,
		Amplification: b.amp, Samples: b.samples, Excess: b.excess,
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

// solvingOn reports whether a solve in progress is on the port: one this
// program waits on, or one the service said at the last reading it was running
// there. The second matters to a lookup pool told the profile is over its
// limit (Pool.overLimit): it closes the idle connections of every port not
// solving, and a solve whose request has gone leaves the port looking idle to
// this program while the solver still works through it.
func (b *Budget) solvingOn(port int) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.solving[port] > 0 || b.svcSolving[port]
}

// profileLoad is one reading of the service summed over a profile's ports.
type profileLoad struct {
	// upstream is the connections to the proxy, client the connections from
	// this program, solving the solves the service is running (PortLoad).
	upstream, client, solving int
	// solvingPorts is the ports with a solve on them; nil when none is.
	solvingPorts map[int]bool
}

// observe records the service's count of connections to the proxy alone, with
// nothing to learn the amplification from. See observeProfile.
func (b *Budget) observe(n int, known bool) (over bool, holders []budgetHolder) {
	return b.observeLoad(n, 0, known)
}

// observeLoad records a reading with no solves of the service's in it, as a
// service from before PortLoad.Solving gives. See observeProfile.
func (b *Budget) observeLoad(upstream, client int, known bool) (over bool, holders []budgetHolder) {
	return b.observeProfile(profileLoad{upstream: upstream, client: client}, known)
}

// observeProfile records a reading of the profile's ports — upstream
// connections to the proxy, client connections from this program, the
// service's solves — learns the amplification from it, keeps how far it was
// above the prediction (the excess), and lets the waiting look again. over is whether the count is above
// the limit; then holders are the pools to tell.
//
// The excess is measured against the prediction priced with the k this very
// reading taught, so a reading that only shows the service's usual two per
// request leaves no excess behind once k has learned the two. A reading that
// failed leaves none either: an unknown count is not a count (see
// Budgets.pollOnce). Nor does it leave the service's solves: kept, a solve
// read before the service restarted would hold its reserve, and pin its port
// against overLimit, for as long as the service could not be read.
//
// The service's solves are stored before k learns and before the excess is
// taken, so both see the solves this very reading counted: a solve whose
// request has gone is the reserve in the sample and in the prediction, not
// connections the sample teaches every request to open and not an excess.
//
// Every reading wakes the waiting, whatever it says: a lower count is room, and
// so is a lower k, which shrinks what every attempt in flight is counted as. A
// higher one needs no wake, but telling the two apart buys nothing — a waiter
// woken for nothing looks once and sleeps again.
func (b *Budget) observeProfile(r profileLoad, known bool) (over bool, holders []budgetHolder) {
	if b == nil {
		return false, nil
	}
	upstream, client := r.upstream, r.client
	b.mu.Lock()
	defer b.mu.Unlock()
	// A reading that lands after the limit was taken off (between a caller
	// collecting the limited budgets and observing) is not stored: Set(0)
	// cleared the count, and an old number would come back with it.
	if b.limit <= 0 {
		return false, nil
	}
	b.observed, b.known, b.excess = upstream, known, 0
	b.svcSolves, b.svcSolving = 0, nil
	if known {
		b.svcSolves, b.svcSolving = r.solving, r.solvingPorts
	}
	if known && client > 0 {
		// The solves counted come out of the upstream — the service's own
		// among them — but only this program's out of the client connections:
		// a solve of its own is a request still waiting on its port, while
		// one the service runs on its own has no client left there.
		b.learnLocked(float64(upstream-b.reserve*b.solvesLocked()) / float64(max(client-b.solves, 1)))
	}
	if known {
		b.excess = max(float64(upstream)-b.computedLocked(), 0)
	}
	b.wakeLocked()
	over = b.limit > 0 && known && upstream > b.limit
	if over {
		holders = b.holdersLocked()
	}
	return over, holders
}

// learnLocked mixes one reading's sample of the amplification into k (see
// ampAlpha): the first taken whole unless it is below the floor, the second
// averaged with it, the rest smoothed. Samples are clamped to [ampFloor,
// ampCeiling], which keeps k there too: it starts at the floor and is only
// ever a mean of clamped samples. A reading that failed, or one with no
// connection from this program on the profile's ports (nothing to divide by:
// the upstream count then is the service's leftovers, not its way with
// requests), is never a sample; the caller leaves k as it was.
func (b *Budget) learnLocked(sample float64) {
	if b.samples == 0 && sample < ampFloor {
		return
	}
	sample = min(max(sample, ampFloor), ampCeiling)
	switch b.samples {
	case 0:
		b.amp = sample
	case 1:
		b.amp = (b.amp + sample) / 2
	default:
		b.amp += ampAlpha * (sample - b.amp)
	}
	b.samples++
}
