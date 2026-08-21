// SPDX-License-Identifier: MIT

package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/settings"
)

// proxiesRefresh is how often the proxy screen asks to be drawn again.
//
// The same span the state screen uses, and for the same reason: what is on it
// moves only while something is running, and every figure was worked out in one
// reading of one pool rather than gathered a piece at a time.
const proxiesRefresh = 5 * time.Second

// proxiesPage is the reading of the pool a screen shows.
//
// It exists because a pool that is going badly and a pool that is going well
// look identical from outside — both are a number of ports and a rate — and the
// difference between them is which of a handful of things is going wrong. Every
// figure here is one somebody can act on: addresses running out, ports being
// set aside, requests failing, and what kind of failure they are.
type proxiesPage struct {
	page

	// Running says whether anything is going through the pool, which decides
	// whether the screen asks for itself again.
	Running bool
	// Refresh is how often, in milliseconds, the screen asks to be drawn again.
	// Nought asks once and stops.
	Refresh int64

	// Since is when the counters below were last put back to nought, which is
	// either the start of this session or the last press of the button.
	Since string

	// Addresses is the egress list: how many there are and how many are resting
	// off a failure right now.
	Addresses int
	Resting   int
	// Ports is what is open, and Quarantined how many of them are set aside.
	Ports       int
	Warm        int
	Quarantined int

	// Requests is how many identities were handed work since the counters were
	// cleared and Attempts how many times a request actually went on the wire,
	// which is the larger of the two: one query walks as many addresses as it
	// needs. Failed is how many attempts did not come back with an answer, and
	// Share is that against the attempts — against the requests it came out
	// above a hundred per cent.
	Requests int64
	Attempts int64
	Failed   int64
	Share    string

	// Rotations, Quarantines and Revivals are what the failures cost: addresses
	// left, ports set aside, ports taken back.
	Rotations   int64
	Quarantines int64
	Revivals    int64
	// Reopenings counts ports opened again on the address or gateway they
	// already had. It stands apart from Rotations because it is the opposite
	// reading: nothing was wrong with where the port was sending its traffic —
	// the port, or the tunnel behind it, had stopped being there.
	Reopenings int64
	// Rejections counts answers that arrived and were refused as unusable. They
	// are not failures of the pool — the request got through — and a run whose
	// rejections climb while its failures do not is being walled rather than
	// broken.
	Rejections int64

	// Kinds is the breakdown, in a fixed order so two readings a minute apart do
	// not shuffle under the reader's eye.
	Kinds []failureRow

	// Form is where the addresses come from and how the ports on them are
	// reached, which is settled on this screen rather than in the settings.
	//
	// It is the same subject the figures above are about: somebody reading that
	// a quarter of the requests never arrive is somebody about to change the
	// list, and a reading that sent them to another screen to act on it would be
	// a reading nobody acts on.
	Form settingsForm
	// Sources are the places a list of addresses can come from, in the order the
	// page offers them, and Complaints is what was wrong with what was typed.
	Sources    []sourceOption
	Complaints []string

	// OnGateways says the job runs on stored VPN configurations rather than on
	// a list of addresses, which is what decides whether the rest of this is
	// drawn at all.
	OnGateways bool
	// Groups are the configurations the service holds, by subscription, with
	// whatever is already chosen ticked. Missing counts the ones chosen that the
	// service no longer has, and GatewayFault is why the list could not be asked
	// for.
	Groups       []gatewayGroup
	Missing      int
	GatewayFault string
	// GatewayFaultAt is the address that was asked, shown beside the fault so
	// the reader can see at once that it is the one they meant. It is empty when
	// the service answered and simply had nothing to offer, where an address
	// would only be noise.
	GatewayFaultAt string
	// Taken is when the list on the screen came from the service, so the refresh
	// button has something to say for itself. Empty when no list was read.
	Taken string
	// Chosen and Offered are the whole list's tally, which the header carries so
	// a reader who has scrolled away from the groups still knows how many of
	// them they have ticked.
	Chosen  int
	Offered int
}

// failureRow is one kind of failure and how often it happened.
type failureRow struct {
	// Key names the kind in the catalogue rather than in a language.
	Key   string
	Count int64
	Share string
}

// failureKeys names each kind in the phrase catalogue. It is a map rather than
// a field on the kind because what a kind is called is this package's business
// and not the pool's.
var failureKeys = map[blanktrail.Failure]string{
	blanktrail.FailureTransport: "proxies.kind.transport",
	blanktrail.FailurePort:      "proxies.kind.port",
	blanktrail.FailureRelay:     "proxies.kind.relay",
	blanktrail.FailureWall:      "proxies.kind.wall",
	blanktrail.FailureTimeout:   "proxies.kind.timeout",
	blanktrail.FailureOther:     "proxies.kind.other",
}

// proxies draws the reading of the pool and the settings it is a reading of.
func (s *Server) proxies(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	view := s.proxiesOf()
	saved, complaints := s.current()
	view.Form = formShowing(saved)
	view.Complaints = complaints
	// A path chosen in the browser arrives here and fills the box, and nothing
	// more: choosing is not saving. The reader sees what they picked standing
	// where they would have typed it, and it is written down when they press
	// save.
	if chosen := r.URL.Query().Get(whereField); chosen != "" {
		view.Form.Where = chosen
		view.Form.Source = sourceFile
	}
	view.Sources = sourcesOffered(view.Form.Source)
	view.OnGateways = view.Form.Source == sourceGateways
	if view.OnGateways {
		if list, taken, err := s.askForGateways(r.Context(), saved, false); err != nil {
			view.GatewayFault = gatewayFault(err)
			view.GatewayFaultAt = saved.ControlURL
		} else if !list.Available {
			view.GatewayFault = "proxies.gateways.unavailable"
		} else {
			view.Groups, view.Missing = gatewaysOffered(list, saved.Proxy.Gateways)
			for _, g := range view.Groups {
				view.Chosen += g.Chosen
				view.Offered += g.Offered
			}
			view.Taken = taken.Format("15:04")
		}
	}
	view.page = s.frame(r, lang, "proxies.title", proxiesAt)
	if view.Running {
		view.Refresh = proxiesRefresh.Milliseconds()
	}
	s.render(w, r, "proxies.html", view)
}

// saveProxies writes down where the addresses come from and how the ports on
// them are reached.
//
// It lays what was typed here over the settings that are saved rather than over
// an empty form, so the boxes this screen does not show — the connection, the
// key, the identities kept warm — are carried through untouched. Everything
// after that is the settings page's own path: the same complaints, the same
// order of complain, open, then write.
func (s *Server) saveProxies(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	saved, unreadable := s.current()

	form := formShowing(saved)
	form.Source = strings.TrimSpace(r.FormValue(sourceField))
	form.Where = strings.TrimSpace(r.FormValue(whereField))
	form.Refresh = strings.TrimSpace(r.FormValue(refreshField))
	form.Wire = strings.TrimSpace(r.FormValue(wireField))
	form.Ban = strings.TrimSpace(r.FormValue(banField))
	form.PerUpstream = strings.TrimSpace(r.FormValue(perUpField))
	form.Gateways = ticked(r, gatewayField)

	next, faults := form.onto(saved)
	if len(faults) > 0 || s.settingsPath == "" {
		view := s.proxiesOf()
		view.Form = form
		view.Sources = sourcesOffered(form.Source)
		view.Complaints = append(unreadable, faults...)
		if s.settingsPath == "" {
			// Nowhere to write. Saying so beats a page that takes the press and
			// quietly forgets it at the next start.
			view.Complaints = append(view.Complaints, "settings.opened.nothing")
		}
		view.page = s.frame(r, lang, "proxies.title", proxiesAt)
		s.render(w, r, "proxies.html", view)
		return
	}
	if err := s.takeIntoUse(next); err != nil {
		s.log.Error("nothing could be opened with the settings that were just saved", "error", err)
		view := s.proxiesOf()
		view.Form = form
		view.Sources = sourcesOffered(form.Source)
		view.Complaints = []string{"settings.opened.nothing"}
		view.page = s.frame(r, lang, "proxies.title", proxiesAt)
		s.render(w, r, "proxies.html", view)
		return
	}
	if err := settings.Save(s.settingsPath, next); err != nil {
		s.fail(w, r, err)
		return
	}
	// A page rendered into the answer to a form is a page the browser's reload
	// button sends again, and this form changes where every later job runs.
	http.Redirect(w, r, proxiesAt, http.StatusSeeOther)
}

// resetProxies puts the counters back to nought and shows the screen again.
//
// It clears counts and not state: an address resting off a failure goes on
// resting and a port set aside stays set aside, because those are not a record
// of what happened but what is true now — and throwing the pool back into
// addresses it has already found dead would make the next reading worse than
// the one it was meant to replace.
func (s *Server) resetProxies(w http.ResponseWriter, r *http.Request) {
	if s.sup != nil {
		if pool := s.sup.pool().Pool; pool != nil {
			pool.ResetStats()
			s.sup.clearedProxies(time.Now())
		}
	}
	http.Redirect(w, r, proxiesAt, http.StatusSeeOther)
}

// releaseRested takes every resting address back into rotation and shows the
// screen again.
//
// It is for a bench filled by something that was never the addresses' doing —
// the proxy service restarting, a network away for a minute — where every entry
// on it is evidence of one event rather than of a whole list. Waiting each rest
// out would take as long as the rests, and nothing in the program can know that
// it should not.
//
// The counts are left alone. What happened still happened, and a reading that
// forgot it would hide the very event this was pressed because of.
func (s *Server) releaseRested(w http.ResponseWriter, r *http.Request) {
	if s.sup != nil {
		if pool := s.sup.pool().Pool; pool != nil {
			pool.ReleaseRested()
		}
	}
	if s.store != nil {
		// Written down so a restart does not walk back into yesterday's dead
		// addresses, so a release that left them there would last until the next
		// start and no longer.
		if err := s.store.ForgetAllRests(r.Context()); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	http.Redirect(w, r, proxiesAt, http.StatusSeeOther)
}

// proxiesOf reads the pool once and works every figure out of that one reading.
func (s *Server) proxiesOf() proxiesPage {
	var view proxiesPage
	if s.sup == nil {
		return view
	}
	facts := s.sup.pool()
	_, view.Running = s.sup.Running()
	view.Since = s.sup.proxiesClearedAt().Format("2006-01-02 15:04")

	st := facts.Stats
	view.Ports = st.Ports
	view.Warm = st.Warm
	view.Quarantined = st.Quarantined
	view.Requests = st.Requests
	view.Attempts = st.Attempts
	view.Rotations = st.EgressRotations
	view.Quarantines = st.Quarantines
	view.Revivals = st.Revivals
	view.Reopenings = st.Reopenings
	view.Rejections = st.Rejections
	if facts.Pool != nil {
		view.Addresses, view.Resting = facts.Pool.Addresses()
	}

	for _, kind := range blanktrail.Failures {
		view.Failed += st.Failures[kind]
	}
	view.Share = shareOf(view.Failed, view.Attempts)
	for _, kind := range blanktrail.Failures {
		n := st.Failures[kind]
		view.Kinds = append(view.Kinds, failureRow{
			Key:   failureKeys[kind],
			Count: n,
			Share: shareOf(n, view.Failed),
		})
	}
	return view
}

// shareOf is n as a percentage of total, or the mark that stands where a figure
// would stand if there were nothing to work one out from.
//
// A share of nothing is not nought per cent: nought per cent is a measurement
// somebody could act on, and nothing has happened yet.
func shareOf(n, total int64) string {
	if total <= 0 {
		return noFigure
	}
	return fmt.Sprintf("%.0f%%", 100*float64(n)/float64(total))
}
