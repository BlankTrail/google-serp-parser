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

func TestLanes_FillAWarmPortBeforeTakingAnother(t *testing.T) {
	// A port in use is a port whose road to Google is open. Spread over every
	// port instead, the lookups at the end of a job came to a lookup or two a
	// minute a port and every one paid the whole road again: 2.7 s where half
	// a second had done. So a lookup joins a port in use while it has room,
	// and only the eleventh takes another — with ports standing free, the set
	// is never asked to widen.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 4)
	ls := &lanes{}
	var asked atomic.Int64

	on := map[int]int{}
	for i := 0; i < 11; i++ {
		l, err := ls.take(t.Context(), f.Pool, widened(&asked))
		if err != nil {
			t.Fatalf("take %d: %v", i+1, err)
		}
		defer l.leave(t.Context())
		on[l.lease.Port()]++
	}
	if len(on) != 2 {
		t.Fatalf("eleven lookups went out through %d ports, want ten on one and the eleventh on another: %v", len(on), on)
	}
	for port, n := range on {
		if n != 10 && n != 1 {
			t.Errorf("port %d carries %d lookups, want ten on one and one on the other: %v", port, n, on)
		}
	}
	if asked.Load() != 0 {
		t.Errorf("the set was asked to widen %d times with ports standing free, want never", asked.Load())
	}
}

func TestLanes_JoinTheLeastCrowdedPortInUse(t *testing.T) {
	// Among the ports in use, the one carrying fewest: seven on one and one on
	// the other, the next lookup goes to the one.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 2)
	ls := &lanes{}
	var asked atomic.Int64

	var first []*lane
	for i := 0; i < 10; i++ {
		l, err := ls.take(t.Context(), f.Pool, widened(&asked))
		if err != nil {
			t.Fatalf("take %d: %v", i+1, err)
		}
		first = append(first, l)
	}
	second, err := ls.take(t.Context(), f.Pool, widened(&asked))
	if err != nil {
		t.Fatalf("take 11: %v", err)
	}
	for _, l := range first[:3] {
		l.leave(t.Context())
	}
	next, err := ls.take(t.Context(), f.Pool, widened(&asked))
	if err != nil {
		t.Fatalf("take 12: %v", err)
	}
	if next != second {
		t.Error("the lookup joined the port carrying seven rather than the one carrying one")
	}
	next.leave(t.Context())
	second.leave(t.Context())
	for _, l := range first[3:] {
		l.leave(t.Context())
	}
}

func TestLanes_PutNoMoreThanTenOnOnePort(t *testing.T) {
	// Ten is the user's number. The eleventh waits — asking the set to widen —
	// and takes the first place that comes free rather than waiting for the
	// whole port.
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
	if asked.Load() == 0 {
		t.Error("a lookup found every port full and the set was not asked to widen")
	}

	held[0].leave(t.Context())
	select {
	case got := <-eleventh:
		if got.err != nil {
			t.Fatalf("the eleventh lookup: %v", got.err)
		}
		if got.l != held[0] {
			t.Error("the eleventh lookup took another lane rather than the place that came free")
		}
		got.l.leave(t.Context())
	case <-time.After(2 * time.Second):
		t.Fatal("the eleventh lookup went on waiting after a place came free")
	}
	for _, l := range held[1:] {
		l.leave(t.Context())
	}
}

func TestLanes_RenewAPortOneCameBackEmptyFromOnceItsLookupsAreDone(t *testing.T) {
	// Whatever the one lookup said about the address holds for the next one
	// too. The address is blamed as a lookup on a port of its own blamed it,
	// nothing more joins the lane, and once the lookups on it are done the
	// port gets another address and another fingerprint before it is given
	// back — the user's rule, in place of any rest.
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
		l.leave(t.Context())
		t.Fatal("a lookup joined the lane another had just come back empty from")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the third lookup: %v, want it to wait", err)
	}

	first.leave(t.Context())
	if got := f.Pool.Stats().ProfileRotations; got != 0 {
		t.Errorf("the port was given a new fingerprint with a lookup still on it (%d)", got)
	}
	second.leave(t.Context())
	if st := f.Pool.Stats(); st.ProfileRotations != 1 {
		t.Errorf("the port was given %d new fingerprints once its lookups were done, want one", st.ProfileRotations)
	}
	third, err := ls.take(t.Context(), f.Pool, widened(&asked))
	if err != nil {
		t.Fatalf("take once the port was given back: %v", err)
	}
	defer third.leave(t.Context())
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
	a.leave(t.Context())
	if got := f.Pool.InUse(); got != 1 {
		t.Errorf("%d ports in use with one lookup still on the port, want it held", got)
	}
	b.leave(t.Context())
	if got := f.Pool.InUse(); got != 0 {
		t.Errorf("%d ports in use once the last lookup left, want it given back", got)
	}
	// And given back as it was: nothing failed and nothing is due.
	if got := f.Pool.Stats().ProfileRotations; got != 0 {
		t.Errorf("a port nothing failed on was given %d new fingerprints, want none", got)
	}
}

func TestLanes_RenewAPortAfterThreeThousandLookups(t *testing.T) {
	// Every so often by the number of lookups, the user's rule. Counted by the
	// port across every lane it has been, and started again once it is renewed.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 1)
	ls := &lanes{}
	var asked atomic.Int64

	one := func() *lane {
		t.Helper()
		l, err := ls.take(t.Context(), f.Pool, widened(&asked))
		if err != nil {
			t.Fatalf("take: %v", err)
		}
		return l
	}
	// Ten at a time for most of them, so the lookups that join a port count as
	// well as the ones that take it.
	for round := 0; round < 299; round++ {
		var ten []*lane
		for i := 0; i < 10; i++ {
			ten = append(ten, one())
		}
		for _, l := range ten {
			l.leave(t.Context())
		}
	}
	for i := 0; i < 9; i++ {
		one().leave(t.Context())
	}
	if got := f.Pool.Stats().ProfileRotations; got != 0 {
		t.Fatalf("the port was renewed %d times in 2999 lookups, want not before three thousand", got)
	}
	last := one()
	// The three thousandth is on the port: nothing more joins it.
	ctx, cancel := context.WithTimeout(t.Context(), 3*lookupsLookAgain)
	defer cancel()
	if l, err := ls.take(ctx, f.Pool, widened(&asked)); err == nil {
		l.leave(t.Context())
		t.Fatal("a lookup joined a port that is to be renewed")
	}
	last.leave(t.Context())
	if got := f.Pool.Stats().ProfileRotations; got != 1 {
		t.Errorf("the port was given %d new fingerprints after three thousand lookups, want one", got)
	}
	one().leave(t.Context())
	if got := f.Pool.Stats().ProfileRotations; got != 1 {
		t.Errorf("the count did not start again after the renewal: %d fingerprints", got)
	}
}

func TestLanes_RenewAPortThatHasStoodTwentyMinutes(t *testing.T) {
	// And every so often by time: a port on a quiet run is not left on one
	// address for the length of it.
	d := newDestination(t)
	f := poolFacing(t, d.addr(), 1)
	at := time.Unix(1000, 0)
	ls := &lanes{now: func() time.Time { return at }}
	var asked atomic.Int64

	l, err := ls.take(t.Context(), f.Pool, widened(&asked))
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	l.leave(t.Context())
	at = at.Add(19 * time.Minute)
	if l, err = ls.take(t.Context(), f.Pool, widened(&asked)); err != nil {
		t.Fatalf("take: %v", err)
	}
	l.leave(t.Context())
	if got := f.Pool.Stats().ProfileRotations; got != 0 {
		t.Fatalf("the port was renewed %d times after nineteen minutes, want not before twenty", got)
	}
	at = at.Add(time.Minute)
	if l, err = ls.take(t.Context(), f.Pool, widened(&asked)); err != nil {
		t.Fatalf("take: %v", err)
	}
	l.leave(t.Context())
	if got := f.Pool.Stats().ProfileRotations; got != 1 {
		t.Errorf("the port was given %d new fingerprints after twenty minutes on its address, want one", got)
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
