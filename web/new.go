// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/run"
	"github.com/blanktrail/google-serp-parser/store"
)

// jobForm is what the new-job page carries in both directions: filled in by the
// reader on the way in, and handed back with its complaints on the way out.
//
// It holds strings where the form holds strings, so a refusal can show exactly
// what was typed rather than a number that failed to parse and became zero.
type jobForm struct {
	Name string
	// Kind is what the job asks Google for, in the words the history files it
	// under. Empty is a parse, so a form posted without the field is the job
	// this program did before there was a choice.
	Kind string
	// Target is the site a position check is about. It is asked for only by that
	// kind and kept whatever the kind, so a reader who chose the wrong one and
	// went back does not type the site again.
	Target string
	// Unique is what the job throws away as a repeat, in the word the history
	// files it under. Empty keeps everything, so a form posted without the field
	// is the job this program did before there was a choice.
	Unique   string
	Queries  string
	Country  string
	Language string
	// Device is which kind of result page this job asks Google for: a desktop or
	// a phone.
	Device  string
	Pages   int
	Threads int
	Ports   int
	// Tries is how many identities one query may be taken to before it is
	// written off, and From says which of the two ways the phrases arrived by.
	Tries int
	From  string
	// Cooldown is how long one identity rests between two requests, in seconds,
	// because that is the unit a person setting it thinks in. Nought is a job
	// that named none, and the pool then works one out from its own size.
	Cooldown int
	// Keep is what each result of this job keeps, as the names the boxes carry,
	// and Chose says the choice was on the form at all.
	//
	// The two are needed because an unticked box sends nothing: without Chose, a
	// form with every box unticked and a request that never carried the choice
	// look identical, and they mean opposite things — the first is a job asked to
	// keep no part of a result, the second is a job that keeps every part.
	Keep  []string
	Chose bool
}

// blankForm is the form a reader is handed before they have typed anything.
//
// The numbers are the ones the command starts from, so a job set up here and
// the same job set up there cost the same, and neither has to be talked out of
// a default the other does not have.
//
// The name is filled in with the moment the form was opened. A job has to be
// named to be found again, and somebody who came to run a list rather than to
// name one now types over a name that already tells two jobs of the same list
// apart. It is the local time, written largest part first so a listing sorts by
// it, and it is a default and not a stamp: whatever is typed over it wins.
// defaultCooldown is the pause the form offers between two requests on one
// identity, in seconds.
//
// Two, which is what this program kept machine-wide before the pause belonged to
// the job: short enough that a pool of any size is not waiting on it, long
// enough that one identity is not asking twice in the same breath.
const defaultCooldown = 2

// defaultTries is what the form offers when nobody has said otherwise. It is
// the run layer's own number, spelled here so the box a reader sees and the
// number a job runs at cannot drift apart.
const defaultTries = 30

// choseField is how a form says the choice of what to keep was on it at all.
//
// An unticked box sends nothing, so without this a form with every box unticked
// and a request that never carried the choice arrive identical — and they mean
// opposite things.
const choseField = "chose"

// defaultCountry and defaultLanguage are what a new job asks Google for.
//
// They are written here rather than left to Google because an unqualified
// search is answered from what Google works out about the identity making it,
// and a pool holds identities in many places: the same list would then come
// back as a different page each run, with nothing on the page saying so.
const (
	defaultCountry  = "us"
	defaultLanguage = "en"
)

func blankForm() jobForm {
	return jobForm{
		Name: time.Now().Format("2006-01-02 15:04"),
		// The ordinary job, and the one somebody who has not read this page yet
		// almost certainly came for: phrases in, everything Google answered with
		// out. The other two ask narrower questions and are chosen deliberately.
		Kind: store.KindParse,
		// Typed in, because that is what somebody opening this page has in hand;
		// a file is chosen by somebody who already has one.
		From: fromBox,
		// The English-language results, asked for plainly. Google answers an
		// unqualified search with whatever it works out about where the request
		// came from, so the same list run twice through two identities comes back
		// as two different pages — and nothing on the page says why. Naming the
		// country and the language makes the answer the same one every time, and
		// it is the answer nearly everybody opening this page came for.
		Country:  defaultCountry,
		Language: defaultLanguage,
		// The desktop, because that is the page most people mean when they say
		// "the results": it is what a search from a computer answers with, and it
		// is what every job this program ran before there was a choice ran on.
		Device:   blanktrail.DeviceDesktop,
		Pages:    1,
		Threads:  2,
		Ports:    6,
		Tries:    defaultTries,
		Cooldown: defaultCooldown,
		// Everything the parser reads, because that is what somebody who has not
		// thought about it means. Turning a part off is a decision about room, and
		// a decision about room is one nobody makes before they have a list.
		Keep: store.EveryField(),
	}
}

// kinds is what a job can be, in the order the form offers them, each with the
// key of what to call it.
//
// The two travel together so the markup cannot offer a choice the handler does
// not take, or file a job under a word the database refuses.
type jobKind struct {
	Value string
	Label string
}

// The parse stands first because it is what the form starts on: the choice a
// reader is offered first is the one they are being told is ordinary.
func kinds() []jobKind {
	return []jobKind{
		{Value: store.KindParse, Label: "form.kind.parse"},
		{Value: store.KindPosition, Label: "form.kind.position"},
		{Value: store.KindIndex, Label: "form.kind.index"},
	}
}

// kindKey is the key of what to call a job of this kind, and whether it is a
// kind at all.
//
// A form arriving with something else is refused rather than run as a parse.
// The three kinds ask Google different questions and the answers mean different
// things, so quietly picking one would file a run under a question nobody asked.
func kindKey(kind string) (string, bool) {
	if kind == "" {
		return "form.kind.parse", true
	}
	for _, k := range kinds() {
		if k.Value == kind {
			return k.Label, true
		}
	}
	// The word itself comes back, for the reason an untranslated key does: it is
	// ugly and it reports itself, which beats a page that quietly calls a job
	// something it is not.
	return kind, false
}

// jobFilter is one way of dropping repeats, with the key of what to call it.
//
// It travels the way a kind does, for the same reason: the markup must not
// offer a choice the handler does not take, or file a job under a word the
// database refuses.
type jobFilter struct {
	Value string
	Label string
}

func filters() []jobFilter {
	return []jobFilter{
		{Value: string(store.UniqueOff), Label: "form.unique.off"},
		{Value: string(store.UniqueURL), Label: "form.unique.url"},
		{Value: string(store.UniqueHost), Label: "form.unique.host"},
	}
}

// filterKey is the key of what to call a way of dropping repeats, and whether
// it is one at all.
//
// Keeping everything is among them and carries the empty word, so a form that
// arrives without the field is a known answer rather than a refusal.
func filterKey(unique string) (string, bool) {
	for _, f := range filters() {
		if f.Value == unique {
			return f.Label, true
		}
	}
	// The word itself comes back, for the reason an untranslated key does: it
	// reports itself rather than letting a page describe a filter that is not
	// there.
	return unique, false
}

// filter is what this job drops as a repeat.
//
// Neither check drops anything, whatever the box says. Their answers are worked
// out from the results filed against each line, and both of those answers turn
// on one result from one site: an index check keeps the address it asked about,
// a position check keeps the site it was looking for. A filter dropping a second
// result from a site already seen would take exactly those away, and the line
// would read back as an address Google does not hold, or a site that ranked
// nowhere — the silent wrong answers both checks exist to avoid. It is settled
// here, as the depth is, so that the job filed in the history and the job that
// runs say one thing.
func (f jobForm) filter() store.UniqueBy {
	if f.Kind == store.KindIndex || f.Kind == store.KindPosition {
		return store.UniqueOff
	}
	return store.UniqueBy(f.Unique)
}

// runKind turns the word the history files a job under into what the runner
// does with it.
//
// This is the one place the two vocabularies meet. The runner holds no words
// the database would accept and the database holds no behaviour, so a kind
// nobody has taught this function is run as a parse — which is why nothing
// reaches here without going through kindKey first.
func runKind(kind string) run.Kind {
	switch kind {
	case store.KindIndex:
		return run.Index
	case store.KindPosition:
		return run.Position
	}
	return run.Parse
}

// depth is how many result pages this job takes per line.
//
// An index check takes one however deep the box is set: presence is settled by
// the first page. It is settled here rather than in the runner so that the job
// filed in the history, the estimate quoted for it and the work actually done
// all name one number.
func (f jobForm) depth() int {
	if f.Kind == store.KindIndex {
		return 1
	}
	return f.Pages
}

// queryOf reads one line of a list and says whether there is a query on it.
//
// This is the one rule this program reads a list by, wherever the list came
// from: the box on the form and a file of a million lines both come through
// here. Two rules would be two answers to "how many queries have I got", and
// the reader would have no way of telling which of them their job ran.
//
// The line is trimmed rather than split on: a box in a browser ends its lines
// the way the web ends them, and a query carrying a stray return is a query
// searched for with one. A blank line is nothing, and a line opening with a
// hash is a note somebody left themselves.
func queryOf(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", false
	}
	return line, true
}

// faults is everything wrong with a job apart from its list.
//
// It stands apart because a list arriving as a file is not there to be looked
// at when these are decided: the boxes reach the server first and the file
// follows them, and a name that is missing has to be found out before a million
// lines are written into a job nobody asked for.
func (f jobForm) faults() []string {
	var complaints []string
	if strings.TrimSpace(f.Name) == "" {
		complaints = append(complaints, "form.name.required")
	}
	if _, known := kindKey(f.Kind); !known {
		complaints = append(complaints, "form.kind.unknown")
	}
	// A filter nobody here knows is refused rather than read as keeping
	// everything. Dropping repeats cannot be undone, and a job run under a rule
	// nobody chose is a run to do again.
	if _, known := filterKey(f.Unique); !known {
		complaints = append(complaints, "form.unique.unknown")
	}
	// A position check with nothing to look for is refused here and would be
	// refused by the history underneath. It is said here because this is the only
	// place that can name the empty box: run, it would answer "not found" about
	// every phrase in the list, and that answer reads exactly like a site that
	// ranks nowhere.
	if f.Kind == store.KindPosition && strings.TrimSpace(f.Target) == "" {
		complaints = append(complaints, "form.target.required")
	}
	if f.depth() < 1 {
		complaints = append(complaints, "form.pages.positive")
	}
	complaints = append(complaints, f.keepFaults()...)
	return complaints
}

// keepFaults is everything wrong with what the job was asked to keep.
//
// The choice is about room, and two things depend on it that a reader thinking
// about room would not connect to it. Both are said here rather than left to be
// discovered afterwards: a job written down under either of them has run before
// anybody can tell.
func (f jobForm) keepFaults() []string {
	keep := store.FieldsOf(f.Keep)
	var complaints []string
	if f.Chose && len(f.Keep) == 0 {
		// Not "everything": that is what a job which never chose means, and this
		// is a reader who unticked every box. A job filed that way writes a row
		// per result holding nothing but its place.
		complaints = append(complaints, "form.keep.none")
	}
	// Dropping repeats needs the thing repeats are told apart by. Without it the
	// filter would keep everything and the job would report nothing dropped,
	// which reads as a list that happened to hold no repeats.
	switch f.filter() {
	case store.UniqueURL:
		if !keep.Keeps(store.FieldURL) {
			complaints = append(complaints, "form.keep.needsurl")
		}
	case store.UniqueHost:
		if !keep.Keeps(store.FieldHost) {
			complaints = append(complaints, "form.keep.needshost")
		}
	}
	return complaints
}

// parse pulls the queries out of the box and lists everything wrong at once.
//
// Every fault is reported together rather than the first one alone: a reader
// with three mistakes should learn all three now, not submit three times.
func (f jobForm) parse() ([]string, []string) {
	complaints := f.faults()

	var queries []string
	for _, line := range strings.Split(f.Queries, "\n") {
		if text, ok := queryOf(line); ok {
			queries = append(queries, text)
		}
	}
	if len(queries) == 0 {
		complaints = append(complaints, "form.queries.required")
	}
	return queries, complaints
}

// spec is the job as the history will file it.
func (f jobForm) spec() store.JobSpec {
	return store.JobSpec{
		Name:     strings.TrimSpace(f.Name),
		Kind:     f.Kind,
		Target:   strings.TrimSpace(f.Target),
		UniqueBy: f.filter(),
		Pages:    f.depth(),
		Country:  f.Country,
		Language: f.Language,
		Device:   f.Device,
		Ports:    f.Ports,
		Threads:  f.Threads,
		Tries:    f.Tries,
		Cooldown: time.Duration(f.Cooldown) * time.Second,
		Fields:   store.FieldsOf(f.Keep),
	}
}

// formOf reads the posted form, leaving numbers that will not parse at zero so
// parse can complain about them in the reader's own language.
func formOf(r *http.Request) jobForm {
	atoi := func(name string) int {
		n, _ := strconv.Atoi(strings.TrimSpace(r.FormValue(name)))
		return n
	}
	return jobForm{
		Name:     strings.TrimSpace(r.FormValue("name")),
		Kind:     strings.TrimSpace(r.FormValue("kind")),
		Target:   strings.TrimSpace(r.FormValue("target")),
		Unique:   strings.TrimSpace(r.FormValue("unique")),
		Queries:  r.FormValue("queries"),
		Country:  strings.TrimSpace(r.FormValue("country")),
		Language: strings.TrimSpace(r.FormValue("language")),
		Device:   strings.TrimSpace(r.FormValue("device")),
		Pages:    atoi("pages"),
		Threads:  atoi("threads"),
		Ports:    atoi("ports"),
		Tries:    atoi("tries"),
		Cooldown: atoi("cooldown"),
		From:     strings.TrimSpace(r.FormValue(fromField)),
		Keep:     r.Form["keep"],
		Chose:    r.FormValue(choseField) != "",
	}
}

// newPage is the form, everything wrong with it, and what it would cost.
type newPage struct {
	page
	Form       jobForm
	Complaints []string
	// Kinds is every kind a job can be, so the choice on the page is the choice
	// the handler takes and not a second list of it.
	Kinds []jobKind
	// Filters is every way of dropping repeats, offered on the same terms.
	Filters []jobFilter
	// Devices is every kind of result page, on the same terms again. The list
	// comes from the package that opens the ports, so a kind offered here is one
	// that can actually be opened.
	Devices []jobDevice
	// Sources is the two ways the phrases can arrive, on the same terms again.
	Sources []jobSource
	// Keeps is every part of a result that can be kept, each with whether this
	// form has it ticked, and Chose is the name of the box that says the choice
	// was on the form.
	Keeps []jobField
	Chose string
}

// jobField is one part of a result, as the form offers it.
type jobField struct {
	Value  string
	Label  string
	Ticked bool
}

// keeps is what a result is made of, in the order a file writes it, each saying
// whether this form has it ticked.
//
// The list is built from the store's own, so the boxes on the page are the
// parts the store knows how to keep rather than a second list of them.
func keeps(f jobForm) []jobField {
	chosen := store.FieldsOf(f.Keep)
	var out []jobField
	for _, name := range store.EveryField() {
		out = append(out, jobField{
			Value: name, Label: "field." + name, Ticked: chosen.Keeps(name),
		})
	}
	return out
}

// jobSource is one way the phrases can arrive.
type jobSource struct {
	Value string
	Label string
}

// sources is where the phrases come from, in the order the form offers them.
//
// Both boxes are drawn whichever is chosen: a box that appeared and disappeared
// would need a script, and this page has none. The switch is what says which of
// the two was meant.
func sources() []jobSource {
	return []jobSource{
		{Value: fromBox, Label: "form.from.box"},
		{Value: fromFile, Label: "form.from.file"},
	}
}

// showNew draws the new-job page, filling in the parts of it that are the same
// however the reader got here. Every way onto this page goes through it, so the
// page a refused upload lands on is the page a refused form lands on.
func (s *Server) showNew(w http.ResponseWriter, r *http.Request, lang Lang,
	form jobForm, complaints []string) {
	s.render(w, r, "new.html", newPage{
		page:       s.frame(r, lang, "new.title", newAt),
		Form:       form,
		Complaints: complaints,
		Kinds:      kinds(),
		Filters:    filters(),
		Devices:    devicesOffered(form.Device),
		Sources:    sources(),
		Keeps:      keeps(form),
		Chose:      choseField,
	})
}

// newJob shows an empty form.
func (s *Server) newJob(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	s.showNew(w, r, lang, blankForm(), nil)
}

// createJob answers the form: it costs the job, or starts it.
//
// The cost is worked out whenever there are queries to cost, whichever button
// was pressed and whatever else is wrong with the form. Somebody who asked what
// ten thousand queries come to has asked one question, and answering it with a
// demand for a name answers a different one.
func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	form := formOf(r)
	queries, complaints := form.parse()

	// Every send of this form is a start. The page used to carry a second button
	// that costed the job without running it, and the costing went with it: the
	// dominant term in it is the time to reach an identity that answers, which
	// was measured at anything from half a minute to nine, so the figure was
	// read as a promise and was not one. What the operator has instead is the
	// numbers the run itself reports.
	{
		switch {
		case s.sup == nil:
			complaints = append(complaints, "form.norunner")
		case !s.sup.canRun():
			// The connection has not been set up yet. The job could be written down
			// and held until it is, but a job held for a setting nobody has made sits
			// in the queue looking started while nothing runs, and the one place that
			// can say why is this page.
			complaints = append(complaints, "form.notsetup")
		}
		if len(complaints) == 0 {
			s.start(w, r, form, queries)
			return
		}
	}
	s.showNew(w, r, lang, form, complaints)
}

// start writes the job down, queues it and sends the browser to its page.
//
// The answer is a redirect and never the job itself. A page rendered into the
// answer to a form is a page the browser's reload button sends again, and the
// second send starts a second job.
func (s *Server) start(w http.ResponseWriter, r *http.Request, form jobForm, queries []string) {
	id, err := s.sup.Enqueue(form.spec(), queries)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/job/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// jobDevice is one kind of result page, with the key of what to call it.
//
// It travels the way a kind does, for the same reason: the markup must not
// offer a choice the handler does not take, or file a job under a word the
// database refuses.
type jobDevice struct {
	Value   string
	Label   string
	Current bool
}

// devicesOffered is every kind of result page, with the one in use already
// chosen. The list comes from the package that opens the ports, so a kind
// offered here is a kind that can actually be opened.
func devicesOffered(current string) []jobDevice {
	if current == "" {
		current = blanktrail.DeviceDesktop
	}
	offered := make([]jobDevice, 0, len(blanktrail.Devices()))
	for _, device := range blanktrail.Devices() {
		offered = append(offered, jobDevice{
			Value:   device,
			Label:   "form.device." + device,
			Current: device == current,
		})
	}
	return offered
}

// deviceKey is the key of what to call a kind of result page, and whether it is
// one at all.
//
// The empty string is a desktop, because that is what every job written before
// there was a choice ran on. Anything else that is not a kind comes back as
// itself, for the reason an untranslated key does: it is ugly and it reports
// itself, which beats a page calling a job something it is not.
func deviceKey(device string) (string, bool) {
	if device == "" {
		return "form.device." + blanktrail.DeviceDesktop, true
	}
	if blanktrail.KnownDevice(device) {
		return "form.device." + device, true
	}
	return device, false
}
