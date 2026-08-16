// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
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
	Name     string
	Started  time.Time
	Pages    int
	Country  string
	Language string
	Spec     string
}

// jobPage is one job: how it was set up, how far it has got, what it has
// captured, and what may be pressed.
type jobPage struct {
	page
	Job      jobSetup
	Progress progressJSON
	// State is the key of what to call the job's state, not the word itself.
	State     string
	Rows      []store.Row
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
	rows, capped, err := s.someRows(r.Context(), sum.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	at := s.progress(sum)

	s.render(w, r, "job.html", jobPage{
		page: s.frame(r, lang, "job.title", jobsAt),
		Job: jobSetup{
			Name:     sum.Name,
			Started:  sum.CreatedAt,
			Pages:    sum.Pages,
			Country:  sum.Country,
			Language: sum.Language,
			Spec:     sum.SpecName,
		},
		Progress: at,
		State:    stateOf(at, sum.PlanReady),
		Rows:     rows,
		Capped:   capped,
		Formats:  export.Formats(),
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
