// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/run"
	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
	"github.com/blanktrail/google-serp-parser/internal/web"
)

// limitedProfile names exits, as every profile a pool is raised on must: one
// that names none gets no pool at all (web.ErrNoExits).
func limitedProfile(t *testing.T, id int64) store.Profile {
	t.Helper()
	p := withExitsAs(t, id)
	p.Name, p.ConnLimit = "limited", 30
	return p
}

func TestDial_PutsEveryPoolOfAProfileOnOneBudget(t *testing.T) {
	// The standing set, a job's searches and its lookups spend one tariff.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	opts.Budgets = blanktrail.NewBudgets(time.Hour, time.Hour)
	saved, _ := opts.saved(io.Discard)

	open := func() web.Identities {
		got, err := opts.dial(t.Context(), saved, web.Wanted{Profile: limitedProfile(t, 7), Threads: 1, Ports: 1,
			Device: blanktrail.DeviceDesktop, Addresses: true})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = got.Search.Close(); _ = got.Addresses.Close() })
		return got
	}
	a, b := open(), open()
	if a.Search.Budget() == nil || a.Search.Budget() != b.Search.Budget() {
		t.Fatal("two pools of one profile are not on one budget")
	}
	if r := a.Search.Budget().Reading(); r.Limit != 30 || r.Reserve != store.DefaultSolverReserve {
		t.Errorf("the budget reads %+v, want the profile's 30 and 5", r)
	}
	lookups, err := a.Addresses.Identities(t.Context())
	if err != nil {
		t.Fatalf("opening the lookup ports: %v", err)
	}
	if lookups.Budget() != a.Search.Budget() {
		t.Error("the lookup ports are not on their profile's budget")
	}
	if held, room := a.Addresses.HeldTo(); held != a.Search.Budget() || room != run.LookupsAtOnce {
		t.Errorf("the lookup set is held to %v with room %d, want the profile's budget and %d", held, room, run.LookupsAtOnce)
	}
}

func TestDial_WithNoRegistryAPoolRunsOnNoBudget(t *testing.T) {
	// The command line and tests set no registry; a pool opened without one has
	// no limit to wait on.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	saved, _ := opts.saved(io.Discard)
	got, err := opts.dial(t.Context(), saved, web.Wanted{Profile: limitedProfile(t, 7), Threads: 1, Ports: 1,
		Device: blanktrail.DeviceDesktop})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = got.Search.Close() })
	if got.Search.Budget() != nil {
		t.Error("a pool opened with no registry is on a budget")
	}
}

func TestPoolConfig_TheCommandLineAndEnvironmentPoolsHaveNoBudget(t *testing.T) {
	// They have no profile for a limit to stand in; poolConfig is what both of
	// them open a pool from.
	if poolConfig(4, 1, false).Budget != nil {
		t.Error("a pool configured without a profile carries a budget")
	}
}

func TestLookupPorts_CountAsLookupsAndTheSearchesAsSearches(t *testing.T) {
	cfg := poolConfig(4, 1, false)
	if cfg.BudgetClass != blanktrail.ClassSearch {
		t.Error("the searching ports do not count as searches")
	}
	if lookupPortsOf(cfg).BudgetClass != blanktrail.ClassLookup {
		t.Error("the ports hidden addresses are read through do not count as lookups")
	}
}

func TestStartBudgets_AsksTheServiceWhileTheServerRuns(t *testing.T) {
	fake := fakebt.New(t)
	cl, err := blanktrail.NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	bs := startBudgets(ctx, blanktrail.NewBudgets(5*time.Millisecond, 5*time.Millisecond))
	bs.For(cl, 1, 30, 5)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range fake.Requests() {
			if r.Method == http.MethodGet && (r.Path == "/api/v1/ports/conns" || r.Path == "/api/v1/ports") {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the server never asked the service what its ports carry")
}
