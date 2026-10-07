// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/semantic"
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
	Fault   string
}

func (s *Server) modelReading() modelReading {
	out := modelReading{SizeMB: int(s.modelSize >> 20)}
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
	err := semantic.Fetch(ctx, &http.Client{}, url, sum, s.semantic.Path(),
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
