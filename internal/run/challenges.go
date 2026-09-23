// SPDX-License-Identifier: MIT

package run

import "sync"

// challengeCrowded is how few requests between two checks reads as too few.
//
// Seven, as the operator set it. A session asked oftener than its own rest
// allows starts paying for a check every handful of requests, and the remedy is
// the rest rather than anything the run can do about it — so this is the number
// at which the screen says so.
const challengeCrowded = 7

// challengeEnough is how many checks must have been met before the rhythm is
// worth judging. Without it the first check of a run — which may be the first
// request of a fresh session, where a check is ordinary — would raise the
// warning on a run that has done nothing yet.
const challengeEnough = 5

// Challenges is the rhythm of Google's checks through one run: how many
// requests were answered between one check and the next.
//
// What it is for is the one question a screen cannot otherwise answer: whether
// the sessions are being asked oftener than they can carry. A run that meets a
// check every fifty requests is a run at a sensible pace; one that meets a
// check every three is a run whose sessions are being spent as fast as they are
// made, and no amount of retrying will mend that — only a longer rest will.
//
// A check is read off the session rather than off the clock. Google issues its
// clearance when a check is passed, so a session holding one it did not hold
// before has just paid for one. Timing the answers would be a guess either way:
// the solver on the live list answered after 15, 25, 44, 63, 89, 210 and 254
// seconds, and an ordinary search through a slow list averaged about twenty, so
// no line drawn through that separates the two.
//
// It is fed from whichever thread took the answer, so it is safe for several at
// once.
type Challenges struct {
	mu sync.Mutex
	// met is how many answers came back with a clearance the session did not
	// have before, and answered is how many came back without one.
	met      int
	answered int
}

// Answer takes one answer from Google: the clearance the session held before
// the request, and the one it holds now.
//
// A clearance that is new, or one that has changed, is a check just passed.
// Nothing else counts: a session that carried the same clearance through the
// request answered without meeting one, and one that has never had a clearance
// has never met one at all.
func (c *Challenges) Answer(before, after string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if after != "" && after != before {
		c.met++
		return
	}
	c.answered++
}

// Rhythm is how a run's checks fell, as a screen reports them.
type Rhythm struct {
	// Met is how many checks this run has paid for, and Answered how many
	// searches came back without one.
	Met      int
	Answered int
	// Between is how many requests the run gets for each check it meets, and
	// Known says whether anything has been met yet to work it out from.
	//
	// It is the whole run's requests over the whole run's checks rather than the
	// average of each session's own figure: a session that answered twice
	// weighs twice, not as much as one that answered a hundred times, and it is
	// the busy sessions that say what the pace is costing.
	Between float64
	Known   bool
	// Crowded says the checks are coming oftener than a run should meet them,
	// which is the sessions being asked oftener than their rest allows.
	Crowded bool
}

// Rhythm reads the count as it stands.
func (c *Challenges) Rhythm() Rhythm {
	if c == nil {
		return Rhythm{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := Rhythm{Met: c.met, Answered: c.answered}
	if c.met == 0 {
		return out
	}
	out.Between = float64(c.answered) / float64(c.met)
	out.Known = true
	out.Crowded = c.met >= challengeEnough && out.Between < challengeCrowded
	return out
}
