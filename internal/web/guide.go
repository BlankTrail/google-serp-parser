// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"net/http"

	"github.com/blanktrail/google-serp-parser/internal/settings"
)

// The quick start: the address it stands at, and the press that puts it aside.
const (
	guideAt     = "/start"
	guideSkipAt = "/start/skip"
)

// step is one thing that has to be done before this program can run anything,
// as the quick start puts it.
//
// It is not a wizard. Each step is a sentence saying what the thing is for, the
// boxes on it that are worth knowing about before they are met, and a press
// that leads to the screen where it is done — the same screen it would be done
// on afterwards. A guide with screens of its own is a second interface to keep
// in step with the first, and the reader learns it instead of learning the
// program.
type step struct {
	// Key names the step and Why says what it is for. Notes are the boxes worth
	// a sentence, Press is the phrase on the way there and URL where it goes.
	Key   string
	Why   string
	Notes []string
	Press string
	URL   string
	// Done says this step has been done. It is read from the machine rather than
	// remembered: a step somebody did before they ever opened this page is done,
	// and a guide that made them do it again to tick it off would be the program
	// arguing with what it can see.
	Done bool
}

// guidePage is the quick start.
type guidePage struct {
	page
	Steps []step
	// Ready says every step is done, which is what the page says instead of
	// offering to walk somebody through what they have already finished.
	Ready bool
}

// guide draws the quick start.
func (s *Server) guide(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	steps := s.steps(r.Context())
	ready := true
	for _, one := range steps {
		ready = ready && one.Done
	}
	s.render(w, r, "guide.html", guidePage{
		page:  s.frame(r, lang, "guide.title", guideAt),
		Steps: steps,
		Ready: ready,
	})
}

// steps is what has to be done, in the order it has to be done in, with what
// this machine has already done marked.
//
// The order is not a preference. Nothing can be reached without the connection,
// nothing can go out anonymously without the exits, and a job set up before
// either would fail on its first phrase and teach its author that the program
// does not work.
func (s *Server) steps(ctx context.Context) []step {
	return []step{
		{
			Key: "guide.link.title",
			Why: "guide.link.why",
			Notes: []string{
				"guide.link.note.address",
				"guide.link.note.key",
				"guide.link.note.hot",
			},
			Press: "guide.link.press",
			URL:   settingsAt,
			Done:  s.connected(),
		},
		{
			Key: "guide.exits.title",
			Why: "guide.exits.why",
			Notes: []string{
				"guide.exits.note.source",
				"guide.exits.note.refresh",
				"guide.exits.note.ban",
			},
			Press: "guide.exits.press",
			URL:   s.exitsAt(ctx),
			Done:  s.exitsSet(ctx),
		},
		{
			Key: "guide.job.title",
			Why: "guide.job.why",
			Notes: []string{
				"guide.job.note.queries",
				"guide.job.note.kind",
				"guide.job.note.pages",
				"guide.job.note.where",
				"guide.job.note.pool",
			},
			Press: "guide.job.press",
			URL:   newAt,
			Done:  s.ranSomething(ctx),
		},
	}
}

// connected says the address and the key have been filled in.
//
// Filled in, and not answering: whether the service is up is the header's
// question and the banner's, and a step that went undone because BlankTrail was
// stopped for the afternoon would be telling the reader to do again what they
// have already done.
func (s *Server) connected() bool {
	if s.settingsPath == "" {
		return false
	}
	saved, err := settings.Load(s.settingsPath)
	return err == nil && saved.ControlURL != "" && saved.APIKey != ""
}

// exitsSet says the profile a job naming none runs on can send a request
// somewhere other than out of this machine's own address.
func (s *Server) exitsSet(ctx context.Context) bool {
	profile, err := s.store.DefaultProfile(ctx)
	return err == nil && !profile.Empty()
}

// exitsAt is where that step's press leads: the default profile's own boxes
// where there is one, and the list of profiles where there is not.
func (s *Server) exitsAt(ctx context.Context) string {
	profile, err := s.store.DefaultProfile(ctx)
	if err != nil {
		return proxiesAt
	}
	return profileAt(profile.ID)
}

// ranSomething says this machine has been used: one job, of any kind, finished
// or not.
func (s *Server) ranSomething(ctx context.Context) bool {
	jobs, err := s.store.Jobs(ctx, 1)
	return err == nil && len(jobs) > 0
}

// skipGuide puts the quick start aside and takes the reader to the screen the
// program would have opened on.
//
// It is a press and not a link, because it writes something down. A link that
// changed what is on this machine is a link anything walking the pages would
// pull, and the guide would be gone before its reader ever saw it.
func (s *Server) skipGuide(w http.ResponseWriter, r *http.Request) {
	saved, err := settings.Load(s.settingsPath)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	saved.GuideDone = true
	if err := settings.Save(s.settingsPath, saved); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, stateAt, http.StatusSeeOther)
}

// guiding says the program should open on the quick start rather than on what
// is happening.
//
// A machine that has never run a job and has not been told to stop offering
// this. Not "anything is unset": a run that fails because the list expired at
// four in the morning must not take the operator off the screen they are
// watching it on and put a beginner's guide there instead. What is wrong on a
// machine that is already in use is what the banners are for.
func (s *Server) guiding(ctx context.Context) bool {
	if s.settingsPath == "" {
		return false
	}
	saved, err := settings.Load(s.settingsPath)
	if err != nil || saved.GuideDone {
		return false
	}
	return !s.ranSomething(ctx)
}

// guideOffered is the way back to the quick start, standing beside the
// settings: whoever put it aside on the first afternoon is the same person who
// wants it a week later, when they set the second machine up.
func (s *Server) guideOffered(under string) *tabLink {
	if s.settingsPath == "" {
		return nil
	}
	return &tabLink{Key: "guide.title", URL: guideAt, Current: under == guideAt}
}
