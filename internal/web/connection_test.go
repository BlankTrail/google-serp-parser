// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"html"
	"net/http"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// bannersOn is every banner a screen carries: the sentence it says and where
// its press leads.
type bannerShown struct {
	Said string
	At   string
}

func bannersOn(t *testing.T, body string) []bannerShown {
	t.Helper()
	var found []bannerShown
	for _, after := range strings.Split(body, `<div class="banner">`)[1:] {
		one, _, ok := strings.Cut(after, "</div>")
		if !ok {
			t.Fatalf("a banner never closes:\n%s", body)
		}
		_, said, _ := strings.Cut(one, "<p>")
		said, _, _ = strings.Cut(said, "</p>")
		_, at, _ := strings.Cut(one, `<a href="`)
		at, _, _ = strings.Cut(at, `"`)
		// Read back through the escaping the template put in: a sentence carrying
		// an apostrophe leaves the server as &#39; and is compared here against
		// the phrase as the catalogue holds it.
		found = append(found, bannerShown{Said: html.UnescapeString(strings.TrimSpace(said)), At: at})
	}
	return found
}

// saidOn is the one banner sentence a screen carries about the given phrase,
// and whether it carries one at all.
func saidOn(t *testing.T, body, key string) (bannerShown, bool) {
	t.Helper()
	for _, one := range bannersOn(t, body) {
		if one.Said == LangEN.T(key) {
			return one, true
		}
	}
	return bannerShown{}, false
}

// listing is a profile that names somewhere to go out through, which is what
// makes a machine ready to run anything.
func listing() store.Profile {
	return store.Profile{Name: "the list", Kind: "url", Location: "http://127.0.0.1:1/list", Default: true}
}

// withProfile is a server keeping settings, with the default profile it is
// handed already written down, on a machine that has been used.
//
// Used, because a machine that has never run anything opens on the quick start
// instead of on what is happening, and these are about the banners a machine in
// use puts up.
func withProfile(t *testing.T, p store.Profile) *Server {
	t.Helper()
	st := testStore(t)
	if _, err := st.CreateProfile(context.Background(), p); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if _, err := st.CreateJob(context.Background(),
		store.JobSpec{Name: "one that has been run", Pages: 1}, []string{"a"}); err != nil {
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
	return s
}

func TestConnection_SaysNothingAboutAConnectionNobodyHasAskedAbout(t *testing.T) {
	// A server that has not finished asking, and one that keeps no settings and
	// so has no connection of its own, both say nothing. The alternative is a
	// program that opens every morning with "BlankTrail is not answering" for as
	// long as the first check takes, which is an interface crying wolf on every
	// start.
	s := withProfile(t, listing())
	body := get(t, s, jobsAt).Body.String()

	if strings.Contains(body, "BlankTrail:") {
		t.Errorf("the header reports on a connection nothing is known about:\n%s", oneTag(t, body, "header"))
	}
	for _, key := range []string{"notice.unset", "notice.silent", "notice.refused", "notice.inactive"} {
		if one, drawn := saidOn(t, body, key); drawn {
			t.Errorf("a screen says %q before anything has been asked", one.Said)
		}
	}
}

func TestConnection_NamesWhatItLastLearnedOnEveryScreenAndSendsTheReaderToTheSettings(t *testing.T) {
	// The state is on every screen because it is true on every screen. The
	// banner is not, on one of them: the settings are where this is put right,
	// and a banner offering to take the reader where they already are reads as a
	// press that does nothing.
	for _, seen := range []struct {
		reach reach
		key   string
	}{
		{reachUnset, "notice.unset"},
		{reachSilent, "notice.silent"},
		{reachRefused, "notice.refused"},
		{reachInactive, "notice.inactive"},
	} {
		s := withProfile(t, listing())
		s.link.saw(seen.reach)

		for _, at := range []string{stateAt, jobsAt, newAt, historyAt} {
			body := get(t, s, at).Body.String()
			if want := "BlankTrail: " + LangEN.T(string(seen.reach)); !strings.Contains(body, want) {
				t.Errorf("%s does not say %q:\n%s", at, want, oneTag(t, body, "header"))
			}
			one, drawn := saidOn(t, body, seen.key)
			if !drawn {
				t.Errorf("%s puts up no banner about a connection that is %q", at, seen.reach)
				continue
			}
			if one.At != settingsAt {
				t.Errorf("%s sends a reader whose connection is %q to %s", at, seen.reach, one.At)
			}
		}

		if one, drawn := saidOn(t, get(t, s, settingsAt).Body.String(), seen.key); drawn {
			t.Errorf("the settings screen offers to take the reader to itself: %q -> %s", one.Said, one.At)
		}
	}
}

func TestConnection_SaysNothingWhileTheConnectionWorks(t *testing.T) {
	// A connection that works is not news. It is named in the header, where
	// somebody looking for it will find it, and nothing is put in front of the
	// screen about it.
	s := withProfile(t, listing())
	s.link.saw(reachGood)

	body := get(t, s, jobsAt).Body.String()
	if want := "BlankTrail: " + LangEN.T(string(reachGood)); !strings.Contains(body, want) {
		t.Errorf("the header does not say %q:\n%s", want, oneTag(t, body, "header"))
	}
	if strings.Contains(body, `class="banners"`) {
		t.Errorf("a working machine is met with a banner:\n%s", body)
	}
}

func TestConnection_TellsTheWaysAServiceCanBeUnreachableApart(t *testing.T) {
	// The three are fixed differently and have to be named differently. A
	// refused key reported as a service that is not answering sends the reader
	// to start something that is already running; a licence that will not open a
	// port reported as a refused key sends them to copy a key that is already
	// right.
	fake := fakebt.New(t)

	for _, one := range []struct {
		said  string
		saved settings.Settings
		set   func()
		want  reach
	}{
		{
			said:  "a service that answers and takes the key",
			saved: settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()},
			want:  reachGood,
		},
		{
			said:  "a key the service will not take",
			saved: settings.Settings{ControlURL: fake.URL(), APIKey: "not-the-key"},
			want:  reachRefused,
		},
		{
			said:  "nothing listening at the address",
			saved: settings.Settings{ControlURL: "http://127.0.0.1:1", APIKey: fake.Key()},
			want:  reachSilent,
		},
		{
			said:  "a licence that will not open a port",
			saved: settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()},
			set:   func() { fake.SetLicense(fakebt.License{Activated: false}) },
			want:  reachInactive,
		},
		{
			said:  "an address and a key nobody has filled in",
			saved: settings.Settings{},
			want:  reachUnset,
		},
	} {
		if one.set != nil {
			one.set()
		}
		s, err := New(Config{Store: testStore(t), Logger: quiet(), SettingsPath: settingsFile(t, one.saved)})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if got := s.probe(context.Background()); got != one.want {
			t.Errorf("%s was read as %q, want %q", one.said, got, one.want)
		}
	}
}

func TestExits_SayWhenAJobWouldGoOutFromThisMachinesOwnAddress(t *testing.T) {
	// The profile a job naming none runs on is written on the first start out of
	// whatever the settings held, and on a fresh machine they hold nothing. So
	// the profile is there, it is the default, and every request goes out from
	// the address the operator is sitting at — which is the one thing this
	// program exists to avoid, and the one thing no screen used to say.
	empty := store.Profile{Name: "Default", Default: true}
	s := withProfile(t, empty)

	body := get(t, s, jobsAt).Body.String()
	one, drawn := saidOn(t, body, "notice.profile.empty")
	if !drawn {
		t.Fatalf("nothing says the default profile has no exits:\n%s", body)
	}
	// The press leads to that profile in the form rather than to the list of
	// them: the boxes to fill in are what the reader is being sent for.
	if !strings.Contains(one.At, profileField+"=") {
		t.Errorf("the press leads to %s, which is not that profile's own boxes", one.At)
	}

	// And it goes when the profile names somewhere to go out through.
	filled := withProfile(t, listing())
	if one, drawn := saidOn(t, get(t, filled, jobsAt).Body.String(), "notice.profile.empty"); drawn {
		t.Errorf("a profile with a list is still reported as empty: %q", one.Said)
	}
}

func TestExits_SayWhenThereIsNoProfileAtAllAndLeadToWhereOneIsMade(t *testing.T) {
	// A database nothing has ever carried a profile into. The way out is the
	// list of profiles rather than one profile's boxes, because there is no
	// profile to put boxes on.
	s, err := New(Config{
		Store:        testStore(t),
		Logger:       quiet(),
		SettingsPath: settingsFile(t, settings.Settings{ControlURL: "http://127.0.0.1:1", APIKey: "k"}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	one, drawn := saidOn(t, get(t, s, jobsAt).Body.String(), "notice.profile.none")
	if !drawn {
		t.Fatal("a machine with no proxy profile at all says nothing about it")
	}
	if one.At != proxiesAt {
		t.Errorf("the press leads to %s rather than to the profiles", one.At)
	}
}

func TestExits_AreNotReportedOnTheScreenTheyAreSetUpOn(t *testing.T) {
	// The reader is already there, with the boxes in front of them.
	s := withProfile(t, store.Profile{Name: "Default", Default: true})
	rec := get(t, s, proxiesAt)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s gave %d, want 200", proxiesAt, rec.Code)
	}
	if one, drawn := saidOn(t, rec.Body.String(), "notice.profile.empty"); drawn {
		t.Errorf("the proxies screen offers to take the reader to itself: %q -> %s", one.Said, one.At)
	}
}

// markOn is the connection mark as the header draws it: which of the two it is,
// and what it says in words.
func markOn(t *testing.T, body string) (string, string) {
	t.Helper()
	_, inside, ok := strings.Cut(body, `<span class="reach `)
	if !ok {
		return "", ""
	}
	inside, _, ok = strings.Cut(inside, "</span>")
	if !ok {
		t.Fatalf("the mark never closes:\n%s", body)
	}
	which, rest, _ := strings.Cut(inside, `"`)
	_, said, _ := strings.Cut(rest, `aria-label="`)
	said, _, _ = strings.Cut(said, `"`)
	return strings.TrimSpace(which), html.UnescapeString(said)
}

func TestConnection_IsAMarkRatherThanAWordInTheHeader(t *testing.T) {
	// The fact every other screen depends on, in the corner of the eye of
	// somebody watching a run: a tick where the service answers and a cross where
	// it does not.
	//
	// The two differ by shape as well as by colour — a reader who cannot tell
	// green from red is left with the shape — and what it means is written out
	// as the mark's own name, which is what a screen reader says and what the
	// pointer rests on.
	s := withProfile(t, listing())
	for _, one := range []struct {
		reach reach
		want  string
	}{
		{reachGood, "reach-good"},
		{reachSilent, "reach-bad"},
		{reachRefused, "reach-bad"},
		{reachInactive, "reach-bad"},
		{reachUnset, "reach-bad"},
	} {
		s.link.saw(one.reach)
		which, said := markOn(t, get(t, s, jobsAt).Body.String())
		if which != one.want {
			t.Errorf("a connection that is %q is drawn as %q, want %q", one.reach, which, one.want)
		}
		if want := "BlankTrail: " + LangEN.T(string(one.reach)); said != want {
			t.Errorf("the mark for %q is named %q, want %q", one.reach, said, want)
		}
	}

	// And nothing at all before anything has been asked.
	quiet := withProfile(t, listing())
	if which, _ := markOn(t, get(t, quiet, jobsAt).Body.String()); which != "" {
		t.Errorf("a connection nothing is known about is drawn as %q", which)
	}
}
