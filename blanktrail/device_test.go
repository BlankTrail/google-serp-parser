// SPDX-License-Identifier: MIT

package blanktrail

import (
	"slices"
	"testing"
)

func TestSpecsFor_OpensADesktopUnderTheOneDefaultAndAPhoneUnderTwo(t *testing.T) {
	// A desktop needs no named template: one template needs no name, and a pool
	// given none opens every port under the default — which is what this program
	// has always opened and what every job written before the choice ran on.
	if got := SpecsFor(DeviceDesktop); got != nil {
		t.Errorf("a desktop asks for the templates %v, want none at all", got)
	}
	// Anything the pool cannot open is a desktop rather than a refusal here: the
	// refusal belongs where the word arrives, and a pool opened on nothing would
	// be a job that fails on every query instead of one that runs.
	if got := SpecsFor("tractor"); got != nil {
		t.Errorf("an unknown kind asks for the templates %v, want the default", got)
	}

	phones := SpecsFor(DeviceMobile)
	if len(phones) != 2 {
		t.Fatalf("a phone opens %d templates, want the two: %v", len(phones), phones)
	}
	// Both phones, and each of them a phone: a mobile run that opened two Windows
	// desktops would be a measurement of the wrong thing under the right name.
	var browsers, systems []string
	for _, spec := range phones {
		if spec.Name == "" {
			t.Error("a template with no name cannot be told from another in the statistics")
		}
		browsers = append(browsers, spec.Spec.Browser)
		systems = append(systems, spec.Spec.OS)
	}
	for _, want := range []string{"safari", "chrome"} {
		if !slices.Contains(browsers, want) {
			t.Errorf("no phone runs %s: %v", want, browsers)
		}
	}
	for _, want := range []string{"ios", "android"} {
		if !slices.Contains(systems, want) {
			t.Errorf("no phone runs %s: %v", want, systems)
		}
	}
}

func TestSpecsFor_ChangesNothingButTheBrowserAndTheSystem(t *testing.T) {
	// A phone differs from a desktop in what it says it is, not in how this
	// program treats it. Everything else — where the profile comes from, the
	// challenge solver, the cookie jar, the timeouts — is one set of defaults,
	// and a second set here would be a second place for them to drift.
	base := DefaultPortSpec()
	for _, phone := range SpecsFor(DeviceMobile) {
		want := base
		want.Browser = phone.Spec.Browser
		want.OS = phone.Spec.OS
		if phone.Spec != want {
			t.Errorf("the %s template differs from the default by more than the browser "+
				"and the system:\n got %+v\nwant %+v", phone.Name, phone.Spec, want)
		}
	}
}

func TestSpecsFor_HandsBackACopyRatherThanWhatItKeeps(t *testing.T) {
	// The pool keeps what it is given. A caller that changed a template
	// afterwards would change every pool that had ever been opened from it, and
	// the run that noticed would be one nobody could reproduce.
	first := SpecsFor(DeviceMobile)
	first[0].Name = "changed"
	first[0].Spec.OS = "windows"

	second := SpecsFor(DeviceMobile)
	if second[0].Name == "changed" || second[0].Spec.OS == "windows" {
		t.Error("changing one caller's templates changed everybody's")
	}
}

func TestKnownDevice_TakesTheTwoAndNothingElse(t *testing.T) {
	// The word arrives from a form and from a program's request body, and a kind
	// nobody can open has to be refused where it arrives. Taken quietly as a
	// desktop it would file a run under a name it never ran as.
	for _, device := range Devices() {
		if !KnownDevice(device) {
			t.Errorf("KnownDevice(%q) is false, and it is on the list this package offers", device)
		}
	}
	for _, wrong := range []string{"", "Desktop", "phone", "ios", "tractor"} {
		if KnownDevice(wrong) {
			t.Errorf("KnownDevice(%q) is true, and no port is opened for it", wrong)
		}
	}
}
