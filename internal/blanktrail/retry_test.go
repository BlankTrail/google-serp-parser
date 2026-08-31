// SPDX-License-Identifier: MIT

package blanktrail

import (
	"net/http"
	"testing"
	"time"
)

func TestRetryable(t *testing.T) {
	cases := map[int]bool{
		http.StatusOK:                  false,
		http.StatusCreated:             false,
		http.StatusMovedPermanently:    false,
		http.StatusBadRequest:          false,
		http.StatusUnauthorized:        false,
		http.StatusForbidden:           false,
		http.StatusNotFound:            false,
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          true,
		http.StatusServiceUnavailable:  true,
		http.StatusGatewayTimeout:      true,
	}
	for status, want := range cases {
		if got := Retryable(status); got != want {
			t.Errorf("Retryable(%d)=%v, want %v", status, got, want)
		}
	}
}

func TestRetryable_NotFoundIsAnAnswerNotAFailure(t *testing.T) {
	// A 404 is the origin telling us the thing is not there. Repeating the
	// request cannot change that, and doing so burns the retry budget that a
	// genuinely transient failure needs.
	if Retryable(http.StatusNotFound) {
		t.Error("Retryable(404)=true; a definitive answer must not be repeated")
	}
}

func TestRetryAfter_Seconds(t *testing.T) {
	// Comfortably under maxRetryAfter, so this exercises plain passthrough, not
	// the cap — that is TestRetryAfter_IsCapped's job.
	h := http.Header{}
	h.Set("Retry-After", "4")
	if got := retryAfter(h, time.Unix(1_700_000_000, 0)); got != 4*time.Second {
		t.Errorf("retryAfter=%v, want 4s", got)
	}
}

func TestRetryAfter_HTTPDate(t *testing.T) {
	// Comfortably under maxRetryAfter, so this exercises plain passthrough, not
	// the cap — that is TestRetryAfter_IsCapped's job.
	now := time.Unix(1_700_000_000, 0).UTC()
	h := http.Header{}
	h.Set("Retry-After", now.Add(8*time.Second).Format(http.TimeFormat))

	got := retryAfter(h, now)
	if got < 7*time.Second || got > 8*time.Second {
		t.Errorf("retryAfter=%v, want about 8s", got)
	}
}

func TestRetryAfter_AbsentOrUnusable(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	cases := map[string]string{
		"absent":       "",
		"garbage":      "soon please",
		"negative":     "-10",
		"past date":    now.Add(-time.Hour).Format(http.TimeFormat),
		"empty string": "   ",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			h := http.Header{}
			if value != "" {
				h.Set("Retry-After", value)
			}
			if got := retryAfter(h, now); got != 0 {
				t.Errorf("retryAfter=%v, want 0", got)
			}
		})
	}
}

func TestRetryAfter_IsCapped(t *testing.T) {
	// Origins have asked for hours. Honouring that would stall a run outright;
	// the caller can always decide to stop, but the transport must not sleep for
	// an unbounded time on its own.
	h := http.Header{}
	h.Set("Retry-After", "86400")
	if got := retryAfter(h, time.Unix(1_700_000_000, 0)); got != maxRetryAfter {
		t.Errorf("retryAfter=%v, want it capped at %v", got, maxRetryAfter)
	}
}

func TestRetryAfter_UsesTheRealClock(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "5")
	if got := RetryAfter(h); got != 5*time.Second {
		t.Errorf("RetryAfter=%v, want 5s", got)
	}
}
