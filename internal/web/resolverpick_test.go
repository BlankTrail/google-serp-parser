// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// fakeRoads stands in for the service and the addresses behind it: reaches says
// whether one host answers through one address under one way of resolving.
type fakeRoads struct {
	mu      sync.Mutex
	reaches func(method string, eg blanktrail.Egress, host string) bool
	specs   []blanktrail.PortSpec
	asked   map[string]int
}

func (f *fakeRoads) ProbeHosts(_ context.Context, spec blanktrail.PortSpec, eg blanktrail.Egress,
	_ *x509.CertPool, urls []string) ([]blanktrail.HostProbe, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	if f.asked == nil {
		f.asked = map[string]int{}
	}
	f.asked[spec.Resolver]++
	f.mu.Unlock()
	out := make([]blanktrail.HostProbe, 0, len(urls))
	for _, u := range urls {
		p := blanktrail.HostProbe{URL: u}
		if f.reaches(spec.Resolver, eg, hostsOf([]string{u})[0]) {
			p.Status = 204
		} else {
			p.Status, p.Reason = 523, "upstream_unreachable"
		}
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeRoads) FetchCAPool(context.Context) (*x509.CertPool, error) { return nil, nil }

// madeUpExits is a list of n made-up exits.
func madeUpExits(n int) []blanktrail.Egress {
	out := make([]blanktrail.Egress, 0, n)
	for i := range n {
		out = append(out, blanktrail.Egress{Upstream: fmt.Sprintf("socks5://192.0.2.%d:1080", i+1)})
	}
	return out
}

// pickOn runs one pick to its end over the roads given and says what it saved.
func pickOn(t *testing.T, roads *fakeRoads, prof store.Profile, list []blanktrail.Egress) (pickReading, *store.Profile) {
	t.Helper()
	var p resolverPick
	var saved *store.Profile
	p.start(context.Background(), roads, prof,
		func(context.Context) ([]blanktrail.Egress, error) { return list, nil },
		func(_ context.Context, got store.Profile) error { saved = &got; return nil })
	deadline := time.Now().Add(10 * time.Second)
	for p.Running() {
		if time.Now().After(deadline) {
			t.Fatal("the pick did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return p.Reading(), saved
}

func listProfileNamed(name string) store.Profile {
	p := store.NewProfile()
	p.ID, p.Name, p.Kind, p.Location = 7, name, "url", "https://example.test/list"
	return p
}

func TestPickResolver_KeepsDelegatingWhereItReachesEverything(t *testing.T) {
	// Handing the name to the proxy costs no lookup and no connection of its
	// own, so where it reaches every host a search needs, nothing else is even
	// tried: each try is ports opened down a road a provider watches.
	roads := &fakeRoads{reaches: func(string, blanktrail.Egress, string) bool { return true }}
	got, saved := pickOn(t, roads, listProfileNamed("wingate"), madeUpExits(50))

	if got.Chosen != blanktrail.ResolverDelegate || saved == nil || saved.Resolver != blanktrail.ResolverDelegate {
		t.Fatalf("picked %q and saved %+v, want delegating", got.Chosen, saved)
	}
	if saved.StrictBypass {
		t.Error("a profile kept on delegating was told to keep names from the proxy")
	}
	if roads.asked[blanktrail.ResolverISP] != 0 || roads.asked[blanktrail.ResolverPool] != 0 {
		t.Errorf("other ways were tried although delegating reached everything: %v", roads.asked)
	}
	if roads.asked[blanktrail.ResolverDelegate] != pickSample || got.Sample != pickSample || got.Holds != 50 {
		t.Errorf("delegating was tried on %d of %d addresses (sample %d), want %d",
			roads.asked[blanktrail.ResolverDelegate], got.Holds, got.Sample, pickSample)
	}
}

func TestPickResolver_MovesOnWhereDelegatingIsRefusedANameAndKeepsNamesAway(t *testing.T) {
	// A residential gateway refused the host Google's check loads its script
	// from by name, and carried the same host by address. Delegating is refused
	// there, the provider's resolvers reach it, and the profile keeps them —
	// with names kept from the proxy, or the fallback to the name the first
	// time an address fails would walk straight back into the refusal.
	roads := &fakeRoads{reaches: func(method string, _ blanktrail.Egress, host string) bool {
		return method != blanktrail.ResolverDelegate || !strings.HasSuffix(host, "gstatic.com")
	}}
	prof := listProfileNamed("residential")
	prof.FirstHop, prof.AllowMITM = "socks5://198.51.100.7:1080", true
	got, saved := pickOn(t, roads, prof, madeUpExits(12))

	if saved == nil || saved.Resolver != blanktrail.ResolverISP || !saved.StrictBypass {
		t.Fatalf("saved %+v, want the provider's resolvers with names kept from the proxy", saved)
	}
	if len(got.Tallies) != 2 || got.Tallies[0].passes() || !got.Tallies[1].passes() {
		t.Errorf("tallies %+v, want delegating refused and the provider's resolvers passing", got.Tallies)
	}
	if roads.asked[blanktrail.ResolverPool] != 0 {
		t.Error("the pool was tried after the provider's resolvers had reached everything")
	}
	for _, spec := range roads.specs {
		if spec.VDNSStrictBypass != (spec.Resolver != blanktrail.ResolverDelegate) {
			t.Errorf("way %q was tried with names kept away %v: a way that may fall back to the name "+
				"is tried as delegating", spec.Resolver, spec.VDNSStrictBypass)
		}
		if spec.FirstHop.Proxy == "" || !spec.AllowMITMUpstream || spec.JSSolver {
			t.Errorf("way %q was tried off the profile's road: hop %+v, mitm %v, solver %v",
				spec.Resolver, spec.FirstHop, spec.AllowMITMUpstream, spec.JSSolver)
		}
	}
	if got.Sample != 12 {
		t.Errorf("a list of twelve was sampled as %d", got.Sample)
	}
}

func TestPickResolver_FallsToThePoolAndLeavesAProfileAloneWhenNothingReachesEverything(t *testing.T) {
	onlyPool := &fakeRoads{reaches: func(method string, _ blanktrail.Egress, host string) bool {
		return method == blanktrail.ResolverPool || host == "www.google.com"
	}}
	if _, saved := pickOn(t, onlyPool, listProfileNamed("x"), madeUpExits(8)); saved == nil ||
		saved.Resolver != blanktrail.ResolverPool {
		t.Errorf("saved %+v, want the pool, the last way in the chain", saved)
	}

	// Addresses that answer, and no way that reaches the hosts the check needs.
	nowhere := &fakeRoads{reaches: func(_ string, _ blanktrail.Egress, host string) bool { return host == "www.google.com" }}
	got, saved := pickOn(t, nowhere, listProfileNamed("x"), madeUpExits(8))
	if saved != nil || got.Fault != "proxies.pick.none" {
		t.Errorf("saved %+v with fault %q, want nothing saved and the reason said", saved, got.Fault)
	}

	// And a sample in which nothing answered at all says so: that is about the
	// addresses, not about any way of resolving.
	dead := &fakeRoads{reaches: func(string, blanktrail.Egress, string) bool { return false }}
	got, saved = pickOn(t, dead, listProfileNamed("x"), madeUpExits(8))
	if saved != nil || got.Fault != "proxies.pick.dead" {
		t.Errorf("saved %+v with fault %q, want nothing saved and the addresses blamed", saved, got.Fault)
	}
}

func TestPickTally_ForgivesAFewDropsAndNotARefusal(t *testing.T) {
	// One exit that drops one connection is not a way that blocks a host; a
	// host most of the answering addresses cannot reach is.
	few := pickTally{Live: 20, Reached: []int{20, 17, 20, 20}}
	if !few.passes() {
		t.Error("a way that missed one host on three addresses in twenty was failed")
	}
	most := pickTally{Live: 20, Reached: []int{20, 15, 20, 20}}
	if most.passes() {
		t.Error("a way that missed one host on a quarter of the addresses passed")
	}
	if (pickTally{Live: 0, Reached: []int{0, 0, 0, 0}}).passes() {
		t.Error("a way under which nothing answered passed")
	}
}

func TestProfile_OffersToPickTheWayOfResolvingOnAProfileThatIsWrittenDown(t *testing.T) {
	// The pick is tried on the profile's own addresses, so a profile that is not
	// written down yet has none to try it on.
	s, _ := proxyProfileServer(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})
	fresh := getBody(t, s, proxiesAt+"?"+profileField+"=new")
	if !strings.Contains(fresh, `id="pick-resolver" disabled`) {
		t.Error("a profile that is not written down is offered the pick")
	}
	prof := store.NewProfile()
	prof.Name, prof.Kind, prof.Location = "wingate", "url", "https://example.test/list"
	id, err := s.store.CreateProfile(t.Context(), prof)
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	body := getBody(t, s, boxesOf(id))
	if !strings.Contains(body, `form="resolver-pick" id="pick-resolver">`) {
		t.Error("a written-down profile is not offered the pick")
	}
	if !strings.Contains(body, `<form id="resolver-pick" method="post" action="/proxies/resolver">`) {
		t.Error("the pick has no form of its own to send")
	}
}
