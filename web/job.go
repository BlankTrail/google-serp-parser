// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/export"
	"github.com/blanktrail/google-serp-parser/store"
)

// rowsShown is how much of a job's results the page draws.
//
// A job of ten thousand queries holds a million rows. Nobody reads a million
// rows in a browser, no browser lays them out quickly, and the export beside
// them hands over every one.
const rowsShown = 200

// refreshEvery is how often the page asks what has changed. The answer is four
// numbers, so asking costs nothing worth counting, and asking faster would only
// tighten the loop around queries that take seconds each.
const refreshEvery = 2 * time.Second

// errEnough ends the walk over a job's results once the page holds as many as
// it draws. store.Rows hands a caller's error back unchanged, which is what
// makes this a way of stopping rather than a failure.
var errEnough = errors.New("web: the page holds every row it draws")

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
	Spec     string
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
	Capped    bool
	Formats   []string
	CanStop   bool
	CanResume bool
	// RefreshMS is how often the page asks again, in the milliseconds a browser
	// counts in. It reaches the script through the markup so that the interval
	// is decided in one place and not in two.
	RefreshMS int64
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
	var capped bool
	var err error
	switch sum.Kind {
	case store.KindIndex:
		verdicts, capped, err = s.someVerdicts(r.Context(), sum.ID)
	case store.KindPosition:
		standings, capped, err = s.someStandings(r.Context(), sum.ID)
	default:
		rows, capped, err = s.someRows(r.Context(), sum.ID)
	}
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

	s.render(w, r, "job.html", jobPage{
		page: s.frame(r, lang, "job.title", jobsAt),
		Job: jobSetup{
			Name:     sum.Name,
			Started:  sum.CreatedAt,
			Kind:     kind,
			Target:   sum.Target,
			Filter:   filter,
			Pages:    sum.Pages,
			Country:  sum.Country,
			Language: sum.Language,
			Spec:     sum.SpecName,
			Ports:    sum.Ports,
			Threads:  sum.Threads,
			Tries:    sum.Tries,
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
		Capped:     capped,
		Formats:    export.Formats(),
		// Neither button is offered by a server started to read a history: it has
		// nothing to press them against, and a button that cannot work is one
		// somebody presses until they conclude the job cannot be stopped at all.
		CanStop: s.sup != nil && at.Running,
		// A job whose list never finished arriving is not offered either. The
		// queries it holds are a fraction of a list, and nothing will run them.
		CanResume: s.sup != nil && sum.PlanReady &&
			!at.Running && !at.Queued && !at.Finished && at.Pending > 0,
		RefreshMS: refreshEvery.Milliseconds(),
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

// someRows reads as much of a job's results as the page draws, and says whether
// there was more.
//
// The walk is stopped rather than the rows counted afterwards: the whole point
// is not to read a million rows out of the history to throw all but two hundred
// of them away.
func (s *Server) someRows(ctx context.Context, jobID int64) ([]store.Row, bool, error) {
	rows := make([]store.Row, 0, rowsShown)
	err := s.store.Rows(ctx, jobID, func(row store.Row) error {
		if len(rows) == rowsShown {
			return errEnough
		}
		rows = append(rows, row)
		return nil
	})
	if errors.Is(err, errEnough) {
		return rows, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return rows, false, nil
}

// someVerdicts reads as many of an index job's answers as the page draws, and
// says whether there were more.
//
// The walk is stopped for the reason someRows is stopped: a list of addresses is
// as long as somebody's file, and reading a million of them to draw two hundred
// is a million rows read to be thrown away.
func (s *Server) someVerdicts(ctx context.Context, jobID int64) ([]store.Verdict, bool, error) {
	out := make([]store.Verdict, 0, rowsShown)
	err := s.store.Verdicts(ctx, jobID, func(v store.Verdict) error {
		if len(out) == rowsShown {
			return errEnough
		}
		out = append(out, v)
		return nil
	})
	if errors.Is(err, errEnough) {
		return out, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return out, false, nil
}

// someStandings reads as many of a position check's answers as the page draws,
// and says whether there were more.
//
// The walk is stopped for the reason someVerdicts is stopped: a list of phrases
// is as long as somebody's file, and reading a million of them to draw two
// hundred is a million rows read to be thrown away.
func (s *Server) someStandings(ctx context.Context, jobID int64) ([]store.Standing, bool, error) {
	out := make([]store.Standing, 0, rowsShown)
	err := s.store.Standings(ctx, jobID, func(st store.Standing) error {
		if len(out) == rowsShown {
			return errEnough
		}
		out = append(out, st)
		return nil
	})
	if errors.Is(err, errEnough) {
		return out, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return out, false, nil
}

// stateOf is the key of what to call a job's state.
//
// It answers with a key and never a sentence, because every phrase on every
// page goes through the catalogue, and a state named in English here would be
// the one English word on a Russian page.
//
// A list that never finished arriving is said first and over everything else.
// Such a job has queries waiting and no stamp on it, which is exactly what an
// unfinished run looks like, and calling the two by one name would tell the
// reader to carry on with a job that cannot be carried on.
func stateOf(p progressJSON, listReady bool) string {
	switch {
	case !listReady:
		return "job.state.listunfinished"
	case p.Finished:
		return "job.state.finished"
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
