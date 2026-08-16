// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeSearcher answers from a script, so pagination can be tested without a
// network and without a page of HTML per case.
type fakeSearcher struct {
	pages     []SERP
	errs      []error
	asked     []int
	askedText []string
}

func (f *fakeSearcher) Search(_ context.Context, q Query) (SERP, error) {
	f.askedText = append(f.askedText, q.Text)
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

// page builds a result page of n results whose pagination bar was not found —
// the case a walk must never read as the end of the results.
func page(n int) SERP {
	s := SERP{}
	for i := 0; i < n; i++ {
		s.Results = append(s.Results, Result{Position: i + 1, Host: "example.com"})
	}
	return s
}

// pageWithBar builds a result page whose pagination bar was found and offers
// pages up to the given start offset.
func pageWithBar(n, maxOffset int) SERP {
	s := page(n)
	s.MaxOffset, s.HasPagination = maxOffset, true
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

func TestSearchDepth_StopsWhenTheBarOffersNothingPastThePageInHand(t *testing.T) {
	// The last page of a result set links back to the pages before it and no
	// further. That is Google stating where the results end, and it is worth
	// stopping on: walking to the requested depth regardless would spend a
	// request per query on a page that is not there.
	f := &fakeSearcher{pages: []SERP{pageWithBar(10, 10), pageWithBar(10, 10)}}
	got, err := SearchDepth(context.Background(), f, Query{Text: "x"}, 5)
	if err != nil {
		t.Fatalf("SearchDepth: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d pages, want 2 — the bar offers nothing past page two", len(got))
	}
	if len(f.asked) != 2 {
		t.Errorf("asked for %d pages, want 2", len(f.asked))
	}
}

func TestSearchDepth_AShortPageIsNotTheEndOfTheResults(t *testing.T) {
	// A page coming back one result lighter than the one before it is ordinary.
	// Treating that as the end reported sites two pages further down as not
	// ranking at all — a wrong answer that reads exactly like a right one, and
	// the reason the walk asks the bar instead of counting results.
	f := &fakeSearcher{pages: []SERP{pageWithBar(10, 40), pageWithBar(9, 40), pageWithBar(10, 40)}}
	got, err := SearchDepth(context.Background(), f, Query{Text: "x"}, 3)
	if err != nil {
		t.Fatalf("SearchDepth: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d pages, want 3 — the bar offers pages past the short one", len(got))
	}
	if len(f.asked) != 3 {
		t.Errorf("asked for %d pages, want 3", len(f.asked))
	}
}

func TestSearchDepth_KeepsWalkingWhenNoBarWasFound(t *testing.T) {
	// A layout whose pagination control this parser does not recognise says
	// nothing about where the results end. Reading that silence as an ending
	// would stop the walk on markup nobody read.
	f := &fakeSearcher{pages: []SERP{page(10), page(4), page(10)}}
	got, err := SearchDepth(context.Background(), f, Query{Text: "x"}, 3)
	if err != nil {
		t.Fatalf("SearchDepth: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d pages, want 3 — an unrecognised bar ends nothing", len(got))
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

// refusal is the error a session returns for a page that carried no data.
func refusal(class Class) error {
	return &ResponseError{Class: class, Query: "x", op: "search", err: fmt.Errorf("%w: no results present", ErrNotSERP)}
}

func TestSearchUntil_KeepsTheResponseClassReadableThroughItsOwnWrapping(t *testing.T) {
	// The run layer decides whether a query is worth taking to another identity
	// from the class, and it reads that class off whatever the walk hands back.
	// The wrapping here is the only thing carrying it that far.
	f := &fakeSearcher{pages: []SERP{{}}, errs: []error{refusal(ClassShell)}}

	err := SearchUntil(context.Background(), f, Query{Text: "x"}, 3, nil)
	got, ok := ClassOf(err)
	if !ok {
		t.Fatalf("the class did not survive the walk's own wrapping: %v", err)
	}
	if got != ClassShell {
		t.Errorf("class=%q, want %q", got, ClassShell)
	}
}

func TestSearchDepth_KeepsTheResponseClassReadableThroughItsOwnWrapping(t *testing.T) {
	f := &fakeSearcher{pages: []SERP{page(10), {}}, errs: []error{nil, refusal(ClassWall)}}

	_, err := SearchDepth(context.Background(), f, Query{Text: "x"}, 3)
	got, ok := ClassOf(err)
	if !ok {
		t.Fatalf("the class did not survive the walk's own wrapping: %v", err)
	}
	if got != ClassWall {
		t.Errorf("class=%q, want %q", got, ClassWall)
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
	_, err := SearchDepth(ctx, f, Query{Text: "x"}, 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want the cancellation", err)
	}
	// Every other way out of the walk names the package and the page it was on.
	// A cancellation reported bare reads as if it came from somewhere else.
	if !strings.Contains(err.Error(), "google: page 1") {
		t.Errorf("err=%q, want it in the package's voice", err)
	}
	if len(f.asked) != 0 {
		t.Errorf("made %d requests on a cancelled context", len(f.asked))
	}
}
