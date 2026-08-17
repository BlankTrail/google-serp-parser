// SPDX-License-Identifier: MIT

package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/export"
	"github.com/blanktrail/google-serp-parser/store"
)

// rowsShown is how much of a job's results the page draws: the last of them,
// newest first.
//
// A job of ten thousand queries holds a million rows. Nobody reads a million
// rows in a browser, no browser lays them out quickly, and the export beside
// them hands over every one. What the page is for is the other question — is
// this still working, and what is it bringing back — and fifty answers that as
// well as a million, on a screen somebody can actually see the bottom of.
const rowsShown = 50

// refreshEvery is how often this page asks to be drawn again while its job is
// moving.
//
// What comes back is the whole screen: the counts, the speed, the sample of
// results and the buttons, all drawn by the server in one pass. It used to be
// four numbers written in place by the browser, which was cheaper and could
// never show a result arriving — and the sample of results is the part somebody
// watching actually reads.
//
// Three seconds rather than two, because the answer is now a page rather than
// four numbers, and because a query takes seconds: asking faster than the work
// happens only tightens a loop around nothing.
const refreshEvery = 3 * time.Second

// jobPath is where a job's own page lives. The list links to it and every
// button sends the reader back to it, so the address is written once.
func jobPath(id int64) string { return "/job/" + strconv.FormatInt(id, 10) }

// jobSetup is a job as it was set up: everything about it that will not change
// while it runs.
//
// The counts are deliberately absent. They live in progressJSON, which is both
// what the page renders and what its script polls, so a number about this job
// has one place to come from.
type jobSetup struct {
	Name    string
	Started time.Time
	// Kind is the key of what to call what this job asks Google, never the word
	// itself: every phrase on every page goes through the catalogue.
	Kind string
	// Target is the site a position check is about, and it is the reader's own
	// word rather than a key: it is an address they typed. It is empty under the
	// other kinds, and the page shows the line only when there is one.
	Target string
	// Filter is the key of what to call what this job drops as a repeat. It is
	// shown among the settings and not among the counts, because it is a thing
	// the job was set up with and cannot be changed now.
	Filter   string
	Pages    int
	Country  string
	Language string
	// Device is which kind of result page this job asked Google for, as the key
	// of what to call it: every phrase on every page goes through the catalogue.
	Device string
	// Ports, Threads and Tries are the pool this job runs on, and the three
	// things about it that can still be changed. Everything above them is what
	// the job is: the depth, the country and the filter are settled by the work
	// already done under them, and changing one afterwards would leave a job
	// whose results were gathered under two rules with nothing saying which.
	//
	// Nought in any of them is a job that named none, and the page shows it as
	// what the run will use rather than as a nought nobody typed.
	Ports   int
	Threads int
	Tries   int
	// KeptAds and KeptRelated say whether this job captured what the page
	// carried besides its results. The extra downloads are offered only where
	// there is something to download: a link to an empty file reads as a page
	// that carried no advertising, which is a different thing.
	KeptAds     bool
	KeptRelated bool
}

// jobPage is one job: how it was set up, how far it has got, what it has
// captured, and what may be pressed.
type jobPage struct {
	page
	Job      jobSetup
	Progress progressJSON
	// State is the key of what to call the job's state, not the word itself.
	State string
	// Reshaped is the key of what to say about a change that has just been made,
	// and empty when the reader did not arrive from one.
	Reshaped string
	// Rows is what a parse job captured, Standings is where a position check
	// found its site, and Verdicts is what an index check established. A job is
	// one kind, so exactly one of the three is ever filled, and the page draws
	// whichever it was handed.
	Rows      []store.Row
	Standings []store.Standing
	Verdicts  []store.Verdict
	// IsIndex and IsPosition say which of the three the page is drawing. They are
	// not read off the lists themselves: a check that has reached nothing yet
	// holds no answers, and drawing it as a parse would tell the reader their
	// list captured no results.
	IsIndex    bool
	IsPosition bool
	// Filtering says the job drops repeats, and it is what puts the count of
	// dropped results on the screen. A job that keeps everything is not given a
	// figure reading nought: a number on a screen is a thing to wonder about,
	// and there is nothing here to wonder about.
	Filtering bool
	// Sampled says the lists below are the last of what the job gathered rather
	// than all of it, and Shown is how many that is. A page cannot draw ten
	// million results and should not try: what somebody watching wants is proof
	// that results are still arriving and a look at what they are. The whole lot
	// is what the export is for.
	Sampled bool
	Shown   int
	// Speed is how fast the job is settling queries now, or the mark when too
	// little has settled to measure. It is on this page as well as the one before
	// it because this is the page somebody opens when they want to know whether
	// to leave the job alone.
	Speed     string
	Formats   []string
	CanStop   bool
	CanResume bool
}

// job draws one job and everything a reader can do with it.
//
// Every number on the page is rendered here. The script that follows a running
// job replaces those numbers in place and adds nothing that was not already
// there, so a reader with no script sees the job as it stood when the page was
// drawn rather than a page of empty boxes.
func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	sum, ok := s.jobAsked(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	// One kind is read and never two. A job of a million addresses holds no
	// captured page to draw, and walking its results to find that out is the
	// million-row read this page is written not to do.
	var rows []store.Row
	var standings []store.Standing
	var verdicts []store.Verdict
	var err error
	switch sum.Kind {
	case store.KindIndex:
		verdicts, err = s.store.LatestVerdicts(r.Context(), sum.ID, rowsShown)
	case store.KindPosition:
		standings, err = s.store.LatestStandings(r.Context(), sum.ID, rowsShown)
	default:
		rows, err = s.store.LatestRows(r.Context(), sum.ID, rowsShown)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pace, err := s.store.Pace(r.Context(), sum.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	at := s.progress(sum)
	// A kind the catalogue has no word for is not drawn as a search. It is a job
	// nothing here can describe, and naming it wrongly is worse than the key.
	kind, _ := kindKey(sum.Kind)
	// A filter the catalogue has no word for is named by its own word, for the
	// reason a kind is: a page that called it "keep everything" would describe a
	// run that dropped results as one that dropped none.
	filter, _ := filterKey(string(sum.UniqueBy))
	// A kind of result page the catalogue has no word for is named by its own
	// word, for the reason a kind of job is.
	device, _ := deviceKey(sum.Device)

	frame := s.frame(r, lang, "job.title", jobsAt)
	// Asked for again only while the job can answer differently. A job nobody is
	// running reads the same in the morning.
	if at.Watch {
		frame.Refresh = refreshEvery.Milliseconds()
	}
	s.render(w, r, "job.html", jobPage{
		page: frame,
		Job: jobSetup{
			Name:        sum.Name,
			Started:     sum.CreatedAt,
			Kind:        kind,
			Target:      sum.Target,
			Filter:      filter,
			Pages:       sum.Pages,
			Country:     sum.Country,
			Language:    sum.Language,
			Device:      device,
			Ports:       sum.Ports,
			Threads:     sum.Threads,
			Tries:       sum.Tries,
			KeptAds:     sum.Kind == store.KindParse && sum.Fields.Keeps(store.FieldAds),
			KeptRelated: sum.Kind == store.KindParse && sum.Fields.Keeps(store.FieldRelated),
		},
		Progress:   at,
		State:      stateOf(at, sum.PlanReady),
		Reshaped:   reshapedSaid(r.URL.Query().Get(reshapedField)),
		Rows:       rows,
		Standings:  standings,
		Verdicts:   verdicts,
		IsIndex:    sum.Kind == store.KindIndex,
		IsPosition: sum.Kind == store.KindPosition,
		Filtering:  sum.UniqueBy != store.UniqueOff,
		// There is always more than this on a job of any size, and the page says
		// so rather than leaving a reader to wonder whether twenty results is all
		// their list produced.
		Sampled: len(rows)+len(standings)+len(verdicts) >= rowsShown,
		Shown:   rowsShown,
		Speed:   perMinute(pace.PerMinute()),
		Formats: export.Formats(),
		// Neither button is offered by a server started to read a history: it has
		// nothing to press them against, and a button that cannot work is one
		// somebody presses until they conclude the job cannot be stopped at all.
		CanStop: s.sup != nil && at.Running,
		// A job whose list never finished arriving is not offered either. The
		// queries it holds are a fraction of a list, and nothing will run them.
		CanResume: s.sup != nil && sum.PlanReady &&
			!at.Running && !at.Queued && !at.Finished && at.Pending > 0,
	})
}

// jobAsked reads the job a request names and answers the reader itself when
// there is none.
//
// One read of the store settles both what the job is and whether it exists, so
// the page, the answer to a poll and a button all say the same thing about a
// job that is not there.
func (s *Server) jobAsked(w http.ResponseWriter, r *http.Request, asked string) (store.JobSummary, bool) {
	id, err := strconv.ParseInt(asked, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.JobSummary{}, false
	}
	sum, err := s.store.Progress(r.Context(), id)
	if errors.Is(err, store.ErrNoJob) {
		http.NotFound(w, r)
		return store.JobSummary{}, false
	}
	if err != nil {
		s.fail(w, r, err)
		return store.JobSummary{}, false
	}
	return sum, true
}

func stateOf(p progressJSON, listReady bool) string {
	switch {
	case !listReady:
		return "job.state.listunfinished"
	case p.Finished:
		return "job.state.finished"
	case p.Running && p.Done+p.Failed == 0:
		// Running and nothing settled yet. What is happening is that the
		// identities are being reached: a cold one meets a challenge on its first
		// request and the answer takes minutes, and a screen of noughts through
		// all of that reads as a job that never started. It is measured, so it can
		// be said.
		return "job.state.starting"
	case p.Running:
		return "job.state.running"
	case p.Queued:
		return "job.state.waiting"
	}
	return "job.state.unfinished"
}

// apiReshape changes the pool a job will next run on.
//
// It changes those three and nothing else. What a job is — its depth, its
// country, its filter — is settled by the work already done under it, and a
// page that offered to change one of those would be offering to file results
// gathered under two rules as though they were one.
//
// A job in flight is reshaped without being disturbed: it holds the pool it
// raised for itself, and what is written here is what the next raise reads. The
// page says that in words rather than leaving the reader to find out by
// watching a number not change.
func (s *Server) apiReshape(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("job"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	err = s.store.Reshape(r.Context(), id,
		countOf(r.FormValue("ports")), countOf(r.FormValue("threads")), countOf(r.FormValue("tries")))
	switch {
	case errors.Is(err, store.ErrNoJob):
		http.NotFound(w, r)
		return
	case errors.Is(err, store.ErrJobFinished):
		// Nothing to change: the job has run. Saying so on its own page beats a
		// refusal the reader has to interpret, and beats accepting it quietly,
		// which reads as applied.
		http.Redirect(w, r, jobPath(id)+"?"+reshapedField+"="+reshapeFinished, http.StatusSeeOther)
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, jobPath(id)+"?"+reshapedField+"="+reshapeDone, http.StatusSeeOther)
}

// countOf reads a number a person typed, and takes anything that is not one as
// nothing said — which is what the store reads a nought as.
func countOf(typed string) int {
	n, err := strconv.Atoi(strings.TrimSpace(typed))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// The answer a reshape leaves in the address it sends the reader back to. It is
// in the address rather than in a session because this page is read by pressing
// reload as often as by following a link, and a message kept anywhere else
// would appear again on a reload that changed nothing.
const (
	reshapedField   = "reshaped"
	reshapeDone     = "done"
	reshapeFinished = "finished"
)

// reshapedSaid turns the word a reshape left in the address into the key of
// what to say about it, and takes anything else as nothing to say.
//
// Anything else is not an error worth a page: the address is typed by hand as
// often as it is followed, and a stranger's word in it means only that this
// reader did not arrive from a change.
func reshapedSaid(word string) string {
	switch word {
	case reshapeDone:
		return "job.reshape.done"
	case reshapeFinished:
		return "job.reshape.finished"
	}
	return ""
}
