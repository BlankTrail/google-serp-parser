// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
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

// screenAgainst is the proxies page for one profile against a service the
// caller has already set up.
func screenAgainst(t *testing.T, f *fakebt.Server, prof store.Profile) string {
	t.Helper()
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
	return get(t, s, boxesOf(id)).Body.String()
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

func TestProxies_DrawsOnlyTheBoxThatBelongsToTheChosenRoad(t *testing.T) {
	// One choice with two shapes: a proxy to type, or a gateway to pick. The box
	// of the shape nobody chose is not filled in, and left on the screen it
	// reads as a setting somebody forgot — so the page is drawn with it away.
	// Drawn away here rather than only by the script, so a browser running none
	// is shown the same thing, and so the value still travels with the form.
	onGateway, _, _ := hopScreen(t, []fakebt.Gateway{{Name: "vless-185", Kind: "vless"}},
		store.Profile{Name: "Default", Kind: sourceFile, Location: "C:/lists/wingate.txt",
			FirstHop: "gw:vless-185", Solver: true})
	if !strings.Contains(onGateway, `data-hop="socks5" hidden`) {
		t.Error("a profile going out through a gateway is shown the box for a proxy address")
	}
	if strings.Contains(onGateway, `data-hop="gateway" hidden`) {
		t.Error("a profile going out through a gateway is not shown which gateway")
	}

	onProxy, _, _ := hopScreen(t, []fakebt.Gateway{{Name: "vless-185", Kind: "vless"}},
		store.Profile{Name: "Default", Kind: sourceFile, Location: "C:/lists/wingate.txt",
			FirstHop: "socks5://198.51.100.7:1080", Solver: true})
	if !strings.Contains(onProxy, `data-hop="gateway" hidden`) {
		t.Error("a profile going out through a proxy is shown the gateway picker")
	}
	if strings.Contains(onProxy, `data-hop="socks5" hidden`) {
		t.Error("a profile going out through a proxy is not shown the proxy it goes through")
	}

	// And a profile that connects to its addresses directly is shown neither.
	straight, _, _ := hopScreen(t, []fakebt.Gateway{{Name: "vless-185", Kind: "vless"}},
		store.Profile{Name: "Default", Kind: sourceFile, Location: "C:/lists/wingate.txt", Solver: true})
	for _, want := range []string{`data-hop="socks5" hidden`, `data-hop="gateway" hidden`} {
		if !strings.Contains(straight, want) {
			t.Errorf("a profile that connects straight to its addresses is still shown %s", want)
		}
	}
}

func TestProxies_DrawsNoFirstHopOnAProfileOnGateways(t *testing.T) {
	// A gateway's road is set on the gateway, in the service, so the question
	// does not arise for a profile on them. The boxes are drawn away rather than
	// left out — the reader may pick a list in the box above and want them back
	// — and what they hold is kept either way: the handler leaves a profile on
	// gateways with the hop it was saved with.
	page, _, _ := hopScreen(t, []fakebt.Gateway{{Name: "vless-185", Kind: "vless"}},
		store.Profile{Name: "Default", Kind: sourceGateways, Gateways: []string{"vless-185"}, Solver: true})
	if row := rowAround(t, page, `name="first_hop"`); !strings.Contains(row, "hidden") {
		t.Errorf("a profile on gateways is shown the road to its addresses: <%s>", row)
	}
}

// rowAround is the opening tag of the row a box stands in.
func rowAround(t *testing.T, body, box string) string {
	t.Helper()
	at := strings.Index(body, box)
	if at < 0 {
		t.Fatalf("the page has no %s", box)
	}
	start := strings.LastIndex(body[:at], `<div class="row"`)
	if start < 0 {
		t.Fatalf("%s stands in no row", box)
	}
	tag, _, _ := strings.Cut(body[start+1:], ">")
	return tag
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

func TestProxies_DrawsTheGatewaysOnAFormThatIsNotOnThem(t *testing.T) {
	// So that picking them in the box above shows the list under it there and
	// then. Fetched only for a profile already on them, the reader would have to
	// save a profile with nothing chosen, come back, and choose then.
	page, _, _ := hopScreen(t, []fakebt.Gateway{{Name: "vless-185", Kind: "vless"}},
		store.Profile{Name: "Default", Kind: sourceURL, Location: "https://example.test/list.txt", Solver: true})
	if !strings.Contains(page, `name="gateway"`) {
		t.Error("a profile read from an address is drawn without the gateways it could be moved onto")
	}
	if !strings.Contains(page, `data-source="gateways" hidden`) {
		t.Error("the gateways are drawn on a profile read from an address without being put away")
	}
}

func TestProfile_OffersToWorkThroughExitsThatTerminateTLSFromTheStart(t *testing.T) {
	// The setting that made a working list look dead. Ten of fifteen addresses
	// the service itself could reach answered a port with a refusal, because
	// the exit presents its own certificate and the port would not have it —
	// and the service's own check never saw it, because that check does not
	// look at the certificate at all. So the box a reader is shown for a fresh
	// profile is already ticked: one who has to find a switch to make their
	// list work is one who concludes the list is bad.
	s, _ := proxyProfileServer(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})

	body := getBody(t, s, proxiesAt+"?"+profileField+"=new")
	if !strings.Contains(body, `name="allow_mitm" type="checkbox" value="1" checked`) {
		t.Error("a fresh profile is offered with exits that terminate TLS refused")
	}
	// The other switch the same blank form was getting wrong. A profile made on
	// this screen came out with the challenge solver off, which is the one
	// setting a search cannot do without, and the screen said nothing.
	if !strings.Contains(body, `name="js_solver" type="checkbox" value="1" checked`) {
		t.Error("a fresh profile is offered with the challenge solver switched off")
	}
}

func TestProfile_LetsNamesThroughToTheProxyUntilASwitchSaysOtherwise(t *testing.T) {
	// A fresh profile is offered with the service's fallback to the name left
	// in place — the operator's choice of the more dependable default — and a
	// reader who ticks the switch, for a provider that refuses names, gets it
	// on; one who unticks it gets it off.
	s, _ := proxyProfileServer(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})
	body := getBody(t, s, proxiesAt+"?"+profileField+"=new")
	if !strings.Contains(body, `name="vdns_strict_bypass" type="checkbox" value="1">`) {
		t.Error("a fresh profile is not offered the switch unticked")
	}

	postForm(t, s, proxiesAt, url.Values{
		"profile": {"0"}, "profile_name": {"residential"},
		"source": {"url"}, "source_at": {"https://example.test/list"},
		"vdns_strict_bypass": {"1"},
	})
	made := profileNamed(t, s, "residential")
	if !made.StrictBypass {
		t.Fatal("the switch was saved off although the form had it on")
	}
	postForm(t, s, proxiesAt, url.Values{
		"profile": {strconv.FormatInt(made.ID, 10)}, "profile_name": {"residential"},
		"source": {"url"}, "source_at": {"https://example.test/list"},
	})
	if again := profileNamed(t, s, "residential"); again.StrictBypass {
		t.Error("the switch was left on although the form that saved it had it off")
	}
}

func TestSaveProxies_KeepsTheAnswerAboutExitsThatTerminateTLS(t *testing.T) {
	// And it is a switch rather than a rule: a reader who turns it off gets it
	// off, and a form that quietly put it back would be a setting nobody can
	// change.
	s, _ := proxyProfileServer(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})
	postForm(t, s, proxiesAt, url.Values{
		"profile": {"0"}, "profile_name": {"datacentre"},
		"source": {"url"}, "source_at": {"https://example.test/list"},
		"allow_mitm": {"1"},
	})
	made := profileNamed(t, s, "datacentre")

	postForm(t, s, proxiesAt, url.Values{
		"profile": {strconv.FormatInt(made.ID, 10)}, "profile_name": {"datacentre"},
		"source": {"url"}, "source_at": {"https://example.test/list"},
	})

	if again := profileNamed(t, s, "datacentre"); again.AllowMITM {
		t.Error("the switch was left on although the form that saved it had it off")
	}
}

// profileNamed is the stored profile with this name.
func profileNamed(t *testing.T, s *Server, name string) store.Profile {
	t.Helper()
	all, err := s.store.Profiles(t.Context())
	if err != nil {
		t.Fatalf("Profiles: %v", err)
	}
	for _, one := range all {
		if one.Name == name {
			return one
		}
	}
	t.Fatalf("no profile named %q among %d", name, len(all))
	return store.Profile{}
}

func TestProfile_LetsTheServiceResolveTheNameUnlessToldOtherwise(t *testing.T) {
	// Two questions, not one: the VDNS list beside it says whether the exit
	// is asked at all, and this says what is asked and by whom. The service's own
	// ladder resolves the name itself and hands the proxy an address, so a
	// proxy that refuses names — as a residential gateway refused the host
	// Google's reCAPTCHA script is served from — cannot refuse this one.
	s, _ := proxyProfileServer(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})

	body := getBody(t, s, proxiesAt+"?"+profileField+"=new")
	box, _, _ := strings.Cut(body[strings.Index(body, `name="resolver"`):], "</select>")
	if !strings.Contains(box, `<option value="" selected>`) {
		t.Error("a fresh profile is not offered with the service resolving the name itself")
	}
	// And the resolvers somebody names are not drawn until naming them is the
	// choice: a box for a list nobody is filling in is a box to wonder about.
	if !strings.Contains(body, `data-resolver="custom" hidden`) {
		t.Error("the box for named resolvers is drawn although they are not the choice")
	}

	postForm(t, s, proxiesAt, url.Values{
		"profile": {"0"}, "profile_name": {"datacentre"},
		"source": {"url"}, "source_at": {"https://example.test/list"},
		"resolver": {"custom"}, "custom_resolvers": {" 1.1.1.1:53 \n\n 9.9.9.9:53 "},
	})

	made := profileNamed(t, s, "datacentre")
	if made.Resolver != "custom" {
		t.Errorf("the profile resolves names by %q, want the way the form named", made.Resolver)
	}
	if !slices.Equal(made.CustomResolvers, []string{"1.1.1.1:53", "9.9.9.9:53"}) {
		t.Errorf("the resolvers read back as %q, want the two named, trimmed and without the blank line",
			made.CustomResolvers)
	}
}

func TestSaveProxies_RefusesAWayOfResolvingTheServiceDoesNotTake(t *testing.T) {
	// The service answers an unknown strategy with a refusal naming the six,
	// and a port that will not open because a form let a typo through is a
	// fault a long way from its cause.
	s, _ := proxyProfileServer(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})
	postForm(t, s, proxiesAt, url.Values{
		"profile": {"0"}, "profile_name": {"datacentre"},
		"source": {"url"}, "source_at": {"https://example.test/list"},
		"resolver": {"whatever"},
	})

	all, err := s.store.Profiles(t.Context())
	if err != nil {
		t.Fatalf("Profiles: %v", err)
	}
	for _, one := range all {
		if one.Name == "datacentre" {
			t.Fatal("a profile was written with a way of resolving names the service does not take")
		}
	}
}
