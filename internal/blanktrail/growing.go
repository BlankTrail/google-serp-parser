// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"sync"
	"time"
)

// Growing is a pool that is not opened until something asks for it, and widens
// while callers are queueing for an identity.
//
// It is what the second set of ports wants to be — the ones the hidden
// addresses are read through. Whether a job needs any of them is not knowable
// when it starts: it depends on what Google answers with, and most regions put
// the address in the markup, where reading it costs no request at all. A set
// opened up front at the size the worst case would want is, on those regions,
// a hundred ports standing idle for the length of the run — and ports are the
// scarce thing, of which a tariff holds a fixed number for everything on the
// machine.
//
// So it opens one port the first time an address actually has to be read, and
// takes another whenever somebody is standing in the queue for one. A region
// that states its addresses never opens a single port; one that hides them
// climbs to what it needs within seconds of the first query settling and stops
// there.
type Growing struct {
	// open makes a pool of n ports. It is a function rather than a config
	// because what those ports are made of belongs to whoever is opening them —
	// this only decides how many and when.
	open func(ctx context.Context, ports int) (*Pool, error)
	// most is the ceiling. Nought and below mean one.
	most int
	// gap is how long to leave between two widenings, so a hundred threads
	// asking at once cost one port rather than a hundred.
	gap time.Duration
	// now is a clock seam for tests.
	now func() time.Time

	mu       sync.Mutex
	pool     *Pool
	grewAt   time.Time
	closed   bool
	openErr  error
	everOpen bool
}

// widenNoOftenThan is the default gap between two widenings.
//
// A quarter of a second, and the number is about the control service rather
// than about the pool: opening a port is a call to it, every thread of a run
// asks this on every query, and a set that widened on each of those would spend
// its first second making a hundred calls to arrive where two would have put
// it. What it costs is that a run needing a hundred ports takes half a minute
// to get them, against a run that opened them all before it knew whether it
// needed any.
const widenNoOftenThan = 250 * time.Millisecond

// NewGrowing returns a set that opens on demand and widens to at most most
// ports.
func NewGrowing(most int, open func(ctx context.Context, ports int) (*Pool, error)) *Growing {
	if most < 1 {
		most = 1
	}
	return &Growing{open: open, most: most, gap: widenNoOftenThan, now: time.Now}
}

// Identities returns the pool, opening it the first time and widening it when
// somebody is queueing for an identity.
//
// A failure to open is remembered and handed back to every later caller rather
// than retried on each: the reasons a pool will not open are the service being
// unreachable, a licence, or no port left on the machine, and none of them is
// answered by asking again a hundred times a second.
func (g *Growing) Identities(ctx context.Context) (*Pool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, ErrPoolExhausted
	}
	if g.everOpen && g.openErr != nil {
		return nil, g.openErr
	}
	if g.pool == nil {
		g.everOpen = true
		// One port. Whether a second is wanted is decided by whether anybody
		// ends up waiting for the first, which is the only honest measure of it.
		pool, err := g.open(ctx, 1)
		if err != nil {
			g.openErr = err
			return nil, err
		}
		g.pool, g.grewAt = pool, g.now()
		return pool, nil
	}

	if g.pool.Size() < g.most && g.now().Sub(g.grewAt) >= g.gap {
		if st := g.pool.Stats(); st.Waiting > 0 {
			// A failure to widen is not a failure to answer: the caller has a
			// pool, it is merely narrower than it would like.
			if err := g.pool.Grow(ctx, 1); err == nil {
				g.grewAt = g.now()
			}
		}
	}
	return g.pool, nil
}

// Opened is the pool if one was ever opened, and nil if nothing ever asked. It
// is what a screen reporting the run's ports reads, and what tells the two
// cases apart: a set of no ports and a set nobody wanted.
func (g *Growing) Opened() *Pool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pool
}

// Close gives back whatever was opened. A set nothing ever asked for closes
// without a word to the service.
func (g *Growing) Close() error {
	g.mu.Lock()
	pool := g.pool
	g.pool, g.closed = nil, true
	g.mu.Unlock()
	if pool == nil {
		return nil
	}
	return pool.Close()
}
