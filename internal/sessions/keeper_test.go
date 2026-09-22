// SPDX-License-Identifier: MIT

package sessions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"
)

// port stands for a leased port of the proxy service: it goes out where it was
// last moved, wears what it was last given, holds the tickets it was last
// loaded with, and writes down every call in the order it came.
type port struct {
	mu        sync.Mutex
	num       int
	exit      string
	list      []string
	resting   map[string]bool
	limit     int
	wearing   string
	fresh     Fingerprint
	freshened int
	tickets   []byte
	calls     []string
	elsewhere bool
	refuse    error
}

func listPort(num int, on string, list ...string) *port {
	return &port{num: num, exit: addrExit + on, list: list, resting: map[string]bool{},
		fresh: Fingerprint{Profile: "Chrome_153_win", Browser: "chrome", OS: "windows", Release: 153}}
}

func gatewayPort(num int, gateway string) *port {
	return &port{num: num, exit: gateExit + gateway, resting: map[string]bool{},
		fresh: Fingerprint{Profile: "Chrome_153_win", Browser: "chrome", OS: "windows", Release: 153}}
}

func (p *port) note(call string) { p.calls = append(p.calls, call) }

func (p *port) Number() int { return p.num }

func (p *port) Exit() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exit
}

func (p *port) Offers(a string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.list, a) && !p.resting[a]
}

func (p *port) Candidates() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, a := range p.list {
		if !p.resting[a] {
			out = append(out, a)
		}
	}
	return out
}

func (p *port) Limit() int {
	if p.limit == 0 {
		return 1
	}
	return p.limit
}

func (p *port) MoveTo(_ context.Context, a string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.note("move " + a)
	p.exit = addrExit + a
	return nil
}

func (p *port) Wear(_ context.Context, profile string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.note("wear " + profile)
	if p.refuse != nil {
		return p.refuse
	}
	p.wearing = profile
	return nil
}

func (p *port) Freshen(context.Context) (Fingerprint, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.note("fresh")
	if p.refuse != nil {
		return Fingerprint{}, p.refuse
	}
	p.freshened++
	fp := p.fresh
	fp.Profile = fmt.Sprintf("%s_%d", fp.Profile, p.freshened)
	p.wearing = fp.Profile
	return fp, nil
}

func (p *port) PutTickets(_ context.Context, a string, t []byte) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.note("tickets " + a)
	p.tickets = t
	return p.elsewhere, nil
}

func (p *port) TakeTickets(context.Context) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tickets, nil
}

// clock is a time a test moves by hand.
type clock struct{ at time.Time }

func (c *clock) now() time.Time       { return c.at }
func (c *clock) pass(d time.Duration) { c.at = c.at.Add(d) }

func keeperAt(h History, c *clock) *Keeper {
	k := NewKeeper(h)
	k.now = c.now
	return k
}

var desktop = Want{Device: "desktop", Pause: 5 * time.Second}

func startClock() *clock { return &clock{at: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)} }

// google is where a test's sessions are given cookies.
var google = &url.URL{Scheme: "https", Host: "www.google.ru", Path: "/"}

func TestKeeper_MakesASessionWhenNoneIsRested(t *testing.T) {
	// How many sessions there are is not a number anybody sets: a thread that
	// finds every session held or still resting gets a new one.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	a := listPort(1, "addr-1", "addr-1", "addr-2")
	b := listPort(2, "addr-2", "addr-1", "addr-2")

	first, err := k.Take(ctx, a, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	second, err := k.Take(ctx, b, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("two threads were handed the same session at once")
	}
	if all, held := k.Count(); all != 2 || held != 2 {
		t.Errorf("the keeper knows %d sessions and %d held, want 2 and 2", all, held)
	}
	// A new session wears a fresh fingerprint from the port's template, not the
	// one the last session on the port wore.
	if first.Profile != "Chrome_153_win_1" {
		t.Errorf("a new session wears %q, want the fresh fingerprint", first.Profile)
	}
}

func TestKeeper_HandsARestedSessionBackOutRatherThanMakingAnother(t *testing.T) {
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := listPort(1, "addr-1", "addr-1", "addr-2")
	s, _ := k.Take(ctx, p, desktop)
	if err := s.Answered(ctx, p); err != nil {
		t.Fatalf("Answered: %v", err)
	}

	c.pass(2 * time.Second)
	early, _ := k.Take(ctx, listPort(2, "addr-2", "addr-1", "addr-2"), desktop)
	if early.ID == s.ID {
		t.Error("a session was handed out before it had rested the pause")
	}
	c.pass(5 * time.Second)
	again, _ := k.Take(ctx, listPort(3, "addr-1", "addr-1", "addr-2"), desktop)
	if again.ID != s.ID {
		t.Errorf("a rested session was not handed out again (got %d, want %d)", again.ID, s.ID)
	}
}

func TestKeeper_RestsASessionForThePauseOfWhoeverTakesItNext(t *testing.T) {
	// The warmer, a job and a search inside a request share sessions and not a
	// pause. A session is judged against the pause of whoever is asking.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := listPort(1, "addr-1", "addr-1")
	s, _ := k.Take(ctx, p, desktop)
	_ = s.Answered(ctx, p)
	c.pass(10 * time.Second)

	patient := desktop
	patient.Pause = time.Minute
	fresh, err := k.Take(ctx, listPort(2, "addr-1", "addr-1", "addr-2"), patient)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if fresh.ID == s.ID {
		t.Error("a session ten seconds rested was handed to a taker whose pause is a minute")
	}
}

func TestKeeper_PutsASessionOnAPortFingerprintFirstAddressSecondTicketsLast(t *testing.T) {
	// A new fingerprint or a new address wipes the port's tickets. Loaded in any
	// other order, the tickets are lost without a word.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	first := listPort(1, "addr-1", "addr-1", "addr-2")
	s, _ := k.Take(ctx, first, desktop)
	first.tickets = []byte(`[{"host":"www.google.ru"}]`)
	_ = s.Answered(ctx, first)
	c.pass(time.Minute)

	next := listPort(2, "addr-2", "addr-1", "addr-2")
	again, _ := k.Take(ctx, next, desktop)
	if again.ID != s.ID {
		t.Fatalf("got session %d, want %d", again.ID, s.ID)
	}
	want := []string{"wear Chrome_153_win_1", "move addr-1", "tickets addr-1"}
	if !slices.Equal(next.calls, want) {
		t.Errorf("the session went on the port as %q, want %q", next.calls, want)
	}
	if string(next.tickets) != `[{"host":"www.google.ru"}]` {
		t.Errorf("the port was loaded with %s, want the session's tickets", next.tickets)
	}
}

func TestKeeper_WipesThePortsTicketsForANewSession(t *testing.T) {
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	p := listPort(1, "addr-1", "addr-1")
	p.tickets = []byte(`[{"host":"left behind"}]`)
	if _, err := k.Take(context.Background(), p, desktop); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if p.tickets != nil {
		t.Errorf("a new session went out with %s, the last session's tickets", p.tickets)
	}
}

func TestKeeper_GivesANewSessionTheAddressWithFewestSessionsThenTheLongestRested(t *testing.T) {
	// The customer's rule: sessions spread over every address before any
	// address takes a second; once they have to share, the address with the
	// fewest goes first, and between equals the one that has rested longest.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	take := func(num int, list ...string) string {
		p := listPort(num, list[0], list...)
		s, err := k.Take(ctx, p, desktop)
		if err != nil {
			t.Fatalf("Take %d: %v", num, err)
		}
		_ = s.Answered(ctx, p)
		c.pass(time.Second)
		return p.Exit()
	}
	// Session one takes a (nothing on either, a first), two takes b (a carries
	// one), and three takes a again: both carry one and a has rested longer —
	// though the third port lists b first, so taking the first in the list
	// would be told apart from taking the longest rested.
	got := []string{take(1, "a", "b"), take(2, "a", "b"), take(3, "b", "a")}
	want := []string{"addr:a", "addr:b", "addr:a"}
	if !slices.Equal(got, want) {
		t.Errorf("new sessions went to %q, want %q", got, want)
	}
}

func TestKeeper_PassesOverASessionWhoseAddressIsAtItsLimit(t *testing.T) {
	// Two sessions came to share address a when there was nothing else. With a
	// limit of one session at a time on an address, the second is not handed
	// out while the first is working through a: the thread gets a new session
	// on b instead.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	roomy := func(num int) *port {
		p := listPort(num, "a", "a")
		p.limit = 2
		return p
	}
	p1, p2 := roomy(1), roomy(2)
	s1, _ := k.Take(ctx, p1, desktop)
	s2, _ := k.Take(ctx, p2, desktop)
	_ = s1.Answered(ctx, p1)
	c.pass(time.Second)
	_ = s2.Answered(ctx, p2)
	c.pass(time.Minute)

	first, _ := k.Take(ctx, listPort(3, "a", "a", "b"), desktop)
	if first.ID != s2.ID {
		t.Fatalf("got %d, want %d, the session used last", first.ID, s2.ID)
	}
	other := listPort(4, "a", "a", "b")
	next, err := k.Take(ctx, other, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if next.ID == s1.ID || other.Exit() != "addr:b" {
		t.Errorf("the second thread got session %d on %q, want a new one on b: a is working a session already",
			next.ID, other.Exit())
	}
}

func TestKeeper_KeepsAGatewaySessionToItsGateway(t *testing.T) {
	// There is no carrying a session to another exit without a challenge, and a
	// gateway is an exit: each gateway has its own sessions.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	north := gatewayPort(1, "north")
	s, _ := k.Take(ctx, north, desktop)
	_ = s.Answered(ctx, north)
	c.pass(time.Minute)

	south := gatewayPort(2, "south")
	other, _ := k.Take(ctx, south, desktop)
	if other.ID == s.ID {
		t.Error("a session made behind one gateway was handed to a port on another")
	}
	back, _ := k.Take(ctx, gatewayPort(3, "north"), desktop)
	if back.ID != s.ID {
		t.Error("a port on the session's own gateway was not handed the session")
	}
	for _, call := range south.calls {
		if call != "fresh" && call != "tickets " {
			t.Errorf("a port on a gateway was told %q; nothing moves a gateway", call)
		}
	}
}

func TestKeeper_MovesASessionWhoseAddressIsGoneByTheSameRule(t *testing.T) {
	// A session whose address has left the list — or is resting after failing
	// to carry anything — takes another, chosen the way a new session's is.
	// There it pays a challenge; that is what changing exit costs.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := listPort(1, "a", "a", "b")
	s, _ := k.Take(ctx, p, desktop)
	_ = s.Answered(ctx, p)
	c.pass(time.Minute)

	// The list no longer holds a. The port stands on c; the rule picks b — both
	// carry nothing and neither has rested less, and b comes first — so a move
	// to b is the rule deciding, not the port's address being taken as found.
	q := listPort(2, "c", "b", "c")
	again, _ := k.Take(ctx, q, desktop)
	if again.ID != s.ID {
		t.Fatalf("got %d, want the rested session", again.ID)
	}
	if q.Exit() != "addr:b" {
		t.Errorf("the session went to %q, want b, the address the rule picks", q.Exit())
	}
}

func TestKeeper_TakesThePortsOwnAddressWhenTheWholeListIsResting(t *testing.T) {
	// Every address resting leaves nothing to choose from. Standing still until
	// the rests run out would stop the job for an hour; the port's own address
	// is what the pool itself falls back on.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	p := listPort(1, "a", "a")
	p.resting["a"] = true
	if _, err := k.Take(context.Background(), p, desktop); err != nil {
		t.Fatalf("with the whole list resting, Take answered %v, want a session on the port's own address", err)
	}
	if p.Exit() != "addr:a" {
		t.Errorf("the session went to %q, want the port's own address", p.Exit())
	}
}

func TestKeeper_WritesTicketsCookiesAndWhereTheRequestEndedUpAfterEveryAnswer(t *testing.T) {
	// A request an address did not carry is taken to another, and the session
	// goes with it. What is written down is where it is now, or its next use
	// sends the new clearance out through the old exit.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := listPort(1, "a", "a", "b")
	s, _ := k.Take(ctx, p, desktop)

	s.Jar.SetCookies(google, []*http.Cookie{{Name: "NID", Value: "x"}})
	p.exit, p.tickets = "addr:b", []byte(`[{"host":"www.google.ru"}]`)
	if err := s.Answered(ctx, p); err != nil {
		t.Fatalf("Answered: %v", err)
	}
	kept, _ := h.Get(s.ID)
	if kept.Exit != "addr:b" || string(kept.Tickets) != `[{"host":"www.google.ru"}]` {
		t.Errorf("written down at %q with %s, want addr:b with the port's tickets", kept.Exit, kept.Tickets)
	}
	jar, err := ReadJar(kept.Cookies)
	if err != nil || len(jar.Cookies(google)) != 1 {
		t.Errorf("the jar written down holds %v (err %v), want the NID", jar.Cookies(google), err)
	}
}

func TestKeeper_RefusesAPortThatStandsElsewhere(t *testing.T) {
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := listPort(1, "a", "a")
	s, _ := k.Take(ctx, p, desktop)
	_ = s.Answered(ctx, p)
	c.pass(time.Minute)

	wrong := listPort(2, "a", "a")
	wrong.elsewhere = true
	if _, err := k.Take(ctx, wrong, desktop); !errors.Is(err, ErrPortElsewhere) {
		t.Fatalf("a port standing elsewhere answered %v, want ErrPortElsewhere", err)
	}
	if _, held := k.Count(); held != 0 {
		t.Error("the session refused a port was not put back")
	}
}

func TestKeeper_HandsOutOnlyWhatTheJobAskedFor(t *testing.T) {
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := listPort(1, "a", "a", "b")
	s, _ := k.Take(ctx, p, desktop)
	_ = s.Answered(ctx, p)
	c.pass(time.Minute)

	firefox := desktop
	firefox.Browser = "firefox"
	q := listPort(2, "b", "a", "b")
	q.fresh = Fingerprint{Profile: "Firefox_140_win", Browser: "firefox", OS: "windows", Release: 140}
	got, _ := k.Take(ctx, q, firefox)
	if got.ID == s.ID {
		t.Error("a job asking for Firefox was handed a Chrome session")
	}
	phone := Want{Device: "mobile", Pause: time.Second}
	r, _ := k.Take(ctx, listPort(3, "a", "a", "b"), phone)
	if r.ID == s.ID {
		t.Error("a phone job was handed a desktop session")
	}
}

func TestKeeper_GivesASessionUpAtTheSecondRefusalInARow(t *testing.T) {
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := listPort(1, "a", "a")
	s, _ := k.Take(ctx, p, desktop)
	if dropped, err := s.Failed(ctx); err != nil || dropped {
		t.Fatalf("first refusal: dropped=%v err=%v", dropped, err)
	}
	c.pass(time.Minute)
	again, _ := k.Take(ctx, p, desktop)
	if again.ID != s.ID {
		t.Fatalf("got %d, want the once-refused session back", again.ID)
	}
	if dropped, err := again.Failed(ctx); err != nil || !dropped {
		t.Errorf("second refusal in a row: dropped=%v err=%v, want dropped", dropped, err)
	}
	if all, _ := k.Count(); all != 0 {
		t.Errorf("the keeper still knows %d sessions after giving one up", all)
	}
}

func TestKeeper_ResumesTheSessionsAnEarlierRunLeft(t *testing.T) {
	c, h := startClock(), NewMemory()
	ctx := context.Background()
	p := listPort(1, "a", "a")
	earlier := keeperAt(h, c)
	s, _ := earlier.Take(ctx, p, desktop)
	s.Jar.SetCookies(google, []*http.Cookie{{Name: "NID", Value: "kept"}})
	_ = s.Answered(ctx, p)
	c.pass(time.Hour)

	later := keeperAt(h, c)
	again, err := later.Take(ctx, listPort(2, "a", "a"), desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if again.ID != s.ID || len(again.Jar.Cookies(google)) != 1 {
		t.Errorf("a new keeper handed out session %d with %v, want %d with its cookie",
			again.ID, again.Jar.Cookies(google), s.ID)
	}
}

func TestKeeper_ForgetsASessionUnusedForTwelveHours(t *testing.T) {
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := listPort(1, "a", "a")
	s, _ := k.Take(ctx, p, desktop)
	_ = s.Answered(ctx, p)
	c.pass(KeptFor + time.Minute)

	next, _ := k.Take(ctx, listPort(2, "a", "a"), desktop)
	if next.ID == s.ID {
		t.Error("a session unused for more than twelve hours was handed out")
	}
	if _, kept := h.Get(s.ID); kept {
		t.Error("a session unused for more than twelve hours is still in the history")
	}
}

func TestKeeper_NeverHandsOutASessionPastItsTwelveHoursBetweenSweeps(t *testing.T) {
	// The sweep runs once an hour; a session that ran out in between is not
	// handed out on the strength of the sweep not having got to it yet.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := listPort(1, "a", "a")
	s, _ := k.Take(ctx, p, desktop)
	_ = s.Answered(ctx, p)

	// Half an hour before it runs out, something takes a session elsewhere,
	// which sweeps; the session is not yet stale and stays.
	c.pass(KeptFor - 30*time.Minute)
	if _, err := k.Take(ctx, gatewayPort(9, "elsewhere"), desktop); err != nil {
		t.Fatalf("Take: %v", err)
	}
	// Thirty-one minutes later it has run out, and no sweep is due.
	c.pass(31 * time.Minute)
	next, err := k.Take(ctx, listPort(2, "a", "a"), desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if next.ID == s.ID {
		t.Error("a session past its twelve hours was handed out because the sweep had not come round")
	}
}

func TestKeeper_TakesTheColdestSessionThatHasRestedLongEnough(t *testing.T) {
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	// Both are made while the other is held, so there are two; the first is
	// then an hour colder than the second.
	pa, pb := listPort(1, "a", "a", "b"), listPort(2, "b", "a", "b")
	first, _ := k.Take(ctx, pa, desktop)
	second, _ := k.Take(ctx, pb, desktop)
	_ = first.Answered(ctx, pa)
	c.pass(time.Hour)
	_ = second.Answered(ctx, pb)

	if _, err := k.TakeColdest(ctx, listPort(3, "a", "a", "b"), desktop, 2*time.Hour); !errors.Is(err, ErrNothingDue) {
		t.Fatalf("nothing is two hours cold, yet TakeColdest answered %v", err)
	}
	got, err := k.TakeColdest(ctx, listPort(4, "a", "a", "b"), desktop, 0)
	if err != nil {
		t.Fatalf("TakeColdest: %v", err)
	}
	if got.ID != first.ID {
		t.Errorf("took session %d, want %d, the one unused longest", got.ID, first.ID)
	}
}

func TestKeeper_ChoosesForThePoolByTheSameRule(t *testing.T) {
	// The address carrying fewer sessions goes first even when it has rested
	// less: b lost its only session a minute ago and carries none, a has
	// carried one since the start.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	pa := listPort(1, "a", "a")
	s, _ := k.Take(ctx, pa, desktop)
	_ = s.Answered(ctx, pa)

	pb := listPort(2, "b", "b")
	gone, _ := k.Take(ctx, pb, desktop)
	_, _ = gone.Failed(ctx)
	c.pass(time.Minute)
	again, _ := k.Take(ctx, listPort(3, "b", "b"), desktop)
	if again.ID != gone.ID {
		t.Fatalf("got %d, want the once-refused session %d back", again.ID, gone.ID)
	}
	if dropped, _ := again.Failed(ctx); !dropped {
		t.Fatal("the second refusal in a row did not give the session up")
	}

	if got, ok := k.Choose([]string{"a", "b"}, 1); !ok || got != "b" {
		t.Errorf("Choose picked %q, want b: it carries no session, a carries one", got)
	}
}

func TestHeld_CannotBeGivenBackTwice(t *testing.T) {
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := listPort(1, "a", "a")
	s, _ := k.Take(ctx, p, desktop)
	if err := s.Answered(ctx, p); err != nil {
		t.Fatalf("Answered: %v", err)
	}
	if err := s.Answered(ctx, p); !errors.Is(err, ErrNotHeld) {
		t.Errorf("a second give-back answered %v, want ErrNotHeld", err)
	}
	c.pass(time.Minute)
	// And a stale handle cannot write over the session once another thread
	// holds it.
	again, _ := k.Take(ctx, p, desktop)
	if _, err := s.Failed(ctx); !errors.Is(err, ErrNotHeld) {
		t.Errorf("a stale handle failed the session another thread holds: %v", err)
	}
	_ = again.Answered(ctx, p)
}

func TestHeld_SavesThePagesOfAWalkWithoutLettingTheSessionGo(t *testing.T) {
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := listPort(1, "a", "a")
	s, _ := k.Take(ctx, p, desktop)
	s.Jar.SetCookies(google, []*http.Cookie{{Name: "NID", Value: "page-one"}})
	if err := s.Save(ctx, p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, held := k.Count(); held != 1 {
		t.Error("saving a page let the session go")
	}
	kept, _ := h.Get(s.ID)
	if jar, _ := ReadJar(kept.Cookies); len(jar.Cookies(google)) != 1 {
		t.Error("the page's cookies were not written down")
	}
}
