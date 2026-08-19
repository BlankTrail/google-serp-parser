// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/blanktrail"
)

func TestProxies_ShowsTheReadingOfThePoolAndBreaksTheFailuresDown(t *testing.T) {
	// A pool going badly and a pool going well look identical from outside —
	// both are a number of ports and a rate — and the difference between them is
	// which of a handful of things is going wrong. A single total of failures
	// cannot say: a run failing on dead addresses wants another list, and one
	// being walled by the origin wants something else entirely, and both read
	// the same added together.
	rec := httptest.NewRecorder()
	testServerWithSupervisor(t).Handler().ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, proxiesAt, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Every kind is drawn, including the ones that have not happened: a row that
	// comes and goes is one the reader has to look for.
	for _, key := range []string{
		"proxies.kind.transport", "proxies.kind.relay",
		"proxies.kind.wall", "proxies.kind.timeout", "proxies.kind.other",
	} {
		phrase := catalogue[LangEN][key]
		if phrase == "" {
			t.Fatalf("the catalogue has nothing under %s", key)
		}
		if !strings.Contains(body, phrase) {
			t.Errorf("the screen does not name the failures called %q", phrase)
		}
	}

	// And the counts the fake pool reports are on it rather than a placeholder.
	for _, want := range []string{
		`id="ports">6<`,
		`id="quarantined">1<`,
		`id="rotations">4<`,
		`id="revivals">2<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the screen does not carry %s", want)
		}
	}

	// Nothing has gone through it, so the share is the mark that stands where a
	// figure would if there were anything to work one out from. Nought per cent
	// is a measurement somebody could act on; nothing has happened yet.
	if !strings.Contains(body, `id="share">`+noFigure+`<`) {
		t.Error("the screen shows a share of nothing as a figure rather than a mark")
	}
}

func TestProxies_IsOfferedAsATabOfItsOwn(t *testing.T) {
	// It is a screen somebody keeps open beside a run, so it has an address that
	// can be bookmarked and a tab that lights up on it.
	var found bool
	for _, tb := range tabs {
		if tb.At == proxiesAt {
			found = true
		}
	}
	if !found {
		t.Fatal("the header does not offer the proxy screen")
	}
	for _, link := range tabsFor(proxiesAt) {
		if link.URL == proxiesAt && !link.Current {
			t.Error("the proxy tab is not lit on the proxy screen")
		}
	}
}

func TestProxiesReset_ClearsTheCountsAndComesBackToTheScreen(t *testing.T) {
	// The button exists so a measurement can be started from a known point
	// without restarting anything, and it is a post so that a browser walking a
	// link cannot wipe somebody's reading.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/api/proxies/reset", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want a redirect back to the screen", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != proxiesAt {
		t.Errorf("the press lands on %q, want %q", got, proxiesAt)
	}

	// A get is refused: the reading is not something a prefetch may destroy.
	plain := httptest.NewRecorder()
	s.Handler().ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/api/proxies/reset", nil))
	if plain.Code == http.StatusSeeOther || plain.Code == http.StatusOK {
		t.Errorf("a walked link cleared the reading: status=%d", plain.Code)
	}
}

func TestShareOf_SaysNothingRatherThanNoughtPerCent(t *testing.T) {
	// A share of nothing is not nought per cent. Nought per cent is a
	// measurement somebody could act on, and nothing has been settled yet.
	if got := shareOf(0, 0); got != noFigure {
		t.Errorf("a share of nothing reads %q, want the mark", got)
	}
	if got := shareOf(1, 4); got != "25%" {
		t.Errorf("one in four reads %q, want 25%%", got)
	}
	if got := shareOf(0, 4); got != "0%" {
		t.Errorf("none of four reads %q, want 0%%", got)
	}
}

func TestFailureKeys_NamesEveryKindThePoolCanReport(t *testing.T) {
	// The screen draws whatever the pool counts. A kind added to the pool and
	// not named here would be drawn as an empty row, which reads as a kind that
	// never happens rather than as a phrase nobody wrote.
	for _, kind := range blanktrail.Failures {
		key, ok := failureKeys[kind]
		if !ok {
			t.Errorf("the screen has no name for the failures called %q", kind)
			continue
		}
		for _, l := range Languages() {
			if catalogue[l][key] == "" {
				t.Errorf("%s has nothing under %s", l, key)
			}
		}
	}
}
