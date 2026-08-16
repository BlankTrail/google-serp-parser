// SPDX-License-Identifier: MIT

package google

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordingProxy stands in for a BlankTrail port: it records what the session
// sent and answers /search with a captured page.
type recordingProxy struct {
	mu    sync.Mutex
	paths []string
	hdrs  []http.Header
	body  []byte
	srv   *httptest.Server
}

func newRecordingProxy(t *testing.T, fixtureName string) *recordingProxy {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", fixtureName))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	p := &recordingProxy{body: raw}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.paths = append(p.paths, r.URL.Path)
		p.hdrs = append(p.hdrs, r.Header.Clone())
		p.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		if r.URL.Path == "/search" {
			_, _ = w.Write(p.body)
			return
		}
		_, _ = w.Write([]byte("<html><body>home</body></html>"))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// rewriteHost sends every request to the test server regardless of the host
// the session built, so the real google.com is never contacted.
type rewriteHost struct{ target string }

func (rt rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	t, err := http.NewRequest(http.MethodGet, rt.target, nil)
	if err != nil {
		return nil, err
	}
	u.Scheme, u.Host = t.URL.Scheme, t.URL.Host
	clone := req.Clone(req.Context())
	clone.URL = &u
	clone.Host = ""
	return http.DefaultTransport.RoundTrip(clone)
}

func TestSession_WarmsUpTheHomePageBeforeSearching(t *testing.T) {
	// A session that jumps straight to /search is making a navigation nobody
	// performed. A browser opening a tab lands on the home page first.
	p := newRecordingProxy(t, "serp_direct_us.html")
	s := NewSession(rewriteHost{target: p.srv.URL})

	if _, err := s.Search(context.Background(), Query{Text: "iphone", Country: "us", Language: "en"}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.paths) < 2 {
		t.Fatalf("paths=%v, want a home page visit before the search", p.paths)
	}
	if p.paths[0] != "/" {
		t.Errorf("first request went to %q, want the home page", p.paths[0])
	}
	if p.paths[1] != "/search" {
		t.Errorf("second request went to %q, want /search", p.paths[1])
	}
}

func TestSession_CarriesAnHonestReferrerChain(t *testing.T) {
	// The first request of a session has no referrer and Sec-Fetch-Site none;
	// the search that follows it is a same-origin navigation from the home
	// page. A constant "none" on every page would mean the tab is reopened
	// for each query, which no browsing session looks like.
	p := newRecordingProxy(t, "serp_direct_us.html")
	s := NewSession(rewriteHost{target: p.srv.URL})
	if _, err := s.Search(context.Background(), Query{Text: "iphone"}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if got := p.hdrs[0].Get("Sec-Fetch-Site"); got != "none" {
		t.Errorf("home Sec-Fetch-Site=%q, want none", got)
	}
	if got := p.hdrs[0].Get("Referer"); got != "" {
		t.Errorf("home carried Referer %q, want none", got)
	}
	if got := p.hdrs[1].Get("Sec-Fetch-Site"); got != "same-origin" {
		t.Errorf("search Sec-Fetch-Site=%q, want same-origin", got)
	}
	if got := p.hdrs[1].Get("Referer"); !strings.Contains(got, "google") {
		t.Errorf("search Referer=%q, want the home page it came from", got)
	}
}

func TestSession_SendsTheAcceptLanguageTheQueryAsksFor(t *testing.T) {
	// Setting this header is this program's own responsibility, and it is
	// how the language axis actually reaches Google.
	p := newRecordingProxy(t, "serp_direct_us.html")
	s := NewSession(rewriteHost{target: p.srv.URL})
	if _, err := s.Search(context.Background(), Query{Text: "тест", Language: "ru"}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if got := p.hdrs[1].Get("Accept-Language"); !strings.HasPrefix(got, "ru") {
		t.Errorf("Accept-Language=%q, want it to lead with ru", got)
	}
}

func TestSession_ReturnsTheParsedPage(t *testing.T) {
	p := newRecordingProxy(t, "serp_direct_us.html")
	s := NewSession(rewriteHost{target: p.srv.URL})
	got, err := s.Search(context.Background(), Query{Text: "iphone"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got.Results) != 8 {
		t.Errorf("results=%d, want 8", len(got.Results))
	}
	if got.Query != "iphone" {
		t.Errorf("Query=%q, want iphone", got.Query)
	}
}

func TestSession_SurfacesAShellAsAnError(t *testing.T) {
	// The shell arrives with HTTP 200. If Search returned it as an empty
	// result the caller would record a real zero.
	p := newRecordingProxy(t, "jsshell.html")
	s := NewSession(rewriteHost{target: p.srv.URL})
	if _, err := s.Search(context.Background(), Query{Text: "iphone"}); err == nil {
		t.Fatal("Search accepted the JavaScript shell as results")
	}
}

func TestSession_HandsBackTheClassOfAnUnusableResponse(t *testing.T) {
	// The shell arrives with HTTP 200, so nothing below this layer can tell it
	// from a good page. If the class does not survive the return, the caller
	// cannot tell "ask for this query again" from "this query has no answer",
	// and the only way left to tell them apart is matching the wording of the
	// message.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body><div id="main"></div></body></html>`))
	}))
	t.Cleanup(srv.Close)

	s := NewSession(rewriteHost{target: srv.URL})
	_, err := s.Search(context.Background(), Query{Text: "x", Country: "us", Language: "en"})
	if err == nil {
		t.Fatal("a page with no results was accepted as a result page")
	}
	class, ok := ClassOf(err)
	if !ok {
		t.Fatalf("Search returned %v, which carries no class", err)
	}
	if class != ClassShell {
		t.Errorf("class=%q, want %q", class, ClassShell)
	}
}

func TestSession_TreatsAnOversizedBodyAsAnError(t *testing.T) {
	// LimitReader alone returns io.EOF at its boundary exactly as it would at
	// a genuine end of body, so a response beyond maxBody would otherwise be
	// cut off with no error at all.
	//
	// The oversized body here is a real result page followed by padding, so
	// the first maxBody bytes of it are a whole page: without the size check
	// the truncated body parses into eight plausible results and a nil error,
	// which is the failure mode this milestone exists to avoid. A body of one
	// repeated character could not show that — it parses into nothing and
	// would fail the classifier for an unrelated reason.
	page, err := os.ReadFile(filepath.Join("testdata", "serp_direct_us.html"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		if r.URL.Path != "/search" {
			_, _ = w.Write([]byte("<html><body>home</body></html>"))
			return
		}
		written, _ := w.Write(page)
		// Past maxBody, written in chunks rather than one huge allocation.
		chunk := bytes.Repeat([]byte("a"), 1<<20) // 1 MiB
		for written < maxBody+1 {
			n, _ := w.Write(chunk)
			written += n
		}
	}))
	t.Cleanup(srv.Close)

	// The premise, stated as an assertion rather than as a claim in a comment:
	// what a silent truncation would have handed the caller.
	truncated, terr := ParseSERP("iphone", append(append([]byte{}, page...),
		bytes.Repeat([]byte("a"), maxBody-len(page))...))
	if terr != nil {
		t.Fatalf("the truncated prefix did not parse, so this test cannot show what truncation costs: %v", terr)
	}
	if len(truncated.Results) == 0 {
		t.Fatal("the truncated prefix yielded no results, so this test cannot show what truncation costs")
	}

	s := NewSession(rewriteHost{target: srv.URL})
	if _, err := s.Search(context.Background(), Query{Text: "iphone"}); err == nil {
		t.Fatal("Search accepted a body beyond maxBody without error")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error=%q, want it to mention the size limit", err)
	}
}

func TestSession_ReportsTheOriginAnEncryptedLinkMustBeJoinedTo(t *testing.T) {
	// Result.Link is origin-relative under the encrypted form. Without the
	// origin travelling with the page, a caller has to invent one, and the
	// obvious invention — www.google.com — is wrong for every capture aimed at
	// a ccTLD.
	p := newRecordingProxy(t, "serp_goto_ru.html")
	s := NewSession(rewriteHost{target: p.srv.URL})
	got, err := s.Search(context.Background(), Query{Text: "тест", Country: "ru"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got.Origin == "" {
		t.Fatal("the parsed page carries no origin")
	}
	if len(got.Results) == 0 {
		t.Fatal("no results to check a link against")
	}
	// Joined to the origin, an origin-relative link is a URL an http.Client
	// will accept — which the bare link is not.
	joined, err := url.Parse(got.Origin + got.Results[0].Link)
	if err != nil || joined.Scheme == "" || joined.Host == "" {
		t.Fatalf("origin %q joined to link %q is not an absolute URL: %v",
			got.Origin, got.Results[0].Link, err)
	}
	if bare, err := url.Parse(got.Results[0].Link); err == nil && bare.Scheme != "" {
		t.Errorf("link %q is already absolute; this test no longer pins anything",
			got.Results[0].Link)
	}
}

func TestSession_SecondSearchAdvancesTheChainInsteadOfResetting(t *testing.T) {
	// warmed exists precisely so a session's second and later searches skip
	// the home-page visit and the referrer chain keeps advancing — the
	// difference between a session that looks like one browsing tab and one
	// that looks like a script reopening a tab for every query.
	p := newRecordingProxy(t, "serp_direct_us.html")
	s := NewSession(rewriteHost{target: p.srv.URL})

	firstQuery := Query{Text: "iphone"}
	if _, err := s.Search(context.Background(), firstQuery); err != nil {
		t.Fatalf("first Search: %v", err)
	}
	if _, err := s.Search(context.Background(), Query{Text: "ipad"}); err != nil {
		t.Fatalf("second Search: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.paths) != 3 {
		t.Fatalf("paths=%v, want home + two searches", p.paths)
	}
	homeVisits := 0
	for _, path := range p.paths {
		if path == "/" {
			homeVisits++
		}
	}
	if homeVisits != 1 {
		t.Errorf("home page visited %d times across two searches, want exactly 1", homeVisits)
	}

	firstSearchURL, err := firstQuery.URL()
	if err != nil {
		t.Fatalf("firstQuery.URL: %v", err)
	}
	if got := p.hdrs[2].Get("Referer"); got != firstSearchURL {
		t.Errorf("second search Referer=%q, want the first search's URL %q, not the home page", got, firstSearchURL)
	}
	if got := p.hdrs[2].Get("Sec-Fetch-Site"); got != "same-origin" {
		t.Errorf("second search Sec-Fetch-Site=%q, want same-origin", got)
	}
}
