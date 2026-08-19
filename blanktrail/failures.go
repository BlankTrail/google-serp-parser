// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Failure is one kind of thing that goes wrong on the way to an answer.
//
// They exist because "the run had errors" is not a fact anybody can act on. An
// address that never carried the request is a dead proxy and the remedy is
// another address; a refusal to relay TLS is a setting; a wall is the origin
// saying no to the identity, which no amount of rotating fixes; a silence that
// ran out our own clock is this program's own patience. Counted apart, the
// shape of a bad run is readable at a glance and each shape has a different
// thing to do about it.
type Failure string

// The kinds, and every failure is exactly one of them.
const (
	// FailureTransport is a request that never arrived: the connection was
	// refused, reset, or closed without an answer. On a large cheap list this is
	// most of them, and it means the address is dead.
	FailureTransport Failure = "transport"
	// FailureRelay is the proxy refusing to carry the request through an
	// upstream that terminates TLS itself and presents its own certificate.
	FailureRelay Failure = "relay refused"
	// FailureWall is the origin refusing this identity: a redirect to its
	// challenge page, or a rate limit.
	FailureWall Failure = "wall"
	// FailureTimeout is our own clock running out — the deadline this program
	// set, not one the far end kept.
	FailureTimeout Failure = "timeout"
	// FailureOther is any other answer that was not a success.
	FailureOther Failure = "other"
)

// Failures is every kind in the order a screen should show them, so two screens
// cannot disagree about the order.
var Failures = []Failure{FailureTransport, FailureRelay, FailureWall, FailureTimeout, FailureOther}

// relayRefused is what a proxy answers when the upstream terminates TLS itself
// and it has not been told that is allowed.
const relayRefused = 526

// failureOf names what went wrong with one attempt, from what came back.
//
// Errors are read before statuses because an attempt that produced an error
// produced no status. A nil error and a 2xx is not a failure at all and the
// caller is expected not to ask.
func failureOf(err error, status int) Failure {
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return FailureTimeout
		}
		// The transport's own deadline arrives as an error that says so in
		// words and matches no sentinel: net/http wraps it, and the wrapper is
		// not comparable to anything exported.
		s := strings.ToLower(err.Error())
		if strings.Contains(s, "timeout") || strings.Contains(s, "deadline") {
			return FailureTimeout
		}
		return FailureTransport
	}
	switch {
	case status == relayRefused:
		return FailureRelay
	case status == http.StatusTooManyRequests, status == http.StatusFound, status == http.StatusMovedPermanently:
		return FailureWall
	default:
		return FailureOther
	}
}

// attempted records that a request was put on the wire.
func (p *Pool) attempted() {
	p.mu.Lock()
	p.stats.Attempts++
	p.mu.Unlock()
}

// failed records one failure of a kind against the pool.
func (p *Pool) failed(kind Failure) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stats.Failures == nil {
		p.stats.Failures = map[Failure]int64{}
	}
	p.stats.Failures[kind]++
}

// ResetStats puts every counter this pool keeps back to nought, so a reading
// can be started from a known point without restarting anything.
//
// What it does not touch is state: an address resting off a failure goes on
// resting, and a port set aside stays set aside. Those are not counts of what
// happened, they are what is true now, and zeroing them would not clear a
// reading — it would throw the pool back into addresses it has already found
// dead and make the next reading worse than the one before it.
func (p *Pool) ResetStats() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stats.Requests = 0
	p.stats.Attempts = 0
	p.stats.ProfileRotations = 0
	p.stats.EgressRotations = 0
	p.stats.Renewals = 0
	p.stats.Quarantines = 0
	p.stats.Rejections = 0
	p.stats.Revivals = 0
	p.stats.Failures = nil
}

// counted is a channel that keeps a list and can say how large it is and how
// much of it is resting off a failure.
//
// It is an interface rather than a method on Channel because most channels have
// no list to count: a direct connection and a fixed address are one address
// each and always will be, and putting "how many are resting" on every channel
// would be asking three of them a question only one can answer.
type counted interface {
	Len() int
	Resting() int
}

// Addresses says how many egress addresses this pool can draw on and how many
// of them are resting off a failure right now.
//
// Resting is the honest word for what a screen calls banned: an address that
// failed is set aside for a while and comes back, and a run whose resting count
// climbs towards its total is a run about to have nothing left to try.
func (p *Pool) Addresses() (total, resting int) {
	for _, ch := range p.mixer.Channels() {
		c, ok := ch.(counted)
		if !ok {
			continue
		}
		total += c.Len()
		resting += c.Resting()
	}
	return total, resting
}
