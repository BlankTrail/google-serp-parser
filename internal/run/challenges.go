// SPDX-License-Identifier: MIT

package run

import (
	"sync"
	"time"
)

// challengeCrowded is how few requests between two checks reads as too few.
//
// Seven, as the operator set it. A session asked oftener than its own rest
// allows starts paying for a check every handful of requests, and the remedy is
// the rest rather than anything the run can do about it — so this is the number
// at which the screen says so.
const challengeCrowded = 7

// challengeEnough is how many checks must have been met before the rhythm is
// worth judging. Without it a single check early on — one session's bad luck —
// would raise the warning on a run that has done nothing yet.
const challengeEnough = 5

// challengeSettling is how long a run goes before its rhythm is judged at all.
//
// Ten minutes, as the operator set it. The opening of a run is its sessions
// being let in: every one of them is fresh, and a fresh session pays a check to
// be admitted whatever pace it is asked at. Judged in the first minutes, a run
// starting up reads exactly like a run asking too fast.
const challengeSettling = 10 * time.Minute

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
// no line drawn through that separates the two. Measured on the live list, of
// eight answers five brought a clearance and the same five took over thirty
// seconds — the clearance is the one that says which.
//
// It is fed from whichever thread took the answer, so it is safe for several at
// once.
type Challenges struct {
	// now and began are the clock and when this run started counting, for the
	// waiting period before the rhythm is judged. A counter that was never told
	// when its run began has none.
	now   func() time.Time
	began time.Time

	mu sync.Mutex
	// met is every check this run has paid for, the one a fresh session pays to
	// be let in included: it is a check somebody waited for, and the screen
	// reports it as one.
	met int
	// asked is how many answers came back on sessions Google had already
	// admitted, and askedMet how many of those met a check anyway. The rhythm is
	// worked out from these two alone — see Held.Admitted.
	asked    int
	askedMet int
}

// NewChallenges starts counting for a run beginning now.
func NewChallenges() *Challenges {
	return &Challenges{now: time.Now, began: time.Now()}
}

// Answer takes one answer from Google: whether Google had answered this session
// before, and the clearance it held before the request and holds now.
//
// A clearance that is new, or one that has changed, is a check just passed.
// Nothing else counts: a session that carried the same clearance through the
// request answered without meeting one, and one that has never had a clearance
// has never met one at all.
func (c *Challenges) Answer(admitted bool, before, after string) {
	if c == nil {
		return
	}
	passed := after != "" && after != before
	c.mu.Lock()
	defer c.mu.Unlock()
	if passed {
		c.met++
	}
	if !admitted {
		// The session's first answer. Whatever it cost, it is the price of being
		// let in rather than a reading of the pace.
		return
	}
	c.asked++
	if passed {
		c.askedMet++
	}
}

// Rhythm is how a run's checks fell, as a screen reports them.
type Rhythm struct {
	// Met is how many checks this run has paid for, the one a fresh session pays
	// to be let in included.
	Met int
	// Asked is how many requests went out on sessions Google had already
	// admitted, and AskedMet how many of those met a check.
	Asked    int
	AskedMet int
	// Between is how many requests the run gets for each check it meets on a
	// session already admitted, and Known says whether enough has happened to
	// work it out from.
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
	out := Rhythm{Met: c.met, Asked: c.asked, AskedMet: c.askedMet}
	if c.askedMet == 0 {
		return out
	}
	out.Between = float64(c.asked-c.askedMet) / float64(c.askedMet)
	out.Known = true
	out.Crowded = c.settled() && c.askedMet >= challengeEnough && out.Between < challengeCrowded
	return out
}

// settled says the run has been going long enough for its rhythm to be worth
// judging. A counter nobody told when the run began has no waiting period, and
// says so at once.
func (c *Challenges) settled() bool {
	return c.began.IsZero() || c.now().Sub(c.began) >= challengeSettling
}
