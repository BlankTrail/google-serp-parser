// SPDX-License-Identifier: MIT

package google

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// Class is what a response turned out to be.
type Class string

const (
	// ClassSERP is a page of results.
	ClassSERP Class = "serp"
	// ClassEmpty is Google stating it found nothing. A successful capture.
	ClassEmpty Class = "empty"
	// ClassShell is the JavaScript shell served to a plain HTTP client:
	// HTTP 200, a valid page, and no results in it, ever.
	ClassShell Class = "shell"
	// ClassWall is a challenge or a rate limit.
	ClassWall Class = "wall"
	// ClassBanned is a refusal.
	ClassBanned Class = "banned"
	// ClassHTTP is any other unusable status.
	ClassHTTP Class = "http"
)

// Usable reports whether the response can be parsed for data. Only a page of
// results and an honest empty answer qualify; everything else must be retried
// rather than recorded as a result.
func (c Class) Usable() bool { return c == ClassSERP || c == ClassEmpty }

// ErrNotSERP wraps every class that carries no data, so a caller can branch
// on one error and still read the class for its logs.
var ErrNotSERP = errors.New("google: response is not a result page")

// ResponseError is a response that carried no data, with the class that says
// which kind of nothing it was.
//
// The class travels as a value rather than as words in the message because the
// caller branches on it: a shell will never carry results however often it is
// asked for, while a wall is the same request refused for now and worth asking
// again over another transport. A caller that had to read the class out of the
// message would be reading prose, and its decision would break the first time
// the prose was reworded.
type ResponseError struct {
	Class Class
	// Query is the search text, so a log line names the query without the
	// caller threading it through alongside the error.
	Query string

	// op is what was under way when the response turned out to be unusable.
	// The same class is reached by fetching a page and by parsing a body that
	// was already in hand, and a message that named only one of them would
	// misreport the other.
	op  string
	err error
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("google: %s %q: %v (class %s)", e.op, e.Query, e.err, e.Class)
}

// Unwrap keeps errors.Is(err, ErrNotSERP) working for callers written before
// the class was available.
func (e *ResponseError) Unwrap() error { return e.err }

// ClassOf returns the class an error carries, however deeply it has been
// wrapped, and reports whether it carried one at all. A transport failure
// carries none: nothing was classified because nothing arrived.
func ClassOf(err error) (Class, bool) {
	var re *ResponseError
	if errors.As(err, &re) {
		return re.Class, true
	}
	return "", false
}

// wallMarkers are phrases that only appear on a challenge page. They are
// checked AFTER results and AFTER the empty markers, never before — see
// Classify.
//
// "/sorry/index" is deliberately not here: it appears in Google's own inline
// JavaScript on ordinary result pages — measured on 17 of 17 real captures —
// so as a body marker it would misfire on genuine pages. The sound version of
// that signal is the finalURL check in Classify, which sees where the
// response actually landed rather than a string anywhere in the markup.
var wallMarkers = []string{
	"our systems have detected unusual traffic",
	"unusual traffic from your computer network",
	"наши системы обнаружили необычный трафик",
	"необычный трафик, поступающий из вашей компьютерной сети",
}

// emptyMarkers are how Google words "nothing found". Absence of results is
// not enough on its own: the shell has no results either.
//
// The two lists are not judged by the same standard. ClassEmpty is Usable, so
// a false Empty is recorded permanently as a real zero and never retried,
// while a false Wall merely costs a retry. Anything added here must therefore
// clear a much higher bar than anything added to wallMarkers: it has to be
// wording that cannot occur on a page which did find something.
//
// A missed marker is not free either — it degrades to ClassShell and is
// retried forever — which is why the English wording is carried in both the
// forms Google has been seen to serve.
var emptyMarkers = []string{
	"did not match any documents",
	"no results found for",
	"ничего не найдено",
}

// Classify decides what came back.
//
// The order of the checks is load-bearing and was established by measurement,
// not by taste: the presence of results is tested FIRST, because a genuine
// result page mentions reCAPTCHA in its own markup. A run that checked wall
// markers first declared a page carrying eleven extracted links to be a wall.
//
// Google's own "nothing found" wording is tested before the wall phrases in
// the body for the same reason: "/sorry/index" — a genuine wall signal in a
// redirect target — also turns up inside ordinary pages' own inline
// JavaScript, and a body-marker scan run first turned an honest zero on a
// site: query into a reported wall.
//
// Nothing here names a vendor or a challenge type. This program only needs
// to know the response is unusable; a signature list here would have to be
// maintained against pages this program never sees.
func Classify(status int, finalURL string, body []byte) (Class, error) {
	// 1. Results present — a page with results is never anything else.
	if hasResults(body) {
		return ClassSERP, nil
	}

	// 2. The redirect target and the rate-limit status are unambiguous.
	if strings.Contains(finalURL, "/sorry") || status == http.StatusTooManyRequests {
		return ClassWall, fmt.Errorf("%w: challenge or rate limit", ErrNotSERP)
	}

	lower := bytes.ToLower(body)

	// 3. Google saying it found nothing — a successful capture. Checked
	// before the wall phrases in the body: those phrases can appear
	// incidentally (see wallMarkers), and an honest zero must not lose to one.
	for _, m := range emptyMarkers {
		if bytes.Contains(lower, []byte(m)) {
			return ClassEmpty, nil
		}
	}

	// 4. Wall phrases in the body.
	for _, m := range wallMarkers {
		if bytes.Contains(lower, []byte(m)) {
			return ClassWall, fmt.Errorf("%w: challenge page", ErrNotSERP)
		}
	}

	// 5 and 6. Refusals and server-side failures.
	if status == http.StatusForbidden {
		return ClassBanned, fmt.Errorf("%w: HTTP 403", ErrNotSERP)
	}
	if status != http.StatusOK {
		return ClassHTTP, fmt.Errorf("%w: HTTP %d", ErrNotSERP, status)
	}

	// 7. HTTP 200 with no sign of results and no explanation: the shell.
	return ClassShell, fmt.Errorf("%w: JavaScript shell, no results present", ErrNotSERP)
}

// hasResults reports whether the body carries at least one organic result by
// the same rule the parser uses. Sharing the rule is deliberate: a page the
// classifier calls a SERP and the parser then reads as empty would be the
// worst of both answers.
func hasResults(body []byte) bool {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return false
	}
	found := false
	doc.Find("a[data-ved]").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if isAd(s) {
			return true
		}
		if title, _ := firstHeading(s); title == "" {
			return true
		}
		href, _ := s.Attr("href")
		if _, _, ok := classifyLink(href); ok {
			found = true
			return false
		}
		return true
	})
	return found
}
