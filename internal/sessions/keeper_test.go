// SPDX-License-Identifier: MIT

package sessions

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/store"
)

// history is the history in memory, as the store keeps it: the same rule for
// giving a session up, so a keeper test is testing the keeper and not a second
// idea of when a session dies.
type history struct {
	mu       sync.Mutex
	next     int64
	sessions map[int64]store.Session
	answered map[int64][]byte
	swept    time.Time
}

func newHistory() *history {
	return &history{sessions: map[int64]store.Session{}, answered: map[int64][]byte{}}
}

func (h *history) Sessions(_ context.Context, device string, since time.Time) ([]store.Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []store.Session
	for _, s := range h.sessions {
		if s.Device == device && !s.UsedAt.Before(since) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (h *history) NewSession(_ context.Context, s store.Session) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	s.ID = h.next
	h.sessions[s.ID] = s
	return s.ID, nil
}

func (h *history) SessionAnswered(_ context.Context, id int64, cookies []byte, at time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	if !ok {
		return store.ErrNoSession
	}
	s.Cookies, s.UsedAt, s.Failures = cookies, at, 0
	h.sessions[id] = s
	h.answered[id] = cookies
	return nil
}

func (h *history) SessionFailed(_ context.Context, id int64, at time.Time) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	if !ok {
		return false, store.ErrNoSession
	}
	s.Failures++
	s.UsedAt = at
	if s.Failures >= store.SessionFailuresAllowed {
		delete(h.sessions, id)
		return true, nil
	}
	h.sessions[id] = s
	return false, nil
}

func (h *history) DropStaleSessions(_ context.Context, before time.Time) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.swept = before
	n := 0
	for id, s := range h.sessions {
		if s.UsedAt.Before(before) {
			delete(h.sessions, id)
			n++
		}
	}
	return n, nil
}

// wearer stands for the proxy service: every port wears the profile it was last
// asked for, and a port asked for nothing wears the one it was opened under.
type wearer struct {
	mu      sync.Mutex
	opened  string
	wearing map[int]string
	asked   []string
	refuse  error
}

func newWearer(opened string) *wearer {
	return &wearer{opened: opened, wearing: map[int]string{}}
}

func (w *wearer) WearSession(_ context.Context, port int, profile string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked = append(w.asked, profile)
	if w.refuse != nil {
		return "", w.refuse
	}
	if profile == "" {
		if got, ok := w.wearing[port]; ok {
			return got, nil
		}
		return w.opened, nil
	}
	w.wearing[port] = profile
	return profile, nil
}

// clock is a time a test moves by hand.
type clock struct{ at time.Time }

func (c *clock) now() time.Time       { return c.at }
func (c *clock) pass(d time.Duration) { c.at = c.at.Add(d) }
func newKeeperAt(h History, w Wearer, pause time.Duration, c *clock) *Keeper {
	k := NewKeeper(h, w, "desktop", pause)
	k.now = c.now
	return k
}

func TestKeeper_MakesASessionWhenNoneIsRested(t *testing.T) {
	// How many sessions there are is not a number anybody sets. A thread that
	// finds every session held or still resting gets a new one — which is how
	// a job with a long pause ends up with more sessions than one with a short
	// pause, without either being worked out in advance.
	h, w := newHistory(), newWearer("Chrome_153_win")
	c := &clock{at: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
	k := newKeeperAt(h, w, 5*time.Second, c)
	ctx := context.Background()

	first, err := k.Take(ctx, 20001)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	// The first thread still holds its session, so the second needs another.
	second, err := k.Take(ctx, 20002)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("two threads were handed the same session at once")
	}
	// A new session is the port as it is: what it wears is written down as the
	// session's fingerprint.
	if first.Profile != "Chrome_153_win" {
		t.Errorf("a new session wears %q, want what the port was wearing", first.Profile)
	}
	if all, held := k.Count(); all != 2 || held != 2 {
		t.Errorf("the keeper knows %d sessions and %d held, want 2 and 2", all, held)
	}
}

func TestKeeper_HandsARestedSessionBackOutRatherThanMakingAnother(t *testing.T) {
	// A session that has rested for the job's pause is handed out again, on
	// whatever port the thread holds — its fingerprint put on that port first.
	h, w := newHistory(), newWearer("Chrome_153_win")
	c := &clock{at: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
	k := newKeeperAt(h, w, 5*time.Second, c)
	ctx := context.Background()

	s, _ := k.Take(ctx, 20001)
	s.Jar.SetCookies(at(t, "https://www.google.ru/"), []*http.Cookie{{Name: "GOOGLE_ABUSE_EXEMPTION", Value: "won", Path: "/"}})
	if err := s.Answered(ctx); err != nil {
		t.Fatalf("Answered: %v", err)
	}

	// Still resting: another thread gets a new session instead.
	early, _ := k.Take(ctx, 20002)
	if early.ID == s.ID {
		t.Fatal("a session was handed out again before it had rested for the pause")
	}
	_ = early.Answered(ctx)

	c.pass(5 * time.Second)
	again, err := k.Take(ctx, 20003)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if again.ID != s.ID && again.ID != early.ID {
		t.Fatalf("a rested session was not handed out; a new one was made (%d)", again.ID)
	}
	// And the fingerprint was put on the port it is now on.
	if got := w.wearing[20003]; got != again.Profile {
		t.Errorf("port 20003 wears %q, want the session's %q", got, again.Profile)
	}
}

func TestKeeper_WritesTheCookiesDownOnEveryAnswer(t *testing.T) {
	// A run can end between an answer and the moment the session is let go of
	// — stopped, or the machine gone — and a session whose clearance was never
	// written down is a session that pays for it again next run.
	h, w := newHistory(), newWearer("Chrome_153_win")
	c := &clock{at: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
	k := newKeeperAt(h, w, time.Second, c)
	ctx := context.Background()

	s, _ := k.Take(ctx, 20001)
	s.Jar.SetCookies(at(t, "https://www.google.ru/"), []*http.Cookie{{Name: "GOOGLE_ABUSE_EXEMPTION", Value: "won", Path: "/"}})
	if err := s.Answered(ctx); err != nil {
		t.Fatalf("Answered: %v", err)
	}
	back, err := ReadJar(h.answered[s.ID])
	if err != nil {
		t.Fatalf("ReadJar: %v", err)
	}
	if got := names(back, at(t, "https://www.google.ru/")); got["GOOGLE_ABUSE_EXEMPTION"] != "won" {
		t.Errorf("the history holds %v for the session, want the clearance it answered with", got)
	}
}

func TestKeeper_ResumesTheSessionsAnEarlierRunLeft(t *testing.T) {
	// The reason for writing sessions down: the next run of the job starts on
	// the sessions the last one made, rather than paying for every challenge
	// again. Only the ones used in the last twelve hours; the rest are swept.
	h, w := newHistory(), newWearer("Chrome_153_win")
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	jar := NewJar()
	jar.SetCookies(at(t, "https://www.google.ru/"), []*http.Cookie{{Name: "GOOGLE_ABUSE_EXEMPTION", Value: "yesterday", Path: "/"}})
	written, _ := jar.MarshalJSON()
	recent, _ := h.NewSession(context.Background(), store.Session{
		Profile: "Firefox_155_lin", Device: "desktop", Cookies: written, UsedAt: now.Add(-3 * time.Hour)})
	stale, _ := h.NewSession(context.Background(), store.Session{
		Profile: "Edge_153_win", Device: "desktop", UsedAt: now.Add(-13 * time.Hour)})

	c := &clock{at: now}
	k := newKeeperAt(h, w, 5*time.Second, c)
	s, err := k.Take(context.Background(), 20001)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if s.ID != recent {
		t.Fatalf("the run started on session %d, want the one the last run left (%d)", s.ID, recent)
	}
	if got := names(s.Jar, at(t, "https://www.google.ru/")); got["GOOGLE_ABUSE_EXEMPTION"] != "yesterday" {
		t.Errorf("the resumed session sends %v, want the clearance it was written down with", got)
	}
	if w.wearing[20001] != "Firefox_155_lin" {
		t.Errorf("port 20001 wears %q, want the resumed session's fingerprint", w.wearing[20001])
	}
	if _, ok := h.sessions[stale]; ok {
		t.Error("a session unused for thirteen hours was not swept")
	}
}

func TestKeeper_GivesASessionUpAtTheSecondRefusalInARow(t *testing.T) {
	h, w := newHistory(), newWearer("Chrome_153_win")
	c := &clock{at: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
	k := newKeeperAt(h, w, time.Second, c)
	ctx := context.Background()

	s, _ := k.Take(ctx, 20001)
	if dropped, err := s.Failed(ctx); err != nil || dropped {
		t.Fatalf("the first refusal: dropped=%v err=%v", dropped, err)
	}
	c.pass(time.Second)
	again, _ := k.Take(ctx, 20001)
	if again.ID != s.ID {
		t.Fatalf("after one refusal the session was not handed out again")
	}
	dropped, err := again.Failed(ctx)
	if err != nil {
		t.Fatalf("Failed: %v", err)
	}
	if !dropped {
		t.Fatal("the second refusal in a row left the session standing")
	}
	if all, _ := k.Count(); all != 0 {
		t.Errorf("the keeper still knows %d sessions after giving the only one up", all)
	}
}

func TestKeeper_LeavesASessionThePortWouldNotTakeForAnotherThread(t *testing.T) {
	// A port that will not take a profile is the port's trouble and not the
	// session's. Giving the session up over it would throw away a clearance
	// that another port would have carried.
	h, w := newHistory(), newWearer("Chrome_153_win")
	c := &clock{at: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
	k := newKeeperAt(h, w, time.Second, c)
	ctx := context.Background()

	s, _ := k.Take(ctx, 20001)
	_ = s.Answered(ctx)
	c.pass(time.Second)

	w.refuse = errors.New("the port is gone")
	if _, err := k.Take(ctx, 20009); err == nil {
		t.Fatal("a port that refused the profile was handed the session anyway")
	}
	w.refuse = nil
	again, err := k.Take(ctx, 20002)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if again.ID != s.ID {
		t.Errorf("the session was lost over one port refusing it")
	}
}

func TestHeld_CannotBeGivenBackTwice(t *testing.T) {
	// Given back twice, a session would be counted rested twice and handed to
	// two threads at once — two identities that are one.
	h, w := newHistory(), newWearer("Chrome_153_win")
	c := &clock{at: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
	k := newKeeperAt(h, w, time.Second, c)
	s, _ := k.Take(context.Background(), 20001)
	if err := s.Answered(context.Background()); err != nil {
		t.Fatalf("Answered: %v", err)
	}
	if err := s.Answered(context.Background()); !errors.Is(err, ErrNotHeld) {
		t.Errorf("a session given back twice answered %v, want ErrNotHeld", err)
	}

	// The case that matters: the session has rested and another thread holds
	// it now. The first thread's old handle must not be able to let it go out
	// from under the second — that would hand the one session to a third
	// thread while the second is still searching with it.
	c.pass(time.Second)
	other, err := k.Take(context.Background(), 20002)
	if err != nil || other.ID != s.ID {
		t.Fatalf("the rested session was not handed to the next thread (%v, %v)", other, err)
	}
	if err := s.Answered(context.Background()); !errors.Is(err, ErrNotHeld) {
		t.Errorf("a stale handle gave back a session another thread holds: %v", err)
	}
	if _, held := k.Count(); held != 1 {
		t.Errorf("%d sessions are held after the stale handle, want the other thread's one", held)
	}
}

func TestHeld_SavesThePagesOfAWalkWithoutLettingTheSessionGo(t *testing.T) {
	// A query walked page by page keeps one session for all its pages, so it
	// cannot give the session back after each one — but a run can end between
	// two pages, and a clearance won on page one and never written down is one
	// the next run pays for again.
	h, w := newHistory(), newWearer("Chrome_153_win")
	c := &clock{at: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
	k := newKeeperAt(h, w, time.Minute, c)
	ctx := context.Background()

	s, _ := k.Take(ctx, 20001)
	s.Jar.SetCookies(at(t, "https://www.google.ru/"), []*http.Cookie{{Name: "GOOGLE_ABUSE_EXEMPTION", Value: "page-one", Path: "/"}})
	if err := s.Save(ctx); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Written down…
	back, _ := ReadJar(h.answered[s.ID])
	if got := names(back, at(t, "https://www.google.ru/")); got["GOOGLE_ABUSE_EXEMPTION"] != "page-one" {
		t.Errorf("after the first page the history holds %v", got)
	}
	// …and still held: another thread asking now gets a different session.
	other, _ := k.Take(ctx, 20002)
	if other.ID == s.ID {
		t.Error("a session saved mid-walk was handed to another thread")
	}
	// The walk goes on and ends normally.
	if err := s.Answered(ctx); err != nil {
		t.Errorf("the walk could not give its session back after saving it: %v", err)
	}
	// And a handle that has been given back cannot save.
	if err := s.Save(ctx); !errors.Is(err, ErrNotHeld) {
		t.Errorf("a session given back could still be saved: %v", err)
	}
}
