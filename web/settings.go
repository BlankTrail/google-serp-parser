// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/settings"
	"github.com/blanktrail/google-serp-parser/store"
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
	searchField  = "search_ports"
	pauseField   = "cooldown"
	hotField     = "hot_ports"
	hotKindField = "hot_device"
	sourceField  = "source"
	whereField   = "source_at"
	refreshField = "source_refresh"
	tongueField  = "language"
)

// The two places a list of addresses can come from, as the form names them. The
// empty one is no list at all, which is how a list is turned off: a box left
// blank cannot mean both "unchanged" and "none".
const (
	sourceNone = ""
	sourceFile = "file"
	sourceURL  = "url"
)

// pauseUnit and refreshUnit are what the two duration boxes are read in. A
// person setting a pause between two requests thinks in seconds and a person
// setting how often a list is read again thinks in minutes, and a box holding
// nanoseconds would be filled in wrong once and blamed on the program forever.
const (
	pauseUnit   = time.Second
	refreshUnit = time.Minute
)

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
	Search string
	Pause  string
	// Hot is how many identities this machine keeps open and warm between jobs,
	// and HotDevice is which kind of result page they are opened for.
	Hot       string
	HotDevice string
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
		ControlURL: strings.TrimSpace(r.FormValue(urlField)),
		APIKey:     strings.TrimSpace(r.FormValue(keyField)),
		Search:     strings.TrimSpace(r.FormValue(searchField)),
		Pause:      strings.TrimSpace(r.FormValue(pauseField)),
		Hot:        strings.TrimSpace(r.FormValue(hotField)),
		HotDevice:  strings.TrimSpace(r.FormValue(hotKindField)),
		Source:     strings.TrimSpace(r.FormValue(sourceField)),
		Where:      strings.TrimSpace(r.FormValue(whereField)),
		Refresh:    strings.TrimSpace(r.FormValue(refreshField)),
		Tongue:     strings.TrimSpace(r.FormValue(tongueField)),
	}
}

// formShowing is the form as the settings that are saved fill it in.
//
// The key is not among them, and that is the rule the rest of this file is
// built around: what the page carries reaches the browser's history, the
// operator's screen and anything that photographs either.
func formShowing(saved settings.Settings) settingsForm {
	return settingsForm{
		ControlURL: saved.ControlURL,
		Search:     strconv.Itoa(saved.SearchPorts),
		Pause:      spellUnits(saved.Cooldown, pauseUnit),
		Hot:        strconv.Itoa(saved.HotPorts),
		HotDevice:  saved.HotDevice,
		Source:     saved.Proxy.Kind,
		Where:      saved.Proxy.Location,
		Refresh:    spellUnits(saved.Proxy.Refresh, refreshUnit),
		Tongue:     saved.Language,
	}
}

// spellUnits writes a stored duration back into the box it was typed in.
func spellUnits(d, unit time.Duration) string { return strconv.FormatInt(int64(d/unit), 10) }

// boxes reads the boxes of a form, gathering every complaint rather than
// stopping at the first: a reader with three mistakes should learn all three
// now instead of submitting three times.
type boxes struct{ complaints []string }

// count reads a box holding a number of things, of which there has to be at
// least one.
//
// An empty box is the number that is already saved. A form posted without a
// field must not quietly set it to nought, and nought ports is a server that
// looks configured and runs nothing.
func (b *boxes) count(typed string, saved int, complaint string) int {
	if typed == "" {
		return saved
	}
	n, err := strconv.Atoi(typed)
	if err != nil || n < 1 {
		b.complaints = append(b.complaints, complaint)
		return saved
	}
	return n
}

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
	next.SearchPorts = b.count(f.Search, saved.SearchPorts, "settings.search.count")
	// Nought is an answer here — it is how keeping identities warm is turned off
	// — so this is read as a number of things that may be none of them, and only
	// a negative or a word is a mistake.
	next.HotPorts = b.none(f.Hot, saved.HotPorts, "settings.hot.count")
	next.HotDevice = f.HotDevice
	if next.HotDevice != "" && !blanktrail.KnownDevice(next.HotDevice) {
		b.complaints = append(b.complaints, "settings.hot.kind")
	}
	next.Cooldown = b.span(f.Pause, saved.Cooldown, pauseUnit, "settings.pause.length")

	// The list is switched off by choosing no source, which is why the source is
	// a choice and not a box: a blank box would have to mean both "leave it" and
	// "use none", and one of the two would be wrong every time.
	switch f.Source {
	case sourceNone:
		next.Proxy = settings.ProxySource{}
	case sourceFile, sourceURL:
		next.Proxy = settings.ProxySource{
			Kind:     f.Source,
			Location: f.Where,
			Refresh:  b.span(f.Refresh, saved.Proxy.Refresh, refreshUnit, "settings.refresh.length"),
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
// The order is the whole of it: complain, ask, save, then open. Nothing is
// written until there is nothing left to complain about and the question a
// running job raises has been answered, because settings written before either
// are settings the reader never agreed to.
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
	if err := s.takeIntoUse(next); err != nil {
		// The settings are saved and nothing was opened with them. The reason stays
		// in the log: it is an address and a refusal from something on this machine,
		// and a page quoting either helps the reader not at all.
		s.log.Error("nothing could be opened with the settings that were just saved", "error", err)
		view.Form = formShowing(next)
		view.KeyTail = tailOf(next)
		view.Complaints = []string{"settings.opened.nothing"}
		s.showSettings(w, r, lang, view)
		return
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
		Ports: atLeastOne(trying.SearchPorts),
	})
	view.Findings = true
	for _, f := range report.Findings {
		view.Checked = append(view.Checked, findingView{
			Severity: severityKeys[f.Severity],
			Title:    f.Title,
			Detail:   f.Detail,
			Action:   f.Action,
		})
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

// severityKeys names how serious a finding is in the catalogue, like every
// other phrase on every page. What the finding itself says comes from the check
// and is not this program's to translate.
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
	if s.sup == nil || s.connect == nil {
		return nil
	}
	// Nothing is opened here. What is handed over is the way to open, and the
	// next job to start is what uses it — which is why saving settings no longer
	// asks anybody what to do about the job that is running: it runs on the pool
	// it raised for itself and is not touched.
	return s.sup.Reconnect(func(ctx context.Context, ports, threads int, device string) (*blanktrail.Pool, error) {
		return s.connect(ctx, saved, ports, threads, device)
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
