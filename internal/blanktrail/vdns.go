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

// How the names are resolved once a port resolves them at all.
//
// The mode above says whether the exit is asked; this says what is asked and
// by whom. They are two knobs because they answer two questions, and the
// service takes them separately.
const (
	// ResolverAuto is the ladder the service walks by itself: the exit's own
	// provider first, then a curated pool through the exit, then the exit
	// resolving for itself. It is the empty string because that is what the
	// service reads as "you did not say".
	ResolverAuto = ""
	// ResolverISP is the exit provider's own resolvers and nothing else, and it
	// answers with a failure rather than walking on where they are unknown or
	// silent.
	ResolverISP = "isp"
	// ResolverPool is a curated set of resolvers, asked through the exit, with
	// the client subnet carried so the answer suits where the traffic leaves.
	ResolverPool = "pool"
	// ResolverExit makes the exit itself the resolver, asking the authoritative
	// servers directly. The country of the lookup then matches the country of
	// the traffic by construction.
	ResolverExit = "exit"
	// ResolverCustom is a list of resolvers the operator names.
	ResolverCustom = "custom"
	// ResolverDelegate hands the name to the proxy and lets it resolve: a SOCKS5
	// request by name rather than by address.
	//
	// It is what this parser asks for unless somebody says otherwise. The name
	// is then resolved by whatever the exit itself uses, which is the one answer
	// that cannot disagree with where the traffic comes out — and it costs no
	// round trip of its own, where every other answer here asks a resolver
	// before the request can start.
	ResolverDelegate = "delegate"
)

// Resolvers is every answer, in the order a form offers them, the automatic one
// first.
func Resolvers() []string {
	return []string{ResolverAuto, ResolverDelegate, ResolverISP, ResolverPool,
		ResolverExit, ResolverCustom}
}

// KnownResolver reports whether a strategy is one the service takes. Anything
// else is refused here rather than sent on, for the reason KnownVDNSMode is.
func KnownResolver(strategy string) bool {
	for _, known := range Resolvers() {
		if strategy == known {
			return true
		}
	}
	return false
}
