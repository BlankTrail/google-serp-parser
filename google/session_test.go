// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"net/http"
	"net/http/httptest"
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
