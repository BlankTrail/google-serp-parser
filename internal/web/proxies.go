// SPDX-License-Identifier: MIT

package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// statsField is how the address asks for one profile's counters rather than its
// boxes. It carries no value: it is a question, and the profile beside it is
// what the question is about.
const statsField = "stats"

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
	// Refused are the gateways that would not carry a port at all, with what the
	// service said about each. Nothing else on this screen shows them: a gateway
	// whose tunnel will not start is stepped over, the job runs on what is left,
	// and the only sign is a pool smaller than was asked for.
	Refused []blanktrail.EgressRefusal
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

	// Editing says the boxes of one profile are open, and Reading says one
	// profile's counters are.
	//
	// Neither is drawn until it is asked for. The screen is a list of profiles:
	// that is what somebody opening it came to see, and a form and two dozen
	// figures under it were a page to scroll past to reach the one thing on it
	// that is always wanted. Both close the moment they are done with — a form
	// that stays open after a save is a form the reader has to dismiss, and one
	// left standing beside the list is a second answer to "which profile is
	// this screen about".
	Editing bool
	Reading bool
	// ReadingOf is the name of the profile whose counters are being read, and
	// Elsewhere the name of the profile the pool actually stands on when that is
	// a different one. Nothing says no pool has been raised at all.
	//
	// One pool runs at a time and it runs on one profile, so the counters belong
	// to that profile and to no other. Drawing them under a profile they are not
	// about would be this screen reporting one list's failures as another's,
	// which is worse than reporting nothing.
	ReadingOf string
	Elsewhere string
	Nothing   bool
	// OnProfile is the profile the pool being read stands on, and OnName its
	// name. They are read off the pool rather than guessed from the address: the
	// screen can be opened on any profile, and only one of them is the one
	// anything is running through.
	OnProfile int64
	OnName    string

	// Form is where the addresses come from and how the ports on them are
	// reached, which is settled on this screen rather than in the settings.
	//
	// It is the same subject the figures above are about: somebody reading that
	// a quarter of the requests never arrive is somebody about to change the
	// list, and a reading that sent them to another screen to act on it would be
	// a reading nobody acts on.
	Form profileForm
	// Profiles are the named sets of exits this machine has, the default one
	// first, and they stand above everything else on the screen. Which addresses
	// the work goes out through is the first thing to set up and the first thing
	// to check; a page that opened on the counters was answering a question
	// nobody had asked yet, and left the impression that the exits were set up
	// somewhere else.
	Profiles []profileRow
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
// profileRow is one profile on the list above the form.
type profileRow struct {
	ID   int64
	Name string
	// Source is the key naming where its addresses come from, and Where is the
	// path or address behind it — empty for the gateways, which have neither.
	Source string
	Where  string
	// Gateways is how many configurations are ticked, for the profile that runs
	// on them.
	Gateways int
	// Default marks the one the warm identities are raised on and a job that
	// named none runs through; Editing marks the one the form below is showing.
	Default bool
	Editing bool
}

// profileRows are the profiles as the list draws them.
func profileRows(all []store.Profile, editing int64) []profileRow {
	rows := make([]profileRow, 0, len(all))
	for _, p := range all {
		rows = append(rows, profileRow{
			ID: p.ID, Name: p.Name,
			Source:   sourceKey(p.Kind),
			Where:    p.Location,
			Gateways: len(p.Gateways),
			Default:  p.Default,
			Editing:  p.ID == editing,
		})
	}
	return rows
}

// sourceKey names a kind of source in the reader's own language.
func sourceKey(kind string) string {
	switch kind {
	case sourceFile:
		return "settings.source.file"
	case sourceURL:
		return "settings.source.url"
	case sourceGateways:
		return "settings.source.gateways"
	default:
		return "settings.source.none"
	}
}

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
	view.Complaints = complaints

	profiles, editing, err := s.profileShown(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// What the address asked for. A profile named with nothing else is its
	// boxes; a profile named with the counters asked for is its reading; an
	// address naming no profile at all is the list and nothing else.
	asked := strings.TrimSpace(r.URL.Query().Get(profileField))
	view.Reading = asked != "" && r.URL.Query().Has(statsField)
	view.Editing = asked != "" && !view.Reading
	if view.Reading {
		for _, p := range profiles {
			if p.ID == view.OnProfile {
				view.OnName = p.Name
			}
		}
		view.readingOf(editing)
		view.page = s.frame(r, lang, "proxies.title", proxiesAt)
		if view.Running {
			view.Refresh = proxiesRefresh.Milliseconds()
		}
		view.Profiles = profileRows(profiles, editing.ID)
		s.render(w, r, "proxies.html", view)
		return
	}
	// Nothing is marked as being edited while nothing is: the mark is what hides
	// a row's own press to open it, and a list with one row unopenable reads as
	// a row that cannot be edited at all.
	view.Profiles = profileRows(profiles, editingOr(editing.ID, view.Editing))
	if !view.Editing {
		// The list alone. Nothing below it is drawn, so nothing below it is
		// asked for either — no gateways fetched from the service to fill a form
		// nobody opened.
		view.page = s.frame(r, lang, "proxies.title", proxiesAt)
		s.render(w, r, "proxies.html", view)
		return
	}
	view.Form = profileShowing(editing)
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
			view.Groups, view.Missing = gatewaysOffered(list, view.Form.Gateways)
			for _, g := range view.Groups {
				view.Chosen += g.Chosen
				view.Offered += g.Offered
			}
			view.Taken = taken.Format("15:04")
		}
	}
	view.page = s.frame(r, lang, "proxies.title", proxiesAt)
	s.render(w, r, "proxies.html", view)
}

// editingOr is the profile whose row is marked as open, and nought where the
// screen is only listing them.
func editingOr(id int64, editing bool) int64 {
	if editing {
		return id
	}
	return 0
}

// readingOf settles whose counters these are, and says so rather than drawing
// them under the wrong name.
func (v *proxiesPage) readingOf(profile store.Profile) {
	v.ReadingOf = profile.Name
	switch {
	case v.OnProfile == 0:
		// Nothing has been raised since this server started. There is no reading
		// to show and no profile to hang one on.
		v.Nothing = true
	case v.OnProfile != profile.ID:
		v.Elsewhere = v.OnName
	}
}

// saveProxies writes one profile down: the one the form names, or a new one
// when it names none.
//
// One button and one handler for making and editing, because the difference
// between the two is a number in the form rather than a different page. What
// the screen does not show for the source in hand — the address while the
// gateways are chosen, the ticks while a list is — is carried through rather
// than emptied; see profileForm.onto.
func (s *Server) saveProxies(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	form := profileFrom(r)
	form.Gateways = ticked(r, gatewayField)

	was := store.Profile{}
	if form.ID != 0 {
		p, err := s.store.Profile(r.Context(), form.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		was = p
	}
	next, faults := form.onto(was)
	if len(faults) > 0 {
		s.showProxies(w, r, lang, form, faults, true)
		return
	}

	id := form.ID
	var err error
	if form.ID == 0 {
		id, err = s.store.CreateProfile(r.Context(), next)
	} else {
		err = s.store.SaveProfile(r.Context(), next)
	}
	if errors.Is(err, store.ErrProfileName) {
		s.showProxies(w, r, lang, form, []string{"proxies.profile.name.taken"}, true)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Back to the list, with the form gone. It has done what it was opened for,
	// and a form left standing after a save is one the reader has to dismiss —
	// and one that answers "which profile is this screen about" a second time.
	//
	// A page rendered into the answer to a form is also a page the browser's
	// reload button sends again, and this form decides where later jobs run.
	_ = id
	http.Redirect(w, r, proxiesAt, http.StatusSeeOther)
}

// makeProfileDefault moves the mark: which profile the identities kept warm are
// raised on, which the API's own search goes through, and which a job that named
// none runs on.
func (s *Server) makeProfileDefault(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue(profileField)), 10, 64)
	if err := s.store.SetDefaultProfile(r.Context(), id); err != nil {
		s.log.Error("a profile could not be made the default", "profile", id, "error", err)
	}
	http.Redirect(w, r, proxiesAt, http.StatusSeeOther)
}

// dropProfile removes one.
//
// The jobs that named it keep the number and fall back to the default when they
// run: rewriting them would be this program deciding which exits somebody's job
// should use, which is the decision the profile was for. The last profile stays
// whatever is pressed, and the screen says why.
func (s *Server) dropProfile(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	id, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue(profileField)), 10, 64)
	if err := s.store.DeleteProfile(r.Context(), id); errors.Is(err, store.ErrLastProfile) {
		s.showProxies(w, r, lang, profileForm{}, []string{"proxies.profile.last"}, false)
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, proxiesAt, http.StatusSeeOther)
}

// showProxies draws the screen again with what was typed still in the boxes and
// the complaints above them.
// The boxes are kept open where the reader was filling them in and closed where
// they were not: a refusal to save is answered with what was typed still there,
// and a refusal to delete is answered with the list, because nothing was being
// typed.
func (s *Server) showProxies(w http.ResponseWriter, r *http.Request, lang Lang,
	form profileForm, complaints []string, editing bool) {
	view := s.proxiesOf()
	view.Editing = editing
	view.Form = form
	view.Sources = sourcesOffered(form.Source)
	view.OnGateways = form.Source == sourceGateways
	view.Complaints = complaints
	if all, err := s.store.Profiles(r.Context()); err == nil {
		view.Profiles = profileRows(all, form.ID)
	}
	view.page = s.frame(r, lang, "proxies.title", proxiesAt)
	s.render(w, r, "proxies.html", view)
}

// profileShown is every profile and the one the form is about: the one the
// address names, or the default when it names none or names one that is gone.
func (s *Server) profileShown(r *http.Request) ([]store.Profile, store.Profile, error) {
	all, err := s.store.Profiles(r.Context())
	if err != nil {
		return nil, store.Profile{}, err
	}
	// "new" rather than a number is the empty form: a profile being made has no
	// id to name it by, and nought would be indistinguishable from the address
	// having said nothing at all.
	if strings.TrimSpace(r.URL.Query().Get(profileField)) == "new" {
		return all, store.Profile{}, nil
	}
	want, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get(profileField)), 10, 64)
	for _, p := range all {
		if p.ID == want {
			return all, p, nil
		}
	}
	for _, p := range all {
		if p.Default {
			return all, p, nil
		}
	}
	if len(all) > 0 {
		return all, all[0], nil
	}
	return all, store.Profile{}, nil
}

// profileAt is the screen showing one profile in its boxes.
func profileAt(id int64) string {
	if id == 0 {
		return proxiesAt
	}
	return proxiesAt + "?" + profileField + "=" + strconv.FormatInt(id, 10)
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

	view.OnProfile = facts.Profile

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
		view.Refused = facts.Pool.Refused()
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
