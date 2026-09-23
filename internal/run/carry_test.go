// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
)

// deepOrigin stands in for Google on a query that has more pages than one. Each
// page it serves carries the address of the next under Google's own mark, with
// a tag naming the page — so a test can see whether the walk asked at the
// address it was given or built one of its own.
type deepOrigin struct {
	*httptest.Server
	depth int // how many pages a query has; past it a page offers no next one
	// refuse makes every page from here on Google refusing the session itself.
	refuse atomic.Bool
	// then, when set, runs after each page is written, with the number of pages
	// served so far. It is for a test that changes the world under a walk — an
	// address that dies between two of its pages.
	then func(n int)
	// clears, when set, says what clearance to hand the session with the nth
	// page, the way Google hands one out when its check has just been passed.
	// An empty answer hands none.
	clears func(n int) string
	// shells, when set, says which searches are answered with the page that
	// carries no results at all — Google's check on the address, handed back
	// unsolved. It is the address's failure rather than the session's, and what
	// it costs the session is the address.
	shells func(n int) bool
	// walls, when set, says which searches are answered with Google refusing
	// the session itself, one request at a time — which refuse cannot say,
	// being every request from where it is set.
	walls func(n int) bool
	mu    sync.Mutex

	asked []string
	count int
}

func newDeepOrigin(t *testing.T, depth int) *deepOrigin {
	t.Helper()
	o := &deepOrigin{depth: depth}
	o.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		// Every request on a connection of its own. A kept-alive connection is
		// already joined to the origin and goes on carrying requests whatever
		// the port is told afterwards, so a test that kills an address under a
		// walk would find the walk reaching Google through it regardless.
		w.Header().Set("Connection", "close")
		if !strings.HasPrefix(r.URL.Path, "/search") {
			_, _ = io.WriteString(w, "<html><body>home</body></html>")
			return
		}
		o.mu.Lock()
		o.asked = append(o.asked, r.URL.RequestURI())
		o.count++
		n := o.count
		o.mu.Unlock()
		if o.refuse.Load() {
			_, _ = io.WriteString(w, wallBody)
			return
		}
		if o.shells != nil && o.shells(n) {
			_, _ = io.WriteString(w, shellBody)
			return
		}
		if o.walls != nil && o.walls(n) {
			_, _ = io.WriteString(w, wallBody)
			return
		}
		defer func() {
			if o.then != nil {
				o.then(n)
			}
		}()
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			_, _ = fmt.Sscanf(p, "%d", &page)
		}
		http.SetCookie(w, &http.Cookie{Name: "NID", Value: fmt.Sprintf("search-%d", n), Path: "/"})
		if o.clears != nil {
			if given := o.clears(n); given != "" {
				http.SetCookie(w, &http.Cookie{Name: "GOOGLE_ABUSE_EXEMPTION", Value: given, Path: "/"})
			}
		}
		body := serpBody("example.com")
		if page < o.depth {
			// The address of the next page, with a tag no address this program
			// builds could carry.
			next := fmt.Sprintf("/search?q=%s&amp;ei=E%d&amp;start=%d&amp;sa=N&amp;page=%d",
				r.URL.Query().Get("q"), page, page*10, page+1)
			body += `<div role="navigation"><a href="` + next + `" id="pnnext">Next</a></div>`
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(o.Close)
	return o
}

func (o *deepOrigin) addr() string {
	return strings.TrimPrefix(o.URL, "https://")
}

func (o *deepOrigin) seen() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.asked...)
}

func TestRunner_AsksDeepPagesAtTheAddressesThePagesCarry(t *testing.T) {
	// The address of every page after the first is the one the page before it
	// carried: Google issued it to this session, and its tags name the search
	// this session was shown. A walk that built the address from the query and
	// an offset would ask as a stranger each time.
	o := newDeepOrigin(t, 5)
	f := poolFacing(t, o.addr(), 1, inSessions)
	h := sessions.NewMemory()
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(h),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}}

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 3})
	if rep.Results[0].Err != nil || len(rep.Results[0].Pages) != 3 {
		t.Fatalf("the walk took %d pages and ended with %v, want three and no error",
			len(rep.Results[0].Pages), rep.Results[0].Err)
	}
	asked := o.seen()
	if len(asked) != 3 {
		t.Fatalf("the origin was asked %d times: %q", len(asked), asked)
	}
	for i, want := range []string{"page=2", "page=3"} {
		if !strings.Contains(asked[i+1], want) {
			t.Errorf("page %d was asked for at %q, want the address the page before carried (%s)",
				i+2, asked[i+1], want)
		}
	}
	// And all of it on one session: the pages belong to the session that was
	// shown the first of them.
	if all, _ := h.Sessions(context.Background(), "desktop", time.Time{}); len(all) != 1 {
		t.Errorf("a three-page walk made %d sessions, want one", len(all))
	}
}

func TestRunner_TakesAnotherQueryWhileASessionRestsBetweenPages(t *testing.T) {
	// The pause belongs to the session, and the port does not wait with it. A
	// thread whose session is resting takes another — made for the purpose if
	// none is rested — so one port a thread is enough. Held the old way, the
	// thread would stand still through every pause and the second query would
	// not start until the first had finished.
	const pause = 300 * time.Millisecond
	o := newDeepOrigin(t, 3)
	f := poolFacing(t, o.addr(), 1, inSessions)
	h := sessions.NewMemory()
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(h),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop, Pause: pause}}

	began := time.Now()
	rep := r.Run(context.Background(), Job{
		Queries: []google.Query{usQuery("one"), usQuery("two")}, Pages: 2})
	took := time.Since(began)
	for i, got := range rep.Results {
		if got.Err != nil || len(got.Pages) != 2 {
			t.Fatalf("query %d took %d pages and ended with %v, want two and no error", i, len(got.Pages), got.Err)
		}
	}
	asked := o.seen()
	if len(asked) < 4 {
		t.Fatalf("the origin was asked %d times, want four", len(asked))
	}
	// The second query's first page comes before the first query's second one:
	// that is the thread working rather than waiting.
	if !strings.Contains(asked[0], "one") || !strings.Contains(asked[1], "two") {
		t.Errorf("the first two asks were %q, want one page of each query before either went deeper", asked[:2])
	}
	if all, _ := h.Sessions(context.Background(), "desktop", time.Time{}); len(all) != 2 {
		t.Errorf("two queries walked on %d sessions, want one each", len(all))
	}
	// And the port did not wait with them. Four pages on two sessions cost about
	// one rest, because whenever one session is resting the thread is asking
	// through the other; a thread that took the pause itself, holding the port
	// through it, would cost four rests for the same four pages.
	if took > 3*pause {
		t.Errorf("four pages took %v, want under %v — about the one rest a thread that keeps working costs",
			took, 3*pause)
	}
}

func TestRunner_StopsAWalkWhereThePageOffersNoNextOne(t *testing.T) {
	// The last page of a query links back and not on. That absence is Google
	// saying the results end here, and a walk that went on would ask for a page
	// nobody offered.
	o := newDeepOrigin(t, 2)
	f := poolFacing(t, o.addr(), 1, inSessions)
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}}

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 9})
	if rep.Results[0].Err != nil || len(rep.Results[0].Pages) != 2 {
		t.Fatalf("the walk took %d pages and ended with %v, want the two the pages offered",
			len(rep.Results[0].Pages), rep.Results[0].Err)
	}
	if asked := o.seen(); len(asked) != 2 {
		t.Errorf("the origin was asked %d times: %q", len(asked), asked)
	}
}

func TestRunner_StopsAWalkAtTheDepthTheJobAskedFor(t *testing.T) {
	// The other end of a walk. The page goes on offering more; the job said how
	// deep it wanted to go, and a request past that is one nobody asked for.
	o := newDeepOrigin(t, 50)
	f := poolFacing(t, o.addr(), 1, inSessions)
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}}

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 2})
	if rep.Results[0].Err != nil || len(rep.Results[0].Pages) != 2 {
		t.Fatalf("the walk took %d pages and ended with %v, want the two the job asked for",
			len(rep.Results[0].Pages), rep.Results[0].Err)
	}
	if asked := o.seen(); len(asked) != 2 {
		t.Errorf("the origin was asked %d times: %q", len(asked), asked)
	}
}

func TestRunner_MovesASessionOffADeadAddressAndCarriesItsWalkOn(t *testing.T) {
	// A page that never reached Google says nothing about the session: the road
	// failed, not the search. So the session keeps its cookies and its place in
	// the walk, the port is moved to another address, and the page is asked for
	// again at once — there is nothing to rest from, because nothing was asked.
	o := newDeepOrigin(t, 5)
	f := poolFacing(t, o.addr(), 1, inSessions)
	h := sessions.NewMemory()
	port := 0
	o.then = func(n int) {
		if n == 1 {
			// The address that carried page one dies under the walk.
			f.kill(f.Fake.UpstreamOf(port))
		}
	}
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(h),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}}
	port = f.onePort(t)

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 3, Tries: 2})
	if rep.Results[0].Err != nil || len(rep.Results[0].Pages) != 3 {
		t.Fatalf("the walk took %d pages and ended with %v, want three and no error — the address died, not the session",
			len(rep.Results[0].Pages), rep.Results[0].Err)
	}
	stood := f.stoodOn(port)
	if len(stood) != 2 {
		t.Errorf("the port stood on %d addresses: %q, want the one that died and the one it moved to", len(stood), stood)
	}
	all, _ := h.Sessions(context.Background(), "desktop", time.Time{})
	if len(all) != 1 {
		t.Fatalf("the walk was carried by %d sessions, want the one that opened it", len(all))
	}
	if all[0].Failures != 0 {
		t.Errorf("the session carries %d refusals, want none: the road failed, not the session", all[0].Failures)
	}
}

func TestRunner_StopsMovingASessionAtTheTriesThePhraseIsAllowed(t *testing.T) {
	// The road to Google is the job's to bound, and the bound is the tries its
	// phrase is allowed — the same budget a query gets anywhere else. Unbounded,
	// a walk whose whole list had gone would move from address to address for as
	// long as there were addresses.
	//
	// What it has collected stands: the query is settled as a finished
	// collection rather than a failure, because the pages it took are pages.
	o := newDeepOrigin(t, 9)
	f := poolFacing(t, o.addr(), 1, inSessions)
	h := sessions.NewMemory()
	o.then = func(n int) {
		if n == 1 {
			f.blackout() // the road to the whole list is gone
		}
	}
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(h),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}}
	port := f.onePort(t)

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 9, Tries: 2})
	if rep.Results[0].Err != nil || len(rep.Results[0].Pages) != 1 {
		t.Fatalf("the walk took %d pages and ended with %v, want the one page it got and no failure",
			len(rep.Results[0].Pages), rep.Results[0].Err)
	}
	if stood := f.stoodOn(port); len(stood) != 2 {
		t.Errorf("the port stood on %d addresses: %q, want two — the one it was on and the one try it was allowed",
			len(stood), stood)
	}
	// And the session is put back as it was found. Nothing reached Google, so
	// there is nothing Google refused: a road counted against the session would
	// give up the whole pool on a list that had gone.
	all, _ := h.Sessions(context.Background(), "desktop", time.Time{})
	if len(all) != 1 || all[0].Failures != 0 {
		t.Errorf("after a walk stopped by the road the history holds %+v, want the session kept with nothing against it", all)
	}
	// Four requests went through the port and no more: the front page a session
	// opens with, page one, and the two tries the phrase allows for the page
	// that never arrived.
	if asks := f.carriedBy(port); asks != 4 {
		t.Errorf("the port carried %d requests, want four — the front page, page one, and two tries", asks)
	}
}

func TestRunner_GivesUpASessionGoogleRefusesAfterTheMoveAndKeepsWhatItTook(t *testing.T) {
	// Only Google's answer decides whether a moved session is alive. Refused
	// from the new address, it does not revive: the deeper pages it was keeping
	// are addressed to an exit it no longer has, and every port it were handed
	// would be spent on a request that can only fail. So it is given up there
	// and then — and what it collected is a finished collection, not a failure.
	o := newDeepOrigin(t, 9)
	f := poolFacing(t, o.addr(), 1, inSessions)
	h := sessions.NewMemory()
	port := 0
	o.then = func(n int) {
		if n == 1 {
			f.kill(f.Fake.UpstreamOf(port))
			o.refuse.Store(true)
		}
	}
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(h),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}}
	port = f.onePort(t)

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 9, Tries: 3})
	if rep.Results[0].Err != nil || len(rep.Results[0].Pages) != 1 {
		t.Fatalf("the walk took %d pages and ended with %v, want the one page it took and no failure",
			len(rep.Results[0].Pages), rep.Results[0].Err)
	}
	if all, _ := h.Sessions(context.Background(), "desktop", time.Time{}); len(all) != 0 {
		t.Errorf("the history holds %+v, want nothing: a session refused after the move is given up", all)
	}
}

func TestRunner_DoesNotCallAQueryCollectedWhenNothingWasCollected(t *testing.T) {
	// A session can die before its first page: the address it was given carries
	// nothing, it moves, and Google refuses it from the new one. The session is
	// given up either way — but the query has not started, and a query settled
	// as a finished collection with no page in it is written down as done and
	// never asked again.
	o := newDeepOrigin(t, 9)
	f := poolFacing(t, o.addr(), 1, inSessions)
	h := sessions.NewMemory()
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(h),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}}
	// The address the port stands on is dead before the run starts, and what
	// answers beyond the move is Google refusing the session.
	f.kill(f.Fake.UpstreamOf(f.onePort(t)))
	o.refuse.Store(true)

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 3, Tries: 2})
	if len(rep.Results[0].Pages) != 0 || rep.Results[0].Err == nil {
		t.Errorf("the query came back with %d pages and %v, want nothing and the refusal that stopped it",
			len(rep.Results[0].Pages), rep.Results[0].Err)
	}
	if rep.Done != 0 || rep.Failed != 1 {
		t.Errorf("the run reports %d done and %d failed, want the one query counted as failed", rep.Done, rep.Failed)
	}
	if all, _ := h.Sessions(context.Background(), "desktop", time.Time{}); len(all) != 0 {
		t.Errorf("the history holds %+v, want nothing: a session refused after the move is given up", all)
	}
}

func TestRunner_CountsACheckWhereGoogleHandsTheSessionAFreshClearance(t *testing.T) {
	// How often the sessions are made to pass Google's check is the one reading
	// that says whether they are being asked oftener than their rest allows.
	// The answer that paid for a check looks like any other — it is the page
	// that was asked for — and the only sign is the clearance that came with it.
	o := newDeepOrigin(t, 1)
	// The first search pays for a check to let the session in; the second is
	// answered on the clearance it won, and the third is made to pass another.
	o.clears = func(n int) string {
		switch n {
		case 1:
			return "let-in"
		case 3:
			return "again"
		}
		return ""
	}
	f := poolFacing(t, o.addr(), 1, inSessions)
	counting := counting()
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}, Challenges: counting}

	rep := r.Run(context.Background(), Job{
		Queries: []google.Query{usQuery("one"), usQuery("two"), usQuery("three")}, Pages: 1})
	for i, got := range rep.Results {
		if got.Err != nil {
			t.Fatalf("query %d: %v", i, got.Err)
		}
	}

	// Two checks paid for, and only the second of them counts towards the
	// rhythm: the first was the price of being let in.
	got := counting.Rhythm()
	if got.Met != 2 || got.Asked != 2 || got.AskedMet != 1 {
		t.Errorf("the run reads %+v, want two checks of which one was on an admitted session", got)
	}
	if !got.Known || got.Between != 1 {
		t.Errorf("the run reads %v requests a check, want one", got.Between)
	}
}

func TestRunner_CountsNoCheckAgainstThePaceWhenTheSessionCameFromSomewhereNew(t *testing.T) {
	// A session arriving at an address Google has not seen it at is a stranger
	// there, and the check it pays is the price of the address. Counted against
	// the pace, a list whose addresses keep dying — every death moving a session
	// somewhere new — would read as a run asking too fast, and the advice would
	// be to rest sessions that are already resting.
	o := newDeepOrigin(t, 1)
	f := poolFacing(t, o.addr(), 1, inSessions)
	port := 0
	o.clears = func(n int) string {
		if n == 1 {
			return "let-in"
		}
		return "somewhere-new"
	}
	o.then = func(n int) {
		if n == 1 {
			// The address the session answered on dies under it, so the next
			// request is carried to another one.
			f.kill(f.Fake.UpstreamOf(port))
		}
	}
	counting := counting()
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}, Challenges: counting}
	port = f.onePort(t)

	rep := r.Run(context.Background(), Job{
		Queries: []google.Query{usQuery("one"), usQuery("two")}, Pages: 1, Tries: 3})
	for i, got := range rep.Results {
		if got.Err != nil {
			t.Fatalf("query %d: %v", i, got.Err)
		}
	}
	if stood := f.stoodOn(port); len(stood) < 2 {
		t.Fatalf("the port stood on %q, so nothing moved and this test proves nothing", stood)
	}

	// Both checks are reported — somebody waited for each — and neither is part
	// of the rhythm: one let the session in, the other let it in somewhere else.
	got := counting.Rhythm()
	if got.Met != 2 {
		t.Errorf("%d checks were reported, want the two that were paid for: %+v", got.Met, got)
	}
	if got.Asked != 0 || got.Known {
		t.Errorf("the rhythm reads %+v, want nothing counted towards the pace", got)
	}
}

func TestRunner_CountsNoCheckAgainstThePaceWhereTheSessionWasMovedBetweenRequests(t *testing.T) {
	// The other way a session arrives somewhere new: the address it answered on
	// would not carry its next request — here it failed Google's check on the
	// address — so the session is taken off it and put on another one before the
	// request is made at all. The check it pays there is the price of the new
	// address, and it is the run's list rather than the run's pace that is being
	// read when one is met.
	o := newDeepOrigin(t, 1)
	o.shells = func(n int) bool { return n == 2 }
	o.clears = func(n int) string {
		switch n {
		case 1:
			return "let-in"
		case 3:
			return "somewhere-new"
		}
		return ""
	}
	f := poolFacing(t, o.addr(), 1, inSessions)
	counting := counting()
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}, Challenges: counting}

	rep := r.Run(context.Background(), Job{
		Queries: []google.Query{usQuery("one"), usQuery("two"), usQuery("three"), usQuery("four")},
		Pages:   1, Tries: 1})
	if rep.Results[1].Err == nil {
		t.Fatal("the search answered with a shell came back as a page, so nothing was moved")
	}

	got := counting.Rhythm()
	// Two checks paid for: one to be let in, one to be let in somewhere else.
	if got.Met != 2 {
		t.Errorf("%d checks were reported, want the two that were paid for: %+v", got.Met, got)
	}
	// And one request counted towards the pace: the fourth, asked by a session
	// Google had already answered from the address it was asking from.
	if got.Asked != 1 || got.AskedMet != 0 {
		t.Errorf("the rhythm reads %+v, want the one request that says anything about the pace", got)
	}
}

func TestRunner_CountsTheSessionsItHadMadeForIt(t *testing.T) {
	// A thread makes a session only when none of the ones there are is ready for
	// it, so what a run has had made for it is the reading of whether it is
	// still widening: a run that has stopped making them has as many as its
	// threads can keep busy, and the speed it is going at is its own.
	o := newDeepOrigin(t, 1)
	f := poolFacing(t, o.addr(), 2, inSessions)
	h := sessions.NewMemory()
	watching := NewRamp(time.Minute)
	r := &Runner{Pool: f.Pool, Threads: 2, Keeper: sessions.NewKeeper(h),
		// A minute of rest and a run of milliseconds: nothing comes due, so
		// every query opened is a session made.
		Want: sessions.Want{Device: blanktrail.DeviceDesktop, Pause: time.Minute},
		Ramp: watching}

	rep := r.Run(context.Background(), Job{
		Queries: []google.Query{usQuery("one"), usQuery("two"), usQuery("three")}, Pages: 1})
	for i, got := range rep.Results {
		if got.Err != nil {
			t.Fatalf("query %d: %v", i, got.Err)
		}
	}

	all, _ := h.Sessions(context.Background(), "desktop", time.Time{})
	if got := watching.Ramping(); got.Made != 3 || got.Made != len(all) {
		t.Errorf("the run reads %d sessions made and the history holds %d, want the three it opened",
			got.Made, len(all))
	}
	// And it is still widening: it has made one this instant.
	if got := watching.Ramping(); got.AtSpeed {
		t.Errorf("a run that has just made a session reads %+v, want it widening", got)
	}
}

func TestRunner_SaysSoWhenThereIsNoAddressLeftToOpenASessionOn(t *testing.T) {
	// The other end of a run widening: it stops not because it has enough
	// sessions but because the list will not carry another. Read as the first,
	// a job whose list is too narrow for its threads would show on the screen
	// as one running at its own speed — and what it wants is a wider list or
	// fewer threads, which is the opposite of leaving it alone.
	o := newDeepOrigin(t, 1)
	only, bad := blanktrail.Parse("192.0.2.1:1080", "socks5")
	if len(bad) > 0 {
		t.Fatalf("Parse rejected %v", bad)
	}
	f := poolFacing(t, o.addr(), 2, inSessions, func(c *blanktrail.PoolConfig) {
		// One address, and one session at a time on it: the second thread has
		// nowhere to put a session of its own.
		c.Channels = []blanktrail.Channel{blanktrail.NewListChannel("list", blanktrail.NewStaticRotor(only))}
		c.MaxPerUpstream = 1
	})
	watching := NewRamp(time.Minute)
	r := &Runner{Pool: f.Pool, Threads: 2, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop, Pause: time.Minute}, Ramp: watching}

	r.Run(context.Background(), Job{
		Queries: []google.Query{usQuery("one"), usQuery("two"), usQuery("three")}, Pages: 1, Tries: 1})

	if got := watching.Ramping(); !got.Short {
		t.Errorf("the run reads %+v, want it short of addresses to open a session on", got)
	}
}

func TestRunner_AsksTheAddressAgainThroughTheSameSessionWhereGoogleWouldNotShowThePage(t *testing.T) {
	// A page Google would not show is its check on the address, handed back
	// unsolved. Condemning the address on the first one costs more than the
	// request: the session is taken off it, lands somewhere else, and pays a
	// check to be let in there. Asked again where it stands, it costs one
	// request and the address keeps the session that was on it.
	o := newDeepOrigin(t, 1)
	o.shells = func(n int) bool { return n == 1 }
	f := poolFacing(t, o.addr(), 1, inSessions)
	h := sessions.NewMemory()
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(h),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}, ShellTries: 1}

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 1, Tries: 5})
	if rep.Results[0].Err != nil || len(rep.Results[0].Pages) != 1 {
		t.Fatalf("the query took %d pages and ended with %v, want the one the second asking brought",
			len(rep.Results[0].Pages), rep.Results[0].Err)
	}
	if asked := o.seen(); len(asked) != 2 {
		t.Fatalf("the origin was asked %d times: %q, want the shell and the asking after it", len(asked), asked)
	}
	// Nothing was held against the address: the pool was never told this was
	// not an answer, so the session is still standing where it stood.
	if got := f.Pool.Stats().Rejections; got != 0 {
		t.Errorf("Stats().Rejections=%d, want none - the address was condemned for a check it then passed", got)
	}
	if all, _ := h.Sessions(context.Background(), "desktop", time.Time{}); len(all) != 1 {
		t.Errorf("the run holds %d sessions, want the one that asked twice", len(all))
	}
}

func TestRunner_CondemnsTheAddressWhereTheSecondAskingIsRefusedTheSameWay(t *testing.T) {
	// The second chance is one. An address that would not show the page twice
	// running is not one waiting to be asked again — the check at the end of it
	// is the address's to pass, and its browser could not — so the session is
	// taken off it, as it was before the second chance existed.
	o := newDeepOrigin(t, 1)
	o.shells = func(n int) bool { return n <= 2 }
	f := poolFacing(t, o.addr(), 1, inSessions)
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}, ShellTries: 1}

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 1, Tries: 5})
	if rep.Results[0].Err != nil || len(rep.Results[0].Pages) != 1 {
		t.Fatalf("the query took %d pages and ended with %v, want the one the third asking brought",
			len(rep.Results[0].Pages), rep.Results[0].Err)
	}
	if asked := o.seen(); len(asked) != 3 {
		t.Fatalf("the origin was asked %d times: %q, want two shells and the asking elsewhere", len(asked), asked)
	}
	if got := f.Pool.Stats().Rejections; got != 1 {
		t.Errorf("Stats().Rejections=%d, want the one the second shell earned", got)
	}
}

func TestRunner_CondemnsTheAddressAtOnceWhereTheRunAllowsNoSecondAsking(t *testing.T) {
	// And where the second chance is refused — a negative number, since nought
	// is the number nobody set — the first shell is what it always was: the
	// address's failure, and the session leaves it.
	o := newDeepOrigin(t, 1)
	o.shells = func(n int) bool { return n == 1 }
	f := poolFacing(t, o.addr(), 1, inSessions)
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}, ShellTries: -1}

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 1, Tries: 5})
	if rep.Results[0].Err != nil || len(rep.Results[0].Pages) != 1 {
		t.Fatalf("the query took %d pages and ended with %v, want the one it got after the shell",
			len(rep.Results[0].Pages), rep.Results[0].Err)
	}
	if got := f.Pool.Stats().Rejections; got != 1 {
		t.Errorf("Stats().Rejections=%d, want the one the shell earned", got)
	}
}

func TestRunner_AsksTheAddressAgainWithoutBeingToldTo(t *testing.T) {
	// The second chance is what a run gets when nobody said anything about it:
	// the number a caller leaves at nought is the number nobody set, and what
	// it means is one.
	o := newDeepOrigin(t, 1)
	o.shells = func(n int) bool { return n == 1 }
	f := poolFacing(t, o.addr(), 1, inSessions)
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}}

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 1, Tries: 5})
	if rep.Results[0].Err != nil || len(rep.Results[0].Pages) != 1 {
		t.Fatalf("the query took %d pages and ended with %v, want the one the second asking brought",
			len(rep.Results[0].Pages), rep.Results[0].Err)
	}
	if got := f.Pool.Stats().Rejections; got != 0 {
		t.Errorf("Stats().Rejections=%d, want none - the address was condemned for a check it then passed", got)
	}
}

func TestRunner_AsksNoRefusalOtherThanAShellAgainAtTheSameAddress(t *testing.T) {
	// Only the shell is the address's. A refusal that answers the session —
	// the wall Google puts up in front of one it has decided about — says the
	// same thing however often it is asked, and asking again at the same
	// address spends a request to be told it twice.
	o := newDeepOrigin(t, 2)
	o.walls = func(n int) bool { return n == 2 }
	f := poolFacing(t, o.addr(), 1, inSessions)
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}, ShellTries: 1}

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 2, Tries: 5})
	if rep.Results[0].Err != nil || len(rep.Results[0].Pages) != 1 {
		t.Fatalf("the query took %d pages and ended with %v, want the one page it had before the wall",
			len(rep.Results[0].Pages), rep.Results[0].Err)
	}
	if asked := o.seen(); len(asked) != 2 {
		t.Errorf("the origin was asked %d times: %q, want the page and the wall that ended the walk",
			len(asked), asked)
	}
}

func TestRunner_AsksTheAddressAsManyTimesAsTheRunWasTold(t *testing.T) {
	// A run that names its own number gets it, and not the one it would have
	// had. What the number is for is a road where the check is handed back for
	// reasons of its own — a solver with nothing free answers every request
	// with a shell until it has — and there the address deserves more than one
	// asking before it is blamed.
	o := newDeepOrigin(t, 1)
	o.shells = func(n int) bool { return n <= 3 }
	f := poolFacing(t, o.addr(), 1, inSessions)
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}, ShellTries: 3}

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 1, Tries: 9})
	if rep.Results[0].Err != nil || len(rep.Results[0].Pages) != 1 {
		t.Fatalf("the query took %d pages and ended with %v, want the one the fourth asking brought",
			len(rep.Results[0].Pages), rep.Results[0].Err)
	}
	if asked := o.seen(); len(asked) != 4 {
		t.Fatalf("the origin was asked %d times: %q, want three shells and the asking after them",
			len(asked), asked)
	}
	if got := f.Pool.Stats().Rejections; got != 0 {
		t.Errorf("Stats().Rejections=%d, want none - the address was blamed although the run allowed it three askings", got)
	}
}

func TestRunner_BoundsTheSecondAskingsByTheTriesThePhraseIsAllowed(t *testing.T) {
	// A second chance is a request, and every request a phrase is allowed is
	// counted against it. An address that answers nothing but shells would
	// otherwise be asked as many times as the second chances allow, on top of
	// the tries the phrase was given.
	o := newDeepOrigin(t, 1)
	o.shells = func(int) bool { return true }
	f := poolFacing(t, o.addr(), 1, inSessions)
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}, ShellTries: 5}

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 1, Tries: 2})
	if rep.Results[0].Err == nil {
		t.Fatal("a query nothing but shells answered came back as a page")
	}
	if asked := o.seen(); len(asked) != 2 {
		t.Errorf("the origin was asked %d times: %q, want the two the phrase was allowed",
			len(asked), asked)
	}
}

func TestRunner_KeepsWhatAWalkHadCollectedWhenTheRunIsStopped(t *testing.T) {
	// The pages of a walk reach the history when its query is settled, and a
	// run that stops has walks part way through: some in a thread's hands, the
	// rest under sessions nobody will come back for. Left as they were, every
	// page they had taken goes with them — measured on a live job, three
	// hundred and seventy pages to one press of stop.
	o := newDeepOrigin(t, 5)
	f := poolFacing(t, o.addr(), 1, inSessions)
	sink := &recordingSink{}
	ctx, stop := context.WithCancel(context.Background())
	// The run is stopped the instant its second page comes back, which is the
	// shape a press of stop has: the walk is holding two pages, owes three
	// more, and the page in hand arrived under a context that has just ended.
	answered := 0
	r := &Runner{Pool: f.Pool, Threads: 1, Sink: sink, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop},
		Watch: func(s Step) {
			if s.Stage != StageAsk || s.Err != nil {
				return
			}
			answered++
			if answered == 2 {
				stop()
			}
		}}

	rep := r.Run(ctx, Job{Queries: []google.Query{usQuery("x")}, Pages: 5, Tries: 3})

	got := sink.records()
	if len(got) != 1 {
		t.Fatalf("the sink was told about %d queries, want the one the run was holding", len(got))
	}
	if len(got[0].Pages) != 2 {
		t.Errorf("the query was written down with %d pages, want the two it had taken", len(got[0].Pages))
	}
	if len(rep.Results[0].Pages) != 2 {
		t.Errorf("the report carries %d pages, want the two the walk had", len(rep.Results[0].Pages))
	}
}

func TestRunner_LeavesAPhraseThatNeverReachedGoogleAsItWasFound(t *testing.T) {
	// Every try spent on addresses that carried nothing is a phrase that was
	// never asked. Written down as failed it says the opposite of what
	// happened, and a job resumed afterwards never asks it again — so on a list
	// that is down for an hour, a run turns its whole list into failures nobody
	// can act on.
	o := newDeepOrigin(t, 1)
	f := poolFacing(t, o.addr(), 1, inSessions)
	f.blackout()
	sink := &recordingSink{}
	r := &Runner{Pool: f.Pool, Threads: 1, Sink: sink, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}}

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 1, Tries: 2})

	if got := sink.records(); len(got) != 0 {
		t.Errorf("the sink was told about %d queries, want none: nothing was ever asked", len(got))
	}
	if rep.Untried != 1 || rep.Failed != 0 {
		t.Errorf("the report says %d untried and %d failed, want the phrase left untried: %+v",
			rep.Untried, rep.Failed, rep.Results[0])
	}
	if rep.Results[0].Err != nil {
		t.Errorf("the phrase carries %v, want nothing held against it", rep.Results[0].Err)
	}
}

func TestRunner_StillRecordsAPhraseGoogleItselfRefused(t *testing.T) {
	// The other half of the rule. A refusal Google read and judged is an answer
	// about the phrase, and leaving it unwritten would have the next run ask it
	// again for as long as Google keeps saying the same thing.
	o := newDeepOrigin(t, 1)
	o.refuse.Store(true)
	f := poolFacing(t, o.addr(), 1, inSessions)
	sink := &recordingSink{}
	r := &Runner{Pool: f.Pool, Threads: 1, Sink: sink, Keeper: sessions.NewKeeper(sessions.NewMemory()),
		Want: sessions.Want{Device: blanktrail.DeviceDesktop}}

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 1, Tries: 2})

	if got := sink.records(); len(got) != 1 {
		t.Fatalf("the sink was told about %d queries, want the one Google refused", len(got))
	}
	if rep.Failed != 1 {
		t.Errorf("the report says %d failed, want the one Google itself refused: %+v", rep.Failed, rep.Results[0])
	}
}
