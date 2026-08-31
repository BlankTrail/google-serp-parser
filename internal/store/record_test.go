// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/google"
)

func jobWith(t *testing.T, s *Store, queries ...string) int64 {
	t.Helper()
	id, err := s.CreateJob(context.Background(), JobSpec{Name: "j", Pages: 2}, queries)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	return id
}

func TestRecord_TakesAQueryOutOfThePendingListOnceItIsWritten(t *testing.T) {
	// Recorded work must stop being pending in the same breath, or a resume
	// runs it a second time and the history counts it twice.
	s := testStore(t)
	id := jobWith(t, s, "a", "b")

	err := s.Record(context.Background(), id, QueryOutcome{
		Ordinal: 0,
		Pages: []google.SERP{{
			Origin:  "https://www.google.com",
			Results: []google.Result{{Title: "One", URL: "https://example.com/1", Host: "example.com"}},
		}},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	pending, err := s.Pending(context.Background(), id)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 || pending[0].Ordinal != 1 {
		t.Errorf("pending is %+v, want only the second query", pending)
	}
}

func TestRecord_KeepsTheRankAcrossPagesRatherThanRestartingOnEach(t *testing.T) {
	// A position is what a user asked for, and the eleventh result is eleventh
	// whichever page carried it. Numbering from one on every page would report
	// every second-page result as first.
	s := testStore(t)
	id := jobWith(t, s, "a")

	page := func(hosts ...string) google.SERP {
		var rs []google.Result
		for _, h := range hosts {
			rs = append(rs, google.Result{Host: h, URL: "https://" + h + "/x", Title: h})
		}
		return google.SERP{Origin: "https://www.google.com", Results: rs}
	}
	err := s.Record(context.Background(), id, QueryOutcome{
		Ordinal: 0,
		Pages:   []google.SERP{page("a.test", "b.test"), page("c.test")},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	var rank int
	err = s.db.QueryRow(`SELECT rank FROM results WHERE host = 'c.test'`).Scan(&rank)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rank != 3 {
		t.Errorf("the first result of page two has rank %d, want 3", rank)
	}
}

func TestRecord_ReadsBackEveryFieldItWasHandedForAPageAndItsResults(t *testing.T) {
	// A history is read long after the run, by someone who cannot go back and
	// look at the page. A field dropped on the way in is a field nobody can
	// recover, and an empty column reads as a page that said nothing.
	s := testStore(t)
	id := jobWith(t, s, "a")

	err := s.Record(context.Background(), id, QueryOutcome{
		Ordinal: 0,
		Pages: []google.SERP{
			{Origin: "https://www.google.de", Results: []google.Result{{
				Title:   "The title",
				URL:     "https://example.com/one",
				Link:    "/goto?url=opaque",
				Host:    "example.com",
				Snippet: "what the page said about it",
			}}},
			{Origin: "https://www.google.de", Results: []google.Result{{Host: "second.test"}}},
		},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	var (
		number                          int
		origin                          string
		title, url, link, host, snippet string
	)
	if err := s.db.QueryRow(
		`SELECT p.number, p.origin, r.title, r.url, r.link, r.host, r.snippet
		 FROM results r JOIN pages p ON p.id = r.page_id
		 WHERE r.host = 'example.com'`,
	).Scan(&number, &origin, &title, &url, &link, &host, &snippet); err != nil {
		t.Fatalf("query: %v", err)
	}
	if number != 1 || origin != "https://www.google.de" {
		t.Errorf("the result sits on page %d of %q, want page 1 of the origin it came from", number, origin)
	}
	if title != "The title" || url != "https://example.com/one" || link != "/goto?url=opaque" ||
		host != "example.com" || snippet != "what the page said about it" {
		t.Errorf("the result reads back as title=%q url=%q link=%q host=%q snippet=%q",
			title, url, link, host, snippet)
	}

	var second int
	if err := s.db.QueryRow(
		`SELECT p.number FROM results r JOIN pages p ON p.id = r.page_id WHERE r.host = 'second.test'`,
	).Scan(&second); err != nil {
		t.Fatalf("query: %v", err)
	}
	if second != 2 {
		t.Errorf("the second page is numbered %d, want 2", second)
	}
}

func TestRecord_WritesAFailureAsAFailureRatherThanAsNothing(t *testing.T) {
	// A query that was tried and refused is not a query nobody reached. A
	// resume must not run it forever, and a reader must be able to see why.
	s := testStore(t)
	id := jobWith(t, s, "a")

	err := s.Record(context.Background(), id, QueryOutcome{
		Ordinal: 0,
		Err:     errors.New("every attempt at this query came back empty"),
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	var state, msg string
	if err := s.db.QueryRow(`SELECT state, err FROM queries WHERE job_id = ? AND ordinal = 0`, id).Scan(&state, &msg); err != nil {
		t.Fatalf("query: %v", err)
	}
	if state != "failed" {
		t.Errorf("state=%q, want failed", state)
	}
	if msg == "" {
		t.Error("the failure carries no reason")
	}
	pending, err := s.Pending(context.Background(), id)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("a failed query is still pending: %+v", pending)
	}
}

func TestRecord_KeepsThePagesAQueryManagedBeforeItGaveUp(t *testing.T) {
	// A query that failed on its third page still learned what the first two
	// held, and throwing those away would make a partial answer indistinguishable
	// from no answer at all.
	s := testStore(t)
	id := jobWith(t, s, "a")

	err := s.Record(context.Background(), id, QueryOutcome{
		Ordinal: 0,
		Pages:   []google.SERP{{Origin: "https://www.google.com", Results: []google.Result{{Host: "a.test"}}}},
		Err:     errors.New("the second page never arrived"),
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	var results int
	if err := s.db.QueryRow(`SELECT count(*) FROM results`).Scan(&results); err != nil {
		t.Fatalf("query: %v", err)
	}
	if results != 1 {
		t.Errorf("%d results kept from a query that failed part way, want the one it managed", results)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM queries WHERE job_id = ? AND ordinal = 0`, id).Scan(&state); err != nil {
		t.Fatalf("query: %v", err)
	}
	if state != "failed" {
		t.Errorf("state=%q, want failed even though some pages arrived", state)
	}
}

func TestRecord_LeavesNothingBehindWhenAPageIsRefusedPartWayThrough(t *testing.T) {
	// A query written half way — page two present, page one missing — is worse
	// than one not written, because it reads as data. The refusal is arranged
	// to land on the second page, so a page and its results already exist when
	// it does.
	s := testStore(t)
	id := jobWith(t, s, "a")
	if _, err := s.db.Exec(
		`CREATE TRIGGER refuse_the_second_page AFTER INSERT ON pages WHEN NEW.number = 2
		 BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatalf("arranging the refusal: %v", err)
	}

	page := google.SERP{Origin: "https://www.google.com", Results: []google.Result{{Host: "a.test"}}}
	if err := s.Record(context.Background(), id, QueryOutcome{
		Ordinal: 0,
		Pages:   []google.SERP{page, page},
	}); err == nil {
		t.Fatal("Record succeeded although a page was refused")
	}

	var pages, results int
	if err := s.db.QueryRow(`SELECT count(*) FROM pages`).Scan(&pages); err != nil {
		t.Fatalf("query: %v", err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM results`).Scan(&results); err != nil {
		t.Fatalf("query: %v", err)
	}
	if pages != 0 || results != 0 {
		t.Errorf("a failed record left %d pages and %d results behind, want none", pages, results)
	}
	pending, err := s.Pending(context.Background(), id)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Error("the query stopped being pending even though nothing was written")
	}
}

func TestRecord_StopsWhenTheCallerHasAlreadyGivenUp(t *testing.T) {
	s := testStore(t)
	id := jobWith(t, s, "a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.Record(ctx, id, QueryOutcome{
		Ordinal: 0,
		Pages:   []google.SERP{{Results: []google.Result{{Host: "a.test"}}}},
	})
	if err == nil {
		t.Fatal("Record succeeded on a cancelled context")
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM pages`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Errorf("%d pages left behind by a failed record", n)
	}
}

func TestRecord_RefusesAnOrdinalTheJobDoesNotHave(t *testing.T) {
	// Silently writing pages that belong to no query would build a history
	// nobody can read back. An outcome carrying nothing has to be refused just
	// as loudly: the schema itself turns a stray page away, but an empty
	// outcome touches no table, so nothing but this check stands between a
	// caller and a cheerful answer about a query that was never there.
	s := testStore(t)
	id := jobWith(t, s, "a")

	for _, outcome := range []QueryOutcome{
		{Ordinal: 7, Pages: []google.SERP{{Results: []google.Result{{Host: "a.test"}}}}},
		{Ordinal: 7},
	} {
		err := s.Record(context.Background(), id, outcome)
		if err == nil {
			t.Fatalf("Record accepted an ordinal outside the job, given %d pages", len(outcome.Pages))
		}
		if !strings.Contains(err.Error(), "ordinal 7") {
			t.Errorf("the refusal reads %q and does not say which query it could not find", err)
		}
	}

	var pages int
	if err := s.db.QueryRow(`SELECT count(*) FROM pages`).Scan(&pages); err != nil {
		t.Fatalf("query: %v", err)
	}
	if pages != 0 {
		t.Errorf("%d pages written for a query the job does not have", pages)
	}
	pending, err := s.Pending(context.Background(), id)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Error("the job's own query stopped being pending on the strength of an ordinal it does not have")
	}
}

func TestRecord_SurvivesSeveralThreadsWritingAtOnce(t *testing.T) {
	// A job is worked on several queries at a time and each one is written down
	// the moment it lands, so recording is called from every thread at once and
	// has to come back with the same answer it gives a single caller.
	s := testStore(t)
	queries := make([]string, 24)
	for i := range queries {
		queries[i] = fmt.Sprintf("q%02d", i)
	}
	id := jobWith(t, s, queries...)

	var wg sync.WaitGroup
	errs := make([]error, len(queries))
	for i := range queries {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.Record(context.Background(), id, QueryOutcome{
				Ordinal: i,
				Pages: []google.SERP{{
					Origin:  "https://www.google.com",
					Results: []google.Result{{Host: fmt.Sprintf("h%02d.test", i)}},
				}},
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Record(%d): %v", i, err)
		}
	}
	pending, err := s.Pending(context.Background(), id)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("%d queries still pending after every one was recorded", len(pending))
	}
	var results int
	if err := s.db.QueryRow(`SELECT count(*) FROM results`).Scan(&results); err != nil {
		t.Fatalf("query: %v", err)
	}
	if results != len(queries) {
		t.Errorf("%d results written for %d queries recorded at once", results, len(queries))
	}
}

func TestRecord_FilesAResultAtThePlaceTheWalkSaidItStood(t *testing.T) {
	// A check that went looking for one site hands over that site and nothing
	// else, and where it stood is the only thing the check produced. Numbering
	// what arrives would file a site that ranked seventh as first — the one
	// number the job was run for, wrong, with the rest of the row right.
	s := testStore(t)
	id := jobWith(t, s, "a")

	if err := s.Record(context.Background(), id, QueryOutcome{
		Ordinal: 0,
		Pages: []google.SERP{{Origin: "https://www.google.com", Results: []google.Result{
			{Position: 7, Title: "the site", URL: "https://example.com/wanted", Host: "example.com"},
		}}},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var rank int
	if err := s.db.QueryRow(`SELECT rank FROM results`).Scan(&rank); err != nil {
		t.Fatalf("reading the result back: %v", err)
	}
	if rank != 7 {
		t.Errorf("the site was filed at %d, and the walk said it stood at 7", rank)
	}
}

func TestRecord_NumbersAnOrdinaryWalkByWhatArrivedRatherThanByThePage(t *testing.T) {
	// A page numbers its results from one again, page after page. The rank runs
	// across the whole walk, so the first result of page two is third here and
	// not first, and a version that believed the page would restart the numbering
	// on every page of every job.
	s := testStore(t)
	id := jobWith(t, s, "a")

	page := func(hosts ...string) google.SERP {
		rs := make([]google.Result, 0, len(hosts))
		for i, h := range hosts {
			rs = append(rs, google.Result{
				Position: i + 1, Title: h, URL: "https://" + h + "/x", Host: h})
		}
		return google.SERP{Origin: "https://www.google.com", Results: rs}
	}
	if err := s.Record(context.Background(), id, QueryOutcome{
		Ordinal: 0,
		Pages:   []google.SERP{page("one.test", "two.test"), page("three.test", "four.test")},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rows, err := s.db.Query(`SELECT host, rank FROM results ORDER BY rank`)
	if err != nil {
		t.Fatalf("reading the results back: %v", err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]int{}
	for rows.Next() {
		var host string
		var rank int
		if err := rows.Scan(&host, &rank); err != nil {
			t.Fatalf("reading a result: %v", err)
		}
		got[host] = rank
	}
	want := map[string]int{"one.test": 1, "two.test": 2, "three.test": 3, "four.test": 4}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the walk was filed as %v, want %v", got, want)
	}
}
