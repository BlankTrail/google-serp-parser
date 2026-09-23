// SPDX-License-Identifier: MIT

package run

import (
	"sync"
	"time"
)

// rampFloor is the shortest a run is watched for before it is called settled.
// A reading that flips within seconds of a job starting is noise on a screen
// rather than a reading of anything.
const rampFloor = time.Minute

// Ramp is how a run gets up to speed.
//
// A thread takes a session for every query it opens and takes a rested one
// where there is one: it makes a new session only when it would otherwise stand
// still. So a run widens by itself at the start — the first threads meet a pool
// with nothing rested in it and make one each — and stops widening the moment
// there are enough that one is always due. That moment is the run at its speed:
// the sessions it holds cover the threads it has, and nothing more is bought by
// making another.
//
// What this watches is that moment arriving. A run still making sessions is a
// run still widening; one that has made none for a whole rest of a session has
// enough of them, because every session it holds has come due in that time and
// no thread reached for one that was not there.
//
// A run that stops making them because it cannot — every address already
// carrying as many sessions as it may — is the other end, and is told apart:
// that one is short, and what it is short of is addresses rather than pace.
type Ramp struct {
	// settle is how long without a new session says the run has enough. It is
	// the longest rest a session may take under this job's span: in that time
	// every session has come due at least once.
	settle time.Duration
	now    func() time.Time
	began  time.Time

	mu        sync.Mutex
	made      int
	lastMade  time.Time
	lastShort time.Time
}

// NewRamp starts watching a run whose sessions rest at most this long.
func NewRamp(rest time.Duration) *Ramp {
	if rest < rampFloor {
		rest = rampFloor
	}
	return &Ramp{settle: rest, now: time.Now, began: time.Now()}
}

// Made records a session this run had made for it, which is a thread that found
// none of the ones there were ready for it.
func (r *Ramp) Made() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.made++
	r.lastMade = r.now()
}

// Short records a thread that wanted a session and could not have one: every
// address the list offers is already carrying as many as it may.
func (r *Ramp) Short() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastShort = r.now()
}

// Ramping is where a run stands against its own speed.
type Ramping struct {
	// Made is how many sessions this run has had made for it, and Since how
	// long ago the last of them was made.
	Made  int
	Since time.Duration
	// AtSpeed says the run has stopped widening and is not short of addresses:
	// what it holds covers what it is doing, and the speed on the screen is the
	// speed this job runs at.
	AtSpeed bool
	// Short says a thread wanted a session and could not have one. The run has
	// stopped widening because the list will not let it, which is a ceiling of a
	// different kind and wants a different answer.
	Short bool
}

// Ramping reads where the run stands.
func (r *Ramp) Ramping() Ramping {
	if r == nil {
		return Ramping{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := Ramping{Made: r.made}
	if !r.lastMade.IsZero() {
		out.Since = now.Sub(r.lastMade)
	}
	out.Short = !r.lastShort.IsZero() && now.Sub(r.lastShort) < r.settle
	// A run that has not been going as long as a session rests has not been
	// watched long enough to say anything: nothing has come due yet.
	quiet := now.Sub(r.began) >= r.settle &&
		(r.lastMade.IsZero() || now.Sub(r.lastMade) >= r.settle)
	out.AtSpeed = quiet && !out.Short
	return out
}
