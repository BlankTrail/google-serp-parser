// SPDX-License-Identifier: MIT

package web

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/google"
	"github.com/blanktrail/google-serp-parser/store"
)

// The shape of the one job the state tests watch. The numbers are all different
// from one another so that a screen reading the wrong one is caught: a page that
// showed the depth where the thread count belongs, or the failures where the
// finished queries belong, would otherwise show a number that happens to fit.
const (
	stateTotal   = 9
	statePages   = 3
	stateDone    = 2
	stateFailed  = 4
	statePending = stateTotal - stateDone - stateFailed
)

// wallBody is a page that says the traffic was unusual, and shellBody a page
// that carries no results and no explanation. Both go through the real
// classifier, so a fixture that stopped meaning what it says is caught in the
// helper rather than read as a screen that lost a whole kind of answer.
const (
	wallBody  = `<html><body>Our systems have detected unusual traffic from your computer network.</body></html>`
	shellBody = `<html><body><div id="main"></div></body></html>`
)

// refusal is a real refusal of the kind a query comes back with, built by the
// code that builds them, and checked here to carry the class the test means.
func refusal(t *testing.T, body string, want google.Class) error {
	t.Helper()
	_, err := google.ParseSERP("iphone 13", []byte(body))
	if err == nil {
		t.Fatalf("the fixture body was read as a result page, so it refuses nothing")
	}
	got, ok := google.ClassOf(err)
	if !ok || got != want {
		t.Fatalf("the fixture came back as class %q (%v), want %q", got, ok, want)
	}
	return err
}

// silence is a query that never got an answer at all: nothing was classified,
// because nothing arrived.
func silence() error { return errors.New("a port stopped answering") }

// stateServer builds a server whose supervisor is holding one job in the middle
// of itself, with some of that job already refused.
//
// A job at rest looks the same however the screen was built, so every question
// about the screen an operator watches has to be asked while something is
// actually running.
func stateServer(t *testing.T) (*Server, *Supervisor, int64) {
	t.Helper()
	st := testStore(t)
	eng := &heldEngine{hold: make(chan struct{})}
	v := newSupervisor(st, eng)
	t.Cleanup(func() { _ = v.Close() })

	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := t.Context()
	queries := make([]string, stateTotal)
	for i := range queries {
		queries[i] = "query " + strconv.Itoa(i+1)
	}
	id, err := st.CreateJob(ctx, store.JobSpec{Name: "nightly", Pages: statePages}, queries)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for i := range stateDone {
		if err := st.Record(ctx, id, store.QueryOutcome{Ordinal: i}); err != nil {
			t.Fatalf("recording query %d as done: %v", i, err)
		}
	}
	// Two of one kind and one each of two others, so a screen that collapsed
	// every refusal into one bucket, or reported only the commonest, is caught.
	for i, why := range []error{
		refusal(t, wallBody, google.ClassWall),
		refusal(t, wallBody, google.ClassWall),
		refusal(t, shellBody, google.ClassShell),
		silence(),
	} {
		if err := st.Record(ctx, id, store.QueryOutcome{Ordinal: stateDone + i, Err: why}); err != nil {
			t.Fatalf("recording query %d as refused: %v", stateDone+i, err)
		}
	}

	if err := v.Resume(id); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})
	return s, v, id
}

// idleServer is a server with a supervisor and nothing whatever to run.
func idleServer(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	v := newSupervisor(st, &heldEngine{hold: make(chan struct{})})
	t.Cleanup(func() { _ = v.Close() })

	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// verdicts are the words a program reaches for when it has decided something on
// the reader's behalf. None of them belongs on this screen in either language.
//
// Facts are not in the list: "failed", "expected" and "left" all say what
// happened or what was quoted, and none of them says whether that is good.
var verdicts = []string{
	"slow", "faster", "behind", "delay", "too many", "too few",
	"should", "warning", "attention", "problem", "degrad", "unhealthy",
	"worse", "trouble", "critical", "danger",
	"медленн", "быстре", "отста", "задержк", "внимание", "проблем",
	"деград", "хуже", "тревог", "критич", "опасн", "слишком",
}

// glyphs are conclusions drawn instead of written. An icon beside a number is
// the same claim as a sentence about it, made where no test would read it.
var glyphs = []string{"⚠", "❗", "‼", "❌", "🔴", "🟡", "🟢", "✗", "✘"}

// verdictIn reports whether a piece of text passes judgement, and on which word.
func verdictIn(text string) (string, bool) {
	lower := strings.ToLower(text)
	for _, word := range verdicts {
		if strings.Contains(lower, word) {
			return word, true
		}
	}
	return "", false
}

func TestState_ShowsTheNumbersWithoutDrawingAConclusion(t *testing.T) {
	// The rule this screen is built on: it shows "24m elapsed, 16m expected" and
	// does not write "running slower than expected". The reader knows what the
	// program does not — that the address list is fresh, that the job is large,
	// that it is night where the target is — and a program that judges wrongly
	// once is not believed afterwards about anything it says.
	//
	// The catalogue is read as well as the pages, because a phrase carrying a
	// verdict is a verdict whether or not any page uses it yet, and the day
	// somebody adds one this has to go red rather than wait for a screenshot.
	for _, l := range Languages() {
		for key, text := range catalogue[l] {
			if word, judged := verdictIn(text); judged {
				t.Errorf("%s says %q under %s, which decides for the reader on %q", l, text, key, word)
			}
			if word, judged := verdictIn(key); judged {
				t.Errorf("the catalogue holds a key named %q, which is a verdict on %q", key, word)
			}
			for _, glyph := range glyphs {
				if strings.Contains(text, glyph) {
					t.Errorf("%s draws %s into %q under %s", l, glyph, text, key)
				}
			}
		}
	}

	// Both the screen with a job on it and the screen without one, in both
	// languages: a sentence written straight into the markup never passes
	// through the catalogue, and a branch nobody renders is a branch nobody read.
	running, _, _ := stateServer(t)
	for _, s := range []*Server{running, idleServer(t)} {
		for _, l := range Languages() {
			body := get(t, s, stateAt+"?lang="+string(l)).Body.String()
			if word, judged := verdictIn(body); judged {
				t.Errorf("the %s screen decides for the reader on %q:\n%s", l, word, body)
			}
		}
	}

	// The screen had every opportunity to draw the conclusion: the figures it
	// would be drawn from are on it.
	body := get(t, running, stateAt).Body.String()
	elapsed, share := shown(t, body, "run-elapsed"), shown(t, body, "ok-share")
	if elapsed == "" || share == "" {
		t.Fatalf("the screen shows elapsed %q against a share of %q, so it never had them to judge by",
			elapsed, share)
	}
	if share == elapsed {
		t.Fatalf("the screen shows the same figure twice (%q), so nothing on it invited a conclusion", share)
	}

	// And the reading itself works. Without this the whole test passes on a
	// scanner that finds nothing anywhere, which is what a scanner that reads
	// the wrong thing does.
	for _, judged := range []string{
		"this job is running slower than expected",
		"идёт медленнее ожидания",
	} {
		if _, found := verdictIn(judged); !found {
			t.Errorf("the reading in this test does not recognise %q as a verdict", judged)
		}
	}
}

func TestState_ReservesColourAndIconsForWhatIsActuallyBroken(t *testing.T) {
	// One accent per screen, and it is the bar. Nothing here is broken yet, so
	// nothing else on this screen is filled or marked: a warning drawn beside an
	// ordinary number is the same claim as a sentence about it, made where the
	// reader cannot argue with it.
	s, _, _ := stateServer(t)
	body := get(t, s, stateAt).Body.String()

	if drawn := strings.Count(body, "<progress"); drawn > 1 {
		t.Errorf("the screen fills %d bars with the accent, and one screen has one accent", drawn)
	}
	for _, glyph := range glyphs {
		if strings.Contains(body, glyph) {
			t.Errorf("the screen draws %s beside a number nothing is wrong with:\n%s", glyph, body)
		}
	}
}

func TestState_ShowsHowLongAndHowFastSideBySide(t *testing.T) {
	// The three figures somebody watching reads together — how long, how fast,
	// how much longer — have to be read in one glance. One at the top of the
	// screen and the others in a footnote is the same comparison made impossible.
	//
	// The clock is held still so the elapsed figure is a number this test knows
	// rather than however long the fixture took to build.
	s, _, id := stateServer(t)
	sum, err := s.store.Progress(t.Context(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	s.now = func() time.Time { return sum.CreatedAt.Add(24 * time.Minute) }

	body := get(t, s, stateAt).Body.String()
	if got := shown(t, body, "run-elapsed"); got != "24m" {
		t.Errorf("the screen says %q has passed, and 24 minutes have", got)
	}

	// In order and with nothing between them. The two speeds stand together
	// because they are one measurement told two ways: the queries a job settles,
	// and the pages it asks Google for to settle them — which on a job taken a
	// hundred pages deep differ by a factor of a hundred.
	from := strings.Index(body, `id="run-elapsed"`)
	through := strings.Index(body, `id="run-speed"`)
	pages := strings.Index(body, `id="run-page-speed"`)
	to := strings.Index(body, `id="run-rest"`)
	if from < 0 || through < 0 || pages < 0 || to < 0 ||
		from >= through || through >= pages || pages >= to {
		t.Fatalf("the screen does not carry the four figures in order:\n%s", body)
	}
	if between := body[from:to]; strings.Count(between, `id="`) != 3 {
		t.Errorf("something else stands between how long it has taken and how much is left:\n%s", between)
	}
}

func TestState_CarriesNoEstimateMadeBeforeTheRun(t *testing.T) {
	// The figure that used to stand here was worked out before a single query
	// went out, and what dominates it — the time to reach an identity that
	// answers — has been measured at anything from half a minute to nine. Beside
	// a real elapsed time it read as a promise, and a promise that wrong makes
	// every other figure on the screen suspect.
	s, _, _ := stateServer(t)
	for _, l := range Languages() {
		body := get(t, s, stateAt+"?lang="+string(l)).Body.String()
		if strings.Contains(body, `id="run-expected"`) {
			t.Errorf("the %s screen still quotes an estimate:\n%s", l, body)
		}
	}
	for _, key := range []string{"state.expected"} {
		for _, l := range Languages() {
			if l.T(key) != key {
				t.Errorf("the catalogue still holds %q in %s, so something can still draw it", key, l)
			}
		}
	}
}

func TestRunningView_SaysWhatIsLeftAtTheSpeedTheJobIsKeepingNow(t *testing.T) {
	// The arithmetic, asked of the thing that does it. Two queries settled over
	// twenty minutes is a tenth of a query a minute; three left at that speed is
	// half an hour. Nothing here is quoted from before the run.
	s := &Server{now: func() time.Time { return time.Date(2026, 8, 17, 12, 30, 0, 0, time.UTC) }}
	sum := store.JobSummary{
		ID: 1, Name: "nightly", Total: 5, Done: 2, Pending: 3,
		CreatedAt: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
	}
	view := s.runningView(sum, store.Pace{Settled: 2, Over: 20 * time.Minute, Known: true})

	if view.Rest != spell(30*time.Minute) {
		t.Errorf("the screen says %q is left at this speed, and %q is", view.Rest, spell(30*time.Minute))
	}
	if view.Speed != "0.1" {
		t.Errorf("the screen says the job is doing %q a minute, and it is doing 0.1", view.Speed)
	}
	if view.Elapsed != "30m" {
		t.Errorf("the screen says %q has passed, and thirty minutes have", view.Elapsed)
	}
}

func TestRunningView_SaysNothingRatherThanNoughtBeforeThereIsASpeed(t *testing.T) {
	// Nought a minute is the speed of a job that has stopped. A job nobody has
	// measured yet has not stopped, and an operator reading nought reaches for
	// the stop button.
	s := &Server{now: time.Now}
	sum := store.JobSummary{ID: 1, Total: 5, Pending: 5, CreatedAt: time.Now()}
	view := s.runningView(sum, store.Pace{})

	if view.Speed != noFigure {
		t.Errorf("a job with nothing measured yet reports a speed of %q", view.Speed)
	}
	if view.Rest != noFigure {
		t.Errorf("a job with nothing measured yet reports %q left", view.Rest)
	}
}

func TestState_ShowsTheSpeedItIsRunningAtNow(t *testing.T) {
	// And the figure reaches the page, since a figure nothing carries onto the
	// screen is a figure nobody has.
	s, _, _ := stateServer(t)
	body := get(t, s, stateAt).Body.String()
	if shown(t, body, "run-speed") == "" {
		t.Errorf("the screen carries no speed at all:\n%s", body)
	}
}

func TestState_BreaksFailuresDownByWhatCameBack(t *testing.T) {
	// "Nine tenths came back" sends the reader to the database for the other
	// tenth. What came back is written down when the query is settled, and the
	// whole point of showing the share is to be able to look under it.
	s, _, _ := stateServer(t)
	body := get(t, s, stateAt).Body.String()

	settled := stateDone + stateFailed
	if got, want := shown(t, body, "ok-share"), strconv.Itoa(stateDone*100/settled)+"%"; got != want {
		t.Errorf("the screen says %q of the settled queries came back, and %q did", got, want)
	}
	// Two came back as one kind and one each as two others. A screen that summed
	// them into a single reason answers three where each of these wants its own.
	for cell, want := range map[string]int{
		"reason-wall": 2, "reason-shell": 1, "reason-silent": 1,
	} {
		if got := shown(t, body, cell); got != strconv.Itoa(want) {
			t.Errorf("the screen counts %s as %q, want %d", cell, got, want)
		}
	}
	// The share is shown over what it was taken over, because a percentage with
	// no base under it is a number nobody can check.
	for cell, want := range map[string]int{
		"ok-count": stateDone, "fail-count": stateFailed,
		"fail-settled": stateDone + stateFailed,
	} {
		if got := shown(t, body, cell); got != strconv.Itoa(want) {
			t.Errorf("the screen shows %s = %q, want %d", cell, got, want)
		}
	}
	// And it says what each of them is, rather than printing a word out of the
	// classifier at a reader who has never seen one.
	for _, key := range []string{"state.class.wall", "state.class.shell", "state.class.silent"} {
		if !strings.Contains(body, LangEN.T(key)) {
			t.Errorf("the screen does not name %s:\n%s", key, body)
		}
	}
}

func TestState_NamesEveryKindOfAnswerInBothLanguages(t *testing.T) {
	// A kind of answer with no phrase behind it reaches the screen as the name of
	// a key, on the day that kind first comes back — which is the day nobody is
	// reading carefully.
	for _, r := range reasons {
		for _, l := range Languages() {
			if l.T(r.Key) == r.Key {
				t.Errorf("%s has no phrase for %s", l, r.Key)
			}
		}
	}
	if len(reasons) < 2 {
		t.Fatal("there is at most one kind of answer, so this test read nothing")
	}
}

func TestState_ShowsThePoolWithoutAJobRunning(t *testing.T) {
	// The pool degrading is one of the three things that make an operator step
	// in, and it degrades between jobs as readily as during one. A screen that
	// only reported it while something was running would go quiet exactly when
	// the ports are being left alone to recover.
	s := idleServer(t)
	body := get(t, s, stateAt).Body.String()

	for cell, want := range map[string]string{
		"pool-alive":       strconv.Itoa(fakePool.Stats.Available),
		"pool-ports":       strconv.Itoa(fakePool.Stats.Ports),
		"pool-rotations":   strconv.FormatInt(fakePool.Stats.EgressRotations, 10),
		"pool-quarantined": strconv.Itoa(fakePool.Stats.Quarantined),
		"pool-revived":     strconv.FormatInt(fakePool.Stats.Revivals, 10),
	} {
		if got := shown(t, body, cell); got != want {
			t.Errorf("the screen shows %s = %q, and the pool says %s", cell, got, want)
		}
	}
	if strings.Contains(body, `id="run-elapsed"`) {
		t.Errorf("the screen draws a running job while nothing is running:\n%s", body)
	}
}

func TestState_InvitesAJobWhenThereIsNothingToShow(t *testing.T) {
	// An empty screen reads as a broken one. A screen that says nothing is
	// running and offers to start something reads as a working program with
	// nothing to do.
	idle := get(t, idleServer(t), stateAt).Body.String()
	if !strings.Contains(idle, LangEN.T("state.invite")) {
		t.Errorf("the screen neither shows a job nor offers to start one:\n%s", idle)
	}

	// And it stops offering once there is something to watch, or the invitation
	// is furniture rather than an answer.
	s, _, _ := stateServer(t)
	if busy := get(t, s, stateAt).Body.String(); strings.Contains(busy, LangEN.T("state.invite")) {
		t.Errorf("the screen offers to start a job while one is running:\n%s", busy)
	}
}

func TestState_CountsTheQueueFromTheSupervisorNotTheHistory(t *testing.T) {
	// A job waiting its turn need not be in the history as waiting: it is written
	// down when it is created, and every unfinished job in there looks pending.
	// The supervisor is the only thing that knows what will actually be taken
	// next, and a screen counting rows instead would report jobs nobody queued.
	s, v, _ := stateServer(t)

	enqueue(t, v, "second in line", "a")
	enqueue(t, v, "third in line", "b")
	waitUntil(t, "two jobs are waiting", func() bool { return len(v.Queued()) == 2 })

	// Two more jobs that are unfinished, pending in every row of the history, and
	// in nobody's queue.
	for _, name := range []string{"shelved one", "shelved two"} {
		if _, err := s.store.CreateJob(t.Context(),
			store.JobSpec{Name: name, Pages: 1}, []string{"x"}); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
	}

	body := get(t, s, stateAt).Body.String()
	if got := shown(t, body, "queue-waiting"); got != "2" {
		t.Errorf("the screen says %q jobs are waiting, and two are", got)
	}
	for _, name := range []string{"second in line", "third in line"} {
		if !strings.Contains(body, name) {
			t.Errorf("the screen does not name %q, which is waiting its turn:\n%s", name, body)
		}
	}
	for _, name := range []string{"shelved one", "shelved two"} {
		if strings.Contains(body, name) {
			t.Errorf("the screen counts %q, which nobody queued:\n%s", name, body)
		}
	}
	// The queue is the jobs behind the one in flight, never the one in flight.
	if strings.Contains(body[strings.Index(body, `id="queue-waiting"`):], "nightly") {
		t.Errorf("the running job is listed among the ones waiting for it:\n%s", body)
	}
}

func TestState_ShowsNoBareKeyWhereAPhraseBelongs(t *testing.T) {
	// A key on the screen is a phrase that was never looked up. It lands on
	// whichever phrase nobody wrote a test about, so this one is over all of them
	// at once, in both languages, with a job running and with none.
	running, _, _ := stateServer(t)
	for _, s := range []*Server{running, idleServer(t), testServer(t)} {
		for _, l := range Languages() {
			body := get(t, s, stateAt+"?lang="+string(l)).Body.String()
			for key := range catalogue[l] {
				if strings.Contains(body, key) {
					t.Errorf("the %s screen shows the key %q where its text belongs", l, key)
				}
			}
		}
	}
}

func TestSpell_WritesADurationTheWayAScreenIsRead(t *testing.T) {
	// A screen is glanced at. "1h32m17.394s" is a measurement; "1h 32m" is an
	// answer, and the seconds under it were never worth the width.
	for _, c := range []struct {
		of   time.Duration
		want string
	}{
		{0, "0s"},
		{-time.Minute, "0s"},
		{45 * time.Second, "45s"},
		{24 * time.Minute, "24m"},
		{24*time.Minute + 29*time.Second, "24m"},
		{time.Hour + 32*time.Minute, "1h 32m"},
		{2*time.Hour - 20*time.Second, "2h"},
	} {
		if got := spell(c.of); got != c.want {
			t.Errorf("spell(%v) = %q, want %q", c.of, got, c.want)
		}
	}
}

func TestState_CountsWhatCameBackRatherThanWhatDidNot(t *testing.T) {
	// A share of refusals reads as a fault report even at nought, and the figure
	// somebody glances at while a job runs is whether it is working. It counts
	// up, from a hundred.
	//
	// The fixture is deliberately a bad run — four refused against two answered
	// — so a screen that swapped the two figures reads plausibly and is caught by
	// the arithmetic rather than by the look of it.
	s, _, _ := stateServer(t)
	body := get(t, s, stateAt).Body.String()

	settled := stateDone + stateFailed
	if got := shown(t, body, "ok-share"); got != strconv.Itoa(stateDone*100/settled)+"%" {
		t.Errorf("the screen reports %q, and %d of %d settled queries came back",
			got, stateDone, settled)
	}
	for _, l := range Languages() {
		body := get(t, s, stateAt+"?lang="+string(l)).Body.String()
		if !strings.Contains(body, l.T("state.answered")) {
			t.Errorf("the %s screen does not name what the share is of:\n%s", l, body)
		}
		if strings.Contains(body, l.T("state.failures")) {
			t.Errorf("the %s screen still leads with the refusals:\n%s", l, body)
		}
	}
}
