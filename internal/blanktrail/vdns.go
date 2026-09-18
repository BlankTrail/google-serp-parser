// SPDX-License-Identifier: MIT

package blanktrail

// Where a port resolves the names it is asked for.
//
// The service takes four answers and this program offers all four, because the
// difference between them is where a request says it came from. A port whose
// traffic leaves through an exit in one country while its names are resolved
// through this machine has told the far end two things, and the second one is
// true.
const (
	// VDNSAuto is the service's own answer: resolving through the exit where
	// the port has a tunnel to resolve through, and through this machine where
	// it has none. It is the empty string because that is what the service reads
	// as "you did not say", and it is what this program asks for unless somebody
	// says otherwise.
	VDNSAuto = ""
	// VDNSOnLeak resolves through the exit only where resolving here would leak
	// the name to a resolver the traffic does not go through.
	VDNSOnLeak = "on_leak"
	// VDNSForced resolves through the exit always.
	VDNSForced = "forced"
	// VDNSOff never does: names are resolved wherever this machine resolves
	// them, whatever the traffic does afterwards.
	VDNSOff = "off"
)

// VDNSModes is every answer, in the order a form offers them: the automatic one
// first, because it is the one nobody has to think about.
func VDNSModes() []string { return []string{VDNSAuto, VDNSOnLeak, VDNSForced, VDNSOff} }

// KnownVDNSMode reports whether a mode is one the service takes.
//
// Anything else is refused where it arrives rather than sent on: the service
// answers an unknown mode with a 400 naming the four, and a port that fails to
// open because a form let a typo through is a fault a long way from its cause.
func KnownVDNSMode(mode string) bool {
	for _, known := range VDNSModes() {
		if mode == known {
			return true
		}
	}
	return false
}
