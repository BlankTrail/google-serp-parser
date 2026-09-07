// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// Where the settings are read, written and tried out. The check has an address
// of its own because it is a different thing from a save and must never be
// mistaken for one: it writes nothing.
const (
	settingsAt = "/settings"
	checkAt    = "/settings/check"
)

// The boxes of the settings form, named once so the page and the handler cannot
// drift into two words for one box.
const (
	urlField     = "control_url"
	keyField     = "api_key"
	hotField     = "hot_ports"
	hotKindField = "hot_device"
	wireField    = "port_protocol"
	banField     = "ban_minutes"
	lanField     = "lan_access"
	lanKeyField  = "lan_password"
	perUpField   = "threads_per_upstream"
	renewField   = "renew_minutes"
	gatewayField = "gateway"
	sourceField  = "source"
	whereField   = "source_at"
	refreshField = "source_refresh"
	// The two the profiles brought: which profile a form is about, and what it
	// is called.
	profileField = "profile"
	nameField    = "profile_name"
	tongueField  = "language"
)

// The two places a list of addresses can come from, as the form names them. The
// empty one is no list at all, which is how a list is turned off: a box left
// blank cannot mean both "unchanged" and "none".
const (
	sourceNone     = ""
	sourceFile     = "file"
	sourceURL      = "url"
	sourceGateways = settings.ProxyGateways
)

// refreshUnit is what the one duration box on this page is read in. A person
// setting how often a list is read again thinks in minutes, and a box holding
// nanoseconds would be filled in wrong once and blamed on the program forever.
const refreshUnit = time.Minute

// banUnit is the unit the box for how long an address is banned is labelled in.
const banUnit = time.Minute

// renewUnit is the unit the box for how often a port changes identity is
// labelled in, for the reason the other two are: nobody sets this in seconds.
const renewUnit = time.Minute

// reachedDomains are the hosts a job cannot work without, and what the check
// asks about. They are named one by one rather than by wildcard because a
// restricted licence names them one by one, and the reader has to be told which
// of them is missing.
var reachedDomains = []string{"www.google.com", "google.com"}

// NewSupervisorWithoutAPool is a supervisor for a machine whose connection has
// not been set up yet.
//
// It holds jobs and runs none, and it exists so that a server started without a
// key is still a server the settings below can hand a pool to. The alternative
// is a server with no supervisor at all, where the answer to a saved connection
// is "start me again", which is the one thing this page is for not having to
// say.
func NewSupervisorWithoutAPool(st *store.Store) *Supervisor { return newSupervisor(st, nil) }

// atLeastOne is a count that has to be a count. Nought threads is a pool
// nothing is ever taken through, and it reaches here only from a file somebody
// has edited by hand.
func atLeastOne(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// settingsForm is what the settings page carries in both directions: filled in
// by the reader on the way in, and handed back with its complaints on the way
// out.
//
// It holds strings where the form holds strings, so a refusal shows exactly
// what was typed rather than a number that failed to parse and became nought.
type settingsForm struct {
	ControlURL string
	// APIKey is empty on the way out and usually empty on the way in. The page
	// cannot show the key, so an empty box means the key that is already saved.
	APIKey string
	// Hot is how many identities this machine keeps open and warm between jobs,
	// and HotDevice is which kind of result page they are opened for.
	Hot       string
	HotDevice string
	// Wire is how this program reaches the identities it opens: socks5 or http.
	Wire string
	// Ban is how long an address that failed is left out of the rotation.
	Ban string
	// PerUpstream is how many identities may work through one egress at once.
	PerUpstream string
	// Renew is how often a port is opened again to change its identity, in
	// minutes. Empty box and nought both mean never.
	Renew string
	// Gateways are the stored VPN configurations ticked on the proxy screen.
	Gateways []string
	// LAN says the interface answers the network rather than this machine
	// alone, and LANPassword is what it asks for when it does. The password is
	// empty on the way out, like the key: the page cannot show one.
	LAN         bool
	LANPassword string
	// LANLocked says a password is already saved, so the page can say so
	// without handing it over.
	LANLocked bool
	Source    string
	Where     string
	Refresh   string
	Tongue    string
}

// settingsFormOf reads the posted settings, leaving every box as text so a box
// that will not parse can be complained about in the reader's own language and
// handed back with what they typed still in it.
func settingsFormOf(r *http.Request) settingsForm {
	return settingsForm{
		ControlURL:  strings.TrimSpace(r.FormValue(urlField)),
		APIKey:      strings.TrimSpace(r.FormValue(keyField)),
		Hot:         strings.TrimSpace(r.FormValue(hotField)),
		HotDevice:   strings.TrimSpace(r.FormValue(hotKindField)),
		Wire:        strings.TrimSpace(r.FormValue(wireField)),
		Ban:         strings.TrimSpace(r.FormValue(banField)),
		PerUpstream: strings.TrimSpace(r.FormValue(perUpField)),
		Renew:       strings.TrimSpace(r.FormValue(renewField)),
		Gateways:    ticked(r, gatewayField),
		LAN:         r.FormValue(lanField) != "",
		LANPassword: strings.TrimSpace(r.FormValue(lanKeyField)),
		Source:      strings.TrimSpace(r.FormValue(sourceField)),
		Where:       strings.TrimSpace(r.FormValue(whereField)),
		Refresh:     strings.TrimSpace(r.FormValue(refreshField)),
		Tongue:      strings.TrimSpace(r.FormValue(tongueField)),
	}
}

// formShowing is the form as the settings that are saved fill it in.
//
// The key is not among them, and that is the rule the rest of this file is
// built around: what the page carries reaches the browser's history, the
// operator's screen and anything that photographs either.
func formShowing(saved settings.Settings) settingsForm {
	return settingsForm{
		ControlURL:  saved.ControlURL,
		Hot:         strconv.Itoa(saved.HotPorts),
		HotDevice:   saved.HotDevice,
		Wire:        blanktrail.ProtocolOr(saved.PortProtocol),
		Ban:         spellUnits(saved.Proxy.Ban, banUnit),
		PerUpstream: strconv.Itoa(atLeastOne(saved.ThreadsPerUpstream)),
		Renew:       spellUnits(saved.RenewEvery, renewUnit),
		Gateways:    saved.Proxy.Gateways,
		LAN:         saved.LANAccess,
		LANLocked:   saved.LANPassword != "",
		Source:      saved.Proxy.Kind,
		Where:       saved.Proxy.Location,
		Refresh:     spellUnits(saved.Proxy.Refresh, refreshUnit),
		Tongue:      saved.Language,
	}
}

// keptOr is what the form said, or what was saved when the form said nothing.
// It is how a box that is off the screen keeps its value.
func keptOr(said, saved string) string {
	if strings.TrimSpace(said) != "" {
		return said
	}
	return saved
}

// keptGateways is keptOr for the ticks: a form that carried none keeps the ones
// already saved.
func keptGateways(said, saved []string) []string {
	if len(said) > 0 {
		return said
	}
	return saved
}

// spellUnits writes a stored duration back into the box it was typed in.
func spellUnits(d, unit time.Duration) string { return strconv.FormatInt(int64(d/unit), 10) }

// boxes reads the boxes of a form, gathering every complaint rather than
// stopping at the first: a reader with three mistakes should learn all three
// now instead of submitting three times.
type boxes struct{ complaints []string }

// none reads a box holding a number of things where none of them is an answer.
//
// It is separate from count because the two differ in exactly one place and it
// is the place that matters: nought ports to search on is a machine that looks
// configured and runs nothing, and nought ports kept warm is somebody switching
// that off.
func (b *boxes) none(typed string, saved int, complaint string) int {
	if typed == "" {
		return saved
	}
	n, err := strconv.Atoi(typed)
	if err != nil || n < 0 {
		b.complaints = append(b.complaints, complaint)
		return saved
	}
	return n
}

// span reads a box holding a length of time, in the unit the box is labelled
// with. Nought is an answer here — no pause is a pause of none, and a list read
// once is a list never read again — so only a negative is a mistake.
func (b *boxes) span(typed string, saved, unit time.Duration, complaint string) time.Duration {
	if typed == "" {
		return saved
	}
	n, err := strconv.Atoi(typed)
	if err != nil || n < 0 {
		b.complaints = append(b.complaints, complaint)
		return saved
	}
	return time.Duration(n) * unit
}

// onto lays the form over the settings that are saved and says what is wrong
// with it.
//
// An empty key box keeps the key that is there. The box cannot show the key, so
// somebody who opened the settings to change the port count and pressed save
// would otherwise be left without a connection, and that is the first thing
// anybody will do.
func (f settingsForm) onto(saved settings.Settings) (settings.Settings, []string) {
	var b boxes
	next := saved
	next.ControlURL = f.ControlURL
	if f.APIKey != "" {
		next.APIKey = f.APIKey
	}
	// Nought is an answer here — it is how keeping identities warm is turned off
	// — so this is read as a number of things that may be none of them, and only
	// a negative or a word is a mistake.
	next.HotPorts = b.none(f.Hot, saved.HotPorts, "settings.hot.count")
	next.PortProtocol = blanktrail.ProtocolOr(f.Wire)
	// One is the floor rather than the default alone: nought identities through
	// an egress is a pool that hands out nothing at all.
	next.ThreadsPerUpstream = atLeastOne(b.none(f.PerUpstream, saved.ThreadsPerUpstream, "settings.perupstream.count"))
	// Nought is an answer here too, and the one that ships: it is how "hold this
	// identity for as long as it works" is said.
	next.RenewEvery = b.span(f.Renew, saved.RenewEvery, renewUnit, "settings.renew.length")

	// The password, and the one rule around it: nothing is opened to the network
	// without one. An empty box keeps the password already saved, the same way
	// the key box does — somebody who came here to change the port count and
	// pressed save would otherwise unlock the interface they had shut.
	next.LANAccess = f.LAN
	if f.LANPassword != "" {
		locked, err := settings.LockPassword(f.LANPassword)
		if err != nil {
			b.complaints = append(b.complaints, "settings.lan.password.short")
		} else {
			next.LANPassword = locked
		}
	}
	if next.LANAccess && next.LANPassword == "" {
		b.complaints = append(b.complaints, "settings.lan.needs.password")
		next.LANAccess = false
	}
	next.HotDevice = f.HotDevice
	if next.HotDevice != "" && !blanktrail.KnownDevice(next.HotDevice) {
		b.complaints = append(b.complaints, "settings.hot.kind")
	}

	// The list is switched off by choosing no source, which is why the source is
	// a choice and not a box: a blank box would have to mean both "leave it" and
	// "use none", and one of the two would be wrong every time.
	switch f.Source {
	case sourceNone:
		next.Proxy = settings.ProxySource{}
	case sourceGateways:
		// A set of gateways runs on no location and no interval: what is behind
		// each name lives in the service and is asked for when a job starts.
		//
		// Both are carried through all the same. Dropped, a reader who tried the
		// gateways for an afternoon came back to an empty box and had to find
		// their list's address again — and the box is not even on the screen
		// while the gateways are chosen, so nothing they can see says it is
		// about to be forgotten.
		next.Proxy = settings.ProxySource{
			Kind:     sourceGateways,
			Ban:      b.span(f.Ban, saved.Proxy.Ban, banUnit, "settings.ban.length"),
			Gateways: f.Gateways,
			Location: keptOr(f.Where, saved.Proxy.Location),
			Refresh:  saved.Proxy.Refresh,
		}
	case sourceFile, sourceURL:
		// And the gateways ticked are kept for the same reason, in the other
		// direction: two-and-thirty boxes are not something to tick twice.
		next.Proxy = settings.ProxySource{
			Kind:     f.Source,
			Location: f.Where,
			Refresh:  b.span(f.Refresh, saved.Proxy.Refresh, refreshUnit, "settings.refresh.length"),
			Ban:      b.span(f.Ban, saved.Proxy.Ban, banUnit, "settings.ban.length"),
			Gateways: keptGateways(f.Gateways, saved.Proxy.Gateways),
		}
	default:
		b.complaints = append(b.complaints, "settings.source.unknown")
	}

	switch lang, known := langOf(f.Tongue); {
	case f.Tongue == "":
		// Nobody has said, so every reader is answered in whatever they ask for.
		next.Language = ""
	case known:
		next.Language = string(lang)
	default:
		b.complaints = append(b.complaints, "settings.language.unknown")
	}
	return next, b.complaints
}

// findingView is one thing the check found, as the page draws it.
//
// Every part of it is drawn. What is wrong and what to do about it is the half
// that helps, and a check reduced to whether it passed throws exactly that half
// away.
type findingView struct {
	// Severity is the key of how serious this is, not the word: every phrase on
	// every page goes through the catalogue.
	Severity string
	Title    string
	Detail   string
	Action   string
}

// settingsView is the settings page: the boxes, everything wrong with them,
// what the check found, and the question a running job raises.
type settingsView struct {
	page
	Form settingsForm
	// KeyTail names the key that is saved without handing it out, and is empty
	// when there is none.
	KeyTail    string
	Complaints []string
	// Checked is what the check found, and Findings whether it was run at all: a
	// check that found nothing to report is not a check nobody ran.
	Checked  []findingView
	Findings bool
	// Sources are the places a list of addresses can come from, in the order the
	// page offers them.
	Sources []sourceOption
	// Devices is every kind of result page the standing identities can be opened
	// for, with the one in use already chosen.
	Devices []jobDevice
	// Tongues are the languages this machine can be set to answer in.
	Tongues []tongueOption
}

// sourceOption is one place a list of addresses can come from.
type sourceOption struct {
	Value   string
	Key     string
	Current bool
}

// tongueOption is one language this machine can be set to answer in, offered by
// the name that language calls itself.
type tongueOption struct {
	Value   string
	Name    string
	Key     string
	Current bool
}

// settingsPage shows what this machine is set up with.
func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	saved, complaints := s.current()
	form := formShowing(saved)
	// A path chosen in the browser arrives here and fills the box, and nothing
	// more: choosing is not saving. The reader sees what they picked standing
	// where they would have typed it, and the settings change when they press
	// save — which is the same rule the key box already follows.
	if chosen := r.URL.Query().Get(whereField); chosen != "" {
		form.Where = chosen
		form.Source = sourceFile
	}
	s.showSettings(w, r, lang, settingsView{
		Form:       form,
		KeyTail:    tailOf(saved),
		Complaints: complaints,
	})
}

// saveSettings writes the settings down and takes them into use.
//
// The order is the whole of it: complain, open, then save. Nothing is written
// until there is nothing left to complain about, because settings written
// before that are settings the reader never agreed to — and nothing is written
// until the identities they ask for are actually open, because a file saying
// ten on a machine keeping none is a file nobody can trust and a screen nobody
// can read.
func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	form := settingsFormOf(r)
	saved, unreadable := s.current()
	next, faults := form.onto(saved)

	view := settingsView{Form: form, KeyTail: tailOf(saved), Complaints: append(unreadable, faults...)}
	// Only what is wrong with the form stops the save. A file that could not be
	// read is said out loud and saved over: the reader has no other way of
	// repairing it, and refusing here would leave them with a page that reports
	// the damage and cannot mend it.
	if len(faults) > 0 {
		s.showSettings(w, r, lang, view)
		return
	}
	if err := s.takeIntoUse(next); err != nil {
		// Nothing was opened, so nothing is written down: the file and the machine
		// say the same thing, which is the state the reader can act on. The reason
		// stays in the log — it is an address and a refusal from something on this
		// machine, and a page quoting either helps the reader not at all.
		s.log.Error("nothing could be opened with the settings that were just saved", "error", err)
		view.Form = form
		view.KeyTail = tailOf(saved)
		view.Complaints = []string{"settings.opened.nothing"}
		s.showSettings(w, r, lang, view)
		return
	}
	if err := settings.Save(s.settingsPath, next); err != nil {
		s.fail(w, r, err)
		return
	}
	// The reader chose this language outright, in this browser, so it is written
	// down here as well as in the file. Without it the choice they made last week
	// with the switcher would go on overruling the one they just made.
	if lang, known := langOf(next.Language); known {
		writeLang(w, lang)
	}
	// A page rendered into the answer to a form is a page the browser's reload
	// button sends again, and this form changes where every later job runs.
	http.Redirect(w, r, settingsAt, http.StatusSeeOther)
}

// checkConnection tries what is in the boxes and writes nothing down.
//
// It tries what is in the boxes rather than what is saved, because a wrong
// address that had been saved first would take the working one away before
// anybody found out it was wrong. The key is the one exception, for the reason
// it is everywhere on this page: an empty box means the key that is saved, so a
// reader changing an address alone can still check it.
func (s *Server) checkConnection(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	form := settingsFormOf(r)
	saved, complaints := s.current()
	trying, faults := form.onto(saved)
	complaints = append(complaints, faults...)

	view := settingsView{Form: form, KeyTail: tailOf(saved), Complaints: complaints}
	client, err := blanktrail.NewClient(trying.ControlURL, trying.APIKey)
	if err != nil {
		view.Complaints = append(view.Complaints, "settings.address.unusable")
		s.showSettings(w, r, lang, view)
		return
	}
	report := blanktrail.Preflight(r.Context(), client, blanktrail.PreflightInput{
		Domains: reachedDomains,
		// The check asks about the pool this machine keeps standing, because that
		// is the only one it can size without a job in front of it. A job's own
		// pool is asked for when the job starts, and what the check reports about
		// the licence and the domains holds for both.
		Ports: atLeastOne(trying.HotPorts),
	})
	view.Findings = true
	for _, f := range report.Findings {
		view.Checked = append(view.Checked, findingSaid(lang, f))
	}
	// A check that found nothing has to say what that means. The check reports a
	// service that will not answer, a key that was refused and a licence that is
	// not active, so nothing to report is the connection working — and a page
	// that says only "nothing to report" reads as a button that did nothing.
	if len(view.Checked) == 0 {
		view.Checked = append(view.Checked, findingView{
			Severity: severityKeys[blanktrail.SeverityOK],
			Title:    lang.T("settings.check.good"),
			Detail:   lang.T("settings.check.good.detail"),
		})
	}
	s.showSettings(w, r, lang, view)
}

// findingSaid is one finding as this page says it: what this program wrote in
// the reader's own language, and what the service said in the words it said it.
//
// The split is the whole of the rule. The name of the fault and what to do about
// it were written here and belong in the catalogue like every other phrase; the
// detail is usually the service quoting itself — a missing binary, an address
// that would not answer, a licence endpoint's own sentence — and translating
// that would mean keeping a table of another program's error strings, which
// would be out of date the first time it changed one.
//
// A finding the catalogue does not know is shown as it came. That is what makes
// this safe to leave alone: a check that grows a new finding says it in English
// on a Russian page rather than saying nothing at all.
func findingSaid(lang Lang, f blanktrail.Finding) findingView {
	said := findingView{
		Severity: severityKeys[f.Severity],
		Title:    f.Title,
		Detail:   f.Detail,
		Action:   f.Action,
	}
	if f.Key == "" {
		return said
	}
	if title := lang.T("check." + f.Key); title != "check."+f.Key {
		said.Title = title
	}
	if f.Action != "" {
		if action := lang.T("check." + f.Key + ".do"); action != "check."+f.Key+".do" {
			said.Action = action
		}
	}
	return said
}

// severityKeys names how serious a finding is in the catalogue, like every
// other phrase on every page.
var severityKeys = map[blanktrail.Severity]string{
	blanktrail.SeverityOK:   "settings.finding.ok",
	blanktrail.SeverityWarn: "settings.finding.warn",
	blanktrail.SeverityFail: "settings.finding.fail",
}

// showSettings draws the page, filling in the parts of it that are the same
// however the reader got here.
func (s *Server) showSettings(w http.ResponseWriter, r *http.Request, lang Lang, view settingsView) {
	view.page = s.frame(r, lang, "settings.title", settingsAt)
	view.Sources = sourcesOffered(view.Form.Source)
	view.Devices = devicesOffered(view.Form.HotDevice)
	view.Tongues = tonguesOffered(view.Form.Tongue)
	s.render(w, r, "settings.html", view)
}

// sourcesOffered is where a list of addresses may come from, with the one in
// use already chosen.
func sourcesOffered(current string) []sourceOption {
	offered := []sourceOption{
		{Value: sourceNone, Key: "settings.source.none"},
		{Value: sourceFile, Key: "settings.source.file"},
		{Value: sourceURL, Key: "settings.source.url"},
		{Value: sourceGateways, Key: "settings.source.gateways"},
	}
	for i := range offered {
		offered[i].Current = offered[i].Value == current
	}
	return offered
}

// tonguesOffered is every language this machine can be set to answer in, plus
// leaving it to each reader.
//
// Each language is offered by the name it calls itself, exactly as the switcher
// offers it: whoever is choosing may not be able to read the page they are
// choosing on.
func tonguesOffered(current string) []tongueOption {
	offered := []tongueOption{{Key: "settings.language.reader", Current: current == ""}}
	for _, l := range Languages() {
		offered = append(offered, tongueOption{
			Value: string(l), Name: l.Name(), Current: string(l) == current,
		})
	}
	return offered
}

// tailOf is what may be shown of the key that is saved: enough to tell one key
// from another and not enough to be one.
func tailOf(saved settings.Settings) string { return saved.Redacted().APIKey }

// current is the settings as they stand, and what to say when they cannot be
// read.
//
// A file that is there and damaged answers with the defaults and says so, so
// that the page which can repair it is the page that reports it. Refusing to
// draw the settings would leave the reader with no way of writing settings the
// program would accept.
func (s *Server) current() (settings.Settings, []string) {
	saved, err := settings.Load(s.settingsPath)
	if err != nil {
		s.log.Error("the settings could not be read", "error", err)
		return settings.Defaults(), []string{"settings.unreadable"}
	}
	return saved, nil
}

// takeIntoUse opens what the saved settings describe and puts the jobs after
// this one through it.
//
// A server with nothing to run jobs on, or with no way of opening anything,
// saves the settings and takes them into use when it is next started. That is
// what a history being read on another machine gets, and it is the whole of the
// difference between the two.
func (s *Server) takeIntoUse(saved settings.Settings) error {
	// The identities kept warm are brought to what was just saved, here and now
	// rather than at the next start. A number typed into a box that does nothing
	// until somebody restarts the program is a box that lies, and this one is
	// read by an operator watching to see whether the ports appear.
	//
	// It happens before the queue is told anything: a job starting in this moment
	// finds the set it was promised rather than the one before it.
	if s.standing != nil {
		ctx, cancel := context.WithTimeout(context.Background(), standingGrace)
		defer cancel()
		if err := s.standing(ctx, saved); err != nil {
			return err
		}
	}
	if s.sup == nil || s.connect == nil {
		return nil
	}
	// Nothing is opened here. What is handed over is the way to open, and the
	// next job to start is what uses it — which is why saving settings no longer
	// asks anybody what to do about the job that is running: it runs on the pool
	// it raised for itself and is not touched.
	return s.sup.Reconnect(func(ctx context.Context, prof store.Profile, ports, threads int,
		device string, cooldown time.Duration, wholePool bool) (*blanktrail.Pool, error) {
		return s.connect(ctx, saved, prof, ports, threads, device, cooldown, wholePool)
	})
}

// tongue is the language this machine was set up to answer in, and empty when
// nobody has said.
//
// It is read from the file where it is needed rather than remembered, because a
// remembered copy has to be kept in step with the file it came from and there
// is nothing to gain by it: the file is a few hundred bytes on this machine and
// every page this server draws already reads a database.
//
// A file that cannot be read is no language rather than a failure. Whoever
// opens the settings is told about it there, on the page that can repair it,
// and a page refusing to draw itself over the language it is drawn in would be
// a worse answer than English.
func (s *Server) tongue() Lang {
	if s.settingsPath == "" {
		return ""
	}
	saved, err := settings.Load(s.settingsPath)
	if err != nil {
		return ""
	}
	lang, _ := langOf(saved.Language)
	return lang
}

// standingGrace bounds bringing the warm identities to a new number.
//
// It is generous because opening a hundred ports through a service that is
// thinking about it takes what it takes, and it exists at all because the
// reader is holding a page open waiting for the answer: a save that never comes
// back is worse than one that says it could not.
const standingGrace = 2 * time.Minute

// ticked is every value of a box that may be ticked more than once.
//
// The form has to have been parsed for this to see anything, which the handlers
// do by reading a value first; a box read this way and never any other would
// come back empty on a form nobody had touched.
func ticked(r *http.Request, name string) []string {
	if r.Form == nil {
		_ = r.ParseForm()
	}
	return append([]string(nil), r.Form[name]...)
}
