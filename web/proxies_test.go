// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/settings"
)

func TestProxies_ShowsTheReadingOfThePoolAndBreaksTheFailuresDown(t *testing.T) {
	// A pool going badly and a pool going well look identical from outside —
	// both are a number of ports and a rate — and the difference between them is
	// which of a handful of things is going wrong. A single total of failures
	// cannot say: a run failing on dead addresses wants another list, and one
	// being walled by the origin wants something else entirely, and both read
	// the same added together.
	rec := httptest.NewRecorder()
	testServerWithSupervisor(t).Handler().ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, proxiesAt, nil))

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
	body := getBody(t, s, proxiesAt)

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

	rec := postForm(t, s, proxiesAt, url.Values{
		"source":         {"file"},
		"source_at":      {"C:/somewhere/list.txt"},
		"source_refresh": {"7"},
		"port_protocol":  {"http"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want a redirect back to the screen", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != proxiesAt {
		t.Errorf("the save lands on %q, want %q", got, proxiesAt)
	}

	after, err := settings.Load(path)
	if err != nil {
		t.Fatalf("reading the settings back: %v", err)
	}
	if after.Proxy.Kind != "file" || after.Proxy.Location != "C:/somewhere/list.txt" {
		t.Errorf("the list reads %+v, want the file that was typed", after.Proxy)
	}
	if after.PortProtocol != blanktrail.ProtocolHTTP {
		t.Errorf("the ports are reached over %q, want the http that was chosen", after.PortProtocol)
	}
	if after.APIKey != before.APIKey {
		t.Error("saving a file path took the key away")
	}
	if after.ControlURL != before.ControlURL {
		t.Errorf("the connection reads %q, want the %q it was", after.ControlURL, before.ControlURL)
	}
	if after.HotPorts != before.HotPorts || after.HotDevice != before.HotDevice {
		t.Errorf("the identities kept warm read %d %q, want %d %q",
			after.HotPorts, after.HotDevice, before.HotPorts, before.HotDevice)
	}
}

func TestSaveProxies_ComplainsAboutWhatWasTypedRatherThanWritingIt(t *testing.T) {
	// The same order the settings screen keeps: complain, open, then write.
	// Nothing is saved while there is anything to complain about, because
	// settings written and refused leave the file and the machine disagreeing.
	before := settings.Settings{ControlURL: "http://127.0.0.1:1"}
	s, path := serverWithSettings(t, before)

	rec := postForm(t, s, proxiesAt, url.Values{
		"source":         {"file"},
		"source_at":      {"C:/list.txt"},
		"source_refresh": {"not a number"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want the screen back with the complaint on it", rec.Code)
	}

	after, err := settings.Load(path)
	if err != nil {
		t.Fatalf("reading the settings back: %v", err)
	}
	if after.Proxy.Kind != "" {
		t.Errorf("a form that would not parse was written down as %+v", after.Proxy)
	}
}

func TestSaveProxies_WritesDownHowLongAnAddressIsBanned(t *testing.T) {
	// How fast a gateway's exits turn over is a property of the list somebody
	// bought rather than of this program, so the length of a ban is theirs to
	// set. A ban that is short brings the same dead addresses back inside one
	// job, which is what a fixed five minutes did.
	s, path := serverWithSettings(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})

	rec := postForm(t, s, proxiesAt, url.Values{
		"source":      {"file"},
		"source_at":   {"C:/list.txt"},
		"ban_minutes": {"90"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want a redirect back to the screen", rec.Code)
	}

	after, err := settings.Load(path)
	if err != nil {
		t.Fatalf("reading the settings back: %v", err)
	}
	if after.Proxy.Ban != 90*time.Minute {
		t.Errorf("an address is banned for %s, want the ninety minutes that were typed",
			after.Proxy.Ban)
	}

	// And the box shows it back in the unit it was typed in.
	if body := getBody(t, s, proxiesAt); !strings.Contains(body, `value="90"`) {
		t.Error("the box does not show the ban that was saved")
	}
}

func TestSaveProxies_WritesDownHowManyThreadsOneEgressCarries(t *testing.T) {
	// A pool can hold more identities than the list has egresses, and this is
	// what stops all of them going through one address at once. One is the
	// floor: nought identities through an egress is a pool that hands out
	// nothing at all.
	s, path := serverWithSettings(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})

	rec := postForm(t, s, proxiesAt, url.Values{
		"source":               {"file"},
		"source_at":            {"C:/list.txt"},
		"threads_per_upstream": {"4"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want a redirect back to the screen", rec.Code)
	}
	after, err := settings.Load(path)
	if err != nil {
		t.Fatalf("reading the settings back: %v", err)
	}
	if after.ThreadsPerUpstream != 4 {
		t.Errorf("one egress carries %d threads, want the four that were typed",
			after.ThreadsPerUpstream)
	}

	// Nought is not an answer here, and neither is a file edited by hand into
	// one: what comes back out is one.
	rec = postForm(t, s, proxiesAt, url.Values{
		"source":               {"file"},
		"source_at":            {"C:/list.txt"},
		"threads_per_upstream": {"0"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want the save to go through", rec.Code)
	}
	if after, err = settings.Load(path); err != nil {
		t.Fatalf("reading the settings back: %v", err)
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
	page := get(t, testServer(t), proxiesAt).Body.String()
	if !strings.Contains(page, `id="reopenings"`) {
		t.Error("the proxy screen never says how many ports were opened again")
	}
}
