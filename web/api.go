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
	// Dropped is how many results the job threw away as repeats. It travels with
	// the counts because it climbs while the job runs, and a reader watching the
	// results come in has to see the ones that did not.
	Dropped int `json:"dropped"`

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
		Dropped:  sum.Dropped,
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

// backField is how a button carries the address of the screen it was pressed
// on. It travels in the form because the screen knows where it is and the
// handler does not: the same stop stands on a job's own page and on the screen
// an operator watches.
const backField = "back"

// backTo is the screen a press comes back to: the one it was pressed on when it
// said which that was, and the job's own page otherwise.
//
// Only a screen this program draws is honoured, and it is looked up in the list
// of them rather than checked for a leading slash. A form is filled in by
// whoever posts it, and a program that sent a reader wherever a posted field
// asked would be a way of pointing at a machine of somebody else's choosing
// from an address the reader trusts.
func backTo(r *http.Request, jobID int64) string {
	asked := r.FormValue(backField)
	for _, t := range tabs {
		if asked == t.At {
			return asked
		}
	}
	return jobPath(jobID)
}

// apiStop ends the job that is running.
func (s *Server) apiStop(w http.ResponseWriter, r *http.Request) {
	s.pressed(w, r, func(v *Supervisor, jobID int64) error { return v.Stop(jobID) })
}

// apiResume takes up a job that was left part way.
func (s *Server) apiResume(w http.ResponseWriter, r *http.Request) {
	s.pressed(w, r, func(v *Supervisor, jobID int64) error { return v.Resume(jobID) })
}

// apiRetry puts a job's failed queries back into its queue and runs it again.
//
// It is a button of its own rather than something resume does quietly. Resume
// means "carry on with what is left", and a reader who presses it does not
// expect twenty thousand phrases that were already answered with a failure to
// be asked all over again. Which of the two they want is theirs to say.
func (s *Server) apiRetry(w http.ResponseWriter, r *http.Request) {
	s.pressed(w, r, func(v *Supervisor, jobID int64) error {
		if _, err := s.store.TryFailedAgain(r.Context(), jobID); err != nil {
			return err
		}
		return v.Resume(jobID)
	})
}

// apiDelete removes a job and everything it gathered.
//
// It is a press of its own rather than one more thing pressed does, because it
// is the one button here that cannot be pressed twice: a stop pressed on a job
// that has already stopped is a race, and a delete pressed on a job that is
// running would throw away a run somebody is watching. It refuses that outright
// rather than stopping the job first — a button that stops a run as a side
// effect of another word is a button nobody can predict.
func (s *Server) apiDelete(w http.ResponseWriter, r *http.Request) {
	sum, ok := s.jobAsked(w, r, r.FormValue("job"))
	if !ok {
		return
	}
	if s.sup != nil {
		if running, ok := s.sup.Running(); ok && running == sum.ID {
			http.Error(w, pickLang(r).T("jobs.delete.running"), http.StatusConflict)
			return
		}
		for _, queued := range s.sup.Queued() {
			if queued == sum.ID {
				http.Error(w, pickLang(r).T("jobs.delete.running"), http.StatusConflict)
				return
			}
		}
	}
	if err := s.store.DeleteJob(r.Context(), sum.ID); err != nil {
		if errors.Is(err, store.ErrNoJob) {
			// Deleted twice, or deleted from another tab. The list is the answer to
			// both: the job is not on it.
			http.Redirect(w, r, jobsAt, http.StatusSeeOther)
			return
		}
		s.fail(w, r, err)
		return
	}
	// Never back to the job: there is no job. The list is where the reader was.
	http.Redirect(w, r, jobsAt, http.StatusSeeOther)
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
	if !s.sup.canRun() {
		// Nothing can be started here until the connection is set up, and a job
		// taken up would wait in the queue with nothing to say why.
		http.Error(w, pickLang(r).T("form.notsetup"), http.StatusServiceUnavailable)
		return
	}
	err := do(s.sup, sum.ID)
	switch {
	case err == nil,
		errors.Is(err, ErrNotRunning), errors.Is(err, ErrBusy), errors.Is(err, ErrNothingLeft),
		errors.Is(err, store.ErrPlanUnfinished):
		// A button pressed twice, or pressed in the second a job ended in, is a
		// race and not a fault. The page the reader lands on says what is true
		// now, which is the answer they were after. A job whose list never
		// finished arriving is the same shape of thing: its page never offered
		// this button, and the page says why.
		http.Redirect(w, r, backTo(r, sum.ID), http.StatusSeeOther)
	default:
		s.fail(w, r, err)
	}
}
