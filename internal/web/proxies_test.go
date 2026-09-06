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
	for _, want := range []string{
		`id="ports">6<`,
		`id="quarantined">1<`,
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

func TestProxies_SavesHowOftenAPortChangesItsIdentity(t *testing.T) {
	// The box is in minutes and what is kept is a duration: saved as minutes it
	// would be read back as nanoseconds and a run would change identity ten
	// million times a second, or never.
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := settings.Save(path, settings.Settings{ControlURL: "http://127.0.0.1:8891", APIKey: "k"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s, err := New(Config{Store: testStore(t), Logger: quiet(), SettingsPath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	prof, err := s.store.CreateProfile(t.Context(), store.Profile{Name: "Default"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	rec := postForm(t, s, proxiesAt, profileValues(prof, url.Values{
		"source":               {"gateways"},
		"ban_minutes":          {"10"},
		"threads_per_upstream": {"10"},
		"renew_minutes":        {"10"},
		"port_protocol":        {"socks5"},
	}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("saving answered %d: %s", rec.Code, rec.Body.String())
	}
	after, err := s.store.Profile(t.Context(), prof)
	if err != nil {
		t.Fatalf("reading the profile back: %v", err)
	}
	if after.RenewEvery != 10*time.Minute {
		t.Errorf("kept %v between changes of identity, want ten minutes", after.RenewEvery)
	}
	// And nought is an answer: it is how "hold this identity for as long as it
	// works" is said, and it is what a long address list wants.
	if rec := postForm(t, s, proxiesAt, profileValues(prof, url.Values{
		"source": {"url"}, "source_at": {"http://example.test/list"}, "renew_minutes": {"0"},
		"ban_minutes": {"10"}, "threads_per_upstream": {"1"}, "port_protocol": {"socks5"},
	})); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving nought answered %d", rec.Code)
	}
	if after, err = s.store.Profile(t.Context(), prof); err != nil {
		t.Fatalf("reading the profile back: %v", err)
	}
	if after.RenewEvery != 0 {
		t.Errorf("nought in the box was kept as %v", after.RenewEvery)
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
