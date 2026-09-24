// SPDX-License-Identifier: MIT

package main

import (
	"context"
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

func TestRaise_RunsOnlyJobsOfItsOwnProfileOnTheStandingIdentities(t *testing.T) {
	// The standing identities are the default profile's: its list, its first
	// hop. A job naming another profile run on them would go out through the
	// wrong list by the wrong road, while its profile's screen said otherwise.
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

	same, err := raise(t.Context(), web.Wanted{Profile: store.Profile{ID: 1, Name: "Default"}, Ports: 1, Threads: 2,
		Device: blanktrail.DeviceDesktop})
	if err != nil {
		t.Fatalf("raising a job on the standing identities' own profile: %v", err)
	}
	if same.Search != standing.Search {
		t.Error("a job on the profile the standing identities were opened on was not run on them")
	}

	other := listProfile(t, "gw:vless-185")
	other.ID = 2
	before := len(opensOf(fake))
	got, err := raise(t.Context(), web.Wanted{Profile: other, Ports: 1, Threads: 1, Device: blanktrail.DeviceDesktop})
	if err != nil {
		t.Fatalf("raising a job on another profile: %v", err)
	}
	t.Cleanup(func() { _ = got.Search.Close() })
	if got.Search == standing.Search {
		t.Fatal("a job on another profile was run on the standing identities")
	}
	opened := opensOf(fake)[before:]
	if len(opened) == 0 {
		t.Fatal("the job on another profile opened no ports of its own")
	}
	for _, body := range opened {
		if !strings.Contains(body, `"chain_gateway":"vless-185"`) {
			t.Errorf("the other profile's port was opened without its first hop: %s", body)
		}
	}
}

func TestWarmSet_RemembersTheProfileItsIdentitiesWereOpenedOn(t *testing.T) {
	// Which profile the standing identities are is decided when they are opened
	// — the default of that moment — so that is when it is written down.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	saved, _ := opts.saved(io.Discard)
	warm := &warmSet{log: opts.logger(io.Discard)}
	warm.dial = func(ctx context.Context, want int, device string) (*blanktrail.Pool, int64, error) {
		got, err := opts.dial(ctx, saved, web.Wanted{Profile: store.Profile{}, Threads: 1, Ports: want, Device: device})
		if err != nil {
			return nil, 0, err
		}
		return got.Search, 7, nil
	}
	if err := warm.bring(t.Context(), 1, blanktrail.DeviceDesktop); err != nil {
		t.Fatalf("opening the standing identities: %v", err)
	}
	t.Cleanup(func() {
		warm.mu.Lock()
		stop, pool := warm.stop, warm.pool
		warm.mu.Unlock()
		if stop != nil {
			stop()
		}
		if pool != nil {
			_ = pool.Close()
		}
	})
	if _, mine, err := warm.raiseFor(t.Context(), 1, 1, blanktrail.DeviceDesktop, 7, false); err != nil || !mine {
		t.Errorf("a job on the profile the set was opened on was turned away (%v)", err)
	}
	if _, mine, _ := warm.raiseFor(t.Context(), 1, 1, blanktrail.DeviceDesktop, 8, false); mine {
		t.Error("a job on another profile was handed the standing identities")
	}
}

func TestRaiseFor_ServesAJobOnAnyProfileFromASetOpenedOnNone(t *testing.T) {
	// A set opened on no profile goes out through the list in the environment,
	// which is every job's list: turning a job away from it for naming a
	// profile would open a second set of ports for the same list.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	saved, _ := opts.saved(io.Discard)
	standing, err := opts.dial(t.Context(), saved, web.Wanted{Profile: store.Profile{}, Threads: 1, Ports: 1,
		Device: blanktrail.DeviceDesktop})
	if err != nil {
		t.Fatalf("opening the standing identities: %v", err)
	}
	t.Cleanup(func() { _ = standing.Search.Close() })
	warm := &warmSet{pool: standing.Search, device: blanktrail.DeviceDesktop}
	if _, mine, err := warm.raiseFor(t.Context(), 1, 1, blanktrail.DeviceDesktop, 5, false); err != nil || !mine {
		t.Errorf("a job naming a profile was turned away from a set opened on none (%v)", err)
	}
}

func TestJobs_OpenTheStandingIdentitiesOnTheDefaultProfileAndSaySo(t *testing.T) {
	// Which profile the standing identities are is the default of the moment
	// they are opened. A set that did not write it down would serve every job
	// whatever its profile, which is how a job on another profile came to run
	// through the default one's list.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	t.Setenv(envControlURL, "")
	t.Setenv(envAPIKey, "")
	t.Setenv(envProxyList, "")
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key(), HotPorts: 1})
	st, err := store.Open(opts.DB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prof := listProfile(t, "gw:vless-185")
	id, err := st.CreateProfile(t.Context(), prof)
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	sup, warm := opts.jobs(t.Context(), io.Discard, st)
	t.Cleanup(func() { _ = sup.Close() })
	warm.mu.Lock()
	on, pool := warm.profile, warm.pool
	warm.mu.Unlock()
	if pool == nil {
		t.Fatal("no standing identities were opened")
	}
	if on != id {
		t.Errorf("the standing identities say they were opened on profile %d, want the default %d", on, id)
	}
}

func TestDial_OpensAListsPortsWithWhatItsProfileSaysAboutNamesAndTLS(t *testing.T) {
	// Two answers that live on the profile and are worth nothing until they
	// reach the ports. Measured on a live list: seven addresses in ten
	// terminate TLS themselves, and a port that refuses that answers 526 on
	// every one of them while the service's own check walks straight through —
	// which is how a working list read as a dead one for a night.
	//
	// The other is where the name is resolved. Delegating hands it to the proxy,
	// so it is resolved by whatever the exit itself uses.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	saved, _ := opts.saved(io.Discard)

	prof := listProfile(t, "")
	prof.Resolver, prof.CustomResolvers = "custom", []string{"1.1.1.1:53"}
	prof.StrictBypass = true
	want, err := opts.dial(t.Context(), saved, web.Wanted{
		Profile: prof, Threads: 2, Ports: 1, Device: blanktrail.DeviceDesktop, Addresses: true})
	if err != nil {
		t.Fatalf("opening the identities: %v", err)
	}
	t.Cleanup(func() {
		_ = want.Search.Close()
		_ = want.Addresses.Close()
	})
	searching := opensOf(fake)
	if len(searching) == 0 {
		t.Fatal("no port was opened at all")
	}
	// And the ports that read a hidden address, which are opened from the job's
	// single template rather than from the spread of browsers: an answer set
	// only on the spread is an answer those ports never get.
	if _, err := want.Addresses.Identities(t.Context()); err != nil {
		t.Fatalf("opening the ports that read addresses: %v", err)
	}
	lookups := opensOf(fake)[len(searching):]
	if len(lookups) == 0 {
		t.Fatal("no port was opened to read addresses through")
	}

	for what, bodies := range map[string][]string{"searching": searching, "reading addresses": lookups} {
		for _, body := range bodies {
			for _, said := range []string{
				`"allow_mitm_upstream":true`,
				`"vdns_strict_bypass":true`,
				`"resolver_strategy":"custom"`,
				`"custom_resolvers":["1.1.1.1:53"]`,
			} {
				if !strings.Contains(body, said) {
					t.Errorf("a port %s was opened without %s", what, said)
				}
			}
		}
	}
}
