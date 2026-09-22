// SPDX-License-Identifier: MIT

package web

import (
	"strconv"
	"strings"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// profileForm is one proxy profile as its boxes on the screen, which is to say
// as text.
//
// It is its own type rather than a corner of the settings form, because a
// profile is its own thing: there are several of them, a job names one, and the
// settings file holds one connection for all of them. The two used to be the
// same form because there used to be one of everything.
type profileForm struct {
	// ID is the profile being edited, and nought is a profile being made. It
	// travels in the form rather than in the path so that a save and a create
	// are one button and one handler: the difference is a number, not a page.
	ID   int64
	Name string

	// Source is where the addresses come from: none, a file, an address, or the
	// gateways stored in the service. Where is the path or the address.
	Source string
	Where  string
	// The three spans, each as the operator typed it, in the units its box is
	// labelled with: minutes.
	Refresh string
	Ban     string
	Renew   string
	// PerUpstream is how many threads share one exit, and Wire is socks5 or
	// http.
	PerUpstream string
	Wire        string
	// Gateways are the configuration names ticked, when the source is the
	// gateways.
	Gateways []string

	// What the ports of this profile are made of, beyond where they go out.
	//
	// VDNSOn says names are resolved through the exit at all and VDNS which of
	// the three ways; Solver says the challenge solver is on them; HTTP3 lets
	// them re-originate over HTTP/3.
	//
	// The switch and the mode are two controls because they are two questions,
	// and one list holding both made the answer to the first unreadable: a
	// reader looking for "off" had to recognise it among three phrasings of
	// "on". Off is now the switch, and the list holds only ways of being on.
	VDNSOn bool
	VDNS   string
	Solver bool
	HTTP3  bool

	// The road the ports take to their addresses: HopKind is none, a SOCKS5
	// proxy or one of the service's gateways, and HopProxy and HopGateway are
	// the one it is. Both boxes are kept whichever kind is chosen, so a reader
	// who tries the other kind and comes back finds what they had typed.
	HopKind    string
	HopProxy   string
	HopGateway string
}

// The kinds of first hop the form offers. None goes to the address directly.
const (
	hopNone    = ""
	hopSOCKS5  = "socks5"
	hopGateway = "gateway"
)

// newProfile is the profile a form that is making one is laid over.
//
// It is not the zero value, and the difference is the solver: a profile written
// from zeroes would have it off, because that is what a false boolean is, and
// the one place that says it should be on — the column's own default — is not
// consulted by a row this program hands over whole. A search Google challenges
// produces nothing without the solver, so off is a decision somebody makes and
// not one a zero value makes for them.
func newProfile() store.Profile { return store.NewProfile() }

// profileShowing is a profile as the boxes that would hold it.
func profileShowing(p store.Profile) profileForm {
	return profileForm{
		ID:          p.ID,
		Name:        p.Name,
		Source:      p.Kind,
		Where:       p.Location,
		Refresh:     spellUnits(p.Refresh, refreshUnit),
		Ban:         spellUnits(p.Ban, banUnit),
		Renew:       spellUnits(p.RenewEvery, renewUnit),
		PerUpstream: strconv.Itoa(atLeastOne(p.ThreadsPerUpstream)),
		Wire:        blanktrail.ProtocolOr(p.Protocol),
		Gateways:    p.Gateways,
		VDNSOn:      p.VDNSMode != blanktrail.VDNSOff,
		VDNS:        vdnsModeOr(p.VDNSMode),
		Solver:      p.Solver,
		HTTP3:       p.HTTP3,
	}.withHop(p.FirstHop)
}

// withHop is the form with a kept first hop in its boxes. One that no longer
// reads — kept by some other hand, or before the rules for it were what they are
// — is put in the proxy's box as it stands, where the reader can see it and put
// it right, rather than dropped without a word.
func (f profileForm) withHop(kept string) profileForm {
	hop, err := blanktrail.ParseFirstHop(kept)
	switch {
	case err != nil:
		f.HopKind, f.HopProxy = hopSOCKS5, kept
	case hop.Gateway != "":
		f.HopKind, f.HopGateway = hopGateway, hop.Gateway
	case hop.Proxy != "":
		f.HopKind, f.HopProxy = hopSOCKS5, hop.Proxy
	}
	return f
}

// profileFrom reads the boxes a browser sent.
func profileFrom(r former) profileForm {
	id, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue(profileField)), 10, 64)
	return profileForm{
		ID:          id,
		Name:        strings.TrimSpace(r.FormValue(nameField)),
		Source:      strings.TrimSpace(r.FormValue(sourceField)),
		Where:       strings.TrimSpace(r.FormValue(whereField)),
		Refresh:     strings.TrimSpace(r.FormValue(refreshField)),
		Ban:         strings.TrimSpace(r.FormValue(banField)),
		Renew:       strings.TrimSpace(r.FormValue(renewField)),
		PerUpstream: strings.TrimSpace(r.FormValue(perUpField)),
		Wire:        strings.TrimSpace(r.FormValue(wireField)),
		// These three are on the form whenever it is shown, so a box that sent
		// nothing is a box somebody unticked rather than one that was not there.
		VDNSOn: r.FormValue(vdnsOnField) != "",
		VDNS:   strings.TrimSpace(r.FormValue(vdnsField)),
		Solver: r.FormValue(solverField) != "",
		HTTP3:  r.FormValue(http3Field) != "",

		HopKind:    strings.TrimSpace(r.FormValue(firstHopField)),
		HopProxy:   strings.TrimSpace(r.FormValue(hopProxyField)),
		HopGateway: strings.TrimSpace(r.FormValue(hopGatewayField)),
	}
}

// onto lays what was typed over the profile it was read from, and says what it
// could not read.
//
// Over the profile rather than over an empty one, for the reason the settings
// page does it: a box that is not on the screen right now — the address while
// the gateways are chosen, the gateways while a list is — keeps what it had
// instead of being emptied by a form that never showed it.
func (f profileForm) onto(p store.Profile) (store.Profile, []string) {
	var b boxes
	next := p
	next.Name = f.Name
	if next.Name == "" {
		b.complaints = append(b.complaints, "proxies.profile.needs.name")
	}
	next.Protocol = blanktrail.ProtocolOr(f.Wire)
	next.Solver, next.HTTP3 = f.Solver, f.HTTP3
	// The switch wins over the list. A reader who turned vDNS off did not also
	// say which way it should be on, and the list under the switch still holds
	// whatever it was showing when they turned it off.
	switch {
	case !f.VDNSOn:
		next.VDNSMode = blanktrail.VDNSOff
	case !blanktrail.KnownVDNSMode(f.VDNS) || f.VDNS == blanktrail.VDNSOff:
		// Refused here rather than sent on: the service answers an unknown mode
		// with a refusal naming the four it takes, and a port that will not open
		// because a form let a typo through is a fault a long way from its cause.
		// Off among the ways of being on is the same kind of nonsense, arriving
		// from a form nobody drew.
		next.VDNSMode = p.VDNSMode
		b.complaints = append(b.complaints, "proxies.vdns.unknown")
	default:
		next.VDNSMode = f.VDNS
	}
	// One is the floor rather than the default alone: nought identities through
	// an egress is a pool that hands out nothing at all.
	next.ThreadsPerUpstream = atLeastOne(b.none(f.PerUpstream, p.ThreadsPerUpstream, "settings.perupstream.count"))
	// Nought is an answer, and the one that ships: it is how "hold this identity
	// for as long as it works" is said.
	next.RenewEvery = b.span(f.Renew, p.RenewEvery, renewUnit, "settings.renew.length")

	// The list is switched off by choosing no source, which is why the source is
	// a choice and not a box: a blank box would have to mean both "leave it" and
	// "use none", and one of the two would be wrong every time.
	switch f.Source {
	case sourceNone:
		next.Kind, next.Location, next.Refresh = "", "", 0
	case sourceGateways:
		// A set of gateways runs on no location and no interval: what is behind
		// each name lives in the service and is asked for when a job starts.
		// Both are carried through all the same, because the box holding the
		// address is not even on the screen while the gateways are chosen, and
		// nothing the reader can see says it is about to be forgotten.
		next.Kind = sourceGateways
		next.Location = keptOr(f.Where, p.Location)
		next.Ban = b.span(f.Ban, p.Ban, banUnit, "settings.ban.length")
		next.Gateways = f.Gateways
	case sourceFile, sourceURL:
		// And the gateways ticked are kept for the same reason, in the other
		// direction: two-and-thirty boxes are not something to tick twice.
		next.Kind = f.Source
		next.Location = f.Where
		next.Refresh = b.span(f.Refresh, p.Refresh, refreshUnit, "settings.refresh.length")
		next.Ban = b.span(f.Ban, p.Ban, banUnit, "settings.ban.length")
		next.Gateways = keptGateways(f.Gateways, p.Gateways)
	default:
		b.complaints = append(b.complaints, "settings.source.unknown")
	}

	// A profile on gateways has its road set on the gateway, in the service,
	// and the box is not on its screen: what it holds is kept, for the day the
	// profile goes back to a list. Every other source shows the box, so what it
	// says is the answer.
	if f.Source != sourceGateways {
		if hop, complaint := f.firstHop(); complaint != "" {
			b.complaints = append(b.complaints, complaint)
		} else {
			next.FirstHop = hop
		}
	}
	return next, b.complaints
}

// firstHop is the first hop the boxes describe, as a profile keeps it, or the
// complaint that stops it being kept.
//
// Refused here rather than sent on: a first hop the service cannot go through
// is a port that never carries anything, found out an hour into a job rather
// than on the screen where it was typed.
func (f profileForm) firstHop() (kept, complaint string) {
	switch f.HopKind {
	case hopNone:
		return "", ""
	case hopSOCKS5:
		if f.HopProxy == "" {
			return "", "proxies.hop.needs.proxy"
		}
		hop, err := blanktrail.ParseFirstHop(f.HopProxy)
		if err != nil || hop.Proxy == "" {
			// A gateway's name written into the proxy's box reads as a gateway,
			// which is not what the reader chose.
			return "", "proxies.hop.proxy.bad"
		}
		return hop.String(), ""
	case hopGateway:
		if f.HopGateway == "" {
			return "", "proxies.hop.needs.gateway"
		}
		return blanktrail.FirstHop{Gateway: f.HopGateway}.String(), ""
	}
	return "", "proxies.hop.unknown"
}

// former is the half of a request these forms read. It is an interface so the
// reading can be exercised without standing up a request.
type former interface {
	FormValue(string) string
}

// vdnsModeOr is the way of resolving names a form shows beside the switch.
//
// A profile with vDNS off still has to show something in that list, because the
// list is on the screen whether or not the switch is on. It shows the automatic
// one, which is what turning the switch back on without touching the list then
// means — and what a new profile starts on.
func vdnsModeOr(mode string) string {
	if mode == blanktrail.VDNSOff || !blanktrail.KnownVDNSMode(mode) {
		return blanktrail.VDNSAuto
	}
	return mode
}
