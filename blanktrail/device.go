// SPDX-License-Identifier: MIT

package blanktrail

// The kinds of result page a run can ask Google for.
//
// Google answers a phone and a desktop with different pages — different
// results, in a different order, with different things around them — and which
// one a run wanted is not something it can be told afterwards. It is a property
// of the identity the request goes through, so it is settled when the ports are
// opened and not per request.
const (
	// DeviceDesktop is the latest Chrome on Windows, which is what this program
	// has always opened.
	DeviceDesktop = "desktop"
	// DeviceMobile is a phone: Safari on iOS and Chrome on Android, mixed.
	//
	// Mixed rather than one of them, because a run of five hundred phones that
	// are all the same phone is a pattern, and because the two answer differently
	// often enough that a run on one of them is a measurement of that one.
	DeviceMobile = "mobile"
)

// Devices is every kind, in the order a form offers them.
func Devices() []string { return []string{DeviceDesktop, DeviceMobile} }

// KnownDevice reports whether a name is one this package opens ports for.
//
// Anything else is refused where it arrives rather than quietly opened as a
// desktop: a run labelled "mobile" that went out on Windows would be a
// measurement of the wrong thing, filed under the right name.
func KnownDevice(name string) bool {
	for _, known := range Devices() {
		if name == known {
			return true
		}
	}
	return false
}

// mobileBrowsers are the phones this program opens, as a template each.
//
// The names are what the pool labels a port with and what the statistics come
// back under, so they are the phone rather than a number: "ios" and "android"
// read as themselves in a report nobody has the code beside.
var mobileBrowsers = []NamedSpec{
	{Name: "ios", Spec: phoneSpec("safari", "ios")},
	{Name: "android", Spec: phoneSpec("chrome", "android")},
}

// phoneSpec is the desktop template with the browser and the system changed.
//
// Everything else is left exactly as it is: the profile source, the challenge
// solver, the cookie jar, the timeouts. A phone differs from a desktop in what
// it says it is, not in how this program treats it, and a second set of
// defaults would be a second place for them to drift.
func phoneSpec(browser, os string) PortSpec {
	spec := DefaultPortSpec()
	spec.Browser = browser
	spec.OS = os
	return spec
}

// SpecsFor is the templates a pool opens for a kind of result page.
//
// Desktop hands back nothing: one template needs no name, and a pool given no
// templates opens every port under PoolConfig.Spec, which is the default. Mobile
// hands back the two phones, and the pool spreads its ports over them —
// guaranteeing each at least one port, so a small pool is still a mixture.
func SpecsFor(device string) []NamedSpec {
	if device == DeviceMobile {
		// Copied rather than handed over: the pool keeps what it is given, and a
		// caller that changed a template afterwards would change every pool that
		// had ever been opened from it.
		return append([]NamedSpec(nil), mobileBrowsers...)
	}
	return nil
}
