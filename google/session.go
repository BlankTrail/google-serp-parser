// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// maxBody caps how much of a response is read. A result page runs to a couple
// of megabytes; anything far beyond that is not a page this parser wants.
const maxBody = 8 << 20

// Session is one browsing session against Google, behaving like a tab rather
// than like a script: it opens the home page before its first search and
// carries an honest referrer chain from there.
//
// It holds no cookie jar. The proxy owns the jar for the port this session
// runs on, and a second jar layered on top would present two identities on
// one connection.
type Session struct {
	Client *http.Client

	// warmed records that the home page has been visited, so the first search
	// of a session is a navigation from somewhere and later ones do not repeat
	// the visit.
	warmed bool
	last   string
}

// NewSession builds a session over one transport — in production, a client
// leased from a proxy port.
func NewSession(rt http.RoundTripper) *Session {
	return &Session{Client: &http.Client{Transport: rt}}
}

// Search performs one capture and returns the parsed page.
func (s *Session) Search(ctx context.Context, q Query) (SERP, error) {
	target, err := q.URL()
	if err != nil {
		return SERP{}, err
	}

	if !s.warmed {
		home := q.Home()
		if _, err := s.fetch(ctx, home, q); err != nil {
			// A failed warm-up is not fatal on its own — the search may still
			// go through — but it is worth not retrying in a loop.
			_ = err
		}
		s.warmed = true
		s.last = home
	}

	body, finalURL, status, err := s.get(ctx, target, q)
	if err != nil {
		return SERP{}, err
	}
	s.last = target

	if class, err := Classify(status, finalURL, body); err != nil {
		return SERP{}, fmt.Errorf("google: search %q: %w (class %s)", q.Text, err, class)
	}
	return ParseSERP(q.Text, body)
}

func (s *Session) fetch(ctx context.Context, target string, q Query) ([]byte, error) {
	body, _, _, err := s.get(ctx, target, q)
	return body, err
}

func (s *Session) get(ctx context.Context, target string, q Query) (body []byte, finalURL string, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", 0, fmt.Errorf("google: build request: %w", err)
	}

	// The proxy owns the User-Agent, Accept and sec-ch-ua — they come from the
	// port's browser profile. Accept-Language is deliberately NOT spoofed by
	// it, which makes it this package's to set, and it must agree with hl.
	req.Header.Set("Accept-Language", q.AcceptLanguage())
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-User", "?1")
	if s.last == "" {
		req.Header.Set("Sec-Fetch-Site", "none")
	} else {
		req.Header.Set("Referer", s.last)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}

	client := s.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", 0, fmt.Errorf("google: fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, "", resp.StatusCode, fmt.Errorf("google: read body: %w", err)
	}
	final := target
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return raw, final, resp.StatusCode, nil
}
