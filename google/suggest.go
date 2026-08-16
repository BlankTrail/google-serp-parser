// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// suggestClient is the client name the completion endpoint is asked under.
//
// It is not optional: measured against the live endpoint, a request with no
// client parameter is answered with HTTP 400. Of the names that work, this one
// returns a plain two-element array — the echo of the query and the list —
// while the others return a JavaScript callback wrapper or XML, neither of
// which this package can decode with encoding/json.
const suggestClient = "chrome"

// maxSuggestBody caps the read. A completion response is a few kilobytes; far
// beyond that is not a response this program wants.
const maxSuggestBody = 1 << 20

// Suggester reads Google's search completions.
//
// It is the one part of this package that parses no markup: completions come
// from their own address as JSON, so nothing here depends on the result page's
// layout and nothing here breaks when that layout changes.
type Suggester struct {
	Client *http.Client
}

// NewSuggester wraps a transport for the completion endpoint.
func NewSuggester(rt http.RoundTripper) *Suggester {
	return &Suggester{Client: &http.Client{Transport: rt}}
}

// Suggest returns the completions Google offers for a query.
func (s *Suggester) Suggest(ctx context.Context, q Query) ([]string, error) {
	target, err := suggestURL(q)
	if err != nil {
		return nil, err
	}
	return s.suggestFrom(ctx, target)
}

// suggestFrom fetches and reads one completion response. It takes a full URL so
// the reading half can be tested without building one.
func (s *Suggester) suggestFrom(ctx context.Context, target string) ([]string, error) {
	client := s.Client
	if client == nil {
		client = &http.Client{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("google: build suggest request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google: suggest: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google: suggest: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSuggestBody))
	if err != nil {
		return nil, fmt.Errorf("google: read suggest body: %w", err)
	}

	// The measured shape is [echo, [suggestions…]] with further elements the
	// caller does not need. Decoding into raw messages and reading the second
	// keeps that tolerant of the tail without guessing at it.
	var envelope []json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("google: suggest body is not the expected array: %w", err)
	}
	if len(envelope) < 2 {
		return nil, fmt.Errorf("google: suggest body has %d elements, want at least 2", len(envelope))
	}
	var out []string
	if err := json.Unmarshal(envelope[1], &out); err != nil {
		return nil, fmt.Errorf("google: suggest list is not a list of phrases: %w", err)
	}
	return out, nil
}

// suggestURL builds the completion request, carrying the same country and
// language axes a search would.
func suggestURL(q Query) (string, error) {
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
	v.Set("client", suggestClient)
	if country != "" {
		v.Set("gl", country)
	}
	if lang := strings.ToLower(strings.TrimSpace(q.Language)); lang != "" {
		v.Set("hl", lang)
	}
	u := url.URL{Scheme: "https", Host: host, Path: "/complete/search", RawQuery: v.Encode()}
	return u.String(), nil
}
