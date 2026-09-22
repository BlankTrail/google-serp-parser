// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// cookieOrigin answers the way Google does for a session: every search is
// answered with a cookie of its own — NID=search-1, NID=search-2 — and what came
// in on each search is written down, so a test can see the cookie one answer
// gave going back out on the next request.
type cookieOrigin struct {
	*origin
	mu   sync.Mutex
	sent []string
}

func newCookieOrigin(t *testing.T, search func(n int) string) *cookieOrigin {
	t.Helper()
	c := &cookieOrigin{origin: &origin{}}
	c.origin.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		if !strings.HasPrefix(r.URL.Path, "/search") {
			c.homes.Add(1)
			_, _ = io.WriteString(w, shellBody)
			return
		}
		c.mu.Lock()
		c.sent = append(c.sent, r.Header.Get("Cookie"))
		c.mu.Unlock()
		n := int(c.searches.Add(1))
		http.SetCookie(w, &http.Cookie{Name: "NID", Value: "search-" + strconv.Itoa(n), Path: "/"})
		_, _ = io.WriteString(w, search(n))
	}))
	t.Cleanup(c.origin.Close)
	return c
}

func (c *cookieOrigin) cookiesSent() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sent...)
}

func inSessions(c *blanktrail.PoolConfig) { c.Sessions = true }

var searchDesktop = sessions.Want{Device: "desktop"}

func TestAttempt_AsksThroughAKeptSessionAndWritesItDown(t *testing.T) {
	// A session is ours now: its cookies go out in the request and come back in
	// the answer, and what it holds after the answer is written down with the
	// address it went out through.
	o := newCookieOrigin(t, func(int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 1, inSessions)
	h := sessions.NewMemory()
	a := &Attempt{Pool: f.Pool, Keeper: sessions.NewKeeper(h), Want: searchDesktop}

	if _, err := a.Search(context.Background(), usQuery("x")); err != nil {
		t.Fatalf("Search: %v", err)
	}
	all, err := h.Sessions(context.Background(), "desktop", time.Time{})
	if err != nil || len(all) != 1 {
		t.Fatalf("the history holds %d sessions (err %v), want the one the search made", len(all), err)
	}
	one := all[0]
	if !strings.Contains(string(one.Cookies), "NID") {
		t.Errorf("the session was written down with %s, want the cookie the answer set", one.Cookies)
	}
	if !strings.HasPrefix(one.Exit, "addr:socks5://192.0.2.") {
		t.Errorf("the session was written down at %q, want the address it went out through", one.Exit)
	}
	for _, port := range f.Fake.OpenPorts() {
		if keep, told := f.Fake.KeepSessionsOf(port); told && keep {
			t.Errorf("port %d was told to keep a session of its own", port)
		}
	}
}

func TestAttempt_ReusesAKeptSessionAndSendsItsCookiesBack(t *testing.T) {
	o := newCookieOrigin(t, func(int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2, inSessions)
	h := sessions.NewMemory()
	a := &Attempt{Pool: f.Pool, Keeper: sessions.NewKeeper(h), Want: searchDesktop}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := a.Search(ctx, usQuery("x")); err != nil {
			t.Fatalf("Search %d: %v", i, err)
		}
	}
	if all, _ := h.Sessions(ctx, "desktop", time.Time{}); len(all) != 1 {
		t.Errorf("two searches with no pause made %d sessions, want one used twice", len(all))
	}
	sent := o.cookiesSent()
	if len(sent) != 2 || strings.Contains(sent[0], "NID") || !strings.Contains(sent[1], "NID=search-1") {
		t.Errorf("the searches went out with cookies %q, want none on the first and the first answer's on the second", sent)
	}
}

// wallBody is Google refusing the session itself: the page it answers a client
// it has decided against with. It is not the JavaScript check, which is the
// address's to pass and is answered for elsewhere.
const wallBody = `<!doctype html><html><body><p>Our systems have detected unusual traffic ` +
	`from your computer network.</p></body></html>`

func TestAttempt_CountsARefusalAgainstTheSessionAndGivesItUpAtTheSecond(t *testing.T) {
	o := newCookieOrigin(t, func(int) string { return wallBody })
	f := poolFacing(t, o.addr(), 1, inSessions)
	h := sessions.NewMemory()
	a := &Attempt{Pool: f.Pool, Keeper: sessions.NewKeeper(h), Want: searchDesktop, Tries: 2}

	if _, err := a.Search(context.Background(), usQuery("x")); err == nil {
		t.Fatal("every answer was a refusal, yet the search succeeded")
	}
	if all, _ := h.Sessions(context.Background(), "desktop", time.Time{}); len(all) != 0 {
		t.Errorf("a session refused twice in a row is still kept: %+v", all)
	}
	if r := f.Pool.Stats().Rejections; r != 0 {
		t.Errorf("the pool was told of %d refusals; in a pool of sessions they are the sessions'", r)
	}
}

func TestRunner_WalksAQueryOnOneKeptSessionAndWritesItDownEveryPage(t *testing.T) {
	o := newCookieOrigin(t, func(int) string { return serpBodyWithBar("example.com") })
	f := poolFacing(t, o.addr(), 1, inSessions)
	h := sessions.NewMemory()
	r := &Runner{Pool: f.Pool, Threads: 1, Keeper: sessions.NewKeeper(h), Want: searchDesktop}

	rep := r.Run(context.Background(), Job{Queries: []google.Query{usQuery("x")}, Pages: 2})
	if rep.Results[0].Err != nil || len(rep.Results[0].Pages) != 2 {
		t.Fatalf("the walk ended with %d pages and %v, want two and no error",
			len(rep.Results[0].Pages), rep.Results[0].Err)
	}
	all, _ := h.Sessions(context.Background(), "desktop", time.Time{})
	if len(all) != 1 {
		t.Fatalf("a two-page walk made %d sessions, want one for the whole walk", len(all))
	}
	if !strings.Contains(string(all[0].Cookies), "NID") {
		t.Errorf("the walk's session was written down with %s, want the cookies its pages were given", all[0].Cookies)
	}
	sent := o.cookiesSent()
	if len(sent) != 2 || !strings.Contains(sent[1], "NID=search-1") {
		t.Errorf("page two went out with cookies %q, want what page one was given", sent)
	}
}

func TestAttempt_WalksOnOneKeptSession(t *testing.T) {
	// A walk asked of the attempt itself keeps one session for its pages, as the
	// thread's walks do: page two goes out with what page one was given.
	o := newCookieOrigin(t, func(int) string { return serpBodyWithBar("example.com") })
	f := poolFacing(t, o.addr(), 1, inSessions)
	h := sessions.NewMemory()
	a := &Attempt{Pool: f.Pool, Keeper: sessions.NewKeeper(h), Want: searchDesktop}

	pages, err := a.Walk(context.Background(), usQuery("x"), 2)
	if err != nil || len(pages) != 2 {
		t.Fatalf("Walk: %d pages, %v; want two and no error", len(pages), err)
	}
	if all, _ := h.Sessions(context.Background(), "desktop", time.Time{}); len(all) != 1 {
		t.Errorf("a two-page walk made %d sessions, want one", len(all))
	}
	sent := o.cookiesSent()
	if len(sent) != 2 || !strings.Contains(sent[1], "NID=search-1") {
		t.Errorf("page two went out with cookies %q, want what page one was given", sent)
	}
}

func TestAttempt_KeepsASessionWhoseRequestNeverReachedGoogle(t *testing.T) {
	// A request no address carried says nothing about the session: counted
	// against it, a list of dead addresses — or a service restarting — gave up
	// every session it touched.
	o := newCookieOrigin(t, func(int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 1, inSessions)
	h := sessions.NewMemory()
	a := &Attempt{Pool: f.Pool, Keeper: sessions.NewKeeper(h), Want: searchDesktop, Tries: 2}
	ctx := context.Background()
	if _, err := a.Search(ctx, usQuery("x")); err != nil {
		t.Fatalf("Search: %v", err)
	}

	o.Close() // nothing answers behind the address any more
	if _, err := a.Search(ctx, usQuery("y")); err == nil {
		t.Fatal("a search with nothing behind the address succeeded")
	}
	all, _ := h.Sessions(ctx, "desktop", time.Time{})
	if len(all) != 1 || all[0].Failures != 0 {
		t.Fatalf("after two requests that never reached Google the history holds %+v, want the session kept "+
			"with nothing against it", all)
	}
}

func TestAttempt_WaitsOutAServiceThatIsAwayWithoutSpendingATry(t *testing.T) {
	// An update of the service is a restart: for a while it answers "not
	// ready". The query waits for it rather than spending its tries on it.
	o := newCookieOrigin(t, func(int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 1, inSessions)
	a := &Attempt{Pool: f.Pool, Keeper: sessions.NewKeeper(sessions.NewMemory()), Want: searchDesktop, Tries: 1}

	f.Fake.SetDown(true)
	back := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.Fake.SetDown(false)
		close(back)
	}()
	_, err := a.Search(context.Background(), usQuery("x"))
	<-back
	if err != nil {
		t.Errorf("a service away for a moment cost the query its only try: %v", err)
	}
}

func TestAttempt_OpensAPortTheServiceLostAgainAndCarriesOn(t *testing.T) {
	// A restart of the service loses every port. A port found lost is opened
	// again, on its address, and the query goes on without spending a try.
	o := newCookieOrigin(t, func(int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 1, inSessions)
	a := &Attempt{Pool: f.Pool, Keeper: sessions.NewKeeper(sessions.NewMemory()), Want: searchDesktop, Tries: 1}
	ctx := context.Background()
	if _, err := a.Search(ctx, usQuery("x")); err != nil {
		t.Fatalf("Search: %v", err)
	}

	f.Fake.Restart()
	if _, err := a.Search(ctx, usQuery("y")); err != nil {
		t.Fatalf("after the service lost its ports the query failed: %v", err)
	}
	if len(f.Fake.OpenPorts()) == 0 {
		t.Error("the lost port was not opened again")
	}
}

func TestRunner_CarriesAJobThroughARestartOfTheService(t *testing.T) {
	o := newCookieOrigin(t, func(int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2, inSessions)
	h := sessions.NewMemory()
	r := &Runner{Pool: f.Pool, Threads: 2, Keeper: sessions.NewKeeper(h), Want: searchDesktop}
	ctx := context.Background()
	if rep := r.Run(ctx, Job{Queries: []google.Query{usQuery("a"), usQuery("b")}, Pages: 1}); rep.Results[0].Err != nil {
		t.Fatalf("the first job: %v", rep.Results[0].Err)
	}
	before, _ := h.Sessions(ctx, "desktop", time.Time{})

	f.Fake.Restart()
	rep := r.Run(ctx, Job{Queries: []google.Query{usQuery("c"), usQuery("d"), usQuery("e")}, Pages: 1})
	for i, q := range rep.Results {
		if q.Err != nil {
			t.Errorf("query %d after the restart: %v", i, q.Err)
		}
	}
	after, _ := h.Sessions(ctx, "desktop", time.Time{})
	for _, s := range before {
		if !slices.ContainsFunc(after, func(a store.Session) bool { return a.ID == s.ID }) {
			t.Errorf("session %d did not survive the restart", s.ID)
		}
	}
}

func TestAttempt_AShellBlamesTheAddressAndTakesTheSessionOffIt(t *testing.T) {
	// The JavaScript check Google sets on an address is the address's to pass:
	// the service passes it in a browser of its own through that same address,
	// and a shell is that browser failing to. So the address is blamed and the
	// session is taken off it, with nothing held against the session — one
	// given up for this would be a session lost to a road, its clearance with
	// it, while the address it could not travel went on being handed out.
	o := newCookieOrigin(t, func(int) string { return shellBody })
	f := poolFacing(t, o.addr(), 2, inSessions)
	h := sessions.NewMemory()
	a := &Attempt{Pool: f.Pool, Keeper: sessions.NewKeeper(h), Want: searchDesktop, Tries: 1}

	if _, err := a.Search(context.Background(), usQuery("x")); err == nil {
		t.Fatal("a shell was taken for an answer")
	}
	all, err := h.Sessions(context.Background(), "desktop", time.Time{})
	if err != nil {
		t.Fatalf("reading the sessions: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("the history holds %d sessions, want the one the search made kept", len(all))
	}
	if all[0].Failures != 0 {
		t.Errorf("the session carries %d refusals, want none: the address failed the check, not the session",
			all[0].Failures)
	}
	if all[0].Exit != "" {
		t.Errorf("the session is still written down at %q, want it off the address that could not pass the check",
			all[0].Exit)
	}
	if got := f.Pool.Stats().Rejections; got == 0 {
		t.Error("the address was not blamed for a check it could not pass")
	}
}
