// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/settings"
)

func TestSubscriptionOf_ReadsItOutOfTheNameBecauseTheServiceDoesNotSayIt(t *testing.T) {
	// Asked directly, the service answers a flat list with no subscription in
	// it. The names carry it instead: one that arrived with a subscription is
	// named for it, and one uploaded on its own has no dot at all.
	for name, want := range map[string]string{
		"WiseKeys.DE-Germaniya":   "WiseKeys",
		"WiseKeys.EE-Mob-Igrovoy": "WiseKeys",
		"my-own-config":           "",
		"":                        "",
		"a.b.c":                   "a",
	} {
		if got := subscriptionOf(name); got != want {
			t.Errorf("%q belongs to %q, want %q", name, got, want)
		}
	}
}

func TestGatewaysOffered_GroupsBySubscriptionAndTicksWhatWasChosen(t *testing.T) {
	// The screen is a list somebody ticks, and on a real service most of it is
	// one subscription: 26 of 32, with the remaining 6 uploaded by hand. Drawn
	// flat, choosing a country out of it is a search rather than a glance.
	list := blanktrail.GatewayList{Available: true, Gateways: []blanktrail.Gateway{
		{Name: "WiseKeys.ES", Kind: "vless"},
		{Name: "solo-two", Kind: "openvpn"},
		{Name: "WiseKeys.DE", Kind: "vless"},
		{Name: "Other.FR", Kind: "shadowsocks"},
		{Name: "solo-one", Kind: "openvpn"},
	}}
	groups, missing := gatewaysOffered(list, []string{"WiseKeys.DE", "solo-one", "gone-last-week"})

	if missing != 1 {
		t.Errorf("%d chosen gateways are missing, want the one that is gone", missing)
	}
	var names []string
	for _, g := range groups {
		names = append(names, g.Name)
	}
	if len(names) != 3 {
		t.Fatalf("the list came back in %d groups: %v", len(names), names)
	}
	// Named subscriptions first, in their own order; the loose ones last,
	// because that group is a remainder rather than something somebody bought.
	if names[0] != "Other" || names[1] != "WiseKeys" || names[2] != "" {
		t.Errorf("the groups read %v, want Other, WiseKeys, then the loose ones", names)
	}

	for _, g := range groups {
		switch g.Name {
		case "WiseKeys":
			if g.Offered != 2 || g.Chosen != 1 {
				t.Errorf("WiseKeys offers %d and %d are ticked, want 2 and 1", g.Offered, g.Chosen)
			}
			if g.Items[0].Name != "WiseKeys.DE" || !g.Items[0].Chosen {
				t.Errorf("the ticked one is %+v", g.Items[0])
			}
		case "":
			if g.Offered != 2 || g.Chosen != 1 {
				t.Errorf("the loose group offers %d and %d are ticked, want 2 and 1", g.Offered, g.Chosen)
			}
		}
	}

	// A name that is chosen and gone is not drawn: a box for something that is
	// not there invites the reader to fix what they cannot reach.
	for _, g := range groups {
		for _, item := range g.Items {
			if item.Name == "gone-last-week" {
				t.Error("a gateway the service no longer has was offered to be ticked")
			}
		}
	}
}

func TestSaveProxies_WritesDownTheGatewaysThatWereTicked(t *testing.T) {
	// The names are what is saved, not the configurations: what is behind a name
	// lives in the service, is edited there, and a copy kept here would be a
	// second answer to what a gateway is.
	s, path := serverWithSettings(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})

	rec := postForm(t, s, proxiesAt, url.Values{
		"source":  {"gateways"},
		"gateway": {"WiseKeys.DE", "WiseKeys.EE"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want a redirect back to the screen", rec.Code)
	}
	after, err := settings.Load(path)
	if err != nil {
		t.Fatalf("reading the settings back: %v", err)
	}
	if after.Proxy.Kind != settings.ProxyGateways {
		t.Errorf("the source reads %q, want the gateways", after.Proxy.Kind)
	}
	if len(after.Proxy.Gateways) != 2 {
		t.Fatalf("%d gateways were saved, want the two that were ticked: %v",
			len(after.Proxy.Gateways), after.Proxy.Gateways)
	}
	if after.Proxy.Location != "" {
		t.Errorf("a set of gateways was saved with a location of %q", after.Proxy.Location)
	}

	// Unticking every one leaves none, rather than keeping what was there: a
	// box that cannot be cleared is a box that cannot be corrected.
	rec = postForm(t, s, proxiesAt, url.Values{"source": {"gateways"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want the save to go through", rec.Code)
	}
	if after, err = settings.Load(path); err != nil {
		t.Fatalf("reading the settings back: %v", err)
	}
	if len(after.Proxy.Gateways) != 0 {
		t.Errorf("%v were kept after every box was cleared", after.Proxy.Gateways)
	}
}

func TestProxies_OffersTheGatewaysAsASource(t *testing.T) {
	// Somebody who runs on VPN gateways picks them where they pick a file or an
	// address, rather than being told to edit a settings file.
	var offered bool
	for _, o := range sourcesOffered("") {
		if o.Value == sourceGateways {
			offered = true
		}
	}
	if !offered {
		t.Error("the proxy screen does not offer the gateways as a source")
	}
}
