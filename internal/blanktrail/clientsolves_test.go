// SPDX-License-Identifier: MIT

package blanktrail

import (
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// k's sample takes out of the client connections the solves that still have a
// request waiting on their port, as the reading counts them port by port, and
// not only the solves this program knows of: at the start of a job the service
// solves fresh sessions' checks half a minute before the ladder hears of them.

func TestBudget_TakesTheFreshAttemptsTheServiceIsSolvingOutOfTheSamplesClients(t *testing.T) {
	// Ten fresh attempts, four of which met a check the service is solving
	// while the ladder knows of none: six ordinary requests at two, four
	// solves of five, 32 upstream on ten client connections. The four solves'
	// requests are still on their ports, so (32 - 20) / (10 - 4) = 2; left in
	// the divisor they would teach (32 - 20) / 10 = 1.2.
	b := NewBudget(100, 5)
	takeFresh(t, b, 10)
	b.observeProfile(profileLoad{
		upstream: 32, client: 10, solving: 4, clientSolving: 4,
		solvingPorts: map[int]int{20101: 1, 20102: 1, 20103: 1, 20104: 1},
	}, true)
	if k := ampOf(b); !near(k, 2) {
		t.Errorf("four solves with their requests waiting among ten taught k %v, want 2", k)
	}
	// A service solve whose request has gone has no client, and none is
	// taken out for it: the case TestBudget_TakesTheServicesSolvesOutOfASample
	// pins.
}

func TestBudgets_CountTheSolvesWithAClientPortByPort(t *testing.T) {
	// Five ports of the profile:
	//   A one client, one solve — the solve's request waits there;
	//   B one client, two solves — only one of them can have a request;
	//   C no client, one solve — its request has gone;
	//   E three clients, one solve — a waiting request and two ordinary;
	//   D two clients, no solve — two ordinary requests.
	// Five solves of five, four ordinary requests at two: 33 upstream, seven
	// client connections of which three are the solves' (1 + 1 + 0 + 1). So
	// (33 - 25) / (7 - 3) = 2.
	fake := fakebt.New(t)
	cfg := testPoolConfig(t, fake, newFakeClock(), 1, 5)
	bs := NewBudgets(time.Hour, time.Hour)
	cfg.Budget = bs.For(cfg.Client, 1, 60, 5)
	p, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	ports := p.portNumbers()
	a, bp, c, e, d := ports[0], ports[1], ports[2], ports[3], ports[4]
	fake.SetConns(a, 1, 5)
	fake.SetSolving(a, 1)
	fake.SetConns(bp, 1, 10)
	fake.SetSolving(bp, 2)
	fake.SetConns(c, 0, 5)
	fake.SetSolving(c, 1)
	fake.SetConns(e, 3, 9)
	fake.SetSolving(e, 1)
	fake.SetConns(d, 2, 4)
	bs.pollOnce(t.Context())
	if k := ampOf(cfg.Budget); !near(k, 2) {
		t.Errorf("thirty-three on seven with three solves waiting on their ports taught k %v, want 2", k)
	}
}
