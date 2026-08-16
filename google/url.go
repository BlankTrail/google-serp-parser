// SPDX-License-Identifier: MIT

package google

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// Device is which kind of results are wanted. It is not a request parameter:
// Google infers the layout from the User-Agent, which this package does not
// set. It lives here because a caller needs one value describing the whole
// capture.
type Device string

const (
	// DeviceDesktop asks for the desktop layout.
	DeviceDesktop Device = "desktop"
	// DeviceMobile asks for the mobile layout.
	DeviceMobile Device = "mobile"
)

// ccTLD maps a country to the Google domain that serves it. The list is
// deliberately short: it covers the markets this tool is used in, and an
// unknown country falls back to .com with gl set rather than failing.
var ccTLD = map[string]string{
	"ru": "www.google.ru", "de": "www.google.de", "fr": "www.google.fr",
	"es": "www.google.es", "it": "www.google.it", "nl": "www.google.nl",
	"pl": "www.google.pl", "tr": "www.google.com.tr", "br": "www.google.com.br",
	"uk": "www.google.co.uk", "gb": "www.google.co.uk", "ua": "www.google.com.ua",
	"kz": "www.google.kz", "by": "www.google.by", "jp": "www.google.co.jp",
	"in": "www.google.co.in", "au": "www.google.com.au", "ca": "www.google.ca",
	"us": "www.google.com",
}

// Query is one capture: what to search for, and the four axes that decide
// which results come back. The axes are independent on purpose — "German
// results in English" is an ordinary request.
type Query struct {
	// Text is the search phrase.
	Text string
	// Country picks the Google domain and the gl parameter. Empty means .com.
	Country string
	// Language picks hl and the Accept-Language header. Empty means the
	// domain's own default.
	Language string
	// Device records which layout the capture is meant to be of. Nothing here
	// acts on it — see Device — so it travels with the query rather than
	// changing the request this package builds.
	Device Device
	// Page is 1-based. Google withdrew num=100, so depth is pages of ten.
	Page int
	// PerPage is how many results a page is expected to hold. It is a
	// declaration for cost estimates, not a request parameter: the real
	// number is whatever the page turns out to contain.
	PerPage int
}

// ErrEmptyQuery is returned when a Query carries no search text.
var ErrEmptyQuery = errors.New("google: query text is empty")

// URL renders the search request.
func (q Query) URL() (string, error) {
	text := strings.TrimSpace(q.Text)
	if text == "" {
		return "", ErrEmptyQuery
	}

	host := "www.google.com"
	country := strings.ToLower(strings.TrimSpace(q.Country))
	if h, ok := ccTLD[country]; ok {
		host = h
	}

	v := url.Values{}
	v.Set("q", text)
	if country != "" {
		v.Set("gl", country)
	}
	if lang := strings.ToLower(strings.TrimSpace(q.Language)); lang != "" {
		v.Set("hl", lang)
	}
	if q.Page > 1 {
		v.Set("start", strconv.Itoa((q.Page-1)*10))
	}

	u := url.URL{Scheme: "https", Host: host, Path: "/search", RawQuery: v.Encode()}
	return u.String(), nil
}

// AcceptLanguage is the header value this capture must send.
//
// Setting this header is this program's own responsibility, and it has to
// agree with hl: a page requested in one language by a client claiming to
// prefer another is a mismatch this program would be creating.
func (q Query) AcceptLanguage() string {
	lang := strings.TrimSpace(q.Language)
	if lang == "" {
		return "en-US,en;q=0.9"
	}

	base, region := lang, ""
	if i := strings.IndexAny(lang, "-_"); i > 0 {
		base, region = lang[:i], lang[i+1:]
	}
	base = strings.ToLower(base)

	if region != "" {
		return base + "-" + strings.ToUpper(region) + "," + base + ";q=0.9"
	}
	if country := strings.TrimSpace(q.Country); country != "" {
		return base + "-" + strings.ToUpper(country) + "," + base + ";q=0.9"
	}
	return base
}

// Home is the domain's front page, which a session visits before its first
// search — see Session.
func (q Query) Home() string {
	host := "www.google.com"
	if h, ok := ccTLD[strings.ToLower(strings.TrimSpace(q.Country))]; ok {
		host = h
	}
	return (&url.URL{Scheme: "https", Host: host, Path: "/"}).String()
}
