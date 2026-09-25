// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

// widened counts how often a lane asked the set to widen.
func widened(n *atomic.Int64) func(context.Context) {
	return func(context.Context) { n.Add(1) }
}

func TestLanes_TakeAPortOfTheirOwnWhileOneStandsFree(t *testing.T) {
	// What Google sees of one exit is as little as the set allows: every port
	// the set holds carries a lookup before any carries two.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 4)
	ls := &lanes{}
	var asked atomic.Int64

	ports := map[int]bool{}
	for i := 0; i < 4; i++ {
		l, err := ls.take(t.Context(), f.Pool, widened(&asked))
		if err != nil {
			t.Fatalf("take %d: %v", i+1, err)
		}
		defer l.leave()
		ports[l.lease.Port()] = true
	}
	if len(ports) != 4 {
		t.Errorf("four lookups went out through %d ports with four standing free, want one each", len(ports))
	}
	if asked.Load() != 0 {
		t.Errorf("the set was asked to widen %d times while a port stood free, want never", asked.Load())
	}
}

func TestLanes_ShareTheLeastCrowdedPortOnceNoneIsFree(t *testing.T) {
	// With every port carrying a lookup, the next one joins the port carrying
	// fewest — so two ports and four lookups is two and two, not three and one
	// — and each time one has to join, the set is asked to widen.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 2)
	ls := &lanes{}
	var asked atomic.Int64

	on := map[int]int{}
	for i := 0; i < 4; i++ {
		l, err := ls.take(t.Context(), f.Pool, widened(&asked))
		if err != nil {
			t.Fatalf("take %d: %v", i+1, err)
		}
		defer l.leave()
		on[l.lease.Port()]++
	}
	if len(on) != 2 {
		t.Fatalf("four lookups went out through %d ports, want the two", len(on))
	}
	for port, n := range on {
		if n != 2 {
			t.Errorf("port %d carries %d lookups, want two on each: %v", port, n, on)
		}
	}
	if asked.Load() != 2 {
		t.Errorf("the set was asked to widen %d times, want once for each lookup that had to share", asked.Load())
	}
}

func TestLanes_PutNoMoreThanTenOnOnePort(t *testing.T) {
	// Ten is the user's number. The eleventh waits, and takes the first place
	// that comes free rather than waiting for the whole port.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 1)
	ls := &lanes{}
	var asked atomic.Int64

	// Ten written out rather than read from the constant: a test that asked
	// the constant how many to put on the port would agree with any number.
	var held []*lane
	for i := 0; i < 10; i++ {
		l, err := ls.take(t.Context(), f.Pool, widened(&asked))
		if err != nil {
			t.Fatalf("take %d: %v", i+1, err)
		}
		held = append(held, l)
	}
	if held[0] != held[len(held)-1] {
		t.Fatal("the lookups on a one-port set went out through two lanes")
	}

	type took struct {
		l   *lane
		err error
	}
	eleventh := make(chan took, 1)
	go func() {
		l, err := ls.take(t.Context(), f.Pool, widened(&asked))
		eleventh <- took{l, err}
	}()
	select {
	case got := <-eleventh:
		t.Fatalf("an eleventh lookup went out on a port carrying ten (err %v)", got.err)
	case <-time.After(3 * lookupsLookAgain):
	}

	held[0].leave()
	select {
	case got := <-eleventh:
		if got.err != nil {
			t.Fatalf("the eleventh lookup: %v", got.err)
		}
		if got.l != held[0] {
			t.Error("the eleventh lookup took another lane rather than the place that came free")
		}
		got.l.leave()
	case <-time.After(2 * time.Second):
		t.Fatal("the eleventh lookup went on waiting after a place came free")
	}
	for _, l := range held[1:] {
		l.leave()
	}
}

func TestLanes_TakeNoMoreLookupsOnAPortOneCameBackEmptyFrom(t *testing.T) {
	// Whatever the one lookup said about the address holds for the next one
	// too. The address is blamed as a lookup on a port of its own blamed it,
	// and nothing more joins the lane until the port has been given back.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 1)
	ls := &lanes{}
	var asked atomic.Int64

	first, err := ls.take(t.Context(), f.Pool, widened(&asked))
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	second, err := ls.take(t.Context(), f.Pool, widened(&asked))
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	first.refuse(t.Context())
	if got := f.Pool.Stats().Rejections; got != 1 {
		t.Errorf("the pool heard %d refusals, want the one", got)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 4*lookupsLookAgain)
	defer cancel()
	if l, err := ls.take(ctx, f.Pool, widened(&asked)); err == nil {
		l.leave()
		t.Fatal("a lookup joined the lane another had just come back empty from")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the third lookup: %v, want it to wait", err)
	}

	first.leave()
	second.leave()
	third, err := ls.take(t.Context(), f.Pool, widened(&asked))
	if err != nil {
		t.Fatalf("take once the port was given back: %v", err)
	}
	defer third.leave()
	if third == first {
		t.Error("the port came back as the lane that was shut")
	}
}

func TestLanes_GiveThePortBackWhenTheLastLookupLeaves(t *testing.T) {
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 1)
	ls := &lanes{}
	var asked atomic.Int64

	a, err := ls.take(t.Context(), f.Pool, widened(&asked))
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	b, err := ls.take(t.Context(), f.Pool, widened(&asked))
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	a.leave()
	if got := f.Pool.InUse(); got != 1 {
		t.Errorf("%d ports in use with one lookup still on the port, want it held", got)
	}
	b.leave()
	if got := f.Pool.InUse(); got != 0 {
		t.Errorf("%d ports in use once the last lookup left, want it given back", got)
	}
}

// crowded is a destination that holds every request a moment and counts the
// most it held at once, which is how many lookups one port carried together
// when a test has one port.
type crowded struct {
	*httptest.Server
	now, most atomic.Int64
}

func newCrowded(t *testing.T, hold time.Duration) *crowded {
	t.Helper()
	c := &crowded{}
	c.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := c.now.Add(1)
		for {
			m := c.most.Load()
			if n <= m || c.most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(hold)
		c.now.Add(-1)
		w.Header().Set("Location", "https://example.com"+r.URL.Path)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *crowded) addr() string { return c.Listener.Addr().String() }

// eightHidden is a query whose one page hides eight addresses.
func eightHidden(origin string) Report {
	var links []google.Result
	for i := 0; i < 8; i++ {
		links = append(links, unread(fmt.Sprintf("/goto/%d", i+1), "example.com"))
	}
	return Report{Results: []QueryResult{{
		Attempted: true,
		Pages:     []google.SERP{{Origin: origin, Results: links}},
	}}}
}

func TestRunner_ReadsSeveralLinksAtOnceThroughOnePortOfItsOwn(t *testing.T) {
	// One port reading one link at a time was the ceiling a job with addresses
	// ran into: a hundred of them read about seven thousand links a minute and
	// a hundred threads searching wanted nine thousand four hundred. A port the
	// lookups have to themselves carries several at once.
	c := newCrowded(t, 40*time.Millisecond)
	searching := poolFacing(t, c.addr(), 1)
	reading := poolFacing(t, c.addr(), 1)
	rep := eightHidden(c.URL)

	r := &Runner{Pool: searching.Pool, Threads: 1,
		Addresses: func(context.Context) (*blanktrail.Pool, error) { return reading.Pool, nil }}
	if got := r.ResolveLinks(t.Context(), &rep, 4); got.Resolved != 8 {
		t.Fatalf("resolved %d of 8: %v", got.Resolved, got.Errs)
	}
	if got := c.most.Load(); got < 2 {
		t.Errorf("the one port read at most %d links at once with four to read together, want several", got)
	}
}

func TestRunner_ReadsNoTwoLinksAtOnceThroughASearchingPort(t *testing.T) {
	// A run with no ports of its own for the lookups reads them through its
	// searching ports, one at a time on each, as it always did: what goes out
	// through a searching port is what its session is seen doing.
	c := newCrowded(t, 40*time.Millisecond)
	f := poolFacing(t, c.addr(), 1)
	rep := eightHidden(c.URL)

	r := &Runner{Pool: f.Pool, Threads: 1}
	if got := r.ResolveLinks(t.Context(), &rep, 4); got.Resolved != 8 {
		t.Fatalf("resolved %d of 8: %v", got.Resolved, got.Errs)
	}
	if got := c.most.Load(); got != 1 {
		t.Errorf("the searching port read %d links at once, want one at a time", got)
	}
}
