// SPDX-License-Identifier: MIT

package web

import (
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seenBy drives the routes with a request carrying whatever a reader has already
// been given: the theme they chose, and nothing else this program writes down.
func seenBy(t *testing.T, s *Server, at, theme string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, at, nil)
	if theme != "" {
		req.AddCookie(&http.Cookie{Name: themeCookie, Value: theme})
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// documentOf is the opening tag of the page, which is where the theme is
// written. It is read rather than searched for anywhere in the body: a page
// that mentions the word somewhere is not a page drawn dark.
func documentOf(t *testing.T, body string) string {
	t.Helper()
	_, opened, ok := strings.Cut(body, "<html")
	if !ok {
		t.Fatalf("the answer is not a document:\n%s", body)
	}
	opened, _, ok = strings.Cut(opened, ">")
	if !ok {
		t.Fatalf("the document never opens:\n%s", body)
	}
	return opened
}

// pressOf is the press that changes the theme, as the header draws it: where it
// goes and what it is called. The address is unescaped because it is read out
// of markup and pressed as a link.
func pressOf(t *testing.T, body string) (string, string) {
	t.Helper()
	for _, after := range strings.Split(body, "<a ")[1:] {
		link, _, ok := strings.Cut(after, "</a>")
		if !ok {
			continue
		}
		_, at, ok := strings.Cut(link, `href="`)
		if !ok {
			continue
		}
		at, said, _ := strings.Cut(at, `"`)
		if !strings.HasPrefix(at, themeAt) {
			continue
		}
		_, said, _ = strings.Cut(said, ">")
		return html.UnescapeString(at), strings.TrimSpace(said)
	}
	t.Fatalf("no screen offers a way to change the theme:\n%s", body)
	return "", ""
}

func TestTheme_DrawsEveryScreenInTheLightUntilSomebodyAsksForOtherwise(t *testing.T) {
	// The interface is light. It is not light because the machine round the
	// browser said so at four in the afternoon and dark because it said so at
	// six: it is what this program is, and the dark is what somebody asked for.
	s := testServer(t)
	for _, tab := range tabs {
		body := seenBy(t, s, tab.At, "").Body.String()
		if doc := documentOf(t, body); strings.Contains(doc, "data-theme") {
			t.Errorf("%s is drawn as <html%s> for a reader who has asked for nothing", tab.At, doc)
		}
		if _, said := pressOf(t, body); said != LangEN.T("theme.dark") {
			t.Errorf("%s offers %q, and what a reader in the light can ask for is the dark", tab.At, said)
		}
	}
}

func TestTheme_DrawsTheDocumentDarkForAReaderWhoAskedForIt(t *testing.T) {
	// The theme is on the document rather than on anything drawn inside it, so
	// that every colour of every screen changes together and nothing on the page
	// has to know it happened.
	s := testServer(t)
	for _, tab := range tabs {
		body := seenBy(t, s, tab.At, themeDark).Body.String()
		if doc := documentOf(t, body); !strings.Contains(doc, `data-theme="dark"`) {
			t.Errorf("%s is drawn as <html%s> for a reader who asked for the dark", tab.At, doc)
		}
		if _, said := pressOf(t, body); said != LangEN.T("theme.light") {
			t.Errorf("%s offers %q, and what a reader in the dark can ask for is the light", tab.At, said)
		}
	}
}

func TestTheme_IsRememberedRatherThanAskedForOnEveryScreen(t *testing.T) {
	// A reader turns the lights out once. What is written down is written down
	// by the server, before the page leaves it, which is why the first screen of
	// a fresh tab is already dark rather than white for as long as a script
	// takes to run.
	s := testServer(t)
	at, _ := pressOf(t, seenBy(t, s, jobsAt, "").Body.String())

	rec := get(t, s, at)
	var kept *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == themeCookie {
			kept = c
		}
	}
	if kept == nil {
		t.Fatalf("pressing %s wrote nothing down, so the next page is light again", at)
	}
	if kept.Value != themeDark {
		t.Errorf("pressing it wrote down %q", kept.Value)
	}
	if kept.MaxAge <= 0 {
		t.Errorf("the choice is kept for %ds, which is until the browser is closed", kept.MaxAge)
	}
}

func TestTheme_SendsTheReaderBackToTheScreenTheyPressedItOn(t *testing.T) {
	// Somebody turning the lights out is reading something, and they expect to
	// go on reading it. A switch that landed them on the front page would be a
	// switch nobody presses twice.
	s := testServer(t)
	for _, at := range []string{jobsAt, historyAt, newAt + "?kind=index"} {
		press, _ := pressOf(t, seenBy(t, s, at, "").Body.String())
		rec := get(t, s, press)
		if rec.Code != http.StatusSeeOther {
			t.Errorf("pressing it on %s answered %d rather than sending the reader back", at, rec.Code)
			continue
		}
		if back := rec.Header().Get("Location"); back != at {
			t.Errorf("pressing it on %s sends the reader to %s", at, back)
		}
	}
}

func TestTheme_RefusesToSendTheReaderToAMachineThatIsNotThisOne(t *testing.T) {
	// The link is written by this program, and an address bar is where anybody
	// can write another one. A program that sent a reader wherever a parameter
	// asked would be a way of pointing at somebody else's machine from an
	// address this reader trusts.
	s := testServer(t)
	for _, elsewhere := range []string{
		"https://elsewhere.example/",
		"//elsewhere.example/",
		`/\elsewhere.example/`,
		"javascript:alert(1)",
		"",
	} {
		rec := get(t, s, themeAt+"?"+themeField+"="+themeDark+"&"+backField+"="+elsewhere)
		if back := rec.Header().Get("Location"); back != stateAt {
			t.Errorf("a press carrying %q sends the reader to %q", elsewhere, back)
		}
	}
}
