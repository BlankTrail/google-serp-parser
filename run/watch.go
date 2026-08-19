// SPDX-License-Identifier: MIT

package run

import "time"

// Stage is one thing a thread of a run does, and how long it took.
//
// The stages exist so that a slow run can be taken apart second by second. A
// thread has only one place where waiting is the work — waiting on an answer
// from the far end — and every other stage here is a place where waiting is a
// fault to be found and removed. Without them, a pool with nothing going
// through it reads the same whether its threads are busy elsewhere, queueing
// for an identity, or resting on a pause nobody asked for.
type Stage string

// The stages of one query, in the order a thread meets them.
const (
	// StagePause is the gap a thread leaves between two of its own requests.
	StagePause Stage = "paused between queries"
	// StageLease is the wait for an identity to become free. Anything but a
	// moment here means the pool is narrower than the threads asking of it.
	StageLease Stage = "waited for an identity"
	// StageAsk is the request itself, which is the one wait that is the work.
	StageAsk Stage = "asked"
	// StageRecord is writing the result down.
	StageRecord Stage = "recorded"
	// StageQuery is the whole of one query, from taking it to writing it down,
	// so the stages above can be checked against something that contains them.
	StageQuery Stage = "query"
)

// Step is one stage a thread passed through.
type Step struct {
	Thread int
	Stage  Stage
	Took   time.Duration
	// Query is the phrase the thread was working on, and Err is what went wrong
	// if anything did.
	Query string
	Err   error
}

// Watch is told each step. It is called from the thread that took it, so an
// implementation that blocks holds up the run — a watcher's job is to write the
// step down and return.
type Watch func(Step)

// step tells the watcher, if there is one, how long something took.
func (r *Runner) step(thread int, stage Stage, began time.Time, query string, err error) {
	if r.Watch == nil {
		return
	}
	r.Watch(Step{
		Thread: thread,
		Stage:  stage,
		Took:   time.Since(began),
		Query:  query,
		Err:    err,
	})
}
