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
	portsField   = "ports"
	threadsField = "threads"
	pauseField   = "cooldown"
	sourceField  = "source"
	whereField   = "source_at"
	refreshField = "source_refresh"
	tongueField  = "language"
)

// whenField is how the form answers the one question a save asks, and the two
// answers it takes. There is no third value standing for "decide for me": that
// is the whole point of asking.
const (
	whenField = "when"
	whenNow   = "now"
	whenAfter = "after"
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
	APIKey  string
	Ports   string
	Threads string
	Pause   string
	Source  string
	Where   string
	Refresh string
	Tongue  string
	When    string
}

// settingsFormOf reads the posted settings, leaving every box as text so a box
// that will not parse can be complained about in the reader's own language and
// handed back with what they typed still in it.
func settingsFormOf(r *http.Request) settingsForm {
	return settingsForm{
		ControlURL: strings.TrimSpace(r.FormValue(urlField)),
		APIKey:     strings.TrimSpace(r.FormValue(keyField)),
		Ports:      strings.TrimSpace(r.FormValue(portsField)),
		Threads:    strings.TrimSpace(r.FormValue(threadsField)),
		Pause:      strings.TrimSpace(r.FormValue(pauseField)),
		Source:     strings.TrimSpace(r.FormValue(sourceField)),
		Where:      strings.TrimSpace(r.FormValue(whereField)),
		Refresh:    strings.TrimSpace(r.FormValue(refreshField)),
		Tongue:     strings.TrimSpace(r.FormValue(tongueField)),
		When:       strings.TrimSpace(r.FormValue(whenField)),
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
		Ports:      strconv.Itoa(saved.Ports),
		Threads:    strconv.Itoa(saved.Threads),
		Pause:      spellUnits(saved.Cooldown, pauseUnit),
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
	next.Ports = b.count(f.Ports, saved.Ports, "settings.ports.count")
	next.Threads = b.count(f.Threads, saved.Threads, "settings.threads.count")
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

// chosen is what the form says to do about the job that is running, and whether
// it said anything at all.
//
// A form that said nothing is not an answer of "now": deciding here would
// either cut somebody's job off or claim the settings had taken effect while
// the job went on running on the old ones.
func (f settingsForm) chosen() (SwapWhen, bool) {
	switch f.When {
	case whenNow:
		return SwapNow, true
	case whenAfter:
		return SwapAfterThisJob, true
	}
	return SwapNow, false
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
	// Asking is a job in flight and a form that has not said what to do about it.
	Asking bool
	// Sources are the places a list of addresses can come from, in the order the
	// page offers them.
	Sources []sourceOption
	// Tongues are the languages this machine can be set to answer in.
	Tongues []tongueOption
	When    whenAnswers
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

// whenAnswers carries the field and the two answers onto the page, so the
// markup and the handler cannot drift into two words for one press.
type whenAnswers struct {
	Field string
	Now   string
	After string
}

// settingsPage shows what this machine is set up with.
func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	saved, complaints := s.current()
	s.showSettings(w, r, lang, settingsView{
		Form:       formShowing(saved),
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
	when, answered := form.chosen()
	if _, running := s.running(); running && !answered {
		view.Asking = true
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
	if err := s.takeIntoUse(r.Context(), next, when); err != nil {
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
		Ports:   atLeastOne(trying.Threads) * atLeastOne(trying.Ports),
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
	view.Tongues = tonguesOffered(view.Form.Tongue)
	view.When = whenAnswers{Field: whenField, Now: whenNow, After: whenAfter}
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

// running is the job in flight, on a server that has something to run one.
func (s *Server) running() (int64, bool) {
	if s.sup == nil {
		return 0, false
	}
	return s.sup.Running()
}

// takeIntoUse opens what the saved settings describe and puts the jobs after
// this one through it.
//
// A server with nothing to run jobs on, or with no way of opening anything,
// saves the settings and takes them into use when it is next started. That is
// what a history being read on another machine gets, and it is the whole of the
// difference between the two.
func (s *Server) takeIntoUse(ctx context.Context, saved settings.Settings, when SwapWhen) error {
	if s.sup == nil || s.connect == nil {
		return nil
	}
	eng, err := s.connect(ctx, saved)
	if err != nil {
		return err
	}
	return s.sup.Swap(ctx, eng, when)
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
