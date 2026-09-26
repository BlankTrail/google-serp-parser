// SPDX-License-Identifier: MIT

package run

import (
	"sync"
	"time"
)

// Doing is one of the places a thread of a run can be standing.
//
// Together they are the whole of a thread's loop: at every instant a thread is
// in exactly one of them, so the count of threads across them is the number of
// threads, and the time across them is the time the run has been going times
// the threads. That is what makes the reading answer "where is the run losing
// its speed" rather than only "what is it doing".
//
// A thread has one place where waiting is the work — DoingAsk, waiting on
// Google. Everywhere else waiting is a fault to be found: a thread standing in
// DoingTake is waiting for the service to dress a port, one in DoingGiveBack is
// waiting for the service and the history to take a session back, and one in
// DoingIdle has nothing due to carry on with at all.
type Doing string

const (
	// DoingPort is waiting for a port to come free.
	DoingPort Doing = "waiting for a port"
	// DoingTake is finding the session to carry on with and putting it on the
	// port: its fingerprint, its address and its tickets, which is three calls
	// to the service before a request goes anywhere.
	DoingTake Doing = "taking a session"
	// DoingBrake is waiting in front of a request because the challenge solver
	// is behind. It is its own place rather than part of the ask: the run is
	// being held back on purpose there, and a run losing its speed to its own
	// brake reads exactly like one losing it to Google.
	DoingBrake Doing = "held back by the brake"
	// DoingAsk is the request itself — the road to the address, Google's check
	// where one is met, and the answer. The one wait here that is the work.
	DoingAsk Doing = "asking"
	// DoingGiveBack is handing the session back: the port's tickets for it and
	// the line in the history.
	DoingGiveBack Doing = "giving the session back"
	// DoingRecord is settling a finished query — the hidden addresses looked
	// up, and the result written down. A thread of a walking job does not do
	// that itself: it hands the query aside and stands here only while there is
	// no room to hand it to, which is the history falling behind the walks (see
	// aside).
	DoingRecord Doing = "writing the result down"
	// DoingIdle is a thread with nothing to do: every session it could carry on
	// with is resting, and there is no query left to open one for.
	DoingIdle Doing = "waiting for something to be due"
)

// doings are the places in the order a thread meets them, which is the order
// they are reported in. A map would report them in a different order every
// time, and a reader comparing two readings of a screen would be reading a
// different table each time.
var doings = []Doing{DoingPort, DoingTake, DoingBrake, DoingAsk, DoingGiveBack, DoingRecord, DoingIdle}

// Where is where the threads of a run are standing, and where its time has
// gone.
//
// It is one per run and written by every thread of it. What it costs is a lock
// taken a few times per request per thread, which against a request measured in
// seconds is nothing — and without it a run at a tenth of the speed it should
// be reads the same whether its threads are queueing for the service, resting
// on a pause, or waiting on Google.
type Where struct {
	now func() time.Time

	mu    sync.Mutex
	at    map[int]standing
	spent map[Doing]time.Duration
}

// standing is what one thread is doing and since when.
type standing struct {
	doing Doing
	since time.Time
}

// NewWhere returns a census with nothing standing anywhere.
func NewWhere() *Where {
	return &Where{now: time.Now, at: map[int]standing{}, spent: map[Doing]time.Duration{}}
}

// At says this thread has moved on to something else, and adds what it spent on
// what it was doing before to that one's total.
//
// A nil census takes everything and reports nothing, so a run nobody is
// watching costs a nil check per stage rather than a branch at every call site.
func (w *Where) At(thread int, doing Doing) {
	if w == nil {
		return
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if was, ok := w.at[thread]; ok {
		w.spent[was.doing] += now.Sub(was.since)
	}
	w.at[thread] = standing{doing: doing, since: now}
}

// Gone takes a thread out of the census: the job it was working is over, and a
// thread left standing in its last place would read as one still there.
func (w *Where) Gone(thread int) {
	if w == nil {
		return
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if was, ok := w.at[thread]; ok {
		w.spent[was.doing] += now.Sub(was.since)
		delete(w.at, thread)
	}
}

// Standing is one place and what the run has of it.
type Standing struct {
	Doing Doing
	// Threads is how many are standing here this instant.
	Threads int
	// Spent is how long the threads of this run have spent here between them,
	// the one standing here now included. Two threads a minute each is two
	// minutes, which is what makes the shares comparable across a run of any
	// width.
	Spent time.Duration
	// Longest is how long the thread that has been here longest has been here.
	// It is what tells a place threads pass through quickly from one they are
	// stuck in: eighty threads asking is a run at work, and eighty threads that
	// have all been asking for four minutes is a run waiting on something that
	// is not coming.
	Longest time.Duration
}

// Census is the whole reading, in the order a thread meets the places.
type Census struct {
	Standing []Standing
	// Threads is how many are in the census at all.
	Threads int
	// Spent is the time across every place, so a share can be read off a single
	// reading without adding the places up again.
	Spent time.Duration
}

// Share is how much of the run's time went here, between nought and one, and
// nought where no time has been spent anywhere yet.
func (c Census) Share(d Doing) float64 {
	if c.Spent <= 0 {
		return 0
	}
	for _, s := range c.Standing {
		if s.Doing == d {
			return float64(s.Spent) / float64(c.Spent)
		}
	}
	return 0
}

// Reading is where the threads are standing now and where the time has gone.
//
// The time a thread is in the middle of spending counts towards the place it is
// standing in, so a run whose threads have all been asking for four minutes
// reads as four minutes a thread spent asking rather than as nothing at all —
// which is what the reading is wanted for.
func (w *Where) Reading() Census {
	if w == nil {
		return Census{}
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	spent := make(map[Doing]time.Duration, len(w.spent))
	for d, took := range w.spent {
		spent[d] = took
	}
	threads := map[Doing]int{}
	longest := map[Doing]time.Duration{}
	for _, was := range w.at {
		since := now.Sub(was.since)
		spent[was.doing] += since
		threads[was.doing]++
		if since > longest[was.doing] {
			longest[was.doing] = since
		}
	}
	out := Census{Threads: len(w.at)}
	for _, d := range doings {
		if spent[d] == 0 && threads[d] == 0 {
			continue
		}
		out.Standing = append(out.Standing, Standing{
			Doing: d, Threads: threads[d], Spent: spent[d], Longest: longest[d]})
		out.Spent += spent[d]
	}
	return out
}
