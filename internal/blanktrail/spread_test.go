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

	got, err := Spread(context.Background(), cl, DeviceDesktop, 2, 0)
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

	got, err := Spread(context.Background(), cl, DeviceDesktop, 1, 0)
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

	got, err := Spread(context.Background(), cl, DeviceDesktop, 4, 3)
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

	got, err := Spread(context.Background(), cl, DeviceMobile, 1, 0)
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
