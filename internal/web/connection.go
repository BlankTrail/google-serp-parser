// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// reach is what this program last learned about its connection to the service,
// and each value is the phrase that names it.
//
// They are the states an operator can do something about, and no others. How
// many ports are open or how warm they are is on the screen for that; this is
// the question that comes before it — whether this machine can reach the thing
// it runs on at all.
type reach string

const (
	// reachUnknown is a machine that has not been asked yet, and a server that
	// keeps no settings and so has no connection of its own to watch. Nothing is
	// said about a connection nothing is known about.
	reachUnknown reach = ""
	reachGood    reach = "link.good"
	// reachUnset is a machine where the address or the key has never been filled
	// in. It is not a fault and it is not a failure to reach anything: nothing
	// has been asked of the service, because there is nothing to ask it with.
	reachUnset reach = "link.unset"
	// reachSilent is a service that did not answer: not running, listening
	// somewhere else, or behind something that swallowed the request.
	reachSilent reach = "link.silent"
	// reachRefused is a service that answered and would not take the key.
	reachRefused reach = "link.refused"
	// reachInactive is a service that took the key and reports a licence that
	// will not open a port.
	reachInactive reach = "link.inactive"
)

// wrong says whether this is something to put a banner up about, and the
// sentence to put in it.
//
// A connection nobody has asked about yet gets no banner. The alternative is a
// program that opens with "BlankTrail is not answering" for the half second
// before it has asked, which is an interface crying wolf on every start.
func (r reach) wrong() (string, bool) {
	switch r {
	case reachUnset:
		return "notice.unset", true
	case reachSilent:
		return "notice.silent", true
	case reachRefused:
		return "notice.refused", true
	case reachInactive:
		return "notice.inactive", true
	}
	return "", false
}

// linkEvery is how often the connection is asked about.
//
// It is a background question rather than a page's question: what it costs is
// four small calls a minute against a service on this same machine, and what it
// buys is that no page ever waits on a network to be drawn. Slow enough not to
// be noise in the service's own log, quick enough that somebody who has just
// started BlankTrail sees the banner go while they are still looking at it.
const linkEvery = 15 * time.Second

// linkGrace bounds one asking. A service that is not there is most often a port
// nothing is listening on, which fails at once; this is for the other kind,
// where something accepts the connection and then says nothing, and it is short
// because a page is reporting the answer.
const linkGrace = 5 * time.Second

// link is the last thing known about the connection.
//
// It is held rather than fetched because every screen carries it and the
// busiest of them redraws itself every three seconds. A page that asked the
// service each time would put a network call in front of every redraw of every
// screen in every browser watching, to answer a question whose answer changes
// about once a day.
type link struct {
	mu   sync.Mutex
	seen reach
}

func (l *link) saw(r reach) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = r
}

func (l *link) last() reach {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen
}

// watchConnection keeps that answer current for as long as the server runs.
//
// A server that keeps no settings watches nothing: it has no connection of its
// own, and a history being read on another machine should not be reporting on
// somebody else's service.
func (s *Server) watchConnection(ctx context.Context) {
	if s.settingsPath == "" {
		return
	}
	for {
		s.link.saw(s.probe(ctx))
		select {
		case <-ctx.Done():
			return
		case <-time.After(linkEvery):
		}
	}
}

// probe asks the service two questions and reads the answers as one of the
// states above.
//
// Two, because they fail differently and are put right differently. The first
// says whether anything is listening at all, and it is answered without a key —
// which is exactly why it cannot be the only one: a machine whose key the
// service will not take passes it and then fails everything after it, and the
// reader is sent to look at whatever the next call happened to be about. The
// second carries the key and reads the licence, so a refusal and a licence that
// will not open a port are told apart and named.
//
// Nothing heavier is asked. The gateways and the certificate belong to the
// check on the settings screen, which somebody presses; this one runs all day.
func (s *Server) probe(ctx context.Context) reach {
	saved, err := settings.Load(s.settingsPath)
	if err != nil || saved.ControlURL == "" || saved.APIKey == "" {
		return reachUnset
	}
	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		return reachUnset
	}
	ctx, cancel := context.WithTimeout(ctx, linkGrace)
	defer cancel()

	if err := client.Health(ctx); err != nil {
		if blanktrail.IsUnauthorized(err) {
			return reachRefused
		}
		return reachSilent
	}
	licence, err := client.LicenseStatus(ctx)
	if err != nil {
		if blanktrail.IsUnauthorized(err) {
			return reachRefused
		}
		return reachSilent
	}
	if !licence.Activated {
		return reachInactive
	}
	return reachGood
}

// notice is a banner: one sentence saying what is not set up, and one press
// leading to where it is set up.
//
// One press and not a paragraph of instructions. Whoever reads this wants the
// screen that fixes it, and a banner explaining the way there is a banner that
// has to be read before it can be obeyed.
type notice struct {
	// Key is the sentence, and Press the phrase on the button beside it.
	Key   string
	Press string
	URL   string
}

// noticesFor is what stands above the screen at the given address.
//
// A banner is not drawn on the screen that puts it right. The reader is already
// there, and a banner offering to take them where they are reads as a button
// that does nothing.
func (s *Server) noticesFor(ctx context.Context, under string) []notice {
	var out []notice
	if under != settingsAt && s.settingsPath != "" {
		if said, wrong := s.link.last().wrong(); wrong {
			out = append(out, notice{Key: said, Press: "notice.settings", URL: settingsAt})
		}
	}
	if under != proxiesAt {
		if said, at, wrong := s.exits(ctx); wrong {
			out = append(out, notice{Key: said, Press: "notice.profile.press", URL: at})
		}
	}
	return out
}

// exits says whether the profile a job naming none would run on can send a
// request anywhere, and where to go about it when it cannot.
//
// There are two ways for it to be wrong, and they read differently. No default
// profile at all is a database nothing has ever carried a profile into, and the
// way out of it is the list of profiles. A default profile naming no exits is
// the ordinary state of a fresh machine — one profile is written on the first
// start, out of settings that named nothing — and the way out of that is that
// profile's own page — which is the proxies screen with that profile already in
// the form, so the boxes are in front of the reader when they arrive.
//
// A database that will not answer says nothing here. A banner is for something
// the reader can put right, and "the history could not be read" is not that:
// the screen they asked for reports it.
func (s *Server) exits(ctx context.Context) (string, string, bool) {
	profile, err := s.store.DefaultProfile(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNoProfile) {
			return "notice.profile.none", proxiesAt, true
		}
		return "", "", false
	}
	if profile.Empty() {
		return "notice.profile.empty", profileAt(profile.ID), true
	}
	return "", "", false
}
