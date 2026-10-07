// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/semantic"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// semanticAt is where the settings page asks for the model to be downloaded.
const semanticAt = "/settings/semantic"

// modelFetchTimeout is how long one download of the model may take. It is about
// 140 MB, which is minutes on a slow line and not much more; an hour is long
// enough for any line that is going to finish and short enough that a
// connection that has stalled without closing does not hold the one download
// there can be for as long as the program runs.
const modelFetchTimeout = time.Hour

// modelFetch is the one download of the model there can be at a time, and
// what the settings page reads of it.
type modelFetch struct {
	mu          sync.Mutex
	running     bool
	done, total int64
	// fault is the catalogue key of what to tell the reader when the last
	// download failed. The cause itself goes to the log, not to the page.
	fault string
}

// modelReading is what the settings page draws.
type modelReading struct {
	// Available is whether this server keeps a model at all.
	Available bool
	Present   bool
	// Damaged is a file at the path that is not the model, and is not used.
	Damaged bool
	Running bool
	Percent int
	SizeMB  int
	// Version is the name of the model's file, which carries its version: the
	// settings page says which model is in use, so that a reader comparing two
	// installations, or a report of odd scores, can tell them apart.
	Version string
	Fault   string
}

func (s *Server) modelReading() modelReading {
	out := modelReading{SizeMB: int(s.modelSize >> 20), Version: semantic.FileName}
	if s.semantic == nil {
		return out
	}
	out.Available = true
	state := s.modelState()
	out.Present, out.Damaged = state == semantic.Ready, state == semantic.Damaged
	s.fetching.mu.Lock()
	defer s.fetching.mu.Unlock()
	out.Running, out.Fault = s.fetching.running, s.fetching.fault
	if s.fetching.total > 0 {
		// Clamped: a server that sends more than it announced is not a download
		// that is 104 percent done.
		out.Percent = int(min(100, max(0, 100*s.fetching.done/s.fetching.total)))
	}
	return out
}

// modelState is the holder's look at the file with the one check only this
// layer can make: the release says how many bytes the model is, and a file of
// another length is not it, however it begins. Cheap enough to do on every
// draw of the settings page; it never loads the model.
func (s *Server) modelState() semantic.State {
	state := s.semantic.Status()
	if state != semantic.Ready {
		return state
	}
	if info, err := os.Stat(s.semantic.Path()); err != nil || info.Size() != s.modelSize {
		return semantic.Damaged
	}
	return semantic.Ready
}

// fetchModel starts the download and goes back to the settings page, which
// follows it. A second press while one runs does nothing, and so does a press
// when the model is already there and sound: the file is replaced only when it
// is missing or damaged, not by a button that is still on a page somebody left
// open. A damaged file is exactly what the button is for.
func (s *Server) fetchModel(w http.ResponseWriter, r *http.Request) {
	if s.semantic == nil {
		http.NotFound(w, r)
		return
	}
	// Looked at before the lock is taken: it is a stat and a read of eight
	// bytes, and the page that polls this state takes the same lock.
	wanted := s.modelState() != semantic.Ready
	s.fetching.mu.Lock()
	if !s.fetching.running && wanted {
		s.fetching.running, s.fetching.done, s.fetching.total, s.fetching.fault = true, 0, s.modelSize, ""
		go s.downloadModel(s.modelURL, s.modelSum)
	}
	s.fetching.mu.Unlock()
	http.Redirect(w, r, settingsAt, http.StatusSeeOther)
}

// downloadModel fetches the model from url and, once it is in place, makes the
// holder read it afresh.
func (s *Server) downloadModel(url, sum string) {
	ctx, cancel := context.WithTimeout(context.Background(), modelFetchTimeout)
	defer cancel()
	err := semantic.Fetch(ctx, &http.Client{}, url, sum, s.modelSize, s.semantic.Path(),
		func(done, total int64) {
			s.fetching.mu.Lock()
			s.fetching.done = done
			if total > 0 {
				s.fetching.total = total
			}
			s.fetching.mu.Unlock()
		})
	if err == nil {
		// Before running is cleared, so a page that sees the download over also
		// sees a holder that will read the new file.
		s.semantic.Reset()
	}
	s.fetching.mu.Lock()
	s.fetching.running = false
	if err != nil {
		s.fetching.fault = "settings.semantic.failed"
		s.log.Error("the model was not downloaded", "error", err)
	}
	s.fetching.mu.Unlock()
}

// jobScoring is the one job whose old suggestions are being scored, and how
// far it has got. One at a time: the model is shared and the work is the
// database's, and two would only slow each other down.
type jobScoring struct {
	mu          sync.Mutex
	job         int64
	running     bool
	done, total int
	// fault is the catalogue key of what to tell the reader when the last
	// scoring stopped; the cause itself goes to the log.
	fault string
}

// scoringOf is what a job's page reads of the scoring: whether it is this job
// that is being scored, how far, and whether the last one stopped. Another
// job's scoring is nothing to this page, which must not show its progress.
func (s *Server) scoringOf(job int64) (running bool, done, total int, fault string) {
	s.scoring.mu.Lock()
	defer s.scoring.mu.Unlock()
	if s.scoring.job != job {
		return false, 0, 0, ""
	}
	return s.scoring.running, s.scoring.done, s.scoring.total, s.scoring.fault
}

// scoreBatchSize is how many suggestions are read, scored and written at once.
// A server's scoreBatch starts from it; a test makes it small to walk a few
// rows in many batches.
const scoreBatchSize = 10_000

// apiScore starts scoring a job's unscored suggestions and goes back to its page.
// Everything the page does not offer is refused quietly with the same redirect,
// for the reason a stop pressed twice is: the page the reader lands on says
// what is true now.
func (s *Server) apiScore(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("job"), 10, 64)
	if err != nil || s.semantic == nil {
		http.NotFound(w, r)
		return
	}
	sum, err := s.store.Progress(r.Context(), id)
	if err != nil || sum.Kind != store.KindSuggest || s.jobInFlight(id) || s.modelState() != semantic.Ready {
		http.Redirect(w, r, backTo(r, id), http.StatusSeeOther)
		return
	}
	m, err := s.semantic.Get()
	if err != nil {
		// The file looked like the model and was not: the holder now calls it
		// damaged, so the button is gone from the page, and the page must say
		// why rather than come back as if the press had not happened. It is
		// told as this job's fault, which is where its page reads one; the
		// cause goes to the log. Not while another scoring runs: that one's
		// page is the one reading the fault, and it has its own.
		s.log.Error("the model did not load for scoring", "job", id, "error", err)
		s.scoring.mu.Lock()
		if !s.scoring.running {
			s.scoring.job, s.scoring.done, s.scoring.total, s.scoring.fault = id, 0, 0, "job.score.damaged"
		}
		s.scoring.mu.Unlock()
		http.Redirect(w, r, backTo(r, id), http.StatusSeeOther)
		return
	}
	// Counted before the lock is taken: it is a query over the job's rows, and
	// the pages that poll the scoring take the same lock. A total that is a
	// moment stale (a press that loses the race to a running scoring counts for
	// nothing) is harmless; a page waiting on a count is not.
	total, err := s.store.UnscoredCount(r.Context(), id)
	if err != nil {
		s.log.Error("counting a job's unscored suggestions failed", "job", id, "error", err)
		http.Redirect(w, r, backTo(r, id), http.StatusSeeOther)
		return
	}
	s.scoring.mu.Lock()
	if !s.scoring.running {
		s.scoring.job, s.scoring.running, s.scoring.done, s.scoring.total, s.scoring.fault = id, true, 0, total, ""
		go s.scoreJob(id, m)
	}
	s.scoring.mu.Unlock()
	http.Redirect(w, r, backTo(r, id), http.StatusSeeOther)
}

// scoreBatchOrDefault is the batch size, which only a test changes.
func (s *Server) scoreBatchOrDefault() int {
	if s.scoreBatch > 0 {
		return s.scoreBatch
	}
	return scoreBatchSize
}

// scoringElsewhere is whether a scoring of some other job is under way. One at
// a time: pressing the button of another job would do nothing, so its page
// does not offer it and says what is going on instead.
func (s *Server) scoringElsewhere(job int64) bool {
	s.scoring.mu.Lock()
	defer s.scoring.mu.Unlock()
	return s.scoring.running && s.scoring.job != job
}

// jobInFlight is whether the job is the one being run right now: its rows are
// still arriving, and a row written after the scoring has passed would be left
// unmeasured by a run that then reports itself finished.
func (s *Server) jobInFlight(id int64) bool {
	if s.sup == nil {
		return false
	}
	running, ok := s.sup.Running()
	return ok && running == id
}

// scoreJob walks the job's unscored suggestions batch by batch. Stopped part way
// - the program closed - it has lost nothing: what was written is scored, and
// the next press carries on with what is not.
func (s *Server) scoreJob(id int64, m *semantic.Model) {
	ctx := context.Background()
	var after int64
	var fault string
	for {
		batch, err := s.store.Unscored(ctx, id, after, s.scoreBatchOrDefault())
		if err != nil {
			fault = "job.score.failed"
			s.log.Error("scoring a job's suggestions stopped", "job", id, "error", err)
			break
		}
		if len(batch) == 0 {
			break
		}
		scores := make(map[int64]float64, len(batch))
		// A batch is rows in id order, which is a key's rows together, but a
		// batch boundary or a resumed job can put one key in two places, so the
		// rows are gathered by key first: each key is turned into a vector once
		// per batch rather than once per row.
		var keys []string
		byKey := map[string][]store.Unscored{}
		for _, u := range batch {
			if _, ok := byKey[u.Key]; !ok {
				keys = append(keys, u.Key)
			}
			byKey[u.Key] = append(byKey[u.Key], u)
		}
		for _, key := range keys {
			rows := byKey[key]
			texts := make([]string, len(rows))
			for i, u := range rows {
				texts[i] = u.Text
			}
			for i, v := range m.ScoreAll(key, texts) {
				scores[rows[i].ID] = float64(v)
			}
		}
		if err := s.store.SetSimilarity(ctx, scores); err != nil {
			fault = "job.score.failed"
			s.log.Error("scoring a job's suggestions stopped", "job", id, "error", err)
			break
		}
		after = batch[len(batch)-1].ID
		s.scoring.mu.Lock()
		s.scoring.done += len(batch)
		s.scoring.mu.Unlock()
	}
	s.scoring.mu.Lock()
	s.scoring.running, s.scoring.fault = false, fault
	s.scoring.mu.Unlock()
}
