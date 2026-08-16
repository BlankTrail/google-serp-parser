// SPDX-License-Identifier: MIT

package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/blanktrail/google-serp-parser/store"
)

// progressJSON is a job's counts as its own page reads them back.
//
// It is not an interface for anybody else to build on: it is what this
// program's page polls, it is not documented anywhere a reader would find it,
// and it changes when the page changes.
type progressJSON struct {
	ID      int64 `json:"id"`
	Total   int   `json:"total"`
	Done    int   `json:"done"`
	Failed  int   `json:"failed"`
	Pending int   `json:"pending"`

	Finished bool `json:"finished"`
	Running  bool `json:"running"`
	Queued   bool `json:"queued"`
	// Watch says whether anything is going to answer differently later. It is
	// worked out here rather than in the browser so that the page as it is first
	// drawn and every answer after it agree about when to stop asking.
	Watch bool `json:"watch"`
}

// progress is one job as both the page and its script are given it.
//
// The page renders these fields and the answer to a poll carries the same ones,
// so a number on the screen and the number that replaces it two seconds later
// come from one read of one history. Counting them twice is how a page ends up
// disagreeing with itself.
func (s *Server) progress(sum store.JobSummary) progressJSON {
	running, queued := s.holds(sum.ID)
	return progressJSON{
		ID:       sum.ID,
		Total:    sum.Total,
		Done:     sum.Done,
		Failed:   sum.Failed,
		Pending:  sum.Pending,
		Finished: sum.Finished,
		Running:  running,
		Queued:   queued,
		// A job nobody is running will read at three in the morning exactly as it
		// reads now. A page that goes on asking anyway knocks all night for an
		// answer that cannot change until somebody presses something.
		Watch: !sum.Finished && (running || queued),
	}
}

// holds says where the supervisor has this job: in flight, or waiting its turn.
//
// The queue is read before the job in flight, and that order is what makes the
// answer safe to act on. A job leaves the queue by starting, so reading the
// queue first can at worst report a job that has just started as waiting, and a
// page told to wait keeps watching. The other order reports a job that started
// between the two reads as neither, and the page stops watching a job that has
// just begun.
func (s *Server) holds(jobID int64) (running, queued bool) {
	if s.sup == nil {
		return false, false
	}
	queued = slices.Contains(s.sup.Queued(), jobID)
	if now, ok := s.sup.Running(); ok && now == jobID {
		running = true
	}
	return running, queued
}

// apiProgress answers with how far a job has got, for the page's own script.
func (s *Server) apiProgress(w http.ResponseWriter, r *http.Request) {
	sum, ok := s.jobAsked(w, r, r.FormValue("job"))
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(s.progress(sum)); err != nil {
		// The header is out and part of the answer with it, so there is nothing
		// left to tell the browser. The log is where this has to be visible.
		s.log.Error("a poll could not be answered", "job", sum.ID, "error", err)
	}
}

// apiStop ends the job that is running.
func (s *Server) apiStop(w http.ResponseWriter, r *http.Request) {
	s.pressed(w, r, func(v *Supervisor, jobID int64) error { return v.Stop(jobID) })
}

// apiResume takes up a job that was left part way.
func (s *Server) apiResume(w http.ResponseWriter, r *http.Request) {
	s.pressed(w, r, func(v *Supervisor, jobID int64) error { return v.Resume(jobID) })
}

// pressed does what a button on the job page does and sends the reader back to
// the page they pressed it on.
//
// The answer is that page and never a report, because the button is a form and
// a reader with no script has nothing to read a report with. The page they land
// on holds the answer: it says what the job is doing now.
//
// The two of these are reached by post alone. A stop behind a link is a stop
// that a browser prefetching that link, or anything else walking the pages, can
// carry out on somebody else's run.
func (s *Server) pressed(w http.ResponseWriter, r *http.Request, do func(*Supervisor, int64) error) {
	sum, ok := s.jobAsked(w, r, r.FormValue("job"))
	if !ok {
		return
	}
	if s.sup == nil {
		http.Error(w, pickLang(r).T("form.norunner"), http.StatusServiceUnavailable)
		return
	}
	err := do(s.sup, sum.ID)
	switch {
	case err == nil,
		errors.Is(err, ErrNotRunning), errors.Is(err, ErrBusy), errors.Is(err, ErrNothingLeft):
		// A button pressed twice, or pressed in the second a job ended in, is a
		// race and not a fault. The page the reader lands on says what is true
		// now, which is the answer they were after.
		http.Redirect(w, r, jobPath(sum.ID), http.StatusSeeOther)
	default:
		s.fail(w, r, err)
	}
}
