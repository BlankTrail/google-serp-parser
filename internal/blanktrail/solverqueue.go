// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"sync"
	"time"
)

// SolverQueue is how much work the challenge solver has in hand.
//
// Running is how many challenges are being solved this moment and Queued how
// many are waiting for a process to come free. Ceiling is how long the queue is
// allowed to get before the service refuses to take more.
//
// The number that matters is Queued. Running is bounded by the tariff — ten
// processes on this one — and a run that keeps all ten busy is a run using what
// it paid for. Queued is different: every item in it is a request that has
// already been made, is holding an identity, and is waiting for a process that
// somebody else's request is using. It is the only one of the three that says
// the run is asking for more than the licence can carry.
type SolverQueue struct {
	Queued  int
	Running int
	Ceiling int
}

// solverQueueResponse is how the service answers.
type solverQueueResponse struct {
	Queued  int `json:"queued"`
	Running int `json:"running"`
	MaxLen  int `json:"max_len"`
}

// SolverQueue asks the service what the challenge solver has in hand.
func (c *Client) SolverQueue(ctx context.Context) (SolverQueue, error) {
	var out solverQueueResponse
	if err := c.doJSON(ctx, "GET", "/api/v1/solver/queue", nil, &out); err != nil {
		return SolverQueue{}, err
	}
	return SolverQueue{Queued: out.Queued, Running: out.Running, Ceiling: out.MaxLen}, nil
}

// Brake slows a run down while the challenge solver is behind.
//
// A tariff holds a fixed number of solver processes — ten on this one — and a
// challenge takes tens of seconds to solve: measured on the live list, answers
// that went through the solver came back after 15, 25, 44, 63, 89, 210 and 254
// seconds. So ten is not many, and what happens when a run outruns them is an
// avalanche: every request that meets a challenge joins a queue, holds its
// identity while it waits, and the threads behind it go on making more. The
// queue grows, every identity in it ages, and the run ends up spending minutes
// per page on ports it is paying to keep open.
//
// What this does is take the pressure off before that happens. It asks the
// service how deep the queue is, and while two or more requests are waiting for
// a process it puts a pause in front of every new request — longer the deeper
// the queue. Nothing is refused and no thread is stopped: a run that is over
// the line slows until it is under it again, which is the only way out of an
// avalanche that does not end in one.
//
// Two rather than one, because one waiting request is a run using every process
// it has with one in hand — which is the shape of a run at full speed, not of a
// run in trouble.
type Brake struct {
	// ask is how the queue is read. It is a function rather than a client so a
	// test can drive the brake without a service, and so a machine with no
	// connection has a brake that holds nothing rather than a nil one every
	// caller has to check.
	ask func(ctx context.Context) (SolverQueue, error)
	// every is how often the queue is read.
	every time.Duration
	// sleep is the pause itself, a seam so a test does not sit through one.
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time

	mu     sync.Mutex
	held   time.Duration
	readAt time.Time
	queue  SolverQueue
}

const (
	// solverQueueFree is how many requests may be waiting for a solver process
	// before a run is asked to slow down. One waiting request is a run using
	// every process it has with one in hand, which is full speed rather than
	// trouble.
	solverQueueFree = 1
	// brakePerItem is how much pause each request over that line buys.
	//
	// Two seconds, and the number is about what the queue is made of rather
	// than about the brake: an item in it is a challenge waiting for a process,
	// and a challenge measured on the live list takes tens of seconds. A brake
	// of two seconds an item is therefore an order gentler than the thing it is
	// braking against — enough to bend a run that is drifting over the line,
	// and not enough to stop one that crossed it for a moment.
	brakePerItem = 2 * time.Second
	// brakeCeiling is the longest pause the brake will put in front of a
	// request. Past it the run is not going to be rescued by waiting longer,
	// and a pause of minutes reads to whoever is watching as a program that has
	// stopped.
	brakeCeiling = 30 * time.Second
	// brakeReadEvery is how often the queue is read. The call is to a service
	// on this machine and costs a millisecond; what it is spaced for is that
	// every thread of a run passes through here, and a run of a hundred threads
	// would otherwise ask a hundred times a second.
	brakeReadEvery = 2 * time.Second
)

// NewBrake returns a brake that reads the queue through ask.
//
// A nil ask makes a brake that holds nothing, which is what a machine with no
// connection to the service has: the run goes at the speed it would have gone
// at before there was a brake at all.
func NewBrake(ask func(ctx context.Context) (SolverQueue, error)) *Brake {
	return &Brake{
		ask:   ask,
		every: brakeReadEvery,
		sleep: sleepFor,
		now:   time.Now,
	}
}

// BrakeOn returns a brake that reads the queue through this client.
func BrakeOn(cl *Client) *Brake {
	if cl == nil {
		return NewBrake(nil)
	}
	return NewBrake(cl.SolverQueue)
}

// Hold pauses the caller for as long as the solver queue says it should, and
// returns at once when it says nothing.
//
// It is called before a request rather than after one, because the point is to
// make fewer requests while the queue is deep — a pause taken after the request
// has already joined the queue is a pause that changed nothing about the queue.
//
// A queue that cannot be read holds nobody. The service being unreachable is
// about to be reported by the request itself, loudly, and a run that stopped
// because a reading failed would be a run stopped by its own instrument.
func (b *Brake) Hold(ctx context.Context) error {
	if b == nil {
		return ctx.Err()
	}
	wait := b.holdFor(ctx)
	if wait <= 0 {
		return ctx.Err()
	}
	return b.sleep(ctx, wait)
}

// holdFor is how long the next request should wait, reading the queue again
// when the last reading is old enough.
func (b *Brake) holdFor(ctx context.Context) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ask == nil {
		return 0
	}
	if b.readAt.IsZero() || b.now().Sub(b.readAt) >= b.every {
		// Read under the lock on purpose: a hundred threads arriving together
		// otherwise make a hundred readings of the same instant. What it costs
		// is that they queue behind one call to a service on this machine.
		queue, err := b.ask(ctx)
		b.readAt = b.now()
		if err != nil {
			// Nothing is known about the queue, so nothing is held. The last
			// reading is not kept either: a brake still braking on a number
			// from a minute ago is a brake nobody can reason about.
			b.queue, b.held = SolverQueue{}, 0
			return 0
		}
		b.queue = queue
		b.held = brakeFor(queue.Queued)
	}
	return b.held
}

// Queue is the last reading, for a screen that reports what the run is up
// against. It reads nothing itself.
func (b *Brake) Queue() SolverQueue {
	if b == nil {
		return SolverQueue{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.queue
}

// Holding is the pause the brake is putting in front of each request, and
// nought when it is out of the way.
func (b *Brake) Holding() time.Duration {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.held
}

// brakeFor is the pause a queue of this depth buys.
func brakeFor(queued int) time.Duration {
	over := queued - solverQueueFree
	if over <= 0 {
		return 0
	}
	wait := time.Duration(over) * brakePerItem
	if wait > brakeCeiling {
		wait = brakeCeiling
	}
	return wait
}

// sleepFor is a pause that ends early when the caller does.
func sleepFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// BrakeSleepsWith points a brake's pause at another clock, for a test that
// would otherwise sit through one.
//
// It lives here rather than in a test file because the field it moves is
// unexported and belongs to this package: a test elsewhere — the run's, where
// the brake is actually held — has no other way to watch it without waiting out
// every pause for real.
func BrakeSleepsWith(b *Brake, sleep func(ctx context.Context, d time.Duration) error) {
	if b == nil || sleep == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sleep = sleep
}
