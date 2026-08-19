// SPDX-License-Identifier: MIT

package web

import (
	"fmt"
	"net/http"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
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
	// Rejections counts answers that arrived and were refused as unusable. They
	// are not failures of the pool — the request got through — and a run whose
	// rejections climb while its failures do not is being walled rather than
	// broken.
	Rejections int64

	// Kinds is the breakdown, in a fixed order so two readings a minute apart do
	// not shuffle under the reader's eye.
	Kinds []failureRow
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

// proxies draws the reading of the pool.
func (s *Server) proxies(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	view := s.proxiesOf()
	view.page = s.frame(r, lang, "proxies.title", proxiesAt)
	if view.Running {
		view.Refresh = proxiesRefresh.Milliseconds()
	}
	s.render(w, r, "proxies.html", view)
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
