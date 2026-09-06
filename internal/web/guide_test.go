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

// walked is a machine with everything the walk points at already on it: a
// connection, a set of exits, a job in the history, and something known about
// the service so the header draws its mark.
//
// Everything, because this is a test of where the walk points rather than of
// what a fresh machine looks like: a stop pointing at something that is only
// drawn once the machine is set up is still a stop that has to point at
// something.
func walked(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	ctx := context.Background()
	if _, err := st.CreateProfile(ctx, listing()); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if _, err := st.CreateJob(ctx, store.JobSpec{Name: "one that has been run", Pages: 1},
		[]string{"a"}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	s, err := New(Config{
		Store:        st,
		Logger:       quiet(),
		SettingsPath: settingsFile(t, settings.Settings{ControlURL: "http://127.0.0.1:1", APIKey: "k"}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.link.saw(reachGood)
	return s
}

// stopsOn is the walk as a page hands it over: where each stop stands, what it
// points at, and what it says.
func stopsOn(t *testing.T, body string) []stop {
	t.Helper()
	_, inside, ok := strings.Cut(body, `<div id="tour"`)
	if !ok {
		t.Fatalf("the page carries no walk at all:\n%s", body)
	}
	inside, _, ok = strings.Cut(inside, "</div>")
	if !ok {
		t.Fatalf("the walk never closes:\n%s", body)
	}
	var found []stop
	for _, tag := range strings.Split(inside, "<span ")[1:] {
		tag, _, _ = strings.Cut(tag, ">")
		one := stop{}
		for _, pair := range []struct {
			name string
			into *string
		}{
			{"data-at", &one.At},
			{"data-anchor", &one.Anchor},
			{"data-title", &one.Title},
			{"data-said", &one.Said},
		} {
			_, value, _ := strings.Cut(tag, pair.name+`="`)
			value, _, _ = strings.Cut(value, `"`)
			*pair.into = value
		}
		found = append(found, one)
	}
	return found
}

func TestTour_PointsAtSomethingThatIsActuallyOnTheScreenItNames(t *testing.T) {
	// This is the whole of what can go wrong quietly. A stop naming a screen and
	// a thing on it is a promise about markup written somewhere else, and the day
	// somebody renames that thing the walk goes on running and points at nothing
	// — with no error anywhere, because a walk that cannot find its anchor draws
	// the note and no ring.
	s := walked(t)
	all := stopsOn(t, get(t, s, stateAt).Body.String())
	if len(all) < 5 {
		t.Fatalf("the walk has %d stops, which is not a walk", len(all))
	}
	for _, one := range all {
		rec := get(t, s, one.At)
		if rec.Code != http.StatusOK {
			t.Errorf("a stop stands on %s, which answers %d", one.At, rec.Code)
			continue
		}
		// Every anchor is a name of its own, so this is the whole of the check:
		// a stop that pointed at a shape rather than at a name could not be
		// checked here at all, and would be the kind that breaks quietly.
		if !strings.HasPrefix(one.Anchor, "#") {
			t.Errorf("the stop on %s points at %q, which is not a name", one.At, one.Anchor)
			continue
		}
		if want := `id="` + strings.TrimPrefix(one.Anchor, "#") + `"`; !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the stop on %s points at %s, and there is no %s on that screen",
				one.At, one.Anchor, want)
		}
	}
}

func TestTour_SaysOneSentenceAStopInTheReadersOwnLanguage(t *testing.T) {
	// The script carries no words. A phrase written into it is a phrase the
	// catalogue does not hold and nobody translates, so every word the walk says
	// is put on the page by the server, already in the language the page is
	// written in.
	s := walked(t)
	for _, lang := range []struct {
		asked string
		say   Lang
	}{{"en", LangEN}, {"ru", LangRU}} {
		for _, one := range stopsOn(t, get(t, s, stateAt+"?"+langQuery+"="+lang.asked).Body.String()) {
			if one.Title == "" || one.Said == "" {
				t.Errorf("a stop on %s says nothing in %s", one.At, lang.asked)
				continue
			}
			if one.Title == lang.say.T(one.Title) && strings.HasPrefix(one.Title, "tour.") {
				t.Errorf("a stop on %s carries the key %q rather than the phrase", one.At, one.Title)
			}
		}
		body := get(t, s, stateAt+"?"+langQuery+"="+lang.asked).Body.String()
		if want := lang.say.T("tour.key.said"); !strings.Contains(body, want) {
			t.Errorf("the walk in %s does not say %q", lang.asked, want)
		}
	}
}

func TestTour_StartsItselfOnAMachineNothingHasBeenRunOn(t *testing.T) {
	// The screen this program opens on is where the work is watched, and on a
	// fresh machine there is no work: a column of noughts and a pool of nothing,
	// which reads as a program that is broken rather than one nobody has told
	// anything yet. So the walk starts itself there — and nowhere else, because a
	// run that fails at four in the morning must not put a beginner's walk in
	// front of the operator watching it.
	fresh, _ := freshMachine(t)
	if !strings.Contains(get(t, fresh, stateAt).Body.String(), "data-tour-now") {
		t.Error("a machine nothing has been run on is not offered the walk")
	}
	if strings.Contains(get(t, walked(t), stateAt).Body.String(), "data-tour-now") {
		t.Error("a machine with a job behind it starts a beginner's walk anyway")
	}
}

func TestTour_IsOverWhenTheReaderSaysSoAndStaysOver(t *testing.T) {
	// Closed on the first stop or walked to the end, it is the same press and the
	// same answer: written down, so the next screen does not start it again.
	//
	// It is a press and not a link, because it writes. A link that changed what
	// is on this machine is a link anything walking these pages would pull, and
	// the walk would be over before its reader saw the first stop.
	s, path := freshMachine(t)
	if code := get(t, s, guideDoneAt).Code; code == http.StatusNoContent {
		t.Error("the walk can be ended by fetching an address")
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, guideDoneAt, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("the press answered %d", rec.Code)
	}
	saved, err := settings.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !saved.GuideDone {
		t.Error("nothing was written down, so the walk starts itself again")
	}
	if strings.Contains(get(t, s, stateAt).Body.String(), "data-tour-now") {
		t.Error("the walk starts itself after being told it is over")
	}
	// And it is still there for whoever wants it again.
	if !strings.Contains(get(t, s, stateAt).Body.String(), "data-tour") {
		t.Error("the walk is gone once it is closed")
	}
}

func TestTour_IsOfferedFromEveryScreen(t *testing.T) {
	// Whoever closed it on the first afternoon is the same person who wants it a
	// week later, setting the second machine up.
	s := walked(t)
	for _, at := range []string{stateAt, jobsAt, newAt, proxiesAt, historyAt, settingsAt} {
		body := get(t, s, at).Body.String()
		if !strings.Contains(body, "data-tour>") {
			t.Errorf("%s offers no way to start the walk:\n%s", at, oneTag(t, body, "header"))
		}
		if len(stopsOn(t, body)) == 0 {
			t.Errorf("%s carries the press but not the walk", at)
		}
	}
}

// freshMachine is a machine as it comes: settings that name nothing, no profile,
// no job.
func freshMachine(t *testing.T) (*Server, string) {
	t.Helper()
	path := settingsFile(t, settings.Settings{})
	s, err := New(Config{Store: testStore(t), Logger: quiet(), SettingsPath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, path
}
