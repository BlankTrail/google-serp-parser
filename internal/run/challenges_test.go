// SPDX-License-Identifier: MIT

package run

import (
	"testing"
	"time"
)

// counting is a meter for a run that began long enough ago to be judged, so a
// test about the rhythm is not also a test about the waiting period.
func counting() *Challenges {
	c := NewChallenges()
	c.began = c.now().Add(-time.Hour)
	return c
}

func TestChallenges_ReadANewClearanceAsACheckPaidFor(t *testing.T) {
	// Google issues its clearance when a check is passed, so a session holding
	// one it did not hold before has just paid for one. The page that came back
	// says nothing about it — it is the page that was asked for.
	c := counting()
	c.Answer(true, "", "")            // a session with no clearance, and still none
	c.Answer(true, "", "first")       // one just passed a check
	c.Answer(true, "first", "first")  // the same session, asking again on its clearance
	c.Answer(true, "first", "second") // and made to pass another

	got := c.Rhythm()
	if got.Met != 2 || got.Asked != 4 || got.AskedMet != 2 {
		t.Errorf("read %+v, want two checks among four requests", got)
	}
	if !got.Known || got.Between != 1 {
		t.Errorf("the run gets %v requests a check (known=%v), want one", got.Between, got.Known)
	}
}

func TestChallenges_LeaveASessionsFirstAnswerOutOfTheRhythm(t *testing.T) {
	// A fresh session pays for a check to be let in at all, whatever pace it is
	// asked at. Counted against the pace, every run that opens sessions would
	// read as one asking too fast — and the remedy the screen offers, a longer
	// rest, makes more fresh sessions rather than fewer.
	//
	// It is still a check somebody waited for, so it is still reported as one.
	c := counting()
	for range 8 {
		c.Answer(false, "", "let-in") // eight fresh sessions, each let in
	}
	got := c.Rhythm()
	if got.Met != 8 {
		t.Errorf("%d checks were reported, want the eight that were paid for", got.Met)
	}
	if got.Asked != 0 || got.Known || got.Crowded {
		t.Errorf("a run of nothing but fresh sessions reads %+v, want no rhythm to judge", got)
	}

	// And once those sessions are asking again, it is their answers that the
	// rhythm is made of.
	for range 20 {
		c.Answer(true, "let-in", "let-in")
	}
	c.Answer(true, "let-in", "again")
	if got := c.Rhythm(); got.Met != 9 || got.AskedMet != 1 || got.Between != 20 {
		t.Errorf("read %+v, want nine checks in all and twenty requests for the one that counts", got)
	}
}

func TestChallenges_CountNothingWhereAClearanceIsOnlyCarried(t *testing.T) {
	// A session taken up again brings back the clearance it was written down
	// with. Counted as a check, every session resumed from the history would
	// read as one that had just paid for one — and the screen would advise
	// slowing a run down for work it never did.
	c := counting()
	for range 10 {
		c.Answer(true, "kept", "kept")
	}
	if got := c.Rhythm(); got.Met != 0 || got.Asked != 10 || got.Known {
		t.Errorf("read %+v from a session asking on the clearance it had, want ten plain answers", got)
	}
	// And a clearance that has gone — the cookie ran out, or the session was
	// given a fresh jar — is not a check either.
	c.Answer(true, "kept", "")
	if got := c.Rhythm(); got.Met != 0 {
		t.Errorf("a clearance that expired read as %d checks, want none", got.Met)
	}
}

func TestChallenges_SayTheChecksAreCrowdedOnlyOnceThereAreEnoughToJudge(t *testing.T) {
	// The warning is advice about the rest a session takes, and it is worth
	// giving only when the rhythm is a rhythm. One check early on is one
	// session's bad luck — and a run told to slow down after two requests would
	// be told so by noise.
	c := counting()
	for i := range 4 {
		c.Answer(true, clearance(i), clearance(i+1))
		c.Answer(true, clearance(i+1), clearance(i+1))
	}
	if got := c.Rhythm(); got.Crowded {
		t.Errorf("four checks in and the run is already told to slow down: %+v", got)
	}

	c.Answer(true, clearance(4), clearance(5))
	got := c.Rhythm()
	if !got.Crowded {
		t.Errorf("five checks at one request each and nothing is said: %+v", got)
	}

	// And a run that meets them seldom is left alone, however many it has met.
	easy := counting()
	for i := range 10 {
		easy.Answer(true, clearance(i), clearance(i+1))
		for range 8 {
			easy.Answer(true, clearance(i+1), clearance(i+1))
		}
	}
	if got := easy.Rhythm(); got.Crowded {
		t.Errorf("a run answering eight requests a check is told to slow down: %+v", got)
	}
}

func TestChallenges_SayNothingAboutARunThatHasOnlyJustStarted(t *testing.T) {
	// The opening of a run is its sessions being let in, and the checks they pay
	// for land in the first minutes. Judged then, a run starting up reads
	// exactly like a run asking too fast — so the rhythm is reported and the
	// advice is not.
	c := NewChallenges()
	at := c.began
	c.now = func() time.Time { return at.Add(9 * time.Minute) }
	for range 6 {
		c.Answer(true, "was", "new")
		c.Answer(true, "new", "new")
	}
	if got := c.Rhythm(); !got.Known || got.Crowded {
		t.Errorf("nine minutes in the run is already told to slow down: %+v", got)
	}

	c.now = func() time.Time { return at.Add(11 * time.Minute) }
	if got := c.Rhythm(); !got.Crowded {
		t.Errorf("eleven minutes in, at one request a check, nothing is said: %+v", got)
	}
}

func TestChallenges_ReadAsNothingBeforeAnythingHasHappened(t *testing.T) {
	// A screen draws this while a job is starting, before a single answer. A
	// figure worked out from no checks at all would be a division by nought,
	// and a warning drawn from one would be advice about nothing.
	c := counting()
	if got := c.Rhythm(); got.Known || got.Crowded || got.Between != 0 {
		t.Errorf("a run that has asked nothing reads %+v, want nothing known", got)
	}
	c.Answer(true, "", "")
	if got := c.Rhythm(); got.Known || got.Crowded {
		t.Errorf("a run that has met no check reads %+v, want no figure to report", got)
	}
	// And a run counting nothing — one whose ports are the identities, where
	// there is no session to hold a clearance — is read by the same screen.
	var none *Challenges
	none.Answer(true, "", "won")
	if got := none.Rhythm(); got != (Rhythm{}) {
		t.Errorf("a run with nothing counting reads %+v, want nothing", got)
	}
}

// clearance is the nth clearance a session might be given, so a test can say
// "and then it was made to pass another".
func clearance(n int) string {
	return string(rune('a' + n))
}
