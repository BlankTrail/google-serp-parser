// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
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

func TestAttempt_CountsARefusalAgainstTheSessionAndGivesItUpAtTheSecond(t *testing.T) {
	o := newCookieOrigin(t, func(int) string { return shellBody })
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
