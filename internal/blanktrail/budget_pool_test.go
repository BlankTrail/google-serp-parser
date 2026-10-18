// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"io"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

func TestPool_CountsItsPortsIntoItsProfilesBudget(t *testing.T) {
	fake := fakebt.New(t)
	cfg := testPoolConfig(t, fake, newFakeClock(), 1, 3)
	b := NewBudget(30, 5)
	cfg.Budget = b
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	got := b.ports()
	slices.Sort(got)
	if want := fake.OpenPorts(); !slices.Equal(got, want) {
		t.Errorf("the budget knows ports %v, want the pool's %v", got, want)
	}
	if p.Budget() != b {
		t.Error("the pool does not say which budget it is in")
	}
	_ = p.Close()
	if got := b.ports(); len(got) != 0 {
		t.Errorf("the budget still knows %v after the pool closed", got)
	}
	// The ports are gone with the pool whether or not it left: what must go is
	// the pool itself, or the budget would go on telling a closed pool about
	// the limit.
	if _, holders := b.observe(31, true); len(holders) != 0 {
		t.Errorf("the budget still counts %d pools in after this one closed", len(holders))
	}
}

func TestPool_ClosesTheIdleConnectionsOfItsLookupPortsWhenTheProfileIsOver(t *testing.T) {
	// The connections a lookup port keeps alive between two lookups are the
	// one thing that can be given back at once without cutting anything in
	// flight. A port with a solve on it is not touched, and nothing is asked of
	// the service: no Refresh, no move to another address because of the limit.
	fake := fakebt.New(t)
	cfg := testPoolConfig(t, fake, newFakeClock(), 1, 3)
	b := NewBudget(30, 5)
	cfg.Budget, cfg.BudgetClass = b, ClassLookup
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	var closed []int
	p.closeIdle = func(pt *poolPort) { closed = append(closed, pt.num) }
	ports := fake.OpenPorts()
	u, err := b.Acquire(t.Context(), ClassSearch)
	if err != nil {
		t.Fatal(err)
	}
	u.Solving(ports[0])
	before := len(fake.Requests())
	over, holders := b.observe(31, true)
	if !over {
		t.Fatal("31 against 30 reads as not over")
	}
	for _, h := range holders {
		h.overLimit()
	}
	slices.Sort(closed)
	if !slices.Equal(closed, ports[1:]) {
		t.Errorf("closed the idle connections of %v, want %v — every port but the one solving", closed, ports[1:])
	}
	for _, r := range fake.Requests()[before:] {
		t.Errorf("the pool asked the service %s %s because of the limit", r.Method, r.Path)
	}
}

func TestPool_LeavesTheSearchesConnectionsAloneWhenTheProfileIsOver(t *testing.T) {
	fake := fakebt.New(t)
	cfg := testPoolConfig(t, fake, newFakeClock(), 1, 2)
	cfg.Budget = NewBudget(30, 5)
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	var closed []int
	p.closeIdle = func(pt *poolPort) { closed = append(closed, pt.num) }
	p.overLimit()
	if len(closed) != 0 {
		t.Errorf("a searching pool closed connections on %v because of the limit", closed)
	}
}

func TestPool_ItsPortsTakeTheirPlacesInTheProfilesBudget(t *testing.T) {
	// The wiring end to end: a request through a leased port holds a place in
	// the pool's budget while its answer is unread, and gives it back at the end.
	var conns atomic.Int64
	origin := countingTLSOrigin(t, &conns)
	b := NewBudget(10, 2)
	pool, _ := poolOnAStandIn(t, origin.Listener.Addr().String(), func(cfg *PoolConfig) { cfg.Budget = b })
	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	resp, err := lease.Do(req)
	if err != nil {
		t.Fatalf("the request: %v", err)
	}
	if got := b.Reading().InFlight; got != 1 {
		t.Errorf("%d places held while the answer is unread, want one", got)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if got := b.Reading().InFlight; got != 0 {
		t.Errorf("%d places held after the answer was read, want none", got)
	}
}
