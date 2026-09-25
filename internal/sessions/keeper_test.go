// SPDX-License-Identifier: MIT

package sessions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/store"
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
	// stayed is what the port was last told about keeping its address.
	stayed bool
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

// Rests reads the rests alone, as the proxy service's list does: an address the
// list no longer holds keeps the rest it was serving.
func (p *port) Rests(a string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resting[a]
}

func (p *port) Stay(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stayed = on
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
	// Every rest is the pause and no more, unless a test says otherwise: the
	// spread is a test of its own.
	k.rand = func() float64 { return 0 }
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

func TestKeeper_MovesASessionWhoseAddressHasStoppedByTheSameRule(t *testing.T) {
	// A session with nothing to wait for whose address is resting after failing
	// to carry anything takes another, chosen the way a new session's is.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := listPort(1, "a", "a", "b", "c")
	s, _ := k.Take(ctx, p, desktop)
	s.PutBack()
	c.pass(time.Minute)

	// The port stands on c; the rule picks b — both carry nothing and neither
	// has rested less, and b comes first — so a move to b is the rule deciding,
	// not the port's address being taken as found.
	q := listPort(2, "c", "a", "b", "c")
	q.resting["a"] = true
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

func TestKeeper_TakesOneOfTheNamedSessionsAndMakesNone(t *testing.T) {
	// A thread walking queries asks for the sessions carrying one, and for no
	// others: the page it would take next is addressed to one of those and
	// nothing else can take it. So where none of them has rested, the answer is
	// that none is due — Take would answer the same asking with a new session,
	// and a thread looking every few milliseconds would collect one per glance.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	pa, pb := listPort(1, "a", "a", "b"), listPort(2, "b", "a", "b")
	carrying, _ := k.Take(ctx, pa, desktop)
	other, _ := k.Take(ctx, pb, desktop)
	_ = carrying.Answered(ctx, pa)
	_ = other.Answered(ctx, pb)

	// A thread carrying nothing names nothing, and is given nothing.
	if _, err := k.TakeOneOf(ctx, listPort(3, "a", "a", "b"), desktop, nil); !errors.Is(err, ErrNothingDue) {
		t.Fatalf("naming no session answered %v, want nothing due", err)
	}
	// Both have just answered, so neither has rested the pause.
	if _, err := k.TakeOneOf(ctx, listPort(4, "a", "a", "b"), desktop, []int64{carrying.ID}); !errors.Is(err, ErrNothingDue) {
		t.Fatalf("a session that has not rested was handed out: %v", err)
	}

	c.pass(time.Minute)
	got, err := k.TakeOneOf(ctx, listPort(5, "b", "a", "b"), desktop, []int64{carrying.ID})
	if err != nil {
		t.Fatalf("TakeOneOf: %v", err)
	}
	if got.ID != carrying.ID {
		t.Errorf("took session %d, want %d, the one named", got.ID, carrying.ID)
	}
	if all, _ := h.Sessions(ctx, "desktop", time.Time{}); len(all) != 2 {
		t.Errorf("the history holds %d sessions, want the two that were made — this asking makes none", len(all))
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

// answeredOn makes a session that has answered on address a: Google gave it a
// cookie, and it was written down.
func answeredOn(t *testing.T, k *Keeper, a string, list ...string) *Held {
	t.Helper()
	ctx := context.Background()
	p := listPort(90, a, list...)
	s, err := k.Take(ctx, p, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	s.Jar.SetCookies(google, []*http.Cookie{{Name: "GOOGLE_ABUSE_EXEMPTION", Value: "clearance"}})
	if err := s.Answered(ctx, p); err != nil {
		t.Fatalf("Answered: %v", err)
	}
	return s
}

func TestKeeper_KeepsASessionThatHasAnsweredWaitingForItsRestingAddress(t *testing.T) {
	// A session holds a clearance for the address it answered through. That
	// address failing once is a reason to rest it, not to spend the clearance
	// on a challenge somewhere else: the session waits, and a thread meanwhile
	// takes another.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	s := answeredOn(t, k, "a", "a", "b")
	c.pass(time.Minute)

	resting := listPort(1, "b", "a", "b")
	resting.resting["a"] = true
	other, err := k.Take(ctx, resting, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if other.ID == s.ID {
		t.Fatal("a session that has answered was carried off its resting address")
	}
	// The other session stays held, so the only one free once a is back is
	// the one that waited for it.
	c.pass(time.Minute)

	back, err := k.Take(ctx, listPort(2, "b", "a", "b"), desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if back.ID != s.ID {
		t.Errorf("once its address was back, got session %d, want the one that waited (%d)", back.ID, s.ID)
	}
}

func TestKeeper_KeepsASessionOnItsAddressAfterTheListDropsIt(t *testing.T) {
	// A list read again is not a verdict on the addresses it leaves out: a
	// wingate list changes half of them in twenty seconds, and the ones it
	// dropped go on answering. A session is taken off its address when the
	// address stops carrying its requests, and never because a reading of the
	// list no longer has it — carried off an address that works, it pays a
	// challenge where it lands for nothing.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	s := answeredOn(t, k, "a", "a", "b")
	c.pass(time.Minute)

	// The list no longer holds a, and the port stands on b.
	p := listPort(1, "b", "b")
	got, err := k.Take(ctx, p, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got.ID != s.ID || p.Exit() != "addr:a" {
		t.Fatalf("got session %d on %q, want %d on its own address a", got.ID, p.Exit(), s.ID)
	}
	if !p.stayed {
		t.Error("the port was not told to keep the session's address")
	}
	_ = got.Answered(ctx, p)
	if kept, _ := h.Get(s.ID); kept.Exit != "addr:a" {
		t.Errorf("the history has the session going out through %q, want a", kept.Exit)
	}

	// A session that has never answered keeps its address the same way: nothing
	// about the address has changed but the list.
	fresh, err := k.Take(ctx, listPort(2, "c", "c"), desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	fresh.PutBack()
	c.pass(time.Minute)
	q := listPort(3, "b", "b")
	if got, err := k.TakeOneOf(ctx, q, desktop, []int64{fresh.ID}); err != nil || q.Exit() != "addr:c" {
		t.Errorf("got %v on %q (%v), want session %d on its own address c", got, q.Exit(), err, fresh.ID)
	}
}

func TestKeeper_KeepsASessionThatHasAnsweredWaitingForItsRestingAddressAfterTheListDropsIt(t *testing.T) {
	// The address stopped and then went from the list. What the session waits
	// for is the rest, and a reading of the list does not cut it short.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	s := answeredOn(t, k, "a", "a", "b")
	c.pass(time.Minute)

	p := listPort(1, "b", "b")
	p.resting["a"] = true
	got, err := k.Take(ctx, p, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got.ID == s.ID {
		t.Errorf("a session that has answered was carried off its resting address to %q", p.Exit())
	}
}

func TestKeeper_HoldsAnAddressTheListNoLongerHasToItsShareOfSessions(t *testing.T) {
	// An address carries as many sessions at once as it may, listed or not: the
	// share is about what one exit can bear, and a reading of the list does not
	// change the exit.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	first := answeredOn(t, k, "a", "a")
	second := answeredOn(t, k, "a", "a")
	c.pass(time.Minute)

	one, err := k.Take(ctx, listPort(1, "b", "b"), desktop)
	if err != nil || (one.ID != first.ID && one.ID != second.ID) {
		t.Fatalf("got %v (%v), want one of the two sessions on a", one, err)
	}
	p := listPort(2, "b", "b")
	other, err := k.Take(ctx, p, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if other.ID == first.ID || other.ID == second.ID {
		t.Errorf("session %d went out through a while another already was, over a share of one", other.ID)
	}
}

func TestKeeper_LetsASessionThatNeverAnsweredGoWhereAnAddressIsFree(t *testing.T) {
	// A session with nothing to lose does not wait for anything.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	first := listPort(1, "a", "a", "b")
	s, _ := k.Take(ctx, first, desktop)
	s.PutBack()
	c.pass(time.Minute)

	p := listPort(2, "a", "a", "b")
	p.resting["a"] = true
	got, err := k.Take(ctx, p, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got.ID != s.ID || p.Exit() != "addr:b" {
		t.Errorf("got session %d on %q, want %d taken to b", got.ID, p.Exit(), s.ID)
	}
}

func TestKeeper_TellsThePortToKeepItsAddressOnlyForASessionThatHasAnswered(t *testing.T) {
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	answeredOn(t, k, "a", "a")
	c.pass(time.Minute)

	p := listPort(1, "a", "a")
	if _, err := k.Take(ctx, p, desktop); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if !p.stayed {
		t.Error("the port carrying a session that has answered was not told to keep its address")
	}
	q := listPort(2, "b", "a", "b")
	q.stayed = true
	if _, err := k.Take(ctx, q, desktop); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if q.stayed {
		t.Error("the port carrying a new session was told to keep its address")
	}
}

func TestKeeper_RestsASessionBetweenThePauseAndHalfAgainMore(t *testing.T) {
	// Sixty to ninety seconds: the span the operator measured, and no metronome
	// — a session asked again every sixty seconds to the millisecond is a
	// description of a program.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	k.rand = func() float64 { return 1 }
	ctx := context.Background()
	minute := Want{Device: "desktop", Pause: 60 * time.Second}
	p := listPort(1, "a", "a", "b")
	s, _ := k.Take(ctx, p, minute)
	_ = s.Answered(ctx, p)

	c.pass(70 * time.Second)
	early, _ := k.Take(ctx, listPort(2, "b", "a", "b"), minute)
	if early.ID == s.ID {
		t.Fatal("a session drawn to rest ninety seconds was handed out at seventy")
	}
	early.PutBack()
	c.pass(21 * time.Second)
	later, _ := k.Take(ctx, listPort(3, "a", "a", "b"), minute)
	if later.ID != s.ID {
		t.Errorf("at ninety-one seconds got session %d, want the rested one (%d)", later.ID, s.ID)
	}
}

func TestHeld_ElsewhereTakesASessionOffItsAddressAndHoldsNothingAgainstIt(t *testing.T) {
	// The check Google sets on an address is the address's to pass. A session
	// taken off it keeps everything it had — its cookies, its fingerprint, its
	// count of refusals at nought — and is given the next free address when it
	// is taken again.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	s := answeredOn(t, k, "a", "a", "b")
	c.pass(time.Minute)
	held, err := k.Take(ctx, listPort(1, "a", "a", "b"), desktop)
	if err != nil || held.ID != s.ID {
		t.Fatalf("taking the session again: %v (got %d, want %d)", err, held.ID, s.ID)
	}
	if err := held.Elsewhere(ctx); err != nil {
		t.Fatalf("Elsewhere: %v", err)
	}
	kept, ok := h.Get(s.ID)
	if !ok {
		t.Fatal("the session is not in the history")
	}
	if kept.Exit != "" {
		t.Errorf("the session is written down at %q, want it off the address that could not carry it", kept.Exit)
	}
	if kept.Failures != 0 {
		t.Errorf("the session carries %d refusals, want none: the address failed, not the session", kept.Failures)
	}
	if !strings.Contains(string(kept.Cookies), "GOOGLE_ABUSE_EXEMPTION") {
		t.Errorf("the session was written down with %s, want the cookies it still holds", kept.Cookies)
	}
	if len(kept.Tickets) != 0 {
		t.Errorf("the session kept %d bytes of tickets, want none: they are the old exit's", len(kept.Tickets))
	}

	// And it is free again: the next taking puts it on an address that is free.
	c.pass(time.Minute)
	p := listPort(2, "b", "a", "b")
	back, err := k.Take(ctx, p, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if back.ID != s.ID || p.Exit() == addrExit+"a" {
		t.Errorf("got session %d on %q, want %d somewhere other than the address it was taken off", back.ID, p.Exit(), s.ID)
	}
}

func TestHeld_ElsewhereLeavesASessionOnItsGateway(t *testing.T) {
	// A gateway is the whole of what such a session is: moved to another exit it
	// would be another session, and there is nowhere to take it.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := gatewayPort(7, "nl-one")
	s, err := k.Take(ctx, p, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.Answered(ctx, p); err != nil {
		t.Fatalf("Answered: %v", err)
	}
	c.pass(time.Minute)
	again, err := k.Take(ctx, p, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if again.ID != s.ID {
		t.Fatalf("the gateway's session was taken again as %d, want the same one (%d)", again.ID, s.ID)
	}
	if err := again.Elsewhere(ctx); err != nil {
		t.Fatalf("Elsewhere: %v", err)
	}
	kept, ok := h.Get(s.ID)
	if !ok {
		t.Fatal("the session is not in the history")
	}
	if kept.Exit != gateExit+"nl-one" {
		t.Errorf("the session on a gateway is written down at %q, want it left on its gateway", kept.Exit)
	}
}

func TestHeld_MoveOnPutsTheSessionOnAnotherAddressAndKeepsItInHand(t *testing.T) {
	// A page that never reached Google is the road's doing, not the session's.
	// The session is moved to another address and asked again at once — it
	// keeps its place in hand, because nothing has been answered and there is
	// nothing to rest from.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	s := answeredOn(t, k, "a", "a", "b")
	c.pass(time.Minute)
	p := listPort(1, "a", "a", "b")
	held, err := k.Take(ctx, p, desktop)
	if err != nil || held.ID != s.ID {
		t.Fatalf("taking the session: %v (got %d, want %d)", err, held.ID, s.ID)
	}
	if err := held.MoveOn(ctx, p); err != nil {
		t.Fatalf("MoveOn: %v", err)
	}
	if p.Exit() != addrExit+"b" {
		t.Errorf("the port stands on %q, want the other address", p.Exit())
	}
	// Still in hand: the answer that follows is written down against the
	// address it was answered through.
	if err := held.Answered(ctx, p); err != nil {
		t.Fatalf("Answered after MoveOn: %v", err)
	}
	kept, ok := h.Get(s.ID)
	if !ok {
		t.Fatal("the session is not in the history")
	}
	if kept.Exit != addrExit+"b" {
		t.Errorf("the session is written down at %q, want the address it moved to", kept.Exit)
	}
	if kept.Failures != 0 {
		t.Errorf("the session carries %d refusals, want none: the road failed, not the session", kept.Failures)
	}
}

func TestHeld_MoveOnIsRememberedEvenIfNothingIsAnsweredAfterIt(t *testing.T) {
	// The session itself knows where it went, not only the port it was on. Put
	// back without an answer — the page after the move failed too, and the
	// tries ran out — it must not be handed to the next port still pointing at
	// the address it left.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	answeredOn(t, k, "a", "a", "b")
	c.pass(time.Minute)
	held, err := k.Take(ctx, listPort(1, "a", "a", "b"), desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := held.MoveOn(ctx, listPort(1, "a", "a", "b")); err != nil {
		t.Fatalf("MoveOn: %v", err)
	}
	held.PutBack()

	c.pass(time.Minute)
	next := listPort(2, "a", "a", "b")
	if _, err := k.Take(ctx, next, desktop); err != nil {
		t.Fatalf("taking the session again: %v", err)
	}
	if next.Exit() != addrExit+"b" {
		t.Errorf("the next port was put on %q, want the address the session moved to", next.Exit())
	}
}

func TestHeld_MoveOnWillNotPutASessionBackOnTheAddressItIsLeaving(t *testing.T) {
	// The address it is leaving has just shown it cannot carry this session's
	// request. Handed back, the caller would ask again through the same road
	// and call the second failure an answer about the session.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	answeredOn(t, k, "a", "a")
	c.pass(time.Minute)
	p := listPort(1, "a", "a")
	// The address has room for more sessions than this one, so nothing but the
	// rule itself keeps the move off it.
	p.limit = 4
	held, err := k.Take(ctx, p, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := held.MoveOn(ctx, p); !errors.Is(err, ErrNoAddress) {
		t.Errorf("MoveOn with nowhere else to go answered %v, want %v", err, ErrNoAddress)
	}
	if p.Exit() != addrExit+"a" {
		t.Errorf("the port stands on %q, want the address it was on", p.Exit())
	}
}

func TestHeld_MoveOnHasNowhereToTakeASessionOnAGateway(t *testing.T) {
	// A gateway is the whole of what such a session is. There is no other
	// address to try, and saying so lets the caller spend its tries on the
	// gateway rather than on a move that cannot happen.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := gatewayPort(4, "nl-one")
	// Even offered somewhere to go, a session on a gateway does not go: what
	// a gateway channel hands out are gateways, and a session moved to another
	// one is another session.
	p.list = []string{"de-two", "fr-three"}
	held, err := k.Take(ctx, p, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := held.MoveOn(ctx, p); err == nil {
		t.Error("a session on a gateway was moved somewhere")
	}
	if p.Exit() != gateExit+"nl-one" {
		t.Errorf("the port stands on %q, want its gateway", p.Exit())
	}
}

func TestHeld_GiveUpTakesTheSessionOutOfTheHistory(t *testing.T) {
	// A session whose walk cannot go on is dead: its deep links belong to an
	// exit it no longer has, and handing it out again would spend a port on a
	// session that can only fail.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	s := answeredOn(t, k, "a", "a", "b")
	c.pass(time.Minute)
	p := listPort(1, "a", "a", "b")
	held, err := k.Take(ctx, p, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := held.GiveUp(ctx); err != nil {
		t.Fatalf("GiveUp: %v", err)
	}
	if _, ok := h.Get(s.ID); ok {
		t.Error("a session given up is still in the history")
	}
	c.pass(time.Minute)
	again, err := k.Take(ctx, listPort(2, "a", "a", "b"), desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if again.ID == s.ID {
		t.Error("a session given up was handed out again")
	}
	if all, _ := k.Count(); all != 1 {
		t.Errorf("the keeper knows %d sessions, want the one made after the other was given up", all)
	}
}

func TestKeeper_DrawsASessionsRestBetweenTheTwoEndsTheTakerNames(t *testing.T) {
	// The operator sets a span rather than a number — sixty to a hundred and
	// twenty by default — and each session's own rest is drawn inside it. The
	// draw is what keeps a pool of sessions off a metronome: the number is the
	// reader's pace, and a request every sixty seconds to the millisecond
	// describes the program making it.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	k.rand = func() float64 { return 1 } // this session rests the most it may
	ctx := context.Background()
	span := Want{Device: "desktop", Pause: time.Minute, UpTo: 2 * time.Minute}
	p := listPort(1, "a", "a", "b")
	s, _ := k.Take(ctx, p, span)
	_ = s.Answered(ctx, p)

	c.pass(119 * time.Second)
	early, _ := k.Take(ctx, listPort(2, "b", "a", "b"), span)
	if early.ID == s.ID {
		t.Fatal("a session drawn to rest two minutes was handed out at a hundred and nineteen seconds")
	}
	early.PutBack()
	c.pass(2 * time.Second)
	later, _ := k.Take(ctx, listPort(3, "a", "a", "b"), span)
	if later.ID != s.ID {
		t.Errorf("at two minutes and one second got session %d, want the one that had rested its span (%d)",
			later.ID, s.ID)
	}

	// And the least end is a floor: a session drawn to rest the least of the
	// span still rests that much.
	k.rand = func() float64 { return 0 }
	_ = later.Answered(ctx, listPort(3, "a", "a", "b"))
	c.pass(59 * time.Second)
	fresh, _ := k.Take(ctx, listPort(4, "b", "a", "b"), span)
	if fresh.ID == s.ID {
		t.Error("a session was handed out at fifty-nine seconds of a span starting at sixty")
	}
	fresh.PutBack()
	// The draw is taken again at every use rather than once in a session's
	// life: this one fell on the least of the span, so the session that rested
	// two minutes last time is due at one this time.
	c.pass(2 * time.Second)
	if got, _ := k.Take(ctx, listPort(5, "a", "a", "b"), span); got.ID != s.ID {
		t.Errorf("at sixty-one seconds got session %d, want the one whose new draw was the least of the span (%d)",
			got.ID, s.ID)
	}
}

func TestKeeper_StandingCountsTheSessionsACallerCanUseAndThoseStillResting(t *testing.T) {
	// The reading a screen shows while a job runs. What it is really asking is
	// whether the run is waiting on its sessions: nearly all of them resting is
	// a run that will wait, and the total beside it says whether that is the
	// rest being long or the run being wide.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	pa, pb := listPort(1, "a", "a", "b"), listPort(2, "b", "a", "b")
	first, _ := k.Take(ctx, pa, desktop)
	second, _ := k.Take(ctx, pb, desktop)
	_ = first.Answered(ctx, pa)
	_ = second.Answered(ctx, pb)

	// Both have just answered, so both are resting the five seconds this want
	// asks of them, and neither is in anybody's hands.
	if all, held, resting := k.Standing(desktop); all != 2 || held != 0 || resting != 2 {
		t.Errorf("%d sessions, %d in hand, %d resting; want two, none and two", all, held, resting)
	}
	c.pass(time.Minute)
	if all, _, resting := k.Standing(desktop); all != 2 || resting != 0 {
		t.Errorf("a minute on, %d of %d sessions are resting; want none", resting, all)
	}

	// One in somebody's hands is working rather than resting, and is still one
	// of the sessions there are. The three are told apart because what is
	// neither is what a thread coming back for one would find.
	taken, err := k.Take(ctx, listPort(3, "a", "a", "b"), desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if all, held, resting := k.Standing(desktop); all != 2 || held != 1 || resting != 0 {
		t.Errorf("with one in hand: %d sessions, %d in hand, %d resting; want two, one and none",
			all, held, resting)
	}
	taken.PutBack()

	// And the sessions of another kind of result page are another caller's.
	if all, _, _ := k.Standing(Want{Device: "mobile"}); all != 0 {
		t.Errorf("a phone job is told it has %d sessions, want the desktop ones left out", all)
	}
}

func TestKeeper_RestsASessionFromItsAnswerAndNotFromItsAsking(t *testing.T) {
	// Google's check is solved inside the request that met it: the answer comes
	// back tens of seconds, sometimes minutes, after the request went out.
	// Rested from the asking, a session would finish its rest while it was still
	// waiting for that answer, and the next request would go out the instant the
	// last one landed — which is the one thing the rest is for.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	p := listPort(1, "a", "a", "b")
	s, err := k.Take(ctx, p, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	// The request waits three minutes for a check to be solved. The rest this
	// want asks for is five seconds, so it has passed many times over.
	c.pass(3 * time.Minute)
	if err := s.Answered(ctx, p); err != nil {
		t.Fatalf("Answered: %v", err)
	}

	c.pass(4 * time.Second)
	fresh, err := k.Take(ctx, listPort(2, "b", "a", "b"), desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if fresh.ID == s.ID {
		t.Error("the session was handed out four seconds after its answer, so it rested from when it was asked")
	}
	fresh.PutBack()

	// And it comes back once it has rested from the answer.
	c.pass(2 * time.Second)
	again, err := k.Take(ctx, listPort(3, "a", "a", "b"), desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if again.ID != s.ID {
		t.Errorf("six seconds after the answer got session %d, want the one that had rested (%d)",
			again.ID, s.ID)
	}
}

func TestKeeper_TakesAStrandedSessionOffItsRestingAddressWhenAskedTo(t *testing.T) {
	// The one exception to a session that has answered waiting for its own
	// address, which the user chose: the end of a job, with nothing else left to
	// do. Asked for a stranded one, the keeper hands over a session that has
	// served its rest and is waiting for nothing but its address, and puts it on
	// an address that is free — a stranger there, as any session that moves is.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	s := answeredOn(t, k, "a", "a", "b")
	c.pass(time.Minute)

	p := listPort(1, "b", "a", "b")
	p.resting["a"] = true
	if got, err := k.TakeOneOf(ctx, p, desktop, []int64{s.ID}); !errors.Is(err, ErrNothingDue) {
		t.Fatalf("TakeOneOf gave %v (%v), want the session left waiting for its address", got, err)
	}
	got, err := k.TakeStranded(ctx, p, desktop, []int64{s.ID})
	if err != nil {
		t.Fatalf("TakeStranded: %v", err)
	}
	if got.ID != s.ID || p.Exit() != "addr:b" {
		t.Errorf("got session %d on %q, want %d taken to b", got.ID, p.Exit(), s.ID)
	}
	if !got.Moved() {
		t.Error("the session taken off its address is not marked a stranger where it landed")
	}
}

func TestKeeper_LeavesAloneASessionThatIsNotStranded(t *testing.T) {
	// Only a session waiting for nothing but its address is stranded. One still
	// serving its own rest is not, one whose address carries is due in the
	// ordinary way, and one outside the sessions asked about is not the
	// caller's to move.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	s := answeredOn(t, k, "a", "a", "b")

	resting := listPort(1, "b", "a", "b")
	resting.resting["a"] = true
	if got, err := k.TakeStranded(ctx, resting, desktop, []int64{s.ID}); !errors.Is(err, ErrNothingDue) {
		t.Errorf("a session still serving its rest was moved: %v (%v)", got, err)
	}

	c.pass(time.Minute)
	carrying := listPort(2, "b", "a", "b")
	if got, err := k.TakeStranded(ctx, carrying, desktop, []int64{s.ID}); !errors.Is(err, ErrNothingDue) {
		t.Errorf("a session whose address carries was moved: %v (%v)", got, err)
	}
	other := listPort(3, "b", "a", "b")
	other.resting["a"] = true
	if got, err := k.TakeStranded(ctx, other, desktop, []int64{s.ID + 1}); !errors.Is(err, ErrNothingDue) {
		t.Errorf("a session outside the ones asked about was moved: %v (%v)", got, err)
	}
}

func TestKeeper_CountsNoSessionThatNeverAnsweredAsStranded(t *testing.T) {
	// A session that has never answered holds no clearance anywhere and goes
	// wherever an address is free in the ordinary way. Only a session kept off
	// by the rule — one that has answered, waiting for its own address — is
	// stranded.
	c, h := startClock(), NewMemory()
	k := keeperAt(h, c)
	ctx := context.Background()
	first := listPort(1, "a", "a", "b")
	s, err := k.Take(ctx, first, desktop)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	s.PutBack()
	c.pass(time.Minute)

	p := listPort(2, "b", "a", "b")
	p.resting["a"] = true
	if got, err := k.TakeStranded(ctx, p, desktop, []int64{s.ID}); !errors.Is(err, ErrNothingDue) {
		t.Errorf("a session that never answered was taken as stranded: %v (%v)", got, err)
	}
}

// peeking is a history that calls during with the session's id each time the
// keeper writes down how a session it is giving back ended, before the line
// is written: a test's way of standing in the moment between the two.
type peeking struct {
	History
	during func(id int64)
}

func (p peeking) SessionAnswered(ctx context.Context, id int64, a store.Answer, at time.Time) error {
	p.during(id)
	return p.History.SessionAnswered(ctx, id, a, at)
}

func (p peeking) SessionFailed(ctx context.Context, id int64, at time.Time) (bool, error) {
	p.during(id)
	return p.History.SessionFailed(ctx, id, at)
}

func TestKeeper_HandsASessionOutAgainOnlyOnceWhatBecameOfItIsWrittenDown(t *testing.T) {
	// A session sent elsewhere after a shell, or put down after a refusal, was
	// let go before its record said so: in the moment the line was being written
	// it was free, with the time it was last used from before and the address it
	// was leaving, and another thread could take it — and did, carrying on a
	// query the first thread was in the middle of ending. It is let go once its
	// record is what the history is being told.
	for _, c := range []struct {
		name string
		end  func(ctx context.Context, h *Held) error
	}{
		{"sent elsewhere", func(ctx context.Context, h *Held) error { return h.Elsewhere(ctx) }},
		{"put down after a refusal", func(ctx context.Context, h *Held) error { _, err := h.Failed(ctx); return err }},
	} {
		t.Run(c.name, func(t *testing.T) {
			clock := startClock()
			var k *Keeper
			var taken []int64
			var ending int64
			h := peeking{History: NewMemory(), during: func(id int64) {
				if id != ending {
					return
				}
				// Another thread asking for a session this instant.
				if got, err := k.Take(context.Background(), listPort(7, "b", "b"), desktop); err == nil {
					taken = append(taken, got.ID)
					got.PutBack()
				}
			}}
			k = keeperAt(h, clock)
			ctx := context.Background()
			s := answeredOn(t, k, "a", "a", "b")
			clock.pass(time.Minute)
			again, err := k.Take(ctx, listPort(1, "a", "a", "b"), desktop)
			if err != nil || again.ID != s.ID {
				t.Fatalf("got %v (%v), want session %d back", again, err, s.ID)
			}
			ending = s.ID
			if err := c.end(ctx, again); err != nil {
				t.Fatalf("ending: %v", err)
			}
			if slices.Contains(taken, s.ID) {
				t.Errorf("session %d was handed out while what became of it was being written down", s.ID)
			}
		})
	}
}
