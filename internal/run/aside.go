// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"sync"
	"time"
)

// aside is where the finished queries of a walking job are settled — their
// hidden addresses read and their results written down — while the thread that
// finished each one goes back to its sessions.
//
// A thread used to settle its own query before taking up the next one, and
// settling is mostly waiting: every result whose address Google hid costs a
// request of its own, carried from address to address until one answers.
// Measured on the server, a hundred threads over the wingate list: a sixth of
// the threads' whole time went there, and in the minutes when the walks begun
// together ended together, seventy of the hundred stood writing while the
// sessions they could have been working rested.
//
// Nothing a thread does next waits on it. A lookup holds no session and goes
// out through a port of its own, and the query it completes is finished — no
// page of it is still to be asked. So the only reason the thread waited was
// that it was the one holding the results.
//
// It is bounded, one query a thread: a thread that finishes another while that
// many are still being settled waits for room, and that wait is what the
// census calls writing the result down. Past that bound the history would be
// falling behind the walks, and what is held in memory instead of written is
// what a stopped run loses.
type aside struct {
	// ctx is the run's own values under a cancellation of its own. A stop does
	// not end what is being settled: it is what the run collected, and losing it
	// to the press of a button is what the settling after a stop is there to
	// prevent. What ends it is stop, settlingTheRest later.
	ctx    context.Context
	cancel context.CancelFunc
	room   chan struct{}
	wg     sync.WaitGroup
	cut    *time.Timer
}

// newAside is room to settle as many queries at once as a run has threads.
func newAside(ctx context.Context, width int) *aside {
	if width < 1 {
		width = 1
	}
	kept, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &aside{ctx: kept, cancel: cancel, room: make(chan struct{}, width)}
}

// hand settles one query aside once there is room for it. It returns as soon as
// the settling has begun.
func (a *aside) hand(settle func(ctx context.Context)) {
	a.room <- struct{}{}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() { <-a.room }()
		settle(a.ctx)
	}()
}

// stop gives what is being settled, and whatever is handed aside from now on,
// settlingTheRest to finish in, the same span a stopped run always had for its
// walks in hand.
func (a *aside) stop() {
	if a.cut == nil {
		a.cut = time.AfterFunc(settlingTheRest, a.cancel)
	}
}

// wait returns once everything handed aside is settled.
func (a *aside) wait() {
	a.wg.Wait()
	if a.cut != nil {
		a.cut.Stop()
	}
	a.cancel()
}
