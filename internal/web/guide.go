// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"net/http"

	"github.com/blanktrail/google-serp-parser/internal/settings"
)

// guideDoneAt is the one address the walk has: the press that says it is over.
//
// It has no page of its own. A guide that is a page is a page somebody reads and
// then has to go and find everything it described; this one stands on the
// screens themselves, one stop at a time, pointing at the thing it is talking
// about.
const guideDoneAt = "/guide/done"

// stop is one stop of that walk: a screen, the thing on it being pointed at, and
// two short lines about it.
//
// Where the walk goes and what it says are decided here rather than in the
// browser, for the reason every other decision on these screens is: this is
// where it can be read and tested. What the script does with them is carry them
// out — move to the screen, find the thing, put the note beside it.
type stop struct {
	// At is the screen this stop stands on, and Anchor names the one thing on it
	// the note points at, as the stylesheet would name it.
	At     string
	Anchor string
	// Title and Said are the phrases, already in the reader's language: the
	// script carries no words of its own.
	Title string
	Said  string
}

// tourFor is the walk, in the order it is walked.
//
// Seven stops, and each is one sentence. It is the whole of what somebody needs
// before their first run — where the screens are, where the key goes, what a
// profile is, where a job is started, what a job asks for, and where to look to
// see that the machine can reach anything at all. Everything else about this
// program is on the screens themselves, and a walk that tried to say it would be
// a manual nobody finishes.
func tourFor(lang Lang) []stop {
	return []stop{
		{At: stateAt, Anchor: "#tabs", Title: lang.T("tour.screens"), Said: lang.T("tour.screens.said")},
		{At: settingsAt, Anchor: "#api_key", Title: lang.T("tour.key"), Said: lang.T("tour.key.said")},
		{At: proxiesAt, Anchor: "#profiles", Title: lang.T("tour.exits"), Said: lang.T("tour.exits.said")},
		{At: jobsAt, Anchor: "#new-job", Title: lang.T("tour.new"), Said: lang.T("tour.new.said")},
		{At: newAt, Anchor: "#queries", Title: lang.T("tour.queries"), Said: lang.T("tour.queries.said")},
		{At: newAt, Anchor: "#pages", Title: lang.T("tour.depth"), Said: lang.T("tour.depth.said")},
		{At: stateAt, Anchor: "#reach", Title: lang.T("tour.reach"), Said: lang.T("tour.reach.said")},
	}
}

// guideDone writes down that the walk is over, however it ended: walked to the
// end or closed on the first stop.
//
// It is a press rather than a link, because it writes something. A link that
// changed what is on this machine is a link anything walking these pages would
// pull, and the walk would be over before its reader saw the first stop.
func (s *Server) guideDone(w http.ResponseWriter, r *http.Request) {
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
	// The script sends this and stays where it is; a browser running none has
	// never started the walk at all. Either way there is nothing to draw.
	w.WriteHeader(http.StatusNoContent)
}

// guiding says the walk should start itself.
//
// A machine that has never run a job and has not been told the walk is over.
// Not "anything is unset": a run that fails at four in the morning because the
// list expired must not put a beginner's walk in front of the operator watching
// it. What is wrong on a machine already in use is what the banners are for.
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

// ranSomething says this machine has been used: one job, of any kind, finished
// or not.
func (s *Server) ranSomething(ctx context.Context) bool {
	jobs, err := s.store.Jobs(ctx, 1)
	return err == nil && len(jobs) > 0
}

// guideOffered is the press that starts the walk again, standing beside the
// settings: whoever closed it on the first afternoon is the same person who
// wants it a week later, setting the second machine up.
//
// It is an ordinary link to the screen the program opens on, and the script
// takes the press over. A browser running no script has no walk to start, and
// what it does instead — going to the beginning — is the nearest thing there is.
func (s *Server) guideOffered(under string) *tabLink {
	if s.settingsPath == "" {
		return nil
	}
	return &tabLink{Key: "tour.title", URL: stateAt, Current: under == ""}
}
