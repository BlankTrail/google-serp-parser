// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// searchFunc adapts a function to Searcher, so a case can hand back a page and
// a failure at once — a combination the session never produces and the index
// check must never read as an answer.
type searchFunc func(context.Context, Query) (SERP, error)

func (f searchFunc) Search(ctx context.Context, q Query) (SERP, error) { return f(ctx, q) }

func TestSiteQuery_BuildsTheOperator(t *testing.T) {
	cases := []struct{ in, want string }{
		{"example.com", "site:example.com"},
		{"https://example.com/", "site:example.com"},
		{"https://example.com/a/b", "site:example.com/a/b"},
		{"example.com/a/b", "site:example.com/a/b"},
		{"www.example.com", "site:example.com"},
	}
	for _, tc := range cases {
		if got := siteQuery(tc.in); got != tc.want {
			t.Errorf("siteQuery(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSiteQuery_ReducesATargetToHostAndPath(t *testing.T) {
	// The operator reaches a path and no further, so a target carrying a query
	// string is asked about by its path alone. That makes the answer broader
	// than the question, and it is pinned here rather than left to happen: a
	// hit on example.com/page does not establish that example.com/page?id=5 is
	// held.
	cases := []struct{ in, want string }{
		{"example.com/page?id=5", "site:example.com/page"},
		{"https://example.com/page?id=5#top", "site:example.com/page"},
		{"example.com?id=5", "site:example.com"},
	}
	for _, tc := range cases {
		if got := siteQuery(tc.in); got != tc.want {
			t.Errorf("siteQuery(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCheckIndexed_OneResultIsEnough(t *testing.T) {
	// The question is presence, not rank, so a single hit answers it — and
	// answering it needs no address, which is what makes this check cheap
	// under the link form that carries none.
	f := &fakeSearcher{pages: []SERP{serpOf("example.com")}}
	got, err := CheckIndexed(context.Background(), f, Query{Text: "ignored"}, "example.com/page")
	if err != nil {
		t.Fatalf("CheckIndexed: %v", err)
	}
	if !got.Indexed || got.Hits != 1 {
		t.Errorf("indexed=%v hits=%d, want true 1", got.Indexed, got.Hits)
	}
}

func TestCheckIndexed_AsksTheSiteOperatorNotTheCallersText(t *testing.T) {
	// The caller's Query carries the axes — country, language, device. Its
	// text is replaced: the whole point of this check is the operator.
	f := &fakeSearcher{pages: []SERP{serpOf("example.com")}}
	if _, err := CheckIndexed(context.Background(), f, Query{Text: "leftover"}, "example.com"); err != nil {
		t.Fatalf("CheckIndexed: %v", err)
	}
	if len(f.askedText) == 0 || !strings.HasPrefix(f.askedText[0], "site:") {
		t.Errorf("asked %q, want a site: query", f.askedText)
	}
}

func TestCheckIndexed_AnEmptyAnswerIsNotIndexed(t *testing.T) {
	// Google finding nothing is a real answer here, not a failure — and the
	// distinction is the whole reason a response is classified before it is
	// parsed.
	f := &fakeSearcher{pages: []SERP{{}}}
	got, err := CheckIndexed(context.Background(), f, Query{}, "example.com/gone")
	if err != nil {
		t.Fatalf("CheckIndexed: %v", err)
	}
	if got.Indexed || got.Hits != 0 {
		t.Errorf("indexed=%v hits=%d, want false 0", got.Indexed, got.Hits)
	}
}

func TestCheckIndexed_ARefusedResponseIsNotAnEmptyIndex(t *testing.T) {
	// The other half of the distinction above, and the half that decides
	// whether this engine is worth running: a response nobody could read says
	// nothing about the index. Reporting it the way an empty page is reported
	// would answer every blocked request with "not indexed" — a wrong answer a
	// caller has no way to disbelieve.
	boom := errors.New("blocked")
	f := &fakeSearcher{pages: []SERP{{}}, errs: []error{boom}}
	if _, err := CheckIndexed(context.Background(), f, Query{}, "example.com"); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want the refusal", err)
	}
}

func TestCheckIndexed_DoesNotReportAnIndexFromAFailedCapture(t *testing.T) {
	// Results arriving alongside a failure are part of a page, and part of a
	// page is not evidence that Google holds the target. Recording "indexed"
	// from one would put a claim in the report that the run never established.
	boom := errors.New("blocked")
	s := searchFunc(func(context.Context, Query) (SERP, error) {
		return serpOf("example.com"), boom
	})
	got, err := CheckIndexed(context.Background(), s, Query{}, "example.com")
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want the failure", err)
	}
	if got.Indexed || got.Hits != 0 {
		t.Errorf("indexed=%v hits=%d off a failed capture, want false 0", got.Indexed, got.Hits)
	}
}

func TestCheckIndexed_TakesOnlyOnePage(t *testing.T) {
	// Presence is settled by the first page. Walking deeper spends requests to
	// re-answer a question already answered.
	f := &fakeSearcher{pages: []SERP{serpOf("example.com"), serpOf("example.com")}}
	if _, err := CheckIndexed(context.Background(), f, Query{}, "example.com"); err != nil {
		t.Fatalf("CheckIndexed: %v", err)
	}
	if len(f.asked) != 1 {
		t.Errorf("took %d pages, want 1", len(f.asked))
	}
}

func TestCheckIndexed_CarriesAFewResultsAsEvidence(t *testing.T) {
	// Every hit is counted, because the count is the measurement. Only the
	// first few are carried, because the status is a verdict and the page
	// behind it is what ListIndexed is for.
	f := &fakeSearcher{pages: []SERP{serpOf(
		"example.com", "example.com", "example.com", "example.com", "example.com")}}
	got, err := CheckIndexed(context.Background(), f, Query{}, "example.com")
	if err != nil {
		t.Fatalf("CheckIndexed: %v", err)
	}
	if got.Hits != 5 {
		t.Errorf("Hits=%d, want all 5 counted", got.Hits)
	}
	if len(got.Sample) != 3 {
		t.Errorf("carried %d results, want 3", len(got.Sample))
	}
}

func TestListIndexed_KeepsOnlyResultsOnTheSite(t *testing.T) {
	// A site: query is a request, not a guarantee — Google mixes in results
	// from elsewhere often enough that reporting them as indexed pages of the
	// site would be wrong.
	f := &fakeSearcher{pages: []SERP{serpOf("example.com", "other.test", "blog.example.com")}}
	got, err := ListIndexed(context.Background(), f, Query{}, "example.com", 2)
	if err != nil {
		t.Fatalf("ListIndexed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want the 2 on the site: %+v", len(got), got)
	}
	for _, r := range got {
		if !hostBelongsTo(r.Host, "example.com") {
			t.Errorf("kept a result from %q", r.Host)
		}
	}
}

func TestListIndexed_ReportsAPageOnceWhenItComesBackOnTwoPages(t *testing.T) {
	// The page parser already drops a result linked more than once within one
	// page, so a listing that counted the same page twice because Google
	// repeated it across the boundary would disagree with the parser about
	// what "a result" is — and the caller most hurt by that is the one reading
	// the length as a count of what is indexed.
	// The third result below shares a title with the first and is a different
	// page: a site's pages carry repeated titles all the time, and telling
	// repeats apart by title would report the site as holding fewer pages than
	// it does.
	dup := Result{Position: 1, Host: "example.com", Title: "A", Link: "/goto/aaa", Form: LinkEncrypted}
	other := Result{Position: 2, Host: "example.com", Title: "B", Link: "/goto/bbb", Form: LinkEncrypted}
	sameTitle := Result{Position: 3, Host: "example.com", Title: "A", Link: "/goto/ccc", Form: LinkEncrypted}
	f := &fakeSearcher{pages: []SERP{{Results: []Result{dup, other, sameTitle}}, {Results: []Result{dup}}}}
	got, err := ListIndexed(context.Background(), f, Query{}, "example.com", 2)
	if err != nil {
		t.Fatalf("ListIndexed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d results, want the 3 distinct pages: %+v", len(got), got)
	}
}

func TestListIndexed_IdentifiesALinklessResultByItsTitle(t *testing.T) {
	// That is the page parser's rule for the same case, and keeping the two in
	// step matters more than the rule itself: a listing and the page it came
	// from disagreeing about what counts as one result would be a discrepancy
	// no one could explain from either file alone.
	linkless := Result{Position: 1, Host: "example.com", Title: "A"}
	f := &fakeSearcher{pages: []SERP{{Results: []Result{linkless}}, {Results: []Result{linkless}}}}
	got, err := ListIndexed(context.Background(), f, Query{}, "example.com", 2)
	if err != nil {
		t.Fatalf("ListIndexed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(got), got)
	}
}

func TestCheckIndexed_RejectsAnEmptyTarget(t *testing.T) {
	f := &fakeSearcher{pages: []SERP{serpOf("example.com")}}
	if _, err := CheckIndexed(context.Background(), f, Query{}, " "); err == nil {
		t.Error("CheckIndexed accepted an empty target")
	}
}

func TestListIndexed_RejectsAnEmptySite(t *testing.T) {
	f := &fakeSearcher{pages: []SERP{serpOf("example.com")}}
	if _, err := ListIndexed(context.Background(), f, Query{}, "  ", 2); err == nil {
		t.Error("ListIndexed accepted an empty site")
	}
}
