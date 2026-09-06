// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The interface is light, and the dark one is a choice: what follows is how
// that choice is asked for, where it is written down, and how long it is kept.
//
// It is kept in a cookie for the reason the language is: it is about the reader
// and not about the machine. Two people may be watching one server from two
// desks, and neither of them turning the lights out should turn them out on the
// other.
//
// It is written down by the server rather than by a script so that the first
// page of a fresh tab arrives already in the theme that was chosen. A theme a
// script puts on after the page has drawn is a white flash before every screen,
// on a program somebody keeps open all day.
const (
	themeAt     = "/theme"
	themeField  = "to"
	themeCookie = "gserp_theme"
	themeMemory = 365 * 24 * time.Hour
)

// The two themes, by the names the query asks for them under and the document
// carries them under.
const (
	themeLight = "light"
	themeDark  = "dark"
)

// themeOf is the theme this page is drawn in.
//
// Only the dark one is ever written down, so anything else — a cookie from an
// older version, a cookie somebody typed, no cookie at all — is the light one.
// That is the whole of the rule: this interface is light, and the dark is what
// somebody asked for.
//
// What the machine round the browser prefers is not consulted. It used to be,
// through the stylesheet, and it was a guess about the room rather than a
// decision anybody made: an operator on a dark desktop had no way of reading
// this in the light.
func themeOf(r *http.Request) string {
	if c, err := r.Cookie(themeCookie); err == nil && c.Value == themeDark {
		return themeDark
	}
	return themeLight
}

// switchTheme writes a theme down and sends the reader back to the page they
// pressed it on.
//
// It is a screen of its own rather than a parameter every page understands,
// because changing the theme is something done once and reading a page is done
// all day: a parameter would end up in the address bar, in the bookmark made
// from it, and in the link sent to whoever is on the next shift.
func (s *Server) switchTheme(w http.ResponseWriter, r *http.Request) {
	to := themeLight
	if r.URL.Query().Get(themeField) == themeDark {
		to = themeDark
	}
	http.SetCookie(w, &http.Cookie{
		Name:     themeCookie,
		Value:    to,
		Path:     "/",
		MaxAge:   int(themeMemory / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, backHere(r), http.StatusSeeOther)
}

// backHere is the page the theme was changed from, and the screen this program
// opens on when that cannot be read.
func backHere(r *http.Request) string {
	asked := r.URL.Query().Get(backField)
	if !ownAddress(asked) {
		return stateAt
	}
	return asked
}

// ownAddress says whether an address names this server and no other.
//
// It is checked rather than trusted. The link carrying it is written by this
// program, but a link is only a suggestion once it is in an address bar, and a
// program that sent a reader wherever a parameter asked would be a way of
// pointing at somebody else's machine from an address this reader trusts.
//
// What is checked is that the address names no machine at all: no scheme, no
// host, and one leading slash rather than two, since "//elsewhere" is a host
// with the scheme left off. A backslash is refused outright — some browsers
// read it as a slash and some do not, and an address two of them read
// differently is an address this program will not honour.
func ownAddress(asked string) bool {
	if !strings.HasPrefix(asked, "/") || strings.HasPrefix(asked, "//") {
		return false
	}
	if strings.Contains(asked, `\`) {
		return false
	}
	at, err := url.Parse(asked)
	if err != nil || at.Scheme != "" || at.Host != "" {
		return false
	}
	return true
}

// themeLink is the press that changes the theme: where it goes, and the phrase
// naming the theme it goes to.
//
// The phrase names what pressing it does rather than what is on the screen now.
// A switch labelled with the state it is in is read as a switch that is already
// off, and half the people who read it press it to get what it says.
type themeLink struct {
	URL string
	Key string
}

// themeSwitch is that press as it stands on the page being drawn now. It
// carries this page's own address, because whoever presses it is reading this
// page and expects to go on reading it with the lights changed.
func themeSwitch(r *http.Request, now string) themeLink {
	to, key := themeDark, "theme.dark"
	if now == themeDark {
		to, key = themeLight, "theme.light"
	}
	asked := url.Values{themeField: {to}, backField: {addressOf(r)}}
	return themeLink{URL: themeAt + "?" + asked.Encode(), Key: key}
}

// addressOf is the address the page being drawn stands at, with whatever was
// asked for on it: a screen reached with a query is the screen the reader wants
// back, and one of those queries is the language this page is written in.
func addressOf(r *http.Request) string {
	at := r.URL.EscapedPath()
	if at == "" {
		return stateAt
	}
	if r.URL.RawQuery != "" {
		at += "?" + r.URL.RawQuery
	}
	return at
}
