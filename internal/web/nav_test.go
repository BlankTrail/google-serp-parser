// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// oneTag cuts one element out of a page and hands back what stands inside it,
// its own attributes included.
//
// A test reads the part of the page it is about rather than asking whether the
// whole document mentions something somewhere: a page holding the right word in
// the wrong place answers a search over the whole of it exactly as the right
// page does.
func oneTag(t *testing.T, body, name string) string {
	t.Helper()
	_, inside, ok := strings.Cut(body, "<"+name)
	if !ok {
		t.Fatalf("the page carries no <%s>:\n%s", name, body)
	}
	inside, _, ok = strings.Cut(inside, "</"+name+">")
	if !ok {
		t.Fatalf("the page never closes its <%s>:\n%s", name, body)
	}
	return inside
}

// tabShown is one tab as a page hands it to the reader: where it goes, and
// whether the page marks it as the screen being read.
type tabShown struct {
	At      string
	Current bool
}

// tabsOffered reads the header's tabs off a page.
func tabsOffered(t *testing.T, body string) []tabShown {
	t.Helper()
	strip := oneTag(t, body, "nav")
	if !strings.Contains(strip, `id="`+tabsAnchor+`"`) {
		t.Fatalf("the first navigation on the page is not the tabs:\n%s", strip)
	}
	var found []tabShown
	for _, tag := range tagsOf(strip, "a") {
		_, at, ok := strings.Cut(tag, `href="`)
		if !ok {
			t.Fatalf("a tab goes nowhere: <a%s>", tag)
		}
		at, _, _ = strings.Cut(at, `"`)
		found = append(found, tabShown{At: at, Current: strings.Contains(tag, `aria-current="page"`)})
	}
	if len(found) == 0 {
		t.Fatalf("the header offers no tab at all:\n%s", strip)
	}
	return found
}

// currentOf is the one tab a page marks as the screen being read, and says
// whether exactly one was marked.
func currentOf(t *testing.T, body string) (string, bool) {
	t.Helper()
	var marked []string
	for _, tab := range tabsOffered(t, body) {
		if tab.Current {
			marked = append(marked, tab.At)
		}
	}
	if len(marked) != 1 {
		return "", false
	}
	return marked[0], true
}

func TestTabs_AreWholePagesDrawnByTheServerAtTheirOwnAddresses(t *testing.T) {
	// Every tab is fetched, drawn and finished on the server. That is what makes
	// the first paint immediate, what makes a link to a screen worth sending to
	// whoever is on the next shift, and what leaves every screen readable in a
	// browser that runs no script at all.
	s := testServer(t)
	if len(tabs) < 3 {
		t.Fatal("the header offers fewer than three screens, so this test read nothing")
	}
	for _, tab := range tabs {
		rec := get(t, s, tab.At)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s gave %d, want 200", tab.At, rec.Code)
			continue
		}
		body := rec.Body.String()
		if !strings.HasPrefix(body, "<!doctype html>") {
			t.Errorf("%s does not begin a document of its own:\n%s", tab.At, body)
		}
		if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
			t.Errorf("%s is not a finished document:\n%s", tab.At, body)
		}
		if title := oneTag(t, body, "title"); !strings.Contains(title, LangEN.T(tab.Key)) {
			t.Errorf("%s is titled %q, which does not name the screen it draws", tab.At, title)
		}
	}
}

func TestTabs_MarkTheScreenBeingReadAndOfferEveryOther(t *testing.T) {
	// Which tab is lit is a decision, so the server makes it and writes it into
	// the markup. The script that swaps a screen copies the header that came with
	// it and works nothing out, which is why a swapped screen is lit exactly as a
	// fetched one.
	//
	// Every other tab is offered from every screen as well: a header that dropped
	// the tabs it is not on would leave the reader one screen with no way back.
	s := testServer(t)
	for _, tab := range tabs {
		body := get(t, s, tab.At).Body.String()

		reachable := map[string]bool{}
		for _, shown := range tabsOffered(t, body) {
			reachable[shown.At] = true
		}
		for _, other := range tabs {
			if !reachable[other.At] {
				t.Errorf("%s does not offer %s, so a reader on it cannot reach that screen", tab.At, other.At)
			}
		}

		lit, only := currentOf(t, body)
		if !only {
			t.Errorf("%s does not mark exactly one tab as the screen being read:\n%s",
				tab.At, oneTag(t, body, "nav"))
			continue
		}
		if lit != tab.At {
			t.Errorf("%s lights the tab at %s", tab.At, lit)
		}
	}
}

func TestTabs_KeepTheListOfJobsLitWhileOneOfThoseJobsIsBeingRead(t *testing.T) {
	// A job's own page is reached from the list of jobs and belongs to it. A
	// header that went dark there would tell the reader they had left the program.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 3, 2, 1)

	lit, only := currentOf(t, get(t, s, jobPath(id)).Body.String())
	if !only {
		t.Fatalf("the page of a job marks no single tab as the screen being read")
	}
	if lit != jobsAt {
		t.Errorf("the page of a job lights the tab at %s, and a job belongs to %s", lit, jobsAt)
	}
}

func TestScreens_CarryTheAnchorsAFetchedScreenIsPutInto(t *testing.T) {
	// The script takes the screen and the header out of what it fetched and puts
	// them where the old ones stood, and it takes a press over on the tabs alone.
	// Both sides of each of those are named here, because renaming one of them by
	// itself leaves a script that quietly swaps nothing and a reader who notices
	// only that the address changed and the screen did not.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 3, 2, 1)
	script := mustAsset(t, "static/app.js")

	for _, anchor := range []string{pageAnchor, headerAnchor, tabsAnchor} {
		if !strings.Contains(script, `"`+anchor+`"`) {
			t.Errorf("the script never names %q, so it does not put a fetched screen anywhere", anchor)
		}
		for _, at := range append(addresses(), jobPath(id)) {
			if body := get(t, s, at).Body.String(); !strings.Contains(body, `id="`+anchor+`"`) {
				t.Errorf("%s carries no %q for a fetched screen to be put into", at, anchor)
			}
		}
	}
}

// addresses is every tab address, as a plain list a test can walk.
func addresses() []string {
	at := make([]string, 0, len(tabs))
	for _, tab := range tabs {
		at = append(at, tab.At)
	}
	return at
}

func TestScreens_LoadOneDeferredScriptAndCarryNothingElseThatRuns(t *testing.T) {
	// One script, out of this binary, after the page it improves. Anything run
	// from an attribute in the markup is a second script nobody can read as one,
	// and it is beyond every test written about the file this program ships.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 3, 2, 1)

	for _, at := range append(addresses(), jobPath(id)) {
		body := get(t, s, at).Body.String()
		tags := tagsOf(body, "script")
		if len(tags) != 1 {
			t.Errorf("%s carries %d scripts, want the one this program ships", at, len(tags))
			continue
		}
		if !strings.Contains(tags[0], `src="/assets/app.js"`) {
			t.Errorf("%s loads a script that is not the one in this binary: <script%s>", at, tags[0])
		}
		if !strings.Contains(tags[0], "defer") {
			t.Errorf("%s loads its script ahead of the page it improves: <script%s>", at, tags[0])
		}
		for _, run := range []string{" onclick=", " onload=", " onsubmit=", " onchange=", " onerror=", "javascript:"} {
			if strings.Contains(body, run) {
				t.Errorf("%s runs something from its own markup at %q", at, run)
			}
		}
	}
}

func TestScript_WritesEverySwappedScreenIntoTheHistoryTheBackButtonReads(t *testing.T) {
	// Swapping a screen without writing the address down breaks the one
	// expectation every reader has: that back goes back. No test here executes a
	// line of this file — there is no runtime to execute it with — so what is
	// pinned is that the file which swaps screens is also the file that writes
	// them down and answers when the reader steps back through them.
	script := mustAsset(t, "static/app.js")
	for _, part := range []string{"pushState", "popstate"} {
		if !strings.Contains(script, part) {
			t.Errorf("the script never uses %s, so a swapped screen leaves no way back to the one before it", part)
		}
	}
}

func TestState_AsksForItselfAgainOnlyWhileSomethingIsRunning(t *testing.T) {
	// The whole screen comes back from the server every time, so none of it can
	// be half new: there is nothing in the browser that could move the counts of
	// a job and leave the ports and the queue as they were an hour ago.
	//
	// And it stops asking when there is nothing to watch. A screen left open on
	// an idle server would otherwise ask all night for a drawing of numbers that
	// cannot change until somebody presses something.
	running, _, _ := stateServer(t)
	busy := oneTag(t, get(t, running, stateAt).Body.String(), "main")
	want := `data-refresh="` + strconv.FormatInt(stateRefresh.Milliseconds(), 10) + `"`
	if !strings.Contains(busy, want) {
		t.Errorf("the screen with a job running asks for nothing more: <main%s", busy)
	}

	idle := oneTag(t, get(t, idleServer(t), stateAt).Body.String(), "main")
	if strings.Contains(idle, "data-refresh") {
		t.Errorf("the screen goes on asking with nothing running: <main%s", idle)
	}
}

func TestJobs_HaveAnAddressOfTheirOwnSoTheScreenAnOperatorWatchesOpensFirst(t *testing.T) {
	// Whoever has this open all day is following a run, not reading a list, so
	// what is happening is what the bare address answers with. The list keeps
	// everything it had and gains an address that can be sent to somebody.
	s := testServer(t)
	seedJob(t, s, "nightly", 3, 2, 1)

	home := get(t, s, stateAt)
	if home.Code != http.StatusOK {
		t.Fatalf("GET %s gave %d, want 200", stateAt, home.Code)
	}
	opened := home.Body.String()
	if heading := oneTag(t, opened, "h1"); !strings.Contains(heading, LangEN.T("state.title")) {
		t.Errorf("%s opens on %q, and an operator came to see what is happening", stateAt, heading)
	}
	if strings.Contains(opened, "nightly") {
		t.Errorf("%s lists the jobs, which is the screen that now has its own address:\n%s", stateAt, opened)
	}

	list := get(t, s, jobsAt)
	if list.Code != http.StatusOK {
		t.Fatalf("GET %s gave %d, want 200", jobsAt, list.Code)
	}
	listed := list.Body.String()
	if heading := oneTag(t, listed, "h1"); !strings.Contains(heading, LangEN.T("jobs.title")) {
		t.Errorf("%s is headed %q, which is not the list of jobs", jobsAt, heading)
	}
	if !strings.Contains(listed, "nightly") {
		t.Errorf("%s does not list the job the history holds:\n%s", jobsAt, listed)
	}
}

func TestStateScreen_OffersToStopTheJobItIsShowing(t *testing.T) {
	// Stop belongs on the screen the run is being watched from. Until now it
	// stood on the job's own page alone, so an operator watching a run had to
	// leave the screen they were watching to end what they were watching.
	running, _, id := stateServer(t)
	watched := get(t, running, stateAt).Body.String()

	_, form, ok := strings.Cut(watched, `action="/api/stop"`)
	if !ok {
		t.Fatalf("the screen shows a running job and does not offer to stop it:\n%s", watched)
	}
	form, _, _ = strings.Cut(form, "</form>")
	if !strings.Contains(form, `value="`+strconv.FormatInt(id, 10)+`"`) {
		t.Errorf("the stop on the screen names no job, or the wrong one:%s", form)
	}
	if !strings.Contains(form, `value="`+stateAt+`"`) {
		t.Errorf("the stop on the screen does not say to come back to it:%s", form)
	}

	if idle := get(t, idleServer(t), stateAt).Body.String(); strings.Contains(idle, `action="/api/stop"`) {
		t.Errorf("the screen offers to stop something with nothing running:\n%s", idle)
	}
}

func TestStop_LeavesTheOperatorOnTheScreenItWasPressedOn(t *testing.T) {
	// A stop that moves the reader off the screen they were watching takes that
	// screen away at the moment they most want it. The address to come back to
	// travels with the press, and only an address this program draws itself is
	// honoured: a form posted from anywhere else must not be able to use this
	// program as a way of sending somebody to a machine of its choosing.
	for _, c := range []struct {
		name string
		back string
		want string
	}{
		{"the screen an operator watches", stateAt, stateAt},
		{"nothing at all", "", ""},
		{"a machine of somebody else's", "https://elsewhere.example/", ""},
		{"an address dressed as this one", "//elsewhere.example/", ""},
		{"a screen this program does not draw", "/nowhere", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, v, _ := heldServer(t)
			id := enqueue(t, v, "nightly", "a", "b", "c")
			waitUntil(t, "the job is running", func() bool {
				got, ok := v.Running()
				return ok && got == id
			})

			pressed := url.Values{"job": {strconv.FormatInt(id, 10)}}
			if c.back != "" {
				pressed.Set(backField, c.back)
			}
			rec := postForm(t, s, "/api/stop", pressed)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("the press gave %d, want 303", rec.Code)
			}
			want := c.want
			if want == "" {
				want = jobPath(id)
			}
			if got := rec.Header().Get("Location"); got != want {
				t.Errorf("the press sent the reader to %q, want %q", got, want)
			}
		})
	}
}

func TestScreens_AskAgainOnlyWhileSomethingCanAnswerDifferently(t *testing.T) {
	// Three screens follow a run, and each has to stop on its own. A page left
	// open overnight on a machine running nothing would otherwise fetch itself
	// until morning, and every answer would say what the last one said.
	s, v, _ := heldServer(t)

	// Nothing running: none of them asks for anything.
	for _, at := range []string{stateAt, jobsAt} {
		if body := get(t, s, at).Body.String(); strings.Contains(body, "data-refresh") {
			t.Errorf("%s asks to be drawn again with nothing running:\n%s", at, body)
		}
	}

	id := enqueue(t, v, "nightly", "a", "b")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})
	for _, at := range []string{stateAt, jobsAt, jobPath(id)} {
		if body := get(t, s, at).Body.String(); !strings.Contains(body, "data-refresh") {
			t.Errorf("%s does not follow a job that is running:\n%s", at, body)
		}
	}

	if err := v.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitUntil(t, "the job has stopped", func() bool { _, ok := v.Running(); return !ok })
	for _, at := range []string{stateAt, jobsAt, jobPath(id)} {
		if body := get(t, s, at).Body.String(); strings.Contains(body, "data-refresh") {
			t.Errorf("%s keeps asking after the job stopped:\n%s", at, body)
		}
	}
}

func TestScript_AsksNothingWhileNobodyIsLookingAtTheTab(t *testing.T) {
	// A run takes hours and a browser holds a dozen tabs. One that kept fetching
	// a whole screen every few seconds behind another window would spend the
	// afternoon drawing pages nobody sees.
	//
	// No line of the script runs here — there is no runtime to run it with — so
	// what is pinned is that the file consults whether the tab is being looked at
	// and listens for that changing. Both halves are needed: without the second,
	// a tab that went away never starts again.
	script := mustAsset(t, "static/app.js")
	for _, part := range []string{"document.hidden", "visibilitychange"} {
		if !strings.Contains(script, part) {
			t.Errorf("the script never mentions %s, so a hidden tab is fetched like any other", part)
		}
	}
	// One listener for the whole file rather than one per screen: a reader who
	// moves between screens all afternoon would otherwise collect a listener for
	// every screen they have left.
	if got := strings.Count(script, `addEventListener("visibilitychange"`); got != 1 {
		t.Errorf("the script listens for the tab coming back %d times, want once", got)
	}
}

func TestScript_FollowsAScreenByFetchingTheWholeOfIt(t *testing.T) {
	// The counts used to be written in place, one figure at a time, which was
	// cheaper and could never show a result arriving. There is one mechanism now,
	// and what it puts on the page is a whole screen the server drew — so a count
	// and the results beside it are never from two different moments.
	script := mustAsset(t, "static/app.js")
	if strings.Contains(script, "/api/progress") {
		t.Error("the script still fetches counts on their own, which is a second " +
			"mechanism that can disagree with the first")
	}
	if !strings.Contains(script, "dataset.refresh") {
		t.Error("the script does not read how often the server said to ask")
	}
}
