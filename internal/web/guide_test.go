// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// stepsOn is every step the quick start draws: its name, whether the page marks
// it done, and where its press leads.
type stepShown struct {
	Named string
	Done  bool
	At    string
}

func stepsOn(t *testing.T, body string) []stepShown {
	t.Helper()
	var found []stepShown
	for _, after := range strings.Split(body, `<section class="card step">`)[1:] {
		one, _, ok := strings.Cut(after, "</section>")
		if !ok {
			t.Fatalf("a step never closes:\n%s", body)
		}
		_, named, _ := strings.Cut(one, "<h2>")
		named, _, _ = strings.Cut(named, "</h2>")
		_, at, _ := strings.Cut(one, `<p class="export"><a href="`)
		at, _, _ = strings.Cut(at, `"`)
		found = append(found, stepShown{
			Named: strings.TrimSpace(named),
			Done:  strings.Contains(one, `class="step-done"`),
			At:    at,
		})
	}
	if len(found) == 0 {
		t.Fatalf("the quick start draws no step at all:\n%s", body)
	}
	return found
}

// fresh is a machine as it comes: settings that name nothing, no profile, no
// job.
func fresh(t *testing.T) (*Server, string) {
	t.Helper()
	path := settingsFile(t, settings.Settings{})
	s, err := New(Config{Store: testStore(t), Logger: quiet(), SettingsPath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, path
}

// used writes one job, which is what makes a machine one that has been used.
//
// A machine that has never run anything opens on the quick start instead of on
// what is happening, so a test about any other screen has to say which of the
// two it is talking about.
func used(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.store.CreateJob(context.Background(),
		store.JobSpec{Name: "one that has been run", Pages: 1}, []string{"a"}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
}

func TestGuide_OpensOnAMachineNothingHasBeenRunOn(t *testing.T) {
	// The screen this program opens on is where the work is watched, and on a
	// fresh machine there is no work: what it shows is a column of noughts and a
	// pool of nothing, which reads as a program that is broken rather than one
	// that has not been set up yet.
	s, _ := fresh(t)

	rec := get(t, s, stateAt)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a fresh machine answered %s with %d rather than sending the reader anywhere", stateAt, rec.Code)
	}
	if at := rec.Header().Get("Location"); at != guideAt {
		t.Errorf("it sends the reader to %s", at)
	}
}

func TestGuide_StandsAsideOnceTheMachineHasBeenUsed(t *testing.T) {
	// A run that fails at four in the morning because the list expired must not
	// take the operator off the screen they are watching it on and put a
	// beginner's guide there instead. What is wrong on a machine already in use
	// is what the banners are for.
	s, _ := fresh(t)
	if _, err := s.store.CreateJob(context.Background(),
		store.JobSpec{Name: "the first one", Pages: 1}, []string{"a"}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if code := get(t, s, stateAt).Code; code != http.StatusOK {
		t.Errorf("a machine with a job behind it answered %s with %d, want 200", stateAt, code)
	}
}

func TestGuide_ReadsEachStepOffTheMachineRatherThanRememberingIt(t *testing.T) {
	// A step somebody did before they ever opened this page is done. A guide
	// that made them do it again to tick it off would be the program arguing
	// with what it can already see.
	s, _ := fresh(t)
	steps := stepsOn(t, get(t, s, guideAt).Body.String())
	if len(steps) != 3 {
		t.Fatalf("the quick start draws %d steps, want 3", len(steps))
	}
	for _, one := range steps {
		if one.Done {
			t.Errorf("%q is marked done on a machine where nothing has been set up", one.Named)
		}
	}

	// The connection, filled in.
	filled, path := fresh(t)
	if err := settings.Save(path, settings.Settings{
		ControlURL: "http://127.0.0.1:8891/", APIKey: "a-key",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// The exits, named.
	if _, err := filled.store.CreateProfile(context.Background(), listing()); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	// And a job, run.
	if _, err := filled.store.CreateJob(context.Background(),
		store.JobSpec{Name: "the first one", Pages: 1}, []string{"a"}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	for _, one := range stepsOn(t, get(t, filled, guideAt).Body.String()) {
		if !one.Done {
			t.Errorf("%q is still marked undone on a machine where it has been done", one.Named)
		}
	}
}

func TestGuide_SendsEachStepToTheScreenItIsDoneOn(t *testing.T) {
	// The same screen it would be done on afterwards, so what the reader learns
	// here is the program rather than the guide.
	s, _ := fresh(t)
	steps := stepsOn(t, get(t, s, guideAt).Body.String())
	want := []string{settingsAt, proxiesAt, newAt}
	for i, one := range steps {
		if !strings.HasPrefix(one.At, want[i]) {
			t.Errorf("%q leads to %s rather than to %s", one.Named, one.At, want[i])
		}
		if code := get(t, s, one.At).Code; code != http.StatusOK {
			t.Errorf("%q leads to %s, which answers %d", one.Named, one.At, code)
		}
	}
}

func TestGuide_IsPutAsideByAPressAndNotByALink(t *testing.T) {
	// It writes something down, and a link that changed what is on this machine
	// is a link anything walking these pages would pull — the guide would be
	// gone before its reader ever saw it.
	s, path := fresh(t)
	if code := get(t, s, guideSkipAt).Code; code == http.StatusSeeOther {
		t.Error("the guide can be put aside by fetching an address")
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, guideSkipAt, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the press answered %d rather than sending the reader on", rec.Code)
	}
	if at := rec.Header().Get("Location"); at != stateAt {
		t.Errorf("it sends the reader to %s", at)
	}

	saved, err := settings.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !saved.GuideDone {
		t.Error("nothing was written down, so the program opens on the guide again")
	}
	if code := get(t, s, stateAt).Code; code != http.StatusOK {
		t.Errorf("the program still opens on the guide after being told not to: %s answered %d", stateAt, code)
	}
	// And it is still there for whoever wants it.
	if code := get(t, s, guideAt).Code; code != http.StatusOK {
		t.Errorf("the guide is gone once it is put aside: %s answered %d", guideAt, code)
	}
}

func TestGuide_IsOfferedFromEveryScreen(t *testing.T) {
	// Whoever put it aside on the first afternoon is the same person who wants
	// it a week later, setting the second machine up.
	s, _ := fresh(t)
	for _, at := range []string{jobsAt, newAt, proxiesAt, historyAt, settingsAt} {
		body := get(t, s, at).Body.String()
		if !strings.Contains(body, `href="`+guideAt+`"`) {
			t.Errorf("%s offers no way to the quick start:\n%s", at, oneTag(t, body, "header"))
		}
	}
}

func TestGuide_ExplainsWhatEachStepIsForBeforeItNamesABox(t *testing.T) {
	// Somebody filling in a key they do not understand the purpose of fills it
	// in wrongly once and blames the program for the rest of the week. Each step
	// carries its reason and the boxes worth a sentence, in both languages,
	// which is what the catalogue is asked for here.
	s, _ := fresh(t)
	body := get(t, s, guideAt).Body.String()
	for _, key := range []string{
		"guide.link.why", "guide.link.note.key", "guide.link.note.hot",
		"guide.exits.why", "guide.exits.note.source",
		"guide.job.why", "guide.job.note.queries", "guide.job.note.pages",
	} {
		said := LangEN.T(key)
		if said == key {
			t.Errorf("%s is not in the catalogue at all", key)
			continue
		}
		// The first few words are enough: the sentences carry punctuation the
		// template escapes, and this is a test of what is drawn rather than of
		// how an apostrophe travels.
		opening := said
		if len(opening) > 40 {
			opening = opening[:40]
		}
		if !strings.Contains(body, opening) {
			t.Errorf("the quick start does not say %q", opening)
		}
	}
}
