// SPDX-License-Identifier: MIT

package web

// The addresses of the screens the header offers. Each is a page the server
// draws whole, so it can be opened cold, bookmarked and sent to whoever is on
// the next shift, and each is written down once here: the routes answer these,
// the header offers these, and a handler says which of them it stands under.
const (
	stateAt   = "/"
	jobsAt    = "/jobs"
	newAt     = "/new"
	historyAt = "/history"
)

// What the script works on, named here because the markup and the script have
// to agree about it: a name living in one of the two is a name that can be
// changed in that one alone, and the failure is a screen that quietly stops
// being swapped.
//
// The screen and the header are the two parts of a page that differ from one
// address to the next — the header because it carries which tab is lit and
// because the language switcher in it offers this same address in the other
// language — so both are taken from what was fetched. The tabs are named
// separately because they are what a press is taken over on: a press on the
// language switcher is a plain one, since the language a page is written in is
// declared in markup this never touches.
const (
	pageAnchor   = "page"
	headerAnchor = "header"
	tabsAnchor   = "tabs"
)

// tab is one screen the header offers.
type tab struct {
	// Key names the screen in the catalogue rather than in a language, like
	// every other phrase on every page.
	Key string
	// At is the screen's address.
	At string
}

// tabs is the header, in the order it is read.
//
// What is happening comes first because it is what the reader opened this for:
// whoever has the program up all day is following a run, not filling in a form.
var tabs = []tab{
	{Key: "state.title", At: stateAt},
	{Key: "jobs.title", At: jobsAt},
	{Key: "new.title", At: newAt},
	{Key: "history.title", At: historyAt},
}

// tabLink is one tab as the header draws it.
type tabLink struct {
	Key     string
	URL     string
	Current bool
}

// tabsFor is the header of a page standing under the given tab.
//
// Which tab is lit is decided here, on the server, and written into the markup,
// because it is a decision and a decision can be tested. The script that swaps
// one screen for another copies the header that arrived with the screen and
// works nothing out for itself, so a swapped screen is lit exactly as a fetched
// one and there is no second place for the two to disagree.
//
// A page that is not a tab of its own names the tab it belongs to — a job's own
// page stands under the list of jobs — so the header never goes dark on a
// screen the reader reached from it.
func tabsFor(under string) []tabLink {
	links := make([]tabLink, 0, len(tabs))
	for _, t := range tabs {
		links = append(links, tabLink{Key: t.Key, URL: t.At, Current: t.At == under})
	}
	return links
}
