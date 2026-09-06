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
// goes and what it is called.
//
// What it is called is read off the name on the link rather than off anything
// drawn inside it. The switch draws two marks and no words, and the name is
// what a reader who cannot see those marks is told — which makes it the one
// place the phrase has to be right.
//
// The address is unescaped because it is read out of markup and pressed as a
// link.
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
		at, rest, _ := strings.Cut(at, `"`)
		if !strings.HasPrefix(at, themeAt) {
			continue
		}
		_, said, ok := strings.Cut(rest, `aria-label="`)
		if !ok {
			t.Fatalf("the press that changes the theme carries no name:\n%s", link)
		}
		said, _, _ = strings.Cut(said, `"`)
		return html.UnescapeString(at), html.UnescapeString(strings.TrimSpace(said))
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

// switchOn is the whole of the press that changes the theme, as it stands in
// the markup.
func switchOn(t *testing.T, body string) string {
	t.Helper()
	_, inside, ok := strings.Cut(body, `<a class="lights"`)
	if !ok {
		t.Fatalf("no screen draws a switch for the lights:\n%s", body)
	}
	inside, _, ok = strings.Cut(inside, "</a>")
	if !ok {
		t.Fatalf("the switch never closes:\n%s", body)
	}
	return inside
}

func TestTheme_IsASwitchWhoseStateIsTakenFromTheDocument(t *testing.T) {
	// The switch draws the same thing in both themes. Which way the knob stands
	// and which mark is lit are taken from the theme the server has already
	// written on the document, by the one stylesheet — so there is no second
	// place for the two to disagree, and no way to end up with a switch showing
	// one theme over a page drawn in the other.
	s := testServer(t)
	light := switchOn(t, seenBy(t, s, jobsAt, "").Body.String())
	dark := switchOn(t, seenBy(t, s, jobsAt, themeDark).Body.String())

	// Everything but where it goes and what it is called, which are the two
	// things about it that are meant to differ.
	bare := func(markup string) string {
		var kept []string
		for _, line := range strings.Split(markup, "\n") {
			if strings.Contains(line, "href=") {
				continue
			}
			kept = append(kept, line)
		}
		return strings.Join(kept, "\n")
	}
	if bare(light) != bare(dark) {
		t.Errorf("the switch is drawn differently in the two themes:\nlight: %s\ndark:  %s", light, dark)
	}
	for _, mark := range []string{"lights-sun", "lights-moon", "lights-knob"} {
		if !strings.Contains(light, mark) {
			t.Errorf("the switch draws no %s", mark)
		}
	}

	// And the stylesheet is where the state lands: every part of the switch that
	// carries any of it is named again under the theme, so none of them can be
	// left behind pointing at the theme that was on a moment ago.
	for _, part := range []string{"lights-knob", "lights-sun", "lights-moon"} {
		var answers bool
		for _, d := range stylesheet(t) {
			if strings.Contains(d.Selector, `[data-theme="dark"]`) && strings.Contains(d.Selector, part) {
				answers = true
			}
		}
		if !answers {
			t.Errorf("%s reads the same in both themes, so the switch shows the wrong one in the dark", part)
		}
	}
}

func TestTheme_SaysInWordsWhatTheSwitchDoes(t *testing.T) {
	// Two marks and no words is fine for whoever can see them. The name on the
	// link is what everybody else is told, and it is the sentence rather than
	// the theme: a switch named after where it already is gets pressed by
	// everybody who wants to stay there.
	s := testServer(t)
	for _, one := range []struct {
		now  string
		want string
	}{
		{"", "theme.dark"},
		{themeDark, "theme.light"},
	} {
		body := seenBy(t, s, jobsAt, one.now).Body.String()
		if _, said := pressOf(t, body); said != LangEN.T(one.want) {
			t.Errorf("the switch is named %q, want %q", said, LangEN.T(one.want))
		}
		// Nothing but the marks stands inside it: a word left in there would be
		// read out after the name, and the two would drift apart the first time
		// one of them was changed.
		if said := textIn(switchOn(t, body)); said != "" {
			t.Errorf("the switch carries the word %q as well as its name", said)
		}
	}
}

// textIn is whatever an element would read out: what stands between its tags,
// with the tags themselves and everything written inside them dropped.
func textIn(element string) string {
	// Past the opening tag, so its own attributes are not read as text.
	_, rest, _ := strings.Cut(element, ">")
	var said strings.Builder
	for {
		before, after, ok := strings.Cut(rest, "<")
		said.WriteString(strings.TrimSpace(before))
		if !ok {
			break
		}
		if _, rest, ok = strings.Cut(after, ">"); !ok {
			break
		}
	}
	return said.String()
}
