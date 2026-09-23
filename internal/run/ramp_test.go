// SPDX-License-Identifier: MIT

package run

import (
	"testing"
	"time"
)

// watching is a ramp over a run whose sessions rest two minutes, with the clock
// in the test's hand.
func watching(t *testing.T) (*Ramp, func(time.Duration)) {
	t.Helper()
	r := NewRamp(2 * time.Minute)
	at := r.began
	r.now = func() time.Time { return at }
	return r, func(d time.Duration) { at = at.Add(d) }
}

func TestRamp_SaysARunIsAtSpeedOnceItHasStoppedMakingSessions(t *testing.T) {
	// A thread makes a session only when none of the ones there are is ready for
	// it, so a run that has made none for a whole rest has enough: every session
	// it holds has come due in that time and no thread reached for one that was
	// not there.
	r, pass := watching(t)
	for range 8 {
		r.Made()
		pass(10 * time.Second)
	}
	if got := r.Ramping(); got.AtSpeed || got.Made != 8 {
		t.Errorf("a run still making sessions reads %+v, want eight made and not at speed", got)
	}

	// A minute and a half of quiet is not a rest: a session drawn to rest two
	// minutes has not come due yet, so nothing is known.
	pass(90 * time.Second)
	if got := r.Ramping(); got.AtSpeed {
		t.Errorf("at ninety seconds of a two-minute rest the run is called settled: %+v", got)
	}
	pass(31 * time.Second)
	got := r.Ramping()
	if !got.AtSpeed {
		t.Errorf("two minutes without a new session and the run is not at speed: %+v", got)
	}
	if got.Since < 2*time.Minute {
		t.Errorf("the last session was made %v ago, want over two minutes", got.Since)
	}

	// And one more session made puts it back to widening.
	r.Made()
	if got := r.Ramping(); got.AtSpeed || got.Made != 9 {
		t.Errorf("a run that has just made another session reads %+v, want it widening again", got)
	}
}

func TestRamp_SaysNothingUntilTheRunHasBeenGoingARest(t *testing.T) {
	// A run whose sessions were all resumed from an earlier one makes none at
	// all, and is at its speed from the first minute — but not from the first
	// second: until a rest has passed nothing has come due, and a screen saying
	// "at speed" over a job that has done nothing is saying it of nothing.
	r, pass := watching(t)
	if got := r.Ramping(); got.AtSpeed {
		t.Errorf("a run that has just started is already called settled: %+v", got)
	}
	pass(2 * time.Minute)
	if got := r.Ramping(); !got.AtSpeed || got.Made != 0 {
		t.Errorf("a run that needed no session of its own reads %+v, want it at speed", got)
	}
}

func TestRamp_TellsBeingShortOfAddressesFromHavingEnoughSessions(t *testing.T) {
	// Both ends stop a run widening, and they want opposite answers: one is a
	// run with as many sessions as it can use, the other a run that would take
	// more and cannot, because every address is already carrying as many as it
	// may. Read as the same thing, a list too small for the threads would show
	// on the screen as a job running perfectly.
	r, pass := watching(t)
	pass(3 * time.Minute)
	r.Short()
	got := r.Ramping()
	if !got.Short || got.AtSpeed {
		t.Errorf("a run that cannot have the sessions it wants reads %+v, want it short", got)
	}

	// And it stops being short once the threads stop reaching for what is not
	// there — the same rest that settles the other end.
	pass(2*time.Minute + time.Second)
	if got := r.Ramping(); got.Short || !got.AtSpeed {
		t.Errorf("a rest after the last thread went short the run reads %+v, want it at speed", got)
	}
}

func TestRamp_ReadsAsNothingWhereNobodyIsWatching(t *testing.T) {
	// A run whose ports are the identities makes no sessions and counts none:
	// the screen that draws this asks the same question of every job.
	var none *Ramp
	none.Made()
	none.Short()
	if got := none.Ramping(); got != (Ramping{}) {
		t.Errorf("a run with nothing watching reads %+v, want nothing", got)
	}
}

func TestNewRamp_WatchesForALeastAMinuteHoweverShortTheRest(t *testing.T) {
	// A job that rests its sessions two seconds would otherwise be called
	// settled two seconds in, and the word would come and go on the screen
	// faster than anybody could read it.
	r := NewRamp(2 * time.Second)
	if r.settle != rampFloor {
		t.Errorf("a run resting two seconds is watched for %v, want the floor of %v", r.settle, rampFloor)
	}
}
