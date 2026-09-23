// SPDX-License-Identifier: MIT

package run

import "testing"

func TestChallenges_ReadANewClearanceAsACheckPaidFor(t *testing.T) {
	// Google issues its clearance when a check is passed, so a session holding
	// one it did not hold before has just paid for one. The page that came back
	// says nothing about it — it is the page that was asked for.
	var c Challenges
	c.Answer("", "")            // a session with no clearance, and still none
	c.Answer("", "first")       // one just passed a check
	c.Answer("first", "first")  // the same session, asking again on its clearance
	c.Answer("first", "second") // and made to pass another

	got := c.Rhythm()
	if got.Met != 2 || got.Answered != 2 {
		t.Errorf("read %d checks and %d searches, want two of each", got.Met, got.Answered)
	}
	if !got.Known || got.Between != 1 {
		t.Errorf("the run gets %v requests a check (known=%v), want one", got.Between, got.Known)
	}
}

func TestChallenges_CountNothingWhereAClearanceIsOnlyCarried(t *testing.T) {
	// A session taken up again brings back the clearance it was written down
	// with. Counted as a check, every session resumed from the history would
	// read as one that had just paid for one — and the screen would advise
	// slowing a run down for work it never did.
	var c Challenges
	for range 10 {
		c.Answer("kept", "kept")
	}
	if got := c.Rhythm(); got.Met != 0 || got.Answered != 10 || got.Known {
		t.Errorf("read %+v from a session asking on the clearance it had, want ten plain answers", got)
	}
	// And a clearance that has gone — the cookie ran out, or the session was
	// given a fresh jar — is not a check either.
	c.Answer("kept", "")
	if got := c.Rhythm(); got.Met != 0 {
		t.Errorf("a clearance that expired read as %d checks, want none", got.Met)
	}
}

func TestChallenges_SayTheChecksAreCrowdedOnlyOnceThereAreEnoughToJudge(t *testing.T) {
	// The warning is advice about the rest a session takes, and it is worth
	// giving only when the rhythm is a rhythm. The first check of a run may be
	// the first request of a fresh session, where a check is ordinary — and a
	// run told to slow down after two requests would be told so by noise.
	var c Challenges
	for i := range 4 {
		c.Answer(clearance(i), clearance(i+1))
		c.Answer(clearance(i+1), clearance(i+1))
	}
	if got := c.Rhythm(); got.Crowded {
		t.Errorf("four checks in and the run is already told to slow down: %+v", got)
	}

	c.Answer(clearance(4), clearance(5))
	got := c.Rhythm()
	if !got.Crowded {
		t.Errorf("five checks at one request each and nothing is said: %+v", got)
	}

	// And a run that meets them seldom is left alone, however many it has met.
	var easy Challenges
	for i := range 10 {
		easy.Answer(clearance(i), clearance(i+1))
		for range 8 {
			easy.Answer(clearance(i+1), clearance(i+1))
		}
	}
	if got := easy.Rhythm(); got.Crowded {
		t.Errorf("a run answering eight requests a check is told to slow down: %+v", got)
	}
}

func TestChallenges_ReadAsNothingBeforeAnythingHasHappened(t *testing.T) {
	// A screen draws this while a job is starting, before a single answer. A
	// figure worked out from no checks at all would be a division by nought,
	// and a warning drawn from one would be advice about nothing.
	var c Challenges
	if got := c.Rhythm(); got.Known || got.Crowded || got.Between != 0 {
		t.Errorf("a run that has asked nothing reads %+v, want nothing known", got)
	}
	c.Answer("", "")
	if got := c.Rhythm(); got.Known || got.Crowded {
		t.Errorf("a run that has met no check reads %+v, want no figure to report", got)
	}
	// And a run counting nothing — one whose ports are the identities, where
	// there is no session to hold a clearance — is read by the same screen.
	var none *Challenges
	none.Answer("", "won")
	if got := none.Rhythm(); got != (Rhythm{}) {
		t.Errorf("a run with nothing counting reads %+v, want nothing", got)
	}
}

// clearance is the nth clearance a session might be given, so a test can say
// "and then it was made to pass another".
func clearance(n int) string {
	return string(rune('a' + n))
}
