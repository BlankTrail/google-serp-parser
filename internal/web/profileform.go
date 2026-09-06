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
}

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
	}
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
	return next, b.complaints
}

// former is the half of a request these forms read. It is an interface so the
// reading can be exercised without standing up a request.
type former interface {
	FormValue(string) string
}
