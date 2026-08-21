// SPDX-License-Identifier: MIT

package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
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

func TestGatewayFault_TellsTheFourFailuresApart(t *testing.T) {
	// Each of these sends the reader somewhere different: start the service,
	// fix the key, upgrade the service, read what it said. One sentence for all
	// four sent them to check an address that was often perfectly right.
	for _, c := range []struct {
		what string
		err  error
		want string
	}{
		{"nothing listening", errors.New("dial tcp 127.0.0.1:8891: connect: connection refused"), "proxies.gateways.unreachable"},
		{"key refused", &blanktrail.APIError{Status: http.StatusUnauthorized, Path: "/api/v1/ovpn"}, "proxies.gateways.refused"},
		{"no such endpoint", &blanktrail.APIError{Status: http.StatusNotFound, Path: "/api/v1/ovpn"}, "proxies.gateways.unknown"},
		{"service broke", &blanktrail.APIError{Status: http.StatusInternalServerError, Path: "/api/v1/ovpn"}, "proxies.gateways.failed"},
	} {
		if got := gatewayFault(c.err); got != c.want {
			t.Errorf("%s: said %q, want %q", c.what, got, c.want)
		}
	}
}

func TestGatewayFault_ReadsAnAnswerWrappedInAnotherError(t *testing.T) {
	// The client wraps what the service said on its way up, and a fault read
	// off the outermost error alone would call every one of them unreachable.
	wrapped := fmt.Errorf("asking for gateways: %w", &blanktrail.APIError{Status: http.StatusUnauthorized})
	if got := gatewayFault(wrapped); got != "proxies.gateways.refused" {
		t.Errorf("a wrapped refusal read as %q", got)
	}
}

func TestGatewaysOffered_CarriesTheThreePingStatesApart(t *testing.T) {
	// Never measured is not slow and unreachable is not nought. Drawing either
	// of them as a number would be the screen inventing a measurement nobody
	// took.
	list := blanktrail.GatewayList{Available: true, Gateways: []blanktrail.Gateway{
		{Name: "Sub.Answered", Kind: "vless", Ping: blanktrail.GatewayPing{Tried: true, Answered: true, MS: 77}},
		{Name: "Sub.Silent", Kind: "vless", Ping: blanktrail.GatewayPing{Tried: true}},
		{Name: "Sub.Untried", Kind: "vless"},
	}}
	groups, _ := gatewaysOffered(list, nil)
	if len(groups) != 1 {
		t.Fatalf("groups: %d, want 1", len(groups))
	}
	got := map[string]gatewayChoice{}
	for _, item := range groups[0].Items {
		got[item.Name] = item
	}
	if c := got["Sub.Answered"]; !c.Timed || !c.Tried || c.Ping != 77 {
		t.Errorf("a measured gateway came through as %+v", c)
	}
	if c := got["Sub.Silent"]; c.Timed || !c.Tried {
		t.Errorf("a gateway that did not answer came through as %+v", c)
	}
	if c := got["Sub.Untried"]; c.Timed || c.Tried {
		t.Errorf("a gateway nobody measured came through as %+v", c)
	}
}

// gatewayScreen draws the proxies page against a service holding gws.
func gatewayScreen(t *testing.T, gws []fakebt.Gateway, chosen []string) string {
	t.Helper()
	f := fakebt.New(t)
	f.SetGateways(gws)
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := settings.Save(path, settings.Settings{
		ControlURL: f.URL(), APIKey: f.Key(),
		Proxy: settings.ProxySource{Kind: settings.ProxyGateways, Gateways: chosen},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s, err := New(Config{Store: testStore(t), Logger: quiet(), SettingsPath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return get(t, s, proxiesAt).Body.String()
}

func TestProxies_DrawsALongGatewayListInSomethingThatScrolls(t *testing.T) {
	// Two-and-thirty gateways down the page put the button that saves them past
	// the end of it, and the counts the reader came for off the top. The box is
	// what keeps the form the size of a form, and the ticking of a whole
	// subscription is what keeps it from being thirty-two decisions.
	var gws []fakebt.Gateway
	for i := 0; i < 32; i++ {
		gws = append(gws, fakebt.Gateway{Name: fmt.Sprintf("Sub.Gate-%02d", i), Kind: "vless"})
	}
	page := gatewayScreen(t, gws, nil)
	for _, want := range []string{`class="scrolls"`, `class="band-head"`, `data-tally`, `data-chosen`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page carries no %s, so the list is a page-long column of boxes", want)
		}
	}
	// One pair over the whole list and one in the single subscription's legend:
	// counting them is what tells the two apart, since either pair alone would
	// satisfy a test that only asked whether the words appear anywhere.
	for _, tick := range []string{`data-tick="all"`, `data-tick="none"`} {
		if n := strings.Count(page, tick); n != 2 {
			t.Errorf("%s appears %d times, want one over the list and one in the legend", tick, n)
		}
	}
}

func TestProxies_SaysWhatEachGatewayAnsweredWhenItWasMeasured(t *testing.T) {
	// A number, a word for the ones that did not answer, and a word for the ones
	// nobody has measured. Drawing the last two as numbers would be the screen
	// reporting a measurement that was never taken.
	page := gatewayScreen(t, []fakebt.Gateway{
		{Name: "Sub.Quick", Kind: "vless", Pinged: true, PingMS: 77},
		{Name: "Sub.Silent", Kind: "vless", Pinged: true},
		{Name: "Sub.Untried", Kind: "vless"},
	}, nil)
	for _, want := range []string{"77 ms", "did not answer", "not measured"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page never says %q", want)
		}
	}
}

func TestProxies_OffersToReadTheGatewayListAgain(t *testing.T) {
	// What the service holds changes when somebody adds a configuration or
	// measures the tunnels. The page holds its reading for a couple of minutes
	// so a screen that redraws itself does not ask behind every redraw, which
	// leaves the reader needing a way to say "ask now".
	page := gatewayScreen(t, []fakebt.Gateway{{Name: "Sub.One", Kind: "vless"}}, nil)
	if !strings.Contains(page, `action="/proxies/gateways"`) {
		t.Error("the page offers no way to read the list again")
	}
	// The button stands inside the settings form's layout, so it has to belong
	// to a form of its own: a form inside a form is not markup a browser keeps.
	if !strings.Contains(page, `form="gateways-afresh"`) {
		t.Error("the refresh button is not tied to a form of its own")
	}
}

func TestProxies_ReadsTheListAgainWhenAskedTo(t *testing.T) {
	// Pressing it has to reach the service, not redraw what was already held.
	f := fakebt.New(t)
	f.SetGateways([]fakebt.Gateway{{Name: "Sub.One", Kind: "vless"}})
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := settings.Save(path, settings.Settings{
		ControlURL: f.URL(), APIKey: f.Key(),
		Proxy: settings.ProxySource{Kind: settings.ProxyGateways},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s, err := New(Config{Store: testStore(t), Logger: quiet(), SettingsPath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	get(t, s, proxiesAt) // the first drawing reads the list and holds it

	// A configuration added at the service is not on the held reading.
	f.SetGateways([]fakebt.Gateway{{Name: "Sub.One", Kind: "vless"}, {Name: "Sub.Two", Kind: "vless"}})
	if page := get(t, s, proxiesAt).Body.String(); strings.Contains(page, "Sub.Two") {
		t.Fatal("the page asked the service again on a redraw, which is what holding the list is for")
	}

	if rec := postForm(t, s, gatewaysAt, url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("pressing refresh answered %d, want a redirect back to the screen", rec.Code)
	}
	if page := get(t, s, proxiesAt).Body.String(); !strings.Contains(page, "Sub.Two") {
		t.Error("the list was not read again after the press")
	}
}
