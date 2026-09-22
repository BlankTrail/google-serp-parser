// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
	"github.com/blanktrail/google-serp-parser/internal/web"
)

// listProfile is a profile on a file of two addresses, going through firstHop.
func listProfile(t *testing.T, firstHop string) store.Profile {
	t.Helper()
	list := filepath.Join(t.TempDir(), "list.txt")
	if err := os.WriteFile(list, []byte("192.0.2.1:1080\n192.0.2.2:1080\n"), 0o600); err != nil {
		t.Fatalf("writing the list: %v", err)
	}
	prof := store.NewProfile()
	prof.Name, prof.Kind, prof.Location, prof.FirstHop = "wingate", "file", list, firstHop
	return prof
}

// opensOf are the bodies of every port the fake was asked to open.
func opensOf(fake *fakebt.Server) []string {
	var out []string
	for _, r := range fake.Requests() {
		if r.Path == "/api/v1/ports/open" {
			out = append(out, r.Body)
		}
	}
	return out
}

func TestDial_OpensAListsPortsOnTheFirstHopItsProfileNames(t *testing.T) {
	// The first hop is the road a list's ports take to their addresses, and it is
	// the profile's. Set on the profile and missing from the ports, the screen
	// would say one road and the run would take another — the one a wingate
	// list answered nothing on.
	for _, c := range []struct{ kept, said string }{
		{"gw:vless-185", `"chain_gateway":"vless-185"`},
		{"user:secret@198.51.100.7:2334", `"chain_proxy":"socks5://user:secret@198.51.100.7:2334"`},
	} {
		fake := fakebt.New(t)
		fake.SetCA(testCAPEM)
		opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
		saved, _ := opts.saved(io.Discard)
		want, err := opts.dial(t.Context(), saved, web.Wanted{
			Profile: listProfile(t, c.kept), Threads: 2, Ports: 1, Device: blanktrail.DeviceDesktop})
		if err != nil {
			t.Fatalf("opening the identities: %v", err)
		}
		t.Cleanup(func() { _ = want.Search.Close() })
		opened := opensOf(fake)
		if len(opened) == 0 {
			t.Fatal("no port was opened at all")
		}
		for _, body := range opened {
			if !strings.Contains(body, c.said) {
				t.Errorf("a port of a profile going through %q was opened without %s", c.kept, c.said)
			}
		}
	}
}

func TestDial_TakesNoFirstHopForAProfileOnGateways(t *testing.T) {
	// A profile on gateways has its road set on the gateway, in the service.
	// What its box held is kept for the day it goes back to a list, and a port
	// opened on a gateway with it would be a road nobody chose for it.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	fake.SetGateways([]fakebt.Gateway{{Name: "nl-one", Kind: "vless"}})
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	saved, _ := opts.saved(io.Discard)
	prof := store.NewProfile()
	prof.Name, prof.Kind, prof.Gateways = "vpn", settings.ProxyGateways, []string{"nl-one"}
	prof.FirstHop = "gw:vless-185"
	want, err := opts.dial(t.Context(), saved, web.Wanted{Profile: prof, Threads: 1, Ports: 1, Device: blanktrail.DeviceDesktop})
	if err != nil {
		t.Fatalf("opening the identities: %v", err)
	}
	t.Cleanup(func() { _ = want.Search.Close() })
	opened := opensOf(fake)
	if len(opened) == 0 {
		t.Fatal("no port was opened at all")
	}
	for _, body := range opened {
		if strings.Contains(body, "chain_") {
			t.Errorf("a port on a gateway was opened through a first hop: %s", body)
		}
	}
}

func TestDial_RefusesAFirstHopItCannotReadWithoutRepeatingIt(t *testing.T) {
	// A first hop kept by some other hand may be one the service cannot go
	// through. A job run on it anyway would go straight to its addresses — the
	// road the profile says it does not take — so it does not run, and the
	// reason given does not repeat the proxy, whose address carries a password.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	saved, _ := opts.saved(io.Discard)
	_, err := opts.dial(t.Context(), saved, web.Wanted{
		Profile: listProfile(t, "http://user:secret@198.51.100.7:8080"), Threads: 1, Ports: 1,
		Device: blanktrail.DeviceDesktop})
	if err == nil {
		t.Fatal("a job ran on a first hop the service cannot go through")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("the refusal repeats the proxy's password: %v", err)
	}
	if n := len(opensOf(fake)); n != 0 {
		t.Errorf("%d ports were opened for a job that could not run", n)
	}
}

func TestDial_OpensThePortsThatReadAddressesOnTheFirstHopToo(t *testing.T) {
	// The ports that read a hidden address go out through the same list, to the
	// same Google, as the ports that search: a road the searches need is a road
	// the lookups need. They are opened from the job's single template rather
	// than from the spread of browsers, so the first hop has to be on that one
	// as well.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	saved, _ := opts.saved(io.Discard)
	want, err := opts.dial(t.Context(), saved, web.Wanted{
		Profile: listProfile(t, "gw:vless-185"), Threads: 1, Ports: 1,
		Device: blanktrail.DeviceDesktop, Addresses: true})
	if err != nil {
		t.Fatalf("opening the identities: %v", err)
	}
	t.Cleanup(func() {
		_ = want.Search.Close()
		_ = want.Addresses.Close()
	})
	searching := len(opensOf(fake))
	if _, err := want.Addresses.Identities(t.Context()); err != nil {
		t.Fatalf("opening the ports that read addresses: %v", err)
	}
	lookups := opensOf(fake)[searching:]
	if len(lookups) == 0 {
		t.Fatal("no port was opened to read addresses through")
	}
	for _, body := range lookups {
		if !strings.Contains(body, `"chain_gateway":"vless-185"`) {
			t.Errorf("a port reading addresses was opened without the first hop: %s", body)
		}
	}
}
