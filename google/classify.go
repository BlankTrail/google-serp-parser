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
// on another port rather than recorded as a result.
func (c Class) Usable() bool { return c == ClassSERP || c == ClassEmpty }

// ErrNotSERP wraps every class that carries no data, so a caller can branch
// on one error and still read the class for its logs.
var ErrNotSERP = errors.New("google: response is not a result page")

// wallMarkers are phrases that only appear on a challenge page. They are
// checked AFTER results, never before — see Classify.
var wallMarkers = []string{
	"/sorry/index",
	"our systems have detected unusual traffic",
	"unusual traffic from your computer network",
}

// emptyMarkers are how Google words "nothing found". Absence of results is
// not enough on its own: the shell has no results either.
var emptyMarkers = []string{
	"did not match any documents",
	"по запросу", // Russian: "по запросу … ничего не найдено"
	"ничего не найдено",
}

// Classify decides what came back.
//
// The order of the checks is load-bearing and was established by measurement,
// not by taste: the presence of results is tested FIRST, because a genuine
// result page mentions reCAPTCHA in its own markup. A run that checked wall
// markers first declared a page carrying eleven extracted links to be a wall.
//
// Nothing here tries to name a vendor or a challenge type. That belongs to
// the proxy, which solves them; a second signature database in the parser
// would be worse than the first and would drift away from it.
func Classify(status int, finalURL string, body []byte) (Class, error) {
	// 1. Results present — a page with results is never anything else.
	if hasResults(body) {
		return ClassSERP, nil
	}

	// 2. The redirect target and the rate-limit status are unambiguous.
	if strings.Contains(finalURL, "/sorry") || status == http.StatusTooManyRequests {
		return ClassWall, fmt.Errorf("%w: challenge or rate limit", ErrNotSERP)
	}

	// 3. Wall phrases in the body.
	lower := bytes.ToLower(body)
	for _, m := range wallMarkers {
		if bytes.Contains(lower, []byte(m)) {
			return ClassWall, fmt.Errorf("%w: challenge page", ErrNotSERP)
		}
	}

	// 4 and 5. Refusals and server-side failures.
	if status == http.StatusForbidden {
		return ClassBanned, fmt.Errorf("%w: HTTP 403", ErrNotSERP)
	}
	if status != http.StatusOK {
		return ClassHTTP, fmt.Errorf("%w: HTTP %d", ErrNotSERP, status)
	}

	// 6. Google saying it found nothing — a successful capture.
	for _, m := range emptyMarkers {
		if bytes.Contains(lower, []byte(m)) {
			return ClassEmpty, nil
		}
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
