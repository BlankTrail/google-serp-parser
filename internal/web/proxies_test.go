// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

func TestProxies_ShowsTheReadingOfThePoolAndBreaksTheFailuresDown(t *testing.T) {
	// A pool going badly and a pool going well look identical from outside —
	// both are a number of ports and a rate — and the difference between them is
	// which of a handful of things is going wrong. A single total of failures
	// cannot say: a run failing on dead addresses wants another list, and one
	// being walled by the origin wants something else entirely, and both read
	// the same added together.
	s := testServerWithSupervisor(t)
	prof := onlyProfile(t, s)
	// The pool this reading is of stands on that profile. It is written here
	// because nothing has run: a reading shown under a profile the pool is not
	// on is one list's failures reported as another's, and the screen says so
	// instead of drawing figures.
	s.sup.onProfile = prof
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, readingOf(prof), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Every kind is drawn, including the ones that have not happened: a row that
	// comes and goes is one the reader has to look for.
	for _, key := range []string{
		"proxies.kind.transport", "proxies.kind.relay",
		"proxies.kind.wall", "proxies.kind.timeout", "proxies.kind.other",
	} {
		phrase := catalogue[LangEN][key]
		if phrase == "" {
			t.Fatalf("the catalogue has nothing under %s", key)
		}
		if !strings.Contains(body, phrase) {
			t.Errorf("the screen does not name the failures called %q", phrase)
		}
	}

	// And the counts the fake pool reports are on it rather than a placeholder.
	// What the pool is doing this minute — its ports, its addresses, the
	// sessions on them — is on the page of the job it is doing it for.
	for _, want := range []string{
		`id="rotations">4<`,
		`id="revivals">2<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the screen does not carry %s", want)
		}
	}

	// Nothing has gone through it, so the share is the mark that stands where a
	// figure would if there were anything to work one out from. Nought per cent
	// is a measurement somebody could act on; nothing has happened yet.
	if !strings.Contains(body, `id="share">`+noFigure+`<`) {
		t.Error("the screen shows a share of nothing as a figure rather than a mark")
	}
}

func TestProxies_IsOfferedAsATabOfItsOwn(t *testing.T) {
	// It is a screen somebody keeps open beside a run, so it has an address that
	// can be bookmarked and a tab that lights up on it.
	var found bool
	for _, tb := range tabs {
		if tb.At == proxiesAt {
			found = true
		}
	}
	if !found {
		t.Fatal("the header does not offer the proxy screen")
	}
	for _, link := range tabsFor(proxiesAt) {
		if link.URL == proxiesAt && !link.Current {
			t.Error("the proxy tab is not lit on the proxy screen")
		}
	}
}

func TestProxiesReset_ClearsTheCountsAndComesBackToTheScreen(t *testing.T) {
	// The button exists so a measurement can be started from a known point
	// without restarting anything, and it is a post so that a browser walking a
	// link cannot wipe somebody's reading.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/api/proxies/reset", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want a redirect back to the screen", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != proxiesAt {
		t.Errorf("the press lands on %q, want %q", got, proxiesAt)
	}

	// A get is refused: the reading is not something a prefetch may destroy.
	plain := httptest.NewRecorder()
	s.Handler().ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/api/proxies/reset", nil))
	if plain.Code == http.StatusSeeOther || plain.Code == http.StatusOK {
		t.Errorf("a walked link cleared the reading: status=%d", plain.Code)
	}
}

func TestShareOf_SaysNothingRatherThanNoughtPerCent(t *testing.T) {
	// A share of nothing is not nought per cent. Nought per cent is a
	// measurement somebody could act on, and nothing has been settled yet.
	if got := shareOf(0, 0); got != noFigure {
		t.Errorf("a share of nothing reads %q, want the mark", got)
	}
	if got := shareOf(1, 4); got != "25%" {
		t.Errorf("one in four reads %q, want 25%%", got)
	}
	if got := shareOf(0, 4); got != "0%" {
		t.Errorf("none of four reads %q, want 0%%", got)
	}
}

func TestFailureKeys_NamesEveryKindThePoolCanReport(t *testing.T) {
	// The screen draws whatever the pool counts. A kind added to the pool and
	// not named here would be drawn as an empty row, which reads as a kind that
	// never happens rather than as a phrase nobody wrote.
	for _, kind := range blanktrail.Failures {
		key, ok := failureKeys[kind]
		if !ok {
			t.Errorf("the screen has no name for the failures called %q", kind)
			continue
		}
		for _, l := range Languages() {
			if catalogue[l][key] == "" {
				t.Errorf("%s has nothing under %s", l, key)
			}
		}
	}
}

func TestProxiesRelease_TakesTheBenchBackAndComesBackToTheScreen(t *testing.T) {
	// For a bench filled by something that was never the addresses' doing — the
	// proxy service restarting, a network away for a minute. It is a post like
	// the other button, so a browser walking a link cannot undo somebody's
	// measurement of a list.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/api/proxies/release", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want a redirect back to the screen", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != proxiesAt {
		t.Errorf("the press lands on %q, want %q", got, proxiesAt)
	}

	plain := httptest.NewRecorder()
	s.Handler().ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/api/proxies/release", nil))
	if plain.Code == http.StatusSeeOther || plain.Code == http.StatusOK {
		t.Errorf("a walked link let the bench go: status=%d", plain.Code)
	}
}

func TestBrowserPolls_ClaimsEveryAddressThePagesPress(t *testing.T) {
	// Whatever is mounted on /api/ takes every address under it this list does
	// not claim, so an address the pages press and this does not name is a
	// button that answers 404. Both buttons on the proxy screen did exactly that
	// on the day they were added, because the routes and the published list were
	// two lists.
	claimed := map[string]bool{}
	for _, at := range BrowserPolls() {
		claimed[at] = true
	}
	for _, want := range []string{"/api/proxies/reset", "/api/proxies/release"} {
		if !claimed[want] {
			t.Errorf("the pages press %s and nothing claims it: whatever is mounted "+
				"on /api/ takes it", want)
		}
	}

	// And every address registered is one published, which is what makes the two
	// impossible to disagree.
	for _, p := range browserPolls {
		if !claimed[p.path] {
			t.Errorf("%s is registered and not published", p.path)
		}
	}
}

func TestProxies_CarriesTheSettingsTheReadingIsAbout(t *testing.T) {
	// Where the addresses come from and how the ports on them are reached stand
	// on this screen rather than in the settings, because they are the same
	// subject the figures are about: somebody reading that a quarter of the
	// requests never arrive is somebody about to change the list, and a reading
	// that sent them to another screen to act on it would be a reading nobody
	// acts on.
	s, _ := serverWithSettings(t, settings.Settings{
		ControlURL: "http://127.0.0.1:1",
		Proxy:      settings.ProxySource{Kind: "file", Location: "C:/list.txt"},
	})
	body := getBody(t, s, boxesOf(onlyProfile(t, s)))

	for _, want := range []string{`name="source"`, `name="source_at"`, `name="source_refresh"`, `name="port_protocol"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the proxy screen has no %s", want)
		}
	}

	// And the settings screen has given them up rather than showing a second
	// copy: two boxes for one setting is two answers to what is saved.
	settingsBody := getBody(t, s, settingsAt)
	for _, gone := range []string{`name="source_at"`, `name="port_protocol"`} {
		if strings.Contains(settingsBody, gone) {
			t.Errorf("the settings screen still carries %s", gone)
		}
	}
}

func TestSaveProxies_WritesTheListDownAndLeavesEverythingElseAlone(t *testing.T) {
	// The boxes this screen does not show — the connection, the key, the
	// identities kept warm — are carried through untouched. A partial form saved
	// over the whole settings would take the connection away from somebody who
	// came here to change a file path, and that is the first thing anybody will
	// do on this screen.
	before := settings.Settings{
		ControlURL: "http://127.0.0.1:1",
		APIKey:     "the-key-that-must-survive",
		HotPorts:   12,
		HotDevice:  blanktrail.DeviceDesktop,
	}
	s, path := serverWithSettings(t, before)
	prof, err := s.store.CreateProfile(t.Context(), store.Profile{Name: "Default"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	rec := postForm(t, s, proxiesAt, profileValues(prof, url.Values{
		"source":         {"file"},
		"source_at":      {"C:/somewhere/list.txt"},
		"source_refresh": {"7"},
		"port_protocol":  {"http"},
	}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want a redirect back to the screen", rec.Code)
	}
	// Back to the list, with the boxes gone. They were opened to write one
	// profile, they have written it, and a form left standing after a save is one
	// the reader has to dismiss — and a second answer to which profile the screen
	// is about.
	if got, want := rec.Header().Get("Location"), proxiesAt; got != want {
		t.Errorf("the save lands on %q, want %q", got, want)
	}

	after, err := s.store.Profile(t.Context(), prof)
	if err != nil {
		t.Fatalf("reading the profile back: %v", err)
	}
	if after.Kind != "file" || after.Location != "C:/somewhere/list.txt" {
		t.Errorf("the list reads %+v, want the file that was typed", after)
	}
	if after.Protocol != blanktrail.ProtocolHTTP {
		t.Errorf("the ports are reached over %q, want the http that was chosen", after.Protocol)
	}
	// And the connection is left where it is. It is not merely untouched by this
	// form now — it is not this form's at all — but the question the reader has
	// is the same one, so it is still asked here: does changing a file path cost
	// them the key.
	kept, err := settings.Load(path)
	if err != nil {
		t.Fatalf("reading the settings back: %v", err)
	}
	if kept.APIKey != before.APIKey {
		t.Error("saving a file path took the key away")
	}
	if kept.ControlURL != before.ControlURL {
		t.Errorf("the connection reads %q, want the %q it was", kept.ControlURL, before.ControlURL)
	}
	if kept.HotPorts != before.HotPorts || kept.HotDevice != before.HotDevice {
		t.Errorf("the identities kept warm read %d %q, want %d %q",
			kept.HotPorts, kept.HotDevice, before.HotPorts, before.HotDevice)
	}
}

func TestSaveProxies_ComplainsAboutWhatWasTypedRatherThanWritingIt(t *testing.T) {
	// The same order the settings screen keeps: complain, open, then write.
	// Nothing is saved while there is anything to complain about, because
	// settings written and refused leave the file and the machine disagreeing.
	before := settings.Settings{ControlURL: "http://127.0.0.1:1"}
	s, _ := serverWithSettings(t, before)
	prof, err := s.store.CreateProfile(t.Context(), store.Profile{Name: "Default"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	rec := postForm(t, s, proxiesAt, profileValues(prof, url.Values{
		"source":         {"file"},
		"source_at":      {"C:/list.txt"},
		"source_refresh": {"not a number"},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want the screen back with the complaint on it", rec.Code)
	}

	after, err := s.store.Profile(t.Context(), prof)
	if err != nil {
		t.Fatalf("reading the profile back: %v", err)
	}
	if after.Kind != "" {
		t.Errorf("a form that would not parse was written down as %+v", after)
	}
}

func TestSaveProxies_WritesDownHowLongAnAddressIsBanned(t *testing.T) {
	// How fast a gateway's exits turn over is a property of the list somebody
	// bought rather than of this program, so the length of a ban is theirs to
	// set. A ban that is short brings the same dead addresses back inside one
	// job, which is what a fixed five minutes did.
	s, prof := proxyProfileServer(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})

	rec := postForm(t, s, proxiesAt, profileValues(prof, url.Values{
		"source":      {"file"},
		"source_at":   {"C:/list.txt"},
		"ban_minutes": {"90"},
	}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want a redirect back to the screen", rec.Code)
	}

	after, err := s.store.Profile(t.Context(), prof)
	if err != nil {
		t.Fatalf("reading the profile back: %v", err)
	}
	if after.Ban != 90*time.Minute {
		t.Errorf("an address is banned for %s, want the ninety minutes that were typed",
			after.Ban)
	}

	// And the box shows it back in the unit it was typed in.
	if body := getBody(t, s, boxesOf(prof)); !strings.Contains(body, `value="90"`) {
		t.Error("the box does not show the ban that was saved")
	}
}

func TestSaveProxies_WritesDownHowManyThreadsOneEgressCarries(t *testing.T) {
	// A pool can hold more identities than the list has egresses, and this is
	// what stops all of them going through one address at once. One is the
	// floor: nought identities through an egress is a pool that hands out
	// nothing at all.
	s, prof := proxyProfileServer(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})

	rec := postForm(t, s, proxiesAt, profileValues(prof, url.Values{
		"source":               {"file"},
		"source_at":            {"C:/list.txt"},
		"threads_per_upstream": {"4"},
	}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want a redirect back to the screen", rec.Code)
	}
	after, err := s.store.Profile(t.Context(), prof)
	if err != nil {
		t.Fatalf("reading the profile back: %v", err)
	}
	if after.ThreadsPerUpstream != 4 {
		t.Errorf("one egress carries %d threads, want the four that were typed",
			after.ThreadsPerUpstream)
	}

	// Nought is not an answer here, and neither is a file edited by hand into
	// one: what comes back out is one.
	rec = postForm(t, s, proxiesAt, profileValues(prof, url.Values{
		"source":               {"file"},
		"source_at":            {"C:/list.txt"},
		"threads_per_upstream": {"0"},
	}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want the save to go through", rec.Code)
	}
	if after, err = s.store.Profile(t.Context(), prof); err != nil {
		t.Fatalf("reading the profile back: %v", err)
	}
	if after.ThreadsPerUpstream != 1 {
		t.Errorf("one egress carries %d threads after nought was typed, want one",
			after.ThreadsPerUpstream)
	}
}

func TestProxies_ShowsHowManyPortsWereOpenedAgain(t *testing.T) {
	// A port opened again on the address it already had is the opposite reading
	// from a port moved to another address, and a screen that showed only the
	// second reported nought while every port in the job was being reopened.
	s := testServerWithSupervisor(t)
	prof := onlyProfile(t, s)
	s.sup.onProfile = prof
	page := get(t, s, readingOf(prof)).Body.String()
	if !strings.Contains(page, `id="reopenings"`) {
		t.Error("the proxy screen never says how many ports were opened again")
	}
}

func TestProxies_NoLongerOffersToChangeAPortsIdentityOnATimer(t *testing.T) {
	// A port is a place now. The fingerprint, the cookies and the exit belong to
	// the session standing on it, and it is the session that is kept or given
	// up — by what Google answers, not by a clock. A timer that reopened the
	// port underneath one threw away a warm identity in the middle of a walk,
	// and the box that set it could only make a run worse.
	s := testServerWithSupervisor(t)
	prof := onlyProfile(t, s)
	page := get(t, s, boxesOf(prof)).Body.String()
	if strings.Contains(page, `name="renew_minutes"`) {
		t.Error("the proxy profile form still offers to change a port's identity on a timer")
	}
}

func TestProxies_ShowsTheGatewaysThatWouldNotCarryAPort(t *testing.T) {
	// The screen is where an operator finds out; without this the only sign of
	// fourteen dead gateways is a pool smaller than they asked for.
	s := testServerWithSupervisor(t)
	prof := onlyProfile(t, s)
	s.sup.onProfile = prof
	page := get(t, s, readingOf(prof)).Body.String()
	if !strings.Contains(page, `id="reopenings"`) {
		t.Fatal("the proxy screen is not the page this test thinks it is")
	}
	// Nothing refused, so nothing is drawn: a heading over an empty table reads
	// as a fault of its own.
	if strings.Contains(page, "Would not carry a port") {
		t.Error("the screen draws the refusals heading when nothing refused")
	}
}

func TestProxies_KeepsTheAddressOfAListWhileTheGatewaysAreChosen(t *testing.T) {
	// The box is off the screen while the gateways are chosen, so nothing the
	// reader can see says the address is about to be forgotten. Dropped, an
	// afternoon on the gateways cost them their list's address — which on a
	// bought list is not something they can retype from memory.
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := settings.Save(path, settings.Settings{
		ControlURL: "http://127.0.0.1:8891", APIKey: "k",
		Proxy: settings.ProxySource{
			Kind: "url", Location: "https://example.test/list.txt",
			Refresh: 30 * time.Minute, Ban: 10 * time.Minute,
		},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s, err := New(Config{Store: testStore(t), Logger: quiet(), SettingsPath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The list is the profile's now, so that is where the address stands that
	// this test is about keeping.
	prof, err := s.store.CreateProfile(t.Context(), store.Profile{
		Name: "Default", Kind: "url", Location: "https://example.test/list.txt",
		Refresh: 30 * time.Minute, Ban: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	// Over to the gateways, with the address box carrying nothing.
	if rec := postForm(t, s, proxiesAt, profileValues(prof, url.Values{
		"source": {"gateways"}, "source_at": {""}, "gateway": {"Sub.One"},
		"ban_minutes": {"10"}, "threads_per_upstream": {"1"}, "port_protocol": {"socks5"},
	})); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving the gateways answered %d", rec.Code)
	}
	after, err := s.store.Profile(t.Context(), prof)
	if err != nil {
		t.Fatalf("reading the profile back: %v", err)
	}
	if after.Location != "https://example.test/list.txt" {
		t.Errorf("the address of the list is %q after choosing the gateways", after.Location)
	}
	if after.Refresh != 30*time.Minute {
		t.Errorf("how often the list is read again is %v after choosing the gateways", after.Refresh)
	}

	// And back again, with the ticks carrying nothing: they are kept for the
	// same reason, two-and-thirty boxes being no small thing to tick twice.
	if rec := postForm(t, s, proxiesAt, profileValues(prof, url.Values{
		"source": {"url"}, "source_at": {"https://example.test/list.txt"},
		"source_refresh": {"30"}, "ban_minutes": {"10"},
		"threads_per_upstream": {"1"}, "port_protocol": {"socks5"},
	})); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving the list answered %d", rec.Code)
	}
	if after, err = s.store.Profile(t.Context(), prof); err != nil {
		t.Fatalf("reading the profile back: %v", err)
	}
	if len(after.Gateways) != 1 || after.Gateways[0] != "Sub.One" {
		t.Errorf("the gateways ticked are %v after going back to a list", after.Gateways)
	}
}

// boxesOf is the address that opens one profile's boxes, and readingOf the
// address that opens its counters.
//
// The screen is a list of profiles until one is named. A test about the form or
// about a reading has to say which profile it means, the same way a reader does
// by pressing a row.
func boxesOf(id int64) string {
	return proxiesAt + "?" + profileField + "=" + strconv.FormatInt(id, 10)
}

func readingOf(id int64) string {
	return boxesOf(id) + "&" + statsField
}

// proxyProfileServer is a server whose history already holds one profile, which
// is what a machine that has been started once has: the settings it was set up
// with were carried into it. The id comes back because every test below reads
// the profile again to see what the form wrote.
func proxyProfileServer(t *testing.T, saved settings.Settings) (*Server, int64) {
	t.Helper()
	s, _ := serverWithSettings(t, saved)
	id, err := s.store.CreateProfile(t.Context(), store.Profile{Name: "Default"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	return s, id
}

// profileValues is a form for that profile: the boxes a test cares about, and
// the two the screen always sends.
func profileValues(id int64, boxes url.Values) url.Values {
	out := url.Values{
		"profile":      {strconv.FormatInt(id, 10)},
		"profile_name": {"Default"},
	}
	for box, values := range boxes {
		out[box] = values
	}
	return out
}

func TestProxies_OpenOnTheListAndNothingElse(t *testing.T) {
	// The list of profiles is what somebody opening this screen came for. A
	// form and two dozen figures under it were a page to scroll past to reach
	// the one thing on it that is always wanted — and the figures were a reading
	// of one pool standing under a list of profiles, with nothing saying which
	// of them it was about.
	s, prof := proxyProfileServer(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})
	page := get(t, s, proxiesAt).Body.String()

	if !strings.Contains(page, LangEN.T("proxies.profiles")) {
		t.Fatalf("the screen does not carry the profiles:\n%s", page)
	}
	for _, gone := range []string{`name="source_at"`, `id="addresses"`, `id="requests"`} {
		if strings.Contains(page, gone) {
			t.Errorf("the screen opens carrying %s, which nobody asked it for", gone)
		}
	}

	// Both are one press away, on the row of the profile they are about.
	if boxes := get(t, s, boxesOf(prof)).Body.String(); !strings.Contains(boxes, `name="source_at"`) {
		t.Errorf("opening a profile does not open its boxes:\n%s", boxes)
	}
	s.sup = nil
	if reading := get(t, s, readingOf(prof)).Body.String(); !strings.Contains(reading, LangEN.T("proxies.reading")) {
		t.Errorf("asking for a profile's counters draws no reading at all:\n%s", reading)
	}
}

func TestSaveProxies_MakesAProfileWhenTheFormNamesNone(t *testing.T) {
	// One button and one handler for making and editing: the difference is a
	// number in the form. A second screen for making one would be a second place
	// for the same boxes to drift apart.
	s, first := proxyProfileServer(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})

	rec := postForm(t, s, proxiesAt, url.Values{
		"profile":      {"0"},
		"profile_name": {"datacentre"},
		"source":       {"url"},
		"source_at":    {"https://example.test/list"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want a redirect:\n%s", rec.Code, rec.Body.String())
	}
	all, err := s.store.Profiles(t.Context())
	if err != nil {
		t.Fatalf("Profiles: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("%d profiles after making one, want two", len(all))
	}
	made, err := s.store.Profile(t.Context(), first+1)
	if err != nil {
		t.Fatalf("reading the new profile: %v", err)
	}
	if made.Name != "datacentre" || made.Kind != "url" || made.Location != "https://example.test/list" {
		t.Errorf("the profile made reads %+v, want the boxes that were filled in", made)
	}
	if made.Default {
		t.Error("the profile just made took the default mark from the one that had it")
	}

	// And a name another profile carries is refused rather than written: the
	// name is how a job says which exits it wants.
	again := postForm(t, s, proxiesAt, url.Values{
		"profile": {"0"}, "profile_name": {"datacentre"}, "source": {""},
	})
	if again.Code != http.StatusOK {
		t.Fatalf("a repeated name answered %d, want the screen back with the complaint", again.Code)
	}
	if want := LangEN.T("proxies.profile.name.taken"); !strings.Contains(again.Body.String(), want) {
		t.Errorf("the screen does not say %q", want)
	}
}

func TestProxies_MovesTheDefaultMarkAndRefusesToDeleteTheLast(t *testing.T) {
	// Something has to be default: it is what the identities kept warm are
	// raised on and what a job that named no profile runs through. So the mark
	// moves on a press, and the last profile stays whatever is pressed.
	s, first := proxyProfileServer(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})
	second, err := s.store.CreateProfile(t.Context(), store.Profile{Name: "second"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	if rec := postForm(t, s, proxiesAt+"/default", url.Values{
		"profile": {strconv.FormatInt(second, 10)},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("making a profile the default answered %d", rec.Code)
	}
	if def, _ := s.store.DefaultProfile(t.Context()); def.ID != second {
		t.Errorf("the default is %d, want the one the press named (%d)", def.ID, second)
	}

	if rec := postForm(t, s, proxiesAt+"/delete", url.Values{
		"profile": {strconv.FormatInt(first, 10)},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("deleting a profile answered %d", rec.Code)
	}
	left, err := s.store.Profiles(t.Context())
	if err != nil {
		t.Fatalf("Profiles: %v", err)
	}
	if len(left) != 1 || left[0].ID != second {
		t.Fatalf("what is left is %+v, want the one that was not deleted", left)
	}

	rec := postForm(t, s, proxiesAt+"/delete", url.Values{
		"profile": {strconv.FormatInt(second, 10)},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("deleting the last profile answered %d, want the screen back with the reason", rec.Code)
	}
	if want := LangEN.T("proxies.profile.last"); !strings.Contains(rec.Body.String(), want) {
		t.Errorf("the screen does not say %q", want)
	}
	if all, _ := s.store.Profiles(t.Context()); len(all) != 1 {
		t.Errorf("%d profiles after the refusal, want the one that was kept", len(all))
	}
}

func TestProfileForm_OffersWhatThePortsAreMadeOfBeyondWhereTheyGoOut(t *testing.T) {
	// Three settings the service takes when a port is opened, and which this
	// program never offered: where a port resolves names, whether the challenge
	// solver is on it, and whether it may re-originate over HTTP/3.
	//
	// They are on the profile rather than on a job because they belong to the
	// exits: a profile going out through a list of proxies and one going out
	// through gateways want different answers to all three.
	s := testServer(t)
	fresh := store.NewProfile()
	fresh.Name = "exits"
	if _, err := s.store.CreateProfile(t.Context(), fresh); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	all, err := s.store.Profiles(t.Context())
	if err != nil || len(all) == 0 {
		t.Fatalf("Profiles: %v, %d", err, len(all))
	}

	body := get(t, s, proxiesAt+"?"+profileField+"="+strconv.FormatInt(all[0].ID, 10)).Body.String()
	for _, want := range []string{
		`name="vdns"`, `name="vdns_mode"`, `name="js_solver"`, `name="http3"`,
		`value="on_leak"`, `value="forced"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the profile form offers no %s", want)
		}
	}
	// vDNS is a switch and a list of ways of being on, not one list of four.
	// Held in one, "off" was a fourth entry that a reader looking for it had to
	// recognise among three phrasings of "on" — and the screenshot that came
	// back said they had not found it.
	if strings.Contains(body, `value="off"`) {
		t.Error("the mode list still carries off, which is the switch beside it")
	}
	// Both of the boxes a profile ships with are ticked: the solver, without
	// which a challenged search produces nothing, and vDNS, without which the
	// port's names are resolved somewhere other than where its traffic leaves.
	for _, box := range []string{`name="js_solver"`, `name="vdns"`} {
		at := strings.Index(body, box)
		if at < 0 {
			t.Fatalf("no %s box at all", box)
		}
		if tag := body[at : at+strings.Index(body[at:], ">")]; !strings.Contains(tag, "checked") {
			t.Errorf("the %s box is offered as %q, want it ticked on a new profile", box, tag)
		}
	}
	// And the mode beside the switch is the automatic one, which is the answer
	// nobody has to think about.
	at := strings.Index(body, `name="vdns_mode"`)
	rest := body[at:]
	if first := strings.Index(rest, "<option"); !strings.Contains(rest[first:first+60], "selected") {
		t.Errorf("the mode list does not start on the automatic way: %s", rest[first:first+60])
	}
}

func TestProfileForm_TurnsVDNSOffWithTheSwitchRatherThanWithTheList(t *testing.T) {
	// The switch is the whole answer to whether names go through the exit. A
	// reader who turns it off has not also said which way it should be on, and
	// the list under it still shows whatever it was showing — so the switch has
	// to win over it, or unticking the box would save the mode it happens to be
	// displaying.
	off := profileForm{Name: "exits", VDNSOn: false, VDNS: blanktrail.VDNSForced}
	got, complaints := off.onto(store.NewProfile())
	if len(complaints) != 0 {
		t.Fatalf("turning vDNS off was refused: %v", complaints)
	}
	if got.VDNSMode != blanktrail.VDNSOff {
		t.Errorf("the profile was saved as %q, want vDNS off", got.VDNSMode)
	}

	// And back on, at the way the list was showing.
	on := profileForm{Name: "exits", VDNSOn: true, VDNS: blanktrail.VDNSForced}
	if got, _ := on.onto(store.NewProfile()); got.VDNSMode != blanktrail.VDNSForced {
		t.Errorf("the profile was saved as %q, want the way the list named", got.VDNSMode)
	}

	// A profile saved with vDNS off still shows a way of being on beside the
	// switch, because the list is on the screen whether or not the switch is.
	// Empty there would be a fifth entry meaning "off" in a list that no longer
	// has one.
	stored := store.NewProfile()
	stored.VDNSMode = blanktrail.VDNSOff
	shown := profileShowing(stored)
	if shown.VDNSOn {
		t.Error("a profile with vDNS off is drawn with the switch on")
	}
	if shown.VDNS != blanktrail.VDNSAuto {
		t.Errorf("the list beside the off switch shows %q, want the automatic way", shown.VDNS)
	}
}

func TestNewProfile_TurnsTheSolverOnRatherThanLeavingItToAZeroValue(t *testing.T) {
	// A false boolean is a decision, and the one a zero struct makes here is to
	// run every search without the thing that carries it through a challenge.
	// Every place that needs "a profile nobody has filled in" starts here.
	if got := store.NewProfile(); !got.Solver {
		t.Error("a profile nobody has filled in runs without the challenge solver")
	}
	if got := store.NewProfile(); got.HTTP3 {
		t.Error("a profile nobody has filled in allows HTTP/3, which its egress cannot carry")
	}
	if got := store.NewProfile(); got.VDNSMode != "" {
		t.Errorf("a profile nobody has filled in resolves names as %q, want the service's own answer", got.VDNSMode)
	}
}

func TestProxies_DrawsTheSourceInTheShapeOfTheKindItIs(t *testing.T) {
	// Where a profile's addresses come from is one choice with three shapes: a
	// file to pick on this machine, an address to fetch, or the gateways the
	// service holds. Each wants different boxes, and the boxes of the other two
	// are not filled in — left on the screen they read as settings somebody
	// forgot. The page is drawn that way by the server, so a browser running no
	// script is shown what the script would show.
	s := testServerWithSupervisor(t)
	prof, err := s.store.CreateProfile(t.Context(), store.Profile{
		Name: "by address", Kind: sourceURL, Location: "https://example.test/list.txt"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	byAddress := get(t, s, boxesOf(prof)).Body.String()
	if tag := fieldAround(t, byAddress, `id="source_at"`); strings.Contains(tag, "hidden") {
		t.Errorf("a profile read from an address is not shown where to read it: <%s>", tag)
	}
	if tag := tagAround(t, byAddress, "/proxies/browse"); !strings.Contains(tag, "hidden") {
		t.Errorf("a profile read from an address is offered a look through this machine's folders: <%s>", tag)
	}
	if tag := tagAround(t, byAddress, LangEN.T("settings.source.at.file")); !strings.Contains(tag, "hidden") {
		t.Errorf("the box is named for a file on a profile read from an address: <%s>", tag)
	}

	// On the gateways the boxes a list is named in go away, and the gateways
	// themselves are what is left to choose from.
	onGateways, err := s.store.CreateProfile(t.Context(), store.Profile{
		Name: "on gateways", Kind: sourceGateways})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	gateways := get(t, s, boxesOf(onGateways)).Body.String()
	if tag := fieldAround(t, gateways, `id="source_at"`); !strings.Contains(tag, "hidden") {
		t.Errorf("a profile on gateways is asked where to read a list: <%s>", tag)
	}
	if tag := fieldAround(t, gateways, `id="source_refresh"`); !strings.Contains(tag, "hidden") {
		t.Errorf("a profile on gateways is asked how often to read a list again: <%s>", tag)
	}
}

// fieldAround is the opening tag of the field a box stands in, which is where a
// box is drawn away from the screen: the box itself goes on carrying its value
// either way.
func fieldAround(t *testing.T, body, box string) string {
	t.Helper()
	at := strings.Index(body, box)
	if at < 0 {
		t.Fatalf("the page has no %s", box)
	}
	start := strings.LastIndex(body[:at], `<div class="field`)
	if start < 0 {
		t.Fatalf("%s stands in no field", box)
	}
	tag, _, _ := strings.Cut(body[start+1:], ">")
	return tag
}

func TestProxies_PutsTheListOfProfilesAwayWhileAFormIsOpen(t *testing.T) {
	// A machine with twenty profiles would push the boxes somebody has just
	// pressed «new» for below the fold. The form says for itself which profile
	// it is, and the way back to the list is the «cancel» beside the save.
	s := testServerWithSupervisor(t)
	prof := onlyProfile(t, s)

	if body := get(t, s, proxiesAt).Body.String(); !strings.Contains(body, `id="profiles"`) {
		t.Error("the screen without a form open does not list the profiles")
	}
	for _, at := range []string{boxesOf(prof), proxiesAt + "?" + profileField + "=new"} {
		body := get(t, s, at).Body.String()
		if strings.Contains(body, `id="profiles"`) {
			t.Errorf("%s draws the list of profiles above the form", at)
		}
		if !strings.Contains(body, `name="source"`) {
			t.Errorf("%s draws no form at all", at)
		}
	}
	// And the reading of a profile's counters is not a form, so the list stays.
	if body := get(t, s, readingOf(prof)).Body.String(); !strings.Contains(body, `id="profiles"`) {
		t.Error("the reading of one profile's counters hides the list of the others")
	}
}

func TestProxiesReading_KeepsTheRowsOwnPressToOpenIt(t *testing.T) {
	// The reading marked its profile's row as the one being edited, and the mark
	// is what hides a row's own press to open it: pressing «statistics» took
	// «edit» away. Nothing is being edited while a reading is shown.
	s := testServerWithSupervisor(t)
	prof := onlyProfile(t, s)
	s.sup.onProfile = prof
	body := get(t, s, readingOf(prof)).Body.String()
	if !strings.Contains(body, `href="`+boxesOf(prof)+`"`) {
		t.Errorf("the reading of a profile takes away the press that opens its boxes:\n%s", body)
	}
}

func TestProxiesReading_DrawsAProfileNothingHasRunOnAsNoughts(t *testing.T) {
	// A profile the pool being read is not on was answered with a sentence
	// saying there was no reading. There is one: nothing has been counted
	// against it, and the figures say so — the same figures in the same places,
	// each a nought, whether no pool has been raised yet or the pool stands on
	// another profile. The presses that act on the pool are not offered under
	// its name: they would clear or release another profile's.
	s := testServerWithSupervisor(t)
	prof := onlyProfile(t, s)
	other, err := s.store.CreateProfile(t.Context(), store.Profile{Name: "the other list"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	for _, c := range []struct {
		name string
		on   int64
	}{
		{"no pool raised yet", 0},
		{"the pool on another profile", other},
	} {
		t.Run(c.name, func(t *testing.T) {
			s.sup.onProfile = c.on
			body := get(t, s, readingOf(prof)).Body.String()
			for _, id := range []string{"requests", "attempts", "failed", "rotations", "quarantines",
				"revivals", "reopenings", "rejections"} {
				if got := shown(t, body, id); got != "0" {
					t.Errorf("the profile nothing ran on shows %s as %q, want 0", id, got)
				}
			}
			if got := shown(t, body, "share"); got != noFigure {
				t.Errorf("a share of nothing is shown as %q", got)
			}
			for _, press := range []string{"/api/proxies/reset", "/api/proxies/release"} {
				if strings.Contains(body, `action="`+press+`"`) {
					t.Errorf("the reading of a profile the pool is not on offers %s, which acts on another's pool", press)
				}
			}
		})
	}

	// The profile the pool is on keeps its own figures and its presses.
	s.sup.onProfile = prof
	body := get(t, s, readingOf(prof)).Body.String()
	if got := shown(t, body, "rotations"); got != "4" {
		t.Errorf("the profile the pool is on shows rotations %q, want its own 4", got)
	}
	if !strings.Contains(body, `action="/api/proxies/reset"`) {
		t.Error("the profile the pool is on is not offered its reset")
	}
}
