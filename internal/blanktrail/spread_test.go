// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// holding is a service with the given releases of every browser a spread asks
// about, so a test says what it is about rather than listing four catalogues.
func holding(t *testing.T, releases ...int) (*Client, *fakebt.Server) {
	t.Helper()
	f := fakebt.New(t)
	var held []fakebt.StoredProfile
	for _, browser := range []string{"chrome", "firefox", "edge", "safari"} {
		for _, v := range releases {
			held = append(held, fakebt.StoredProfile{
				Name:    fmt.Sprintf("%s_%d", browser, v),
				Browser: browser,
				Version: fmt.Sprintf("%d.0.1234.5", v),
			})
		}
	}
	f.SetProfiles(held...)
	cl, err := NewClient(f.URL(), f.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return cl, f
}

func TestNewestVersions_TakesTheNewestReleasesTheServiceHolds(t *testing.T) {
	// The release number and not the whole version string, because that is what
	// a browser filter carries: the service reads "chrome_153" as the browser
	// and the release. And the newest, because a fleet is meant to look like
	// what people are running.
	cl, _ := holding(t, 148, 153, 150, 151)

	got, err := cl.NewestVersions(context.Background(), "chrome", 3)
	if err != nil {
		t.Fatalf("NewestVersions: %v", err)
	}
	want := []int{153, 151, 150}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v — newest first", got, want)
		}
	}
}

func TestNewestVersions_SaysSoWhenTheServiceHoldsNone(t *testing.T) {
	// A spread built on nothing would open every port under the same template
	// and report it as a fleet. Better to refuse where the answer is missing.
	f := fakebt.New(t)
	cl, err := NewClient(f.URL(), f.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := cl.NewestVersions(context.Background(), "chrome", 3); err == nil {
		t.Error("a service holding no profile at all answered with a version")
	}
}

func TestSpread_IsEveryBrowserOnEverySystemItShipsOn(t *testing.T) {
	// The point of it: a run of three hundred identities that are all the latest
	// Chrome on Windows is one identity three hundred times over.
	cl, _ := holding(t, 152, 153)

	got, err := Spread(context.Background(), cl, DeviceDesktop, Worn{}, 2, 0)
	if err != nil {
		t.Fatalf("Spread: %v", err)
	}
	// Nine combinations at two releases each.
	if len(got) != len(desktopPairs)*2 {
		t.Fatalf("the spread holds %d templates, want %d", len(got), len(desktopPairs)*2)
	}

	seen := map[string]bool{}
	for _, s := range got {
		if seen[s.Name] {
			t.Errorf("template %q is in the spread twice", s.Name)
		}
		seen[s.Name] = true
		if s.Spec.Browser == "" || s.Spec.OS == "" {
			t.Errorf("template %q carries %q on %q", s.Name, s.Spec.Browser, s.Spec.OS)
		}
		// The release travels in the browser filter, which is where the service
		// reads it from.
		if !strings.Contains(s.Spec.Browser, "_15") {
			t.Errorf("template %q asks for browser %q, which names no release", s.Name, s.Spec.Browser)
		}
	}
	for _, want := range []string{"chrome_153_windows", "chrome_153_linux", "safari_153_macos"} {
		if !seen[want] {
			t.Errorf("the spread has no %s", want)
		}
	}
}

func TestSpread_PutsSafariOnMacOSAndNowhereElse(t *testing.T) {
	// An identity that never existed reads as itself. Safari does not ship on
	// Windows or Linux, and a fleet carrying one is a fleet with a tell in it.
	cl, _ := holding(t, 153)

	got, err := Spread(context.Background(), cl, DeviceDesktop, Worn{}, 1, 0)
	if err != nil {
		t.Fatalf("Spread: %v", err)
	}
	var safari int
	for _, s := range got {
		if !strings.HasPrefix(s.Spec.Browser, "safari") {
			continue
		}
		safari++
		if s.Spec.OS != "macos" {
			t.Errorf("Safari is offered on %q", s.Spec.OS)
		}
	}
	if safari == 0 {
		t.Error("the desktop spread carries no Safari at all")
	}
}

func TestSpread_IsCutToTheNumberOfPortsRatherThanRefused(t *testing.T) {
	// A pool opens one port per template at least and refuses to open at all
	// when it has fewer ports than templates. A job of two ports wants two of
	// the combinations, not a refusal — and two such jobs want different two,
	// which is why the cut is a draw rather than the first of the list.
	cl, _ := holding(t, 150, 151, 152, 153)

	got, err := Spread(context.Background(), cl, DeviceDesktop, Worn{}, 4, 3)
	if err != nil {
		t.Fatalf("Spread: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("the spread holds %d templates for a pool of 3", len(got))
	}
	if _, err := planSpecs(got, 3); err != nil {
		t.Errorf("a pool of 3 would not open on its own spread: %v", err)
	}
}

func TestSpread_IsPhonesForAPhoneJob(t *testing.T) {
	// A run asking for the page a phone is answered with is a run whose ports
	// are phones. Nothing in the desktop set belongs in it.
	cl, _ := holding(t, 153)

	got, err := Spread(context.Background(), cl, DeviceMobile, Worn{}, 1, 0)
	if err != nil {
		t.Fatalf("Spread: %v", err)
	}
	for _, s := range got {
		if s.Spec.OS != "ios" && s.Spec.OS != "android" {
			t.Errorf("a phone job carries %q on %q", s.Spec.Browser, s.Spec.OS)
		}
	}
	if len(got) != len(mobilePairs) {
		t.Errorf("the phone spread holds %d templates, want %d", len(got), len(mobilePairs))
	}
}

func TestShips_RefusesAnIdentityThatNeverExisted(t *testing.T) {
	// The form offering these is two boxes, and two boxes can be set to a pair
	// nobody has. Safari on Windows is the one somebody will actually pick, and
	// a run on it is a run whose fingerprint says it is a browser that does not
	// exist there — which is the one thing an identity must never say.
	for _, c := range []struct {
		device, browser, os string
		want                bool
	}{
		{DeviceDesktop, "", "", true},               // the whole spread
		{DeviceDesktop, "chrome", "windows", true},  //
		{DeviceDesktop, "safari", "macos", true},    //
		{DeviceDesktop, "safari", "windows", false}, // Safari does not ship there
		{DeviceDesktop, "edge", "linux", false},     // nor does Edge
		{DeviceDesktop, "chrome", "", true},         // a browser, on whatever it ships on
		{DeviceDesktop, "", "linux", true},          // a system, under whatever ships on it
		{DeviceDesktop, "", "android", false},       // a phone system on a desktop job
		{DeviceDesktop, "opera", "windows", false},  // a browser this program has no matrix for
		{DeviceMobile, "safari", "ios", true},       //
		{DeviceMobile, "edge", "android", true},     //
		{DeviceMobile, "edge", "ios", false},        // Edge does not ship there
		{DeviceMobile, "", "windows", false},        // a desktop system on a phone job
	} {
		if got := Ships(c.device, c.browser, c.os); got != c.want {
			t.Errorf("Ships(%q, %q, %q) = %v, want %v", c.device, c.browser, c.os, got, c.want)
		}
	}
}

func TestBrowsersAndSystems_AreWhatTheMatrixHolds(t *testing.T) {
	// A chooser is offered from the same table the spread is built from, so a
	// screen cannot offer a browser no port will ever open under.
	browsers, systems := Browsers(), Systems()
	for _, p := range append(append([]pair{}, desktopPairs...), mobilePairs...) {
		if !has(browsers, p.browser) {
			t.Errorf("the matrix opens ports under %q and no chooser offers it", p.browser)
		}
		if !has(systems, p.os) {
			t.Errorf("the matrix opens ports on %q and no chooser offers it", p.os)
		}
	}
	// And nothing beyond it: a name on the list that no pair holds is a choice
	// that opens nothing.
	for _, name := range browsers {
		if !Ships(DeviceDesktop, name, "") && !Ships(DeviceMobile, name, "") {
			t.Errorf("the chooser offers the browser %q, which ships nowhere", name)
		}
	}
	for _, name := range systems {
		if !Ships(DeviceDesktop, "", name) && !Ships(DeviceMobile, "", name) {
			t.Errorf("the chooser offers the system %q, which nothing runs on", name)
		}
	}
	// Each of them once. A list with Chrome in it three times is a list nobody
	// reads twice.
	if len(browsers) != len(distinct(browsers)) || len(systems) != len(distinct(systems)) {
		t.Errorf("the choosers repeat themselves: %v / %v", browsers, systems)
	}
}

func has(list []string, want string) bool {
	for _, one := range list {
		if one == want {
			return true
		}
	}
	return false
}

func distinct(list []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, one := range list {
		if !seen[one] {
			seen[one] = true
			out = append(out, one)
		}
	}
	return out
}

func TestSpread_IsNarrowedByWhatTheJobNamed(t *testing.T) {
	// The three boxes on the form. Naming none of them is the whole matrix,
	// which is what the tests above are about; this is what naming part of it
	// does, and the part that matters is that naming one side does not silently
	// pin the other. A job that asked for Firefox asked for Firefox — on
	// whatever Firefox ships on.
	cl, _ := holding(t, 152, 153)

	firefox, err := Spread(context.Background(), cl, DeviceDesktop, Worn{Browser: "firefox"}, 2, 0)
	if err != nil {
		t.Fatalf("Spread on a browser: %v", err)
	}
	// Three systems at two releases.
	if len(firefox) != 6 {
		t.Errorf("a job naming firefox is spread over %d templates, want its three systems at two releases", len(firefox))
	}
	for _, s := range firefox {
		if !strings.HasPrefix(s.Spec.Browser, "firefox_") {
			t.Errorf("a job naming firefox was given %q", s.Spec.Browser)
		}
	}

	mac, err := Spread(context.Background(), cl, DeviceDesktop, Worn{OS: "macos"}, 1, 0)
	if err != nil {
		t.Fatalf("Spread on a system: %v", err)
	}
	// Every browser that ships on a Mac, which is the four of them.
	if len(mac) != 4 {
		t.Errorf("a job naming macos is spread over %d templates, want every browser that ships there", len(mac))
	}
	for _, s := range mac {
		if s.Spec.OS != "macos" {
			t.Errorf("a job naming macos was given a port on %q", s.Spec.OS)
		}
	}

	// All three named is one identity, and the release is taken as it stands.
	one, err := Spread(context.Background(), cl, DeviceDesktop, Worn{Browser: "safari", OS: "macos", Release: 26}, 10, 0)
	if err != nil {
		t.Fatalf("Spread on all three: %v", err)
	}
	if len(one) != 1 || one[0].Spec.Browser != "safari_26" || one[0].Spec.OS != "macos" {
		t.Errorf("a job naming all three was spread over %d templates: %v", len(one), one)
	}
}

func TestSpread_AsksTheServiceForNoVersionsWhenTheJobNamedOne(t *testing.T) {
	// A named release is the caller saying which build. Listing the ones the
	// service holds would be four requests whose answer is thrown away, and
	// worse, it invites this to replace the named build with a held one — which
	// would be a run reporting a version it did not use.
	cl, f := holding(t, 152, 153)
	before := listings(f)

	got, err := Spread(context.Background(), cl, DeviceDesktop, Worn{Browser: "chrome", Release: 999}, 10, 0)
	if err != nil {
		t.Fatalf("Spread: %v", err)
	}
	if n := listings(f) - before; n != 0 {
		t.Errorf("the service was asked for its versions %d times for a job that named one", n)
	}
	for _, s := range got {
		if s.Spec.Browser != "chrome_999" {
			t.Errorf("a job naming chrome 999 was given %q", s.Spec.Browser)
		}
	}
}

func TestSpread_SaysSoWhenTheIdentityShipsNowhere(t *testing.T) {
	// Safari on Windows. The form refuses it before it gets here, and this is
	// the other door: a run started from the command line or the API arrives
	// with the same pair and has to be told, rather than opening a pool of
	// nothing or of something else.
	cl, _ := holding(t, 153)
	_, err := Spread(context.Background(), cl, DeviceDesktop, Worn{Browser: "safari", OS: "windows"}, 1, 0)
	if err == nil {
		t.Fatal("a spread of Safari on Windows was allowed")
	}
	if !strings.Contains(err.Error(), "safari") || !strings.Contains(err.Error(), "windows") {
		t.Errorf("the refusal is %q, which does not name the pair that was asked for", err)
	}
}

// listings is how many times the service has been asked what it holds.
func listings(f *fakebt.Server) int {
	n := 0
	for _, one := range f.Requests() {
		if strings.HasPrefix(one.Path, "/api/v1/profiles") {
			n++
		}
	}
	return n
}

func TestSpread_GoesOnWithoutAServiceThatWillNotSayWhatItHolds(t *testing.T) {
	// A listing that fails costs the spread over releases and nothing else. The
	// browsers and the systems need no service to know, and a browser named
	// without a release is read by the service as the newest it has — so the
	// run goes out on a real fingerprint rather than not going out at all.
	//
	// It is not a failure being swallowed. The reasons a listing fails are the
	// service being unreachable or not having that endpoint, and the first is
	// about to be said out loud by the ports refusing to open through the same
	// connection.
	f := fakebt.New(t)
	// One per browser the matrix asks about, and one over.
	for i := 0; i < 5; i++ {
		f.FailNext("/api/v1/profiles", 500, "the listing is not available")
	}
	cl, err := NewClient(f.URL(), f.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	got, err := Spread(context.Background(), cl, DeviceDesktop, Worn{}, 10, 0)
	if err != nil {
		t.Fatalf("Spread: %v", err)
	}
	if len(got) != len(desktopPairs) {
		t.Fatalf("the spread holds %d templates, want one per pair with no release named", len(got))
	}
	for _, s := range got {
		if strings.ContainsRune(s.Spec.Browser, '_') {
			t.Errorf("template %q names the release %q, which no listing answered", s.Name, s.Spec.Browser)
		}
		if s.Spec.OS == "" {
			t.Errorf("template %q carries no system", s.Name)
		}
	}
}
