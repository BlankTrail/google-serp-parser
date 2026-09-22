// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

func TestProfileForm_ReadsTheFirstHopItWasGiven(t *testing.T) {
	// The first hop is the road a list's ports take to their addresses, kept the
	// way blanktrail reads it: nothing, "gw:" and a gateway, or a SOCKS5 proxy
	// whole — written the way an address on a list may be.
	cases := []struct {
		form profileForm
		want string
	}{
		{profileForm{Source: sourceFile, HopKind: hopSOCKS5, HopProxy: "user:secret@198.51.100.7:2334"},
			"socks5://user:secret@198.51.100.7:2334"},
		{profileForm{Source: sourceURL, HopKind: hopGateway, HopGateway: "vless-185"}, "gw:vless-185"},
		{profileForm{Source: sourceFile, HopKind: hopNone, HopProxy: "socks5://198.51.100.7:2334"}, ""},
	}
	for _, c := range cases {
		c.form.Name = "exits"
		was := store.NewProfile()
		was.FirstHop = "gw:before"
		got, complaints := c.form.onto(was)
		if len(complaints) > 0 || got.FirstHop != c.want {
			t.Errorf("%+v saved the first hop %q with %v, want %q", c.form, got.FirstHop, complaints, c.want)
		}
	}
}

func TestProfileForm_RefusesAFirstHopTheServiceCannotUse(t *testing.T) {
	// Refused here rather than sent on: a first hop the service cannot go
	// through is a port that never carries anything, found out an hour into a
	// job instead of on the screen where it was typed.
	cases := []struct {
		form      profileForm
		complaint string
	}{
		{profileForm{HopKind: hopSOCKS5}, "proxies.hop.needs.proxy"},
		{profileForm{HopKind: hopSOCKS5, HopProxy: "http://198.51.100.7:8080"}, "proxies.hop.proxy.bad"},
		{profileForm{HopKind: hopSOCKS5, HopProxy: "gw:vless-185"}, "proxies.hop.proxy.bad"},
		{profileForm{HopKind: hopGateway}, "proxies.hop.needs.gateway"},
		{profileForm{HopKind: "carrier pigeon"}, "proxies.hop.unknown"},
	}
	for _, c := range cases {
		c.form.Name, c.form.Source = "exits", sourceFile
		was := store.NewProfile()
		was.FirstHop = "gw:before"
		got, complaints := c.form.onto(was)
		if !slices.Contains(complaints, c.complaint) {
			t.Errorf("%+v complained %v, want %s", c.form, complaints, c.complaint)
		}
		if got.FirstHop != was.FirstHop {
			t.Errorf("%+v was refused and still moved the first hop to %q", c.form, got.FirstHop)
		}
	}
}

func TestProfileForm_KeepsTheFirstHopOfAProfileOnGateways(t *testing.T) {
	// A profile on gateways has its road set on the gateway, in the service, and
	// the box is not on its screen. What it holds is kept rather than emptied by
	// a form that never showed it — the profile may go back to its list.
	was := store.NewProfile()
	was.FirstHop = "socks5://198.51.100.7:2334"
	got, complaints := profileForm{Name: "exits", Source: sourceGateways}.onto(was)
	if len(complaints) > 0 || got.FirstHop != was.FirstHop {
		t.Errorf("the first hop is %q (%v) after choosing the gateways, want %q kept", got.FirstHop, complaints, was.FirstHop)
	}
}

func TestProfileShowing_PutsTheFirstHopBackInItsBoxes(t *testing.T) {
	cases := []struct {
		kept                 string
		kind, proxy, gateway string
	}{
		{"", hopNone, "", ""},
		{"gw:vless-185", hopGateway, "", "vless-185"},
		{"socks5://user:secret@198.51.100.7:2334", hopSOCKS5, "socks5://user:secret@198.51.100.7:2334", ""},
		// One that no longer reads is put where the reader can see it and put it
		// right, rather than dropped without a word.
		{"http://198.51.100.7:8080", hopSOCKS5, "http://198.51.100.7:8080", ""},
	}
	for _, c := range cases {
		p := store.NewProfile()
		p.FirstHop = c.kept
		f := profileShowing(p)
		if f.HopKind != c.kind || f.HopProxy != c.proxy || f.HopGateway != c.gateway {
			t.Errorf("%q is shown as %q/%q/%q, want %q/%q/%q", c.kept, f.HopKind, f.HopProxy, f.HopGateway,
				c.kind, c.proxy, c.gateway)
		}
	}
}

// hopScreen is the proxies page for one profile against a service holding gws,
// and the server that drew it.
func hopScreen(t *testing.T, gws []fakebt.Gateway, prof store.Profile) (string, *Server, int64) {
	t.Helper()
	f := fakebt.New(t)
	f.SetGateways(gws)
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := settings.Save(path, settings.Settings{ControlURL: f.URL(), APIKey: f.Key()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st := testStore(t)
	id, err := st.CreateProfile(t.Context(), prof)
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	s, err := New(Config{Store: st, Logger: quiet(), SettingsPath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return get(t, s, boxesOf(id)).Body.String(), s, id
}

func TestProxies_OffersTheServicesGatewaysAsAFirstHop(t *testing.T) {
	// A first hop may be one of the gateways the service holds, chosen from the
	// ones it has rather than typed: a name typed wrong is a port that will not
	// open.
	page, _, _ := hopScreen(t, []fakebt.Gateway{{Name: "vless-185", Kind: "vless"}, {Name: "Sub.Two", Kind: "vless"}},
		store.Profile{Name: "Default", Kind: sourceURL, Location: "https://example.test/list.txt",
			FirstHop: "gw:Sub.Two", Solver: true})
	for _, want := range []string{`name="first_hop"`, `name="first_hop_proxy"`, `name="first_hop_gateway"`,
		`value="vless-185"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the form of a profile on a list offers no %s", want)
		}
	}
	if !strings.Contains(page, `value="Sub.Two" selected`) {
		t.Error("the gateway the profile goes through first is not the one chosen on its form")
	}
}

func TestProxies_DrawsNoFirstHopOnAProfileOnGateways(t *testing.T) {
	page, _, _ := hopScreen(t, []fakebt.Gateway{{Name: "vless-185", Kind: "vless"}},
		store.Profile{Name: "Default", Kind: sourceGateways, Gateways: []string{"vless-185"}, Solver: true})
	if strings.Contains(page, `name="first_hop"`) {
		t.Error("a profile on gateways is offered a first hop, which its gateways set for themselves")
	}
}

func TestSaveProxies_WritesTheFirstHopDown(t *testing.T) {
	_, s, id := hopScreen(t, []fakebt.Gateway{{Name: "vless-185", Kind: "vless"}},
		store.Profile{Name: "Default", Kind: sourceFile, Location: "C:/lists/wingate.txt", Solver: true})
	rec := postForm(t, s, proxiesAt, profileValues(id, url.Values{
		"source": {"file"}, "source_at": {"C:/lists/wingate.txt"}, "port_protocol": {"socks5"},
		"first_hop": {"gateway"}, "first_hop_gateway": {"vless-185"},
	}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("saving answered %d:\n%s", rec.Code, rec.Body.String())
	}
	after, err := s.store.Profile(t.Context(), id)
	if err != nil {
		t.Fatalf("reading the profile back: %v", err)
	}
	if after.FirstHop != "gw:vless-185" {
		t.Errorf("the first hop was written as %q, want the gateway chosen", after.FirstHop)
	}
}

func TestProxies_KeepsAFirstHopGatewayTheServiceNoLongerHas(t *testing.T) {
	// A gateway taken off the service is still the one the profile names. Left
	// out of the choices, the form would show the first of the others as
	// chosen, and a save of something else entirely would move the profile to
	// it without anybody having chosen it.
	page, _, _ := hopScreen(t, []fakebt.Gateway{{Name: "vless-185", Kind: "vless"}},
		store.Profile{Name: "Default", Kind: sourceFile, Location: "C:/lists/wingate.txt",
			FirstHop: "gw:gone-now", Solver: true})
	if !strings.Contains(page, `value="gone-now" selected`) {
		t.Error("the gateway the profile names is not among the choices once the service no longer has it")
	}
}

func TestSaveProxies_OffersTheGatewaysAgainWhenItRefusesTheFirstHop(t *testing.T) {
	// A refused save is answered with the form as it was typed. Drawn without
	// the gateways, the reader who chose the wrong kind of first hop could not
	// choose the right one without leaving the screen.
	_, s, id := hopScreen(t, []fakebt.Gateway{{Name: "vless-185", Kind: "vless"}},
		store.Profile{Name: "Default", Kind: sourceFile, Location: "C:/lists/wingate.txt", Solver: true})
	rec := postForm(t, s, proxiesAt, profileValues(id, url.Values{
		"source": {"file"}, "source_at": {"C:/lists/wingate.txt"}, "port_protocol": {"socks5"},
		"first_hop": {"socks5"}, "first_hop_proxy": {"http://198.51.100.7:8080"},
	}))
	page := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(page, `name="first_hop_gateway"`) {
		t.Fatalf("the refusal answered %d without the first hop's boxes", rec.Code)
	}
	if !strings.Contains(page, `value="vless-185"`) {
		t.Error("the refusal draws the form again without the gateways to choose from")
	}
	if !strings.Contains(page, `value="http://198.51.100.7:8080"`) {
		t.Error("the refusal empties the proxy that was typed")
	}
}
