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
	c.Answer(1, true, "", "")            // a session with no clearance, and still none
	c.Answer(1, true, "", "first")       // one just passed a check
	c.Answer(1, true, "first", "first")  // the same session, asking again on its clearance
	c.Answer(1, true, "first", "second") // and made to pass another

	got := c.Rhythm()
	if got.Met != 2 || got.Intervals != 1 {
		t.Errorf("read %+v, want two checks and the one stretch between them", got)
	}
	if !got.Known || got.Between != 1 {
		t.Errorf("the session carried %v requests between its checks (known=%v), want one", got.Between, got.Known)
	}
}

func TestChallenges_CountWhatOneSessionCarriesBetweenItsOwnTwoChecks(t *testing.T) {
	// What the figure is: how many requests a session carries between one check
	// and its next. It used to be every session's requests over every session's
	// checks, which reads the same only once a run has gone on long enough for
	// nothing else to be counted in it — and a run opening on sessions that
	// rested between jobs is nothing but checks that are not about the pace.
	// Here two sessions are asked in turn: thirty requests between one's checks,
	// ten between the other's, and the answer is twenty.
	c := counting()
	c.Answer(1, true, "", "a")
	c.Answer(2, true, "", "b")
	for i := range 30 {
		c.Answer(1, true, "a", "a")
		if i < 10 {
			c.Answer(2, true, "b", "b")
		}
	}
	c.Answer(2, true, "b", "b2")
	c.Answer(1, true, "a", "a2")

	got := c.Rhythm()
	if got.Intervals != 2 || got.Between != 20 {
		t.Errorf("read %+v, want two stretches of thirty and ten, twenty between checks", got)
	}
}

func TestChallenges_OpenASessionsCountOnItsFirstCheckOfTheRun(t *testing.T) {
	// A session taken up from the history after resting between jobs meets a
	// check on its first request of the run: Google looks again at a session it
	// has not seen for a while, whatever pace it is asked at. That check opens
	// the session's count and closes none — the stretch before it began before
	// the run, and the requests in it were counted there.
	c := counting()
	for s := range 8 {
		c.Answer(int64(s), true, "slept", "woken")
	}
	got := c.Rhythm()
	if got.Met != 8 {
		t.Errorf("%d checks were reported, want the eight that were paid for", got.Met)
	}
	if got.Known || got.Crowded || got.Intervals != 0 {
		t.Errorf("a run of nothing but sessions woken up reads %+v, want nothing to judge yet", got)
	}

	// And the requests that follow, up to each session's next check, are what
	// the figure is made of.
	for s := range 8 {
		for range 12 {
			c.Answer(int64(s), true, "woken", "woken")
		}
		c.Answer(int64(s), true, "woken", "again")
	}
	if got := c.Rhythm(); got.Met != 16 || got.Intervals != 8 || got.Between != 12 {
		t.Errorf("read %+v, want sixteen checks and eight stretches of twelve", got)
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
	for s := range 8 {
		c.Answer(int64(s), false, "", "let-in") // eight fresh sessions, each let in
	}
	got := c.Rhythm()
	if got.Met != 8 {
		t.Errorf("%d checks were reported, want the eight that were paid for", got.Met)
	}
	if got.Known || got.Crowded {
		t.Errorf("a run of nothing but fresh sessions reads %+v, want no rhythm to judge", got)
	}

	// And once one of them is asking again, its answers up to its next check
	// are what the rhythm is made of.
	for range 20 {
		c.Answer(0, true, "let-in", "let-in")
	}
	c.Answer(0, true, "let-in", "again")
	if got := c.Rhythm(); got.Met != 9 || got.Intervals != 1 || got.Between != 20 {
		t.Errorf("read %+v, want nine checks in all and twenty requests for the one that counts", got)
	}
}

func TestChallenges_OpenTheCountOfASessionLetInWithoutACheckOnItsFirstCheck(t *testing.T) {
	// Google does not always check a session it has not seen: some are let in
	// on their first request. What such a session carries before its first check
	// is not a stretch between two checks, and that first check opens its count
	// rather than ending one.
	c := counting()
	c.Answer(1, false, "", "")
	for range 5 {
		c.Answer(1, true, "", "")
	}
	c.Answer(1, true, "", "first")
	if got := c.Rhythm(); got.Met != 1 || got.Intervals != 0 || got.Known {
		t.Errorf("a session let in without a check read %+v at its first check, want a count opened and none ended", got)
	}
	for range 9 {
		c.Answer(1, true, "first", "first")
	}
	c.Answer(1, true, "first", "second")
	if got := c.Rhythm(); got.Intervals != 1 || got.Between != 9 {
		t.Errorf("read %+v, want the one stretch of nine between its two checks", got)
	}
}

func TestChallenges_StartASessionsCountAgainWhenItMoves(t *testing.T) {
	// A session moved to another address arrives there a stranger and pays a
	// check for the address rather than for the pace. That check closes nothing:
	// the stretch it ends was cut short by the move. The count starts again
	// from it.
	c := counting()
	c.Answer(1, true, "", "here")
	for range 5 {
		c.Answer(1, true, "here", "here")
	}
	c.Answer(1, false, "here", "there") // moved, and let in at the new address
	if got := c.Rhythm(); got.Intervals != 0 || got.Known {
		t.Errorf("a move closed a stretch: %+v", got)
	}
	for range 7 {
		c.Answer(1, true, "there", "there")
	}
	c.Answer(1, true, "there", "there-again")
	if got := c.Rhythm(); got.Intervals != 1 || got.Between != 7 {
		t.Errorf("read %+v, want the one stretch of seven after the move", got)
	}
}

func TestChallenges_CountNothingWhereAClearanceIsOnlyCarried(t *testing.T) {
	// A session taken up again brings back the clearance it was written down
	// with. Counted as a check, every session resumed from the history would
	// read as one that had just paid for one — and the screen would advise
	// slowing a run down for work it never did.
	c := counting()
	for range 10 {
		c.Answer(1, true, "kept", "kept")
	}
	if got := c.Rhythm(); got.Met != 0 || got.Known {
		t.Errorf("read %+v from a session asking on the clearance it had, want no check", got)
	}
	// And a clearance that has gone — the cookie ran out, or the session was
	// given a fresh jar — is not a check either.
	c.Answer(1, true, "kept", "")
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
	c.Answer(1, true, clearance(0), clearance(1))
	for i := 1; i < 5; i++ {
		c.Answer(1, true, clearance(i), clearance(i))
		c.Answer(1, true, clearance(i), clearance(i+1))
	}
	if got := c.Rhythm(); got.Crowded {
		t.Errorf("four stretches in and the run is already told to slow down: %+v", got)
	}

	c.Answer(1, true, clearance(5), clearance(5))
	c.Answer(1, true, clearance(5), clearance(6))
	got := c.Rhythm()
	if !got.Crowded {
		t.Errorf("five stretches of one request each and nothing is said: %+v", got)
	}

	// And a run that meets them seldom is left alone, however many it has met.
	easy := counting()
	easy.Answer(1, true, clearance(0), clearance(1))
	for i := 1; i <= 10; i++ {
		for range 8 {
			easy.Answer(1, true, clearance(i), clearance(i))
		}
		easy.Answer(1, true, clearance(i), clearance(i+1))
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
	c.Answer(1, true, "was", "new")
	for i := range 6 {
		c.Answer(1, true, clearance(i), clearance(i))
		c.Answer(1, true, clearance(i), clearance(i+1))
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
	c.Answer(1, true, "", "")
	if got := c.Rhythm(); got.Known || got.Crowded {
		t.Errorf("a run that has met no check reads %+v, want no figure to report", got)
	}
	// And a run counting nothing — one whose ports are the identities, where
	// there is no session to hold a clearance — is read by the same screen.
	var none *Challenges
	none.Answer(1, true, "", "won")
	if got := none.Rhythm(); got != (Rhythm{}) {
		t.Errorf("a run with nothing counting reads %+v, want nothing", got)
	}
}

// clearance is the nth clearance a session might be given, so a test can say
// "and then it was made to pass another".
func clearance(n int) string {
	return string(rune('a' + n))
}
