// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
	"github.com/blanktrail/google-serp-parser/internal/web"
)

func TestDial_KeepsASuggestionsJobsConnectionsAliveAndASearchesNot(t *testing.T) {
	// A search opens a connection a request, as it was measured to want; a
	// suggestions job, which has no sessions on its ports, keeps them alive.
	for _, c := range []struct {
		name      string
		keepAlive bool
	}{{"search", false}, {"suggestions", true}} {
		t.Run(c.name, func(t *testing.T) {
			fake := fakebt.New(t)
			fake.SetCA(testCAPEM)
			opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
			saved, _ := opts.saved(io.Discard)
			got, err := opts.dial(t.Context(), saved, web.Wanted{Profile: listProfile(t, ""), Threads: 1, Ports: 1,
				Device: blanktrail.DeviceDesktop, KeepAlive: c.keepAlive})
			if err != nil {
				t.Fatalf("opening the identities: %v", err)
			}
			t.Cleanup(func() { _ = got.Search.Close() })
			if got.Search.KeepsAlive() != c.keepAlive {
				t.Errorf("the ports keep their connections alive: %v, want %v", got.Search.KeepsAlive(), c.keepAlive)
			}
		})
	}
}

func TestDial_KeepsTheLookupPortsConnectionsAliveWhateverTheSearchDoes(t *testing.T) {
	// The ports the hidden addresses are read through carry ten lookups at
	// once and no session: their connections are kept alive under a search
	// whose own are not.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	saved, _ := opts.saved(io.Discard)
	got, err := opts.dial(t.Context(), saved, web.Wanted{Profile: listProfile(t, ""), Threads: 1, Ports: 1,
		Device: blanktrail.DeviceDesktop, Addresses: true})
	if err != nil {
		t.Fatalf("opening the identities: %v", err)
	}
	t.Cleanup(func() {
		_ = got.Search.Close()
		_ = got.Addresses.Close()
	})
	lookups, err := got.Addresses.Identities(t.Context())
	if err != nil {
		t.Fatalf("opening the ports that read addresses: %v", err)
	}
	if got.Search.KeepsAlive() || !lookups.KeepsAlive() {
		t.Errorf("search keeps alive %v, lookups %v; want false and true", got.Search.KeepsAlive(), lookups.KeepsAlive())
	}
}

func TestRaise_RunsASuggestionsJobOnPortsOfItsOwnRatherThanTheStandingOnes(t *testing.T) {
	// The standing identities were opened for searches, a connection a
	// request. A suggestions job on the same profile opens its own, kept alive.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key(), HotPorts: 2})
	saved, _ := opts.saved(io.Discard)
	standing, err := opts.dial(t.Context(), saved, web.Wanted{Profile: store.Profile{}, Threads: 1, Ports: 2,
		Device: blanktrail.DeviceDesktop})
	if err != nil {
		t.Fatalf("opening the standing identities: %v", err)
	}
	t.Cleanup(func() { _ = standing.Search.Close() })
	standing.Search.KeepWarm()
	warm := &warmSet{pool: standing.Search, device: blanktrail.DeviceDesktop, profile: 1}
	raise := opts.raise(saved, false, warm)
	got, err := raise(t.Context(), web.Wanted{Profile: store.Profile{ID: 1, Name: "Default"}, Ports: 1, Threads: 1,
		Device: blanktrail.DeviceDesktop, KeepAlive: true})
	if err != nil {
		t.Fatalf("raising a suggestions job: %v", err)
	}
	t.Cleanup(func() { _ = got.Search.Close() })
	if got.Search == standing.Search || !got.Search.KeepsAlive() {
		t.Error("a suggestions job was run on the standing identities, or on ports that do not keep alive")
	}
}
