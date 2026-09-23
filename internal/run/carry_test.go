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
	mu   sync.Mutex

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
