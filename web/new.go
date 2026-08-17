// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
	"github.com/blanktrail/google-serp-parser/run"
	"github.com/blanktrail/google-serp-parser/store"
)

// actionField is the field the two buttons answer under, and doEstimate and
// doStart are what each of them carries.
//
// They are two values of one field rather than two fields, because a button
// says what it is worth only when it is the button that was pressed, and that
// is how the page tells the two apart without a line of script behind it.
const (
	actionField = "do"
	doEstimate  = "estimate"
	doStart     = "start"
)

// actions carries the field and the two button values onto the page, so the
// markup and the handler cannot drift into two words for one press.
type actions struct {
	Field    string
	Estimate string
	Start    string
}

func buttons() actions {
	return actions{Field: actionField, Estimate: doEstimate, Start: doStart}
}

// jobForm is what the new-job page carries in both directions: filled in by the
// reader on the way in, and handed back with its complaints on the way out.
//
// It holds strings where the form holds strings, so a refusal can show exactly
// what was typed rather than a number that failed to parse and became zero.
type jobForm struct {
	Name string
	// Kind is what the job asks Google for, in the words the history files it
	// under. Empty is a search, so a form posted without the field is the job
	// this program did before there was a choice.
	Kind     string
	Queries  string
	Country  string
	Language string
	SpecName string
	Pages    int
	Threads  int
	Ports    int
}

// blankForm is the form a reader is handed before they have typed anything.
//
// The numbers are the ones the command starts from, so a job set up here and
// the same job set up there cost the same, and neither has to be talked out of
// a default the other does not have.
func blankForm() jobForm {
	return jobForm{Kind: store.KindSearch, Pages: 1, Threads: 2, Ports: 6}
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

func kinds() []jobKind {
	return []jobKind{
		{Value: store.KindSearch, Label: "form.kind.search"},
		{Value: store.KindIndex, Label: "form.kind.index"},
	}
}

// kindKey is the key of what to call a job of this kind, and whether it is a
// kind at all.
//
// A form arriving with something else is refused rather than run as a search.
// The two kinds ask Google different questions and the answers mean different
// things, so quietly picking one would file a run under a question nobody asked.
func kindKey(kind string) (string, bool) {
	if kind == "" {
		return "form.kind.search", true
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

// runKind turns the word the history files a job under into what the runner
// does with it.
//
// This is the one place the two vocabularies meet. The runner holds no words
// the database would accept and the database holds no behaviour, so a kind
// nobody has taught this function is run as a search — which is why nothing
// reaches here without going through kindKey first.
func runKind(kind string) run.Kind {
	if kind == store.KindIndex {
		return run.Index
	}
	return run.Search
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
	if f.depth() < 1 {
		complaints = append(complaints, "form.pages.positive")
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
		Pages:    f.depth(),
		Country:  f.Country,
		Language: f.Language,
		SpecName: f.SpecName,
	}
}

// work is the job the estimate is of: the queries as they would be searched
// for, at the depth they would be taken to.
func (f jobForm) work(queries []string) run.Job {
	j := run.Job{Kind: runKind(f.Kind), Pages: f.depth(), SpecName: f.SpecName}
	for _, text := range queries {
		j.Queries = append(j.Queries,
			google.Query{Text: text, Country: f.Country, Language: f.Language})
	}
	return j
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
		Queries:  r.FormValue("queries"),
		Country:  strings.TrimSpace(r.FormValue("country")),
		Language: strings.TrimSpace(r.FormValue("language")),
		SpecName: strings.TrimSpace(r.FormValue("spec")),
		Pages:    atoi("pages"),
		Threads:  atoi("threads"),
		Ports:    atoi("ports"),
	}
}

// estimateView is the estimate as the page shows it: counts as counts, and the
// two times already written out, because a template cannot round a duration.
type estimateView struct {
	Queries     int
	Pages       int
	Searches    int
	Requests    int
	MaxRequests int
	Ports       int
	Expected    string
	Floor       string
}

// estimateOf works out what the job would cost, and answers with nothing when
// there is no job to cost.
//
// It opens nothing and asks nothing over the network. This is the first thing
// anybody presses, and an answer that had to open ports first would make the
// question nobody can afford to skip the slowest page on the site.
//
// The pace is the one measured on a live run and the pause between two requests
// is the documented one, because both belong to a run that has not started.
func estimateOf(f jobForm, queries []string) *estimateView {
	if len(queries) == 0 {
		return nil
	}
	// The threads are named. They are the lanes the work is shared between, and
	// passing the ports in their place quotes a job that was never asked for.
	est := run.EstimateWith(f.work(queries), f.Ports, f.Threads,
		blanktrail.DefaultCooldown, run.MeasuredPace)
	return &estimateView{
		Queries:     est.Queries,
		Pages:       est.Pages,
		Searches:    est.Searches,
		Requests:    est.Requests,
		MaxRequests: est.MaxRequests,
		Ports:       est.Ports,
		Expected:    est.Expected.Round(time.Second).String(),
		Floor:       est.Floor.Round(time.Second).String(),
	}
}

// newPage is the form, everything wrong with it, and what it would cost.
type newPage struct {
	page
	Form       jobForm
	Complaints []string
	Estimate   *estimateView
	Do         actions
	// Kinds is every kind a job can be, so the choice on the page is the choice
	// the handler takes and not a second list of it.
	Kinds []jobKind
}

// showNew draws the new-job page, filling in the parts of it that are the same
// however the reader got here. Every way onto this page goes through it, so the
// page a refused upload lands on is the page a refused form lands on.
func (s *Server) showNew(w http.ResponseWriter, r *http.Request, lang Lang,
	form jobForm, complaints []string, est *estimateView) {
	s.render(w, r, "new.html", newPage{
		page:       s.frame(r, lang, "new.title", newAt),
		Form:       form,
		Complaints: complaints,
		Estimate:   est,
		Do:         buttons(),
		Kinds:      kinds(),
	})
}

// newJob shows an empty form.
func (s *Server) newJob(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	s.showNew(w, r, lang, blankForm(), nil, nil)
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

	if r.FormValue(actionField) == doStart {
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
	s.showNew(w, r, lang, form, complaints, estimateOf(form, queries))
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
