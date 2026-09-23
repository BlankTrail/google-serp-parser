// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// deepPage is a page of results that offers another after it, the way a live
// page does: the bar's own links, and the next one under id="pnnext" carrying
// the session's tags.
const deepPage = `<!doctype html><html><body><div id="rso">` +
	`<div data-snc="r0"><a href="https://example.com/a" data-ved="x"><h3>A result</h3></a></div>` +
	`</div><div role="navigation">` +
	`<a href="/search?q=x&amp;ei=E1&amp;start=10">2</a>` +
	`<a href="/search?q=x&amp;ei=E1&amp;start=20&amp;sa=N&amp;ved=2ah" id="pnnext">Следующая</a>` +
	`</div></body></html>`

// pageServer answers every request with deepPage and writes down what it was
// asked for, in order.
type pageServer struct {
	mu      sync.Mutex
	asked   []string
	referer []string
	srv     *httptest.Server
}

func newPageServer(t *testing.T) *pageServer {
	t.Helper()
	p := &pageServer{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.asked = append(p.asked, r.URL.RequestURI())
		p.referer = append(p.referer, r.Header.Get("Referer"))
		p.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		if r.URL.Path != "/search" {
			_, _ = io.WriteString(w, "<html><body>home</body></html>")
			return
		}
		_, _ = io.WriteString(w, deepPage)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *pageServer) seen() ([]string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.asked...), append([]string(nil), p.referer...)
}

func TestSession_SearchAtAsksExactlyTheAddressItWasGiven(t *testing.T) {
	// The address of a deeper page is the one the page before it carried, tags
	// and all: ei, sa, ved and the rest are issued to the session that was
	// shown that page. Rebuilding it from the query and an offset would throw
	// every one of them away.
	p := newPageServer(t)
	s := NewSession(rewriteHost{target: p.srv.URL})
	q := Query{Text: "x", Country: "ru", Language: "ru"}
	first, err := s.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if first.NextPage == "" {
		t.Fatal("the first page offered no next address")
	}

	if _, err := s.SearchAt(context.Background(), q, first.NextPage); err != nil {
		t.Fatalf("SearchAt: %v", err)
	}
	asked, referer := p.seen()
	last := asked[len(asked)-1]
	if want := "/search?q=x&ei=E1&start=20&sa=N&ved=2ah"; last != want {
		t.Errorf("the deeper page was asked for at %q, want %q — the address the page carried", last, want)
	}
	// And it is a navigation from the page before it, as a browser's would be.
	if ref := referer[len(referer)-1]; !strings.Contains(ref, "/search") {
		t.Errorf("the deeper page went out with referer %q, want the page before it", ref)
	}
}

func TestSession_ReportsTheNextPageAsAnAddressThatCanBeAsked(t *testing.T) {
	// The page writes its next address origin-relative. A caller that had to
	// join it would have to know which of Google's domains answered, which is
	// what Origin exists for — so the session joins it here, once, where the
	// answer landed.
	p := newPageServer(t)
	s := NewSession(rewriteHost{target: p.srv.URL})
	got, err := s.Search(context.Background(), Query{Text: "x", Country: "ru"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if want := got.Origin + "/search?q=x&ei=E1&start=20&sa=N&ved=2ah"; got.NextPage != want {
		t.Errorf("the next page is %q, want %q", got.NextPage, want)
	}
	if !strings.HasPrefix(got.NextPage, "http") {
		t.Errorf("the next page %q is not an address a client can ask for", got.NextPage)
	}
}

func TestSession_SearchAtRefusesAnEmptyAddress(t *testing.T) {
	// A walk asks for a page it was offered. Asked for nothing, this would
	// otherwise fetch whatever a bare request lands on and file it as a page of
	// the query.
	p := newPageServer(t)
	s := NewSession(rewriteHost{target: p.srv.URL})
	if _, err := s.SearchAt(context.Background(), Query{Text: "x"}, "   "); err == nil {
		t.Error("SearchAt asked for an empty address")
	}
	if asked, _ := p.seen(); len(asked) != 0 {
		t.Errorf("%d requests went out for an empty address", len(asked))
	}
}
