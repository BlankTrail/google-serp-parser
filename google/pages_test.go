// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"errors"
	"testing"
)

// fakeSearcher answers from a script, so pagination can be tested without a
// network and without a page of HTML per case.
type fakeSearcher struct {
	pages []SERP
	errs  []error
	asked []int
}

func (f *fakeSearcher) Search(_ context.Context, q Query) (SERP, error) {
	f.asked = append(f.asked, q.Page)
	i := q.Page - 1
	if i < len(f.errs) && f.errs[i] != nil {
		return SERP{}, f.errs[i]
	}
	if i >= len(f.pages) {
		return SERP{}, nil
	}
	return f.pages[i], nil
}

func page(n int) SERP {
	s := SERP{}
	for i := 0; i < n; i++ {
		s.Results = append(s.Results, Result{Position: i + 1, Host: "example.com"})
	}
	return s
}

func TestSearchDepth_WalksPagesInOrder(t *testing.T) {
	f := &fakeSearcher{pages: []SERP{page(10), page(10), page(10)}}
	got, err := SearchDepth(context.Background(), f, Query{Text: "x"}, 3)
	if err != nil {
		t.Fatalf("SearchDepth: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d pages, want 3", len(got))
	}
	if len(f.asked) != 3 || f.asked[0] != 1 || f.asked[1] != 2 || f.asked[2] != 3 {
		t.Errorf("asked for pages %v, want 1,2,3 in order", f.asked)
	}
}

func TestSearchDepth_StopsWhenAPageRunsOut(t *testing.T) {
	// Google thins out before the requested depth far more often than it fills
	// it. Asking for page four after page three came back short spends a
	// request on a page that is not there.
	f := &fakeSearcher{pages: []SERP{page(10), page(3)}}
	got, err := SearchDepth(context.Background(), f, Query{Text: "x"}, 5)
	if err != nil {
		t.Fatalf("SearchDepth: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d pages, want 2 — the short page ends the walk", len(got))
	}
	if len(f.asked) != 2 {
		t.Errorf("asked for %d pages, want 2", len(f.asked))
	}
}

func TestSearchDepth_StopsOnAnEmptyPage(t *testing.T) {
	f := &fakeSearcher{pages: []SERP{page(10), page(0)}}
	got, err := SearchDepth(context.Background(), f, Query{Text: "x"}, 4)
	if err != nil {
		t.Fatalf("SearchDepth: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d pages, want 1 — an empty page is not a result page", len(got))
	}
}

func TestSearchDepth_ReturnsWhatItHadWhenAPageFails(t *testing.T) {
	// A failure deep in the walk must not throw away the pages already taken:
	// a position found on page one is still a position, and re-taking page one
	// costs another request.
	boom := errors.New("blocked")
	f := &fakeSearcher{pages: []SERP{page(10), {}, page(10)}, errs: []error{nil, boom}}
	got, err := SearchDepth(context.Background(), f, Query{Text: "x"}, 3)
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want the page-two failure", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d pages alongside the error, want the one that succeeded", len(got))
	}
}

func TestSearchUntil_StopsWhenTheCallbackIsSatisfied(t *testing.T) {
	f := &fakeSearcher{pages: []SERP{page(10), page(10), page(10)}}
	err := SearchUntil(context.Background(), f, Query{Text: "x"}, 3, func(n int, _ SERP) bool {
		return n == 2
	})
	if err != nil {
		t.Fatalf("SearchUntil: %v", err)
	}
	if len(f.asked) != 2 {
		t.Errorf("took %d pages, want 2 - the callback was satisfied on the second", len(f.asked))
	}
}

func TestSearchDepth_RejectsANonPositiveDepth(t *testing.T) {
	f := &fakeSearcher{pages: []SERP{page(10)}}
	if _, err := SearchDepth(context.Background(), f, Query{Text: "x"}, 0); err == nil {
		t.Error("SearchDepth accepted a depth of zero")
	}
}

func TestSearchDepth_HonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeSearcher{pages: []SERP{page(10), page(10)}}
	if _, err := SearchDepth(ctx, f, Query{Text: "x"}, 2); err == nil {
		t.Error("SearchDepth ignored a cancelled context")
	}
}
