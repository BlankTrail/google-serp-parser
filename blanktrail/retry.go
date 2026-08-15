// SPDX-License-Identifier: MIT

package blanktrail

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRetryAfter caps how long a Retry-After header may hold a request.
//
// It is deliberately far below a sensible per-request timeout. The whole retry
// loop runs inside the caller's request deadline, so a cap anywhere near that
// deadline spends the entire budget on one pause and the retry never happens —
// which would defeat the only header this function exists to honour. A target
// asking for longer than this is better served by letting the port cool down
// and picking the work up on a later pass.
const maxRetryAfter = 10 * time.Second

// Retryable reports whether a status is worth sending again.
//
// Only two kinds qualify: a rate limit, which says "later", and a server-side
// error, which says "not my fault, try again". Every other non-2xx is the
// origin's definitive answer to this request — repeating it cannot change the
// outcome and only spends the budget a transient failure would have needed.
//
// Note what is deliberately absent: this package does not reason about why a
// request was refused. Anti-bot handling belongs to the proxy, not here.
func Retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// RetryAfter returns the delay the origin asked for in its Retry-After header,
// capped at maxRetryAfter. It returns zero when the header is absent, malformed
// or points into the past.
func RetryAfter(h http.Header) time.Duration {
	return retryAfter(h, time.Now())
}

// retryAfter is RetryAfter with an injectable clock, for tests.
func retryAfter(h http.Header, now time.Time) time.Duration {
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return 0
	}

	var d time.Duration
	if secs, err := strconv.Atoi(raw); err == nil {
		d = time.Duration(secs) * time.Second
	} else if at, err := http.ParseTime(raw); err == nil {
		d = at.Sub(now)
	} else {
		return 0
	}

	if d <= 0 {
		return 0
	}
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}
