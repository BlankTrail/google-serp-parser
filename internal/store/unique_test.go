// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/google"
)

// jobWithFilter writes a job that drops repeats the way this filter says.
//
// Every call writes a job of its own, which is what lets a test say something
// about two jobs at once. A helper that handed the same job back twice would
// make "within a job" and "across every job" one answer, and every test resting
// on the difference would hold either way.
func jobWithFilter(t *testing.T, s *Store, by UniqueBy, queries ...string) int64 {
	t.Helper()
	id, err := s.CreateJob(context.Background(),
		JobSpec{Name: "j", Pages: 2, UniqueBy: by}, queries)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	return id
}

// mustRecordURLs records one query whose results are these addresses.
//
// The address is what is written and the host is read off it, exactly as a
// captured result carries both. Filling the host in from somewhere else would
// let a filter on hosts pass a test its key never touched.
func mustRecordURLs(t *testing.T, s *Store, jobID int64, ordinal int, urls ...string) {
	t.Helper()
	if err := recordURLs(s, jobID, ordinal, urls...); err != nil {
		t.Fatalf("Record(ordinal %d): %v", ordinal, err)
	}
}

func recordURLs(s *Store, jobID int64, ordinal int, urls ...string) error {
	rs := make([]google.Result, 0, len(urls))
	for i, u := range urls {
		rs = append(rs, google.Result{
			Title:   fmt.Sprintf("result %d", i+1),
			URL:     u,
			Host:    google.CanonicalHost(u),
			Snippet: "what the page said",
		})
	}
	return s.Record(context.Background(), jobID, QueryOutcome{
		Ordinal: ordinal,
		Pages:   []google.SERP{{Origin: "https://www.google.com", Results: rs}},
	})
}

// rowCount is how many results a job kept.
func rowCount(t *testing.T, s *Store, jobID int64) int {
	t.Helper()
	var n int
	err := s.db.QueryRow(
		`SELECT count(*) FROM results r
		   JOIN pages   p ON p.id = r.page_id
		   JOIN queries q ON q.id = p.query_id
		  WHERE q.job_id = ?`, jobID).Scan(&n)
	if err != nil {
		t.Fatalf("counting the results of job %d: %v", jobID, err)
	}
	return n
}

// seenCount is how many keys a job has written down.
func seenCount(t *testing.T, s *Store, jobID int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM seen WHERE job_id = ?`, jobID).Scan(&n); err != nil {
		t.Fatalf("counting what job %d has seen: %v", jobID, err)
	}
	return n
}

func mustDroppedCount(t *testing.T, s *Store, jobID int64) int {
	t.Helper()
	n, err := s.DroppedCount(context.Background(), jobID)
	if err != nil {
		t.Fatalf("DroppedCount: %v", err)
	}
	return n
}

func TestRecord_DropsARepeatedAddressWhenTheJobAsksForUniqueRows(t *testing.T) {
	s := testStore(t)
	id := jobWithFilter(t, s, UniqueURL, "a", "b")
	mustRecordURLs(t, s, id, 0, "https://example.com/one")
	mustRecordURLs(t, s, id, 1, "https://example.com/one")

	if n := rowCount(t, s, id); n != 1 {
		t.Errorf("%d rows kept, want 1 — the repeat was written", n)
	}
	if dropped := mustDroppedCount(t, s, id); dropped != 1 {
		t.Errorf("DroppedCount=%d, want 1", dropped)
	}
}

func TestRecord_TreatsTheSameAddressWrittenDifferentlyAsOne(t *testing.T) {
	// Otherwise the filter is a filter on spelling rather than on pages.
	s := testStore(t)
	id := jobWithFilter(t, s, UniqueURL, "a", "b")
	mustRecordURLs(t, s, id, 0, "https://www.example.com/one/")
	mustRecordURLs(t, s, id, 1, "http://EXAMPLE.com/one")

	if n := rowCount(t, s, id); n != 1 {
		t.Errorf("%d rows kept, want 1", n)
	}
}

func TestRecord_KeepsTwoAddressesOfOneSiteWhenAskedForUniqueRows(t *testing.T) {
	// A filter on addresses that quietly filtered by site would throw away every
	// page of a site but the first, which is a different job from the one that
	// was asked for and loses results nobody agreed to lose.
	s := testStore(t)
	id := jobWithFilter(t, s, UniqueURL, "a")
	mustRecordURLs(t, s, id, 0,
		"https://example.com/one", "https://example.com/two", "https://other.test/x")

	if n := rowCount(t, s, id); n != 3 {
		t.Errorf("%d rows kept, want all three — two pages of one site are two addresses", n)
	}
	if dropped := mustDroppedCount(t, s, id); dropped != 0 {
		t.Errorf("DroppedCount=%d, want 0", dropped)
	}
}

func TestRecord_KeepsOneResultPerDomainWhenAskedForUniqueDomains(t *testing.T) {
	s := testStore(t)
	id := jobWithFilter(t, s, UniqueHost, "a")
	mustRecordURLs(t, s, id, 0,
		"https://example.com/one", "https://example.com/two", "https://other.test/x")

	if n := rowCount(t, s, id); n != 2 {
		t.Errorf("%d rows kept, want one per domain", n)
	}
	if dropped := mustDroppedCount(t, s, id); dropped != 1 {
		t.Errorf("DroppedCount=%d, want 1", dropped)
	}
}

func TestRecord_ReadsTheDomainWithoutTheSchemeTheWwwOrTheCase(t *testing.T) {
	// The same site is one site however Google chose to write it out, and a
	// filter that took the address whole would keep every one of these.
	s := testStore(t)
	id := jobWithFilter(t, s, UniqueHost, "a")
	mustRecordURLs(t, s, id, 0,
		"https://example.com/one", "http://www.example.com/two", "https://WWW.Example.COM/three")

	if n := rowCount(t, s, id); n != 1 {
		t.Errorf("%d rows kept, want 1 — these are one site written three ways", n)
	}
}

func TestRecord_WithNoFilterKeepsEverythingAndWritesNoSeenRow(t *testing.T) {
	// A job that did not ask must not pay for the possibility — neither in
	// rows nor in the time it takes to write them.
	s := testStore(t)
	id := jobWithFilter(t, s, UniqueOff, "a")
	mustRecordURLs(t, s, id, 0, "https://example.com/one", "https://example.com/one")

	if n := rowCount(t, s, id); n != 2 {
		t.Errorf("%d rows kept, want both", n)
	}
	if seen := seenCount(t, s, id); seen != 0 {
		t.Errorf("%d rows written to seen by a job with no filter", seen)
	}
	if dropped := mustDroppedCount(t, s, id); dropped != 0 {
		t.Errorf("DroppedCount=%d for a job that filtered nothing, want 0", dropped)
	}
}

func TestRecord_FiltersWithinOneJobAndNotAcrossThem(t *testing.T) {
	// A domain seen last month must not be missing from tonight's run. This is
	// the one that decides whether the filter is useful at all.
	//
	// Both filters are put through it, because a key that forgot which job it
	// belonged to would be shared by every job under either of them, and one
	// filter passing says nothing about the other.
	for _, c := range []struct {
		by         UniqueBy
		first, and string
	}{
		{UniqueURL, "https://example.com/one", "https://example.com/one"},
		{UniqueHost, "https://example.com/one", "https://example.com/two"},
	} {
		t.Run(string(c.by), func(t *testing.T) {
			s := testStore(t)
			first := jobWithFilter(t, s, c.by, "a")
			mustRecordURLs(t, s, first, 0, c.first)
			second := jobWithFilter(t, s, c.by, "a")
			mustRecordURLs(t, s, second, 0, c.and)

			if n := rowCount(t, s, second); n != 1 {
				t.Errorf("the second job kept %d rows — the first job's addresses filtered it", n)
			}
			if dropped := mustDroppedCount(t, s, second); dropped != 0 {
				t.Errorf("the second job counted %d dropped, want 0", dropped)
			}
		})
	}
}

func TestRecord_RemembersWhatItSawAcrossARestart(t *testing.T) {
	// The reason this lives in the database at all: a filter in memory would
	// forget on the restart that resuming exists to survive.
	dir := t.TempDir()
	path := filepath.Join(dir, "gserp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id := jobWithFilter(t, s, UniqueURL, "a", "b")
	mustRecordURLs(t, s, id, 0, "https://example.com/one")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = again.Close() }()

	mustRecordURLs(t, again, id, 1, "https://example.com/one")
	if n := rowCount(t, again, id); n != 1 {
		t.Errorf("%d rows after the restart — what was seen before was forgotten", n)
	}
	if dropped := mustDroppedCount(t, again, id); dropped != 1 {
		t.Errorf("DroppedCount=%d after the restart, want 1", dropped)
	}
}

func TestRecord_AddsToWhatAJobHadAlreadyDroppedRatherThanStartingOver(t *testing.T) {
	// A job is written down one query at a time, and a count that stood for the
	// last query alone would say a ten-hour run dropped whatever the last minute
	// of it dropped.
	s := testStore(t)
	id := jobWithFilter(t, s, UniqueHost, "a", "b")
	mustRecordURLs(t, s, id, 0, "https://example.com/one", "https://example.com/two")
	mustRecordURLs(t, s, id, 1, "https://example.com/three", "https://other.test/x")

	if dropped := mustDroppedCount(t, s, id); dropped != 2 {
		t.Errorf("DroppedCount=%d, want 2 — one repeat from each query", dropped)
	}
	if n := rowCount(t, s, id); n != 2 {
		t.Errorf("%d rows kept, want one per domain", n)
	}
}

func TestRecord_LeavesTheRankWhereTheResultStoodEvenWhenTheOneAboveIsDropped(t *testing.T) {
	// A position is what a page held it at. Closing the gap left by a repeat
	// would move every result below it up, and a reader would be told a site
	// stood higher than it did.
	s := testStore(t)
	id := jobWithFilter(t, s, UniqueHost, "a")
	mustRecordURLs(t, s, id, 0,
		"https://example.com/one", "https://example.com/two", "https://other.test/x")

	var rank int
	if err := s.db.QueryRow(`SELECT rank FROM results WHERE host = 'other.test'`).Scan(&rank); err != nil {
		t.Fatalf("query: %v", err)
	}
	if rank != 3 {
		t.Errorf("the third result of the page reads as rank %d, want 3", rank)
	}
}

func TestRecord_KeepsAResultItHasNoAddressToRecognise(t *testing.T) {
	// Dropping is final. A result carrying nothing to tell it apart by is not a
	// result already seen, and throwing away everything that could not be
	// identified would lose real captures to an empty field.
	s := testStore(t)
	id := jobWithFilter(t, s, UniqueURL, "a")
	if err := s.Record(context.Background(), id, QueryOutcome{
		Ordinal: 0,
		Pages: []google.SERP{{Origin: "https://www.google.com", Results: []google.Result{
			{Title: "one"}, {Title: "two"},
		}}},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if n := rowCount(t, s, id); n != 2 {
		t.Errorf("%d rows kept, want both — neither could be recognised as a repeat", n)
	}
	if seen := seenCount(t, s, id); seen != 0 {
		t.Errorf("%d keys written for results with no address", seen)
	}
}

func TestRecord_ForgetsWhatItSawWhenTheWriteThatSawItDidNotLand(t *testing.T) {
	// The mark and the result go down together or not at all. A mark left behind
	// by a write that failed would drop that address from the job for good, and
	// the retry the runner does next would come back short with nothing to say
	// why. The refusal is arranged to land on the second page, so the first page
	// has already written its results and its keys when it does.
	s := testStore(t)
	id := jobWithFilter(t, s, UniqueURL, "a")
	if _, err := s.db.Exec(
		`CREATE TRIGGER refuse_the_second_page AFTER INSERT ON pages WHEN NEW.number = 2
		 BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatalf("arranging the refusal: %v", err)
	}

	page := func(u string) google.SERP {
		return google.SERP{Origin: "https://www.google.com", Results: []google.Result{
			{Title: u, URL: u, Host: google.CanonicalHost(u)},
		}}
	}
	err := s.Record(context.Background(), id, QueryOutcome{
		Ordinal: 0,
		Pages:   []google.SERP{page("https://example.com/one"), page("https://example.com/two")},
	})
	if err == nil {
		t.Fatal("Record succeeded although a page was refused")
	}
	if seen := seenCount(t, s, id); seen != 0 {
		t.Fatalf("%d keys outlived the write they belonged to", seen)
	}

	if _, err := s.db.Exec(`DROP TRIGGER refuse_the_second_page`); err != nil {
		t.Fatalf("clearing the refusal: %v", err)
	}
	mustRecordURLs(t, s, id, 0, "https://example.com/one", "https://example.com/two")
	if n := rowCount(t, s, id); n != 2 {
		t.Errorf("%d rows kept on the retry, want both — the failed attempt ate an address", n)
	}
	if dropped := mustDroppedCount(t, s, id); dropped != 0 {
		t.Errorf("DroppedCount=%d after a retry that dropped nothing", dropped)
	}
}

func TestDroppedCount_SaysSoForAJobThatIsNotThere(t *testing.T) {
	// Answering nought for a job nobody stored reads as a run that filtered
	// nothing, which is a sentence about a job that does not exist.
	s := testStore(t)
	if _, err := s.DroppedCount(context.Background(), 4242); !errors.Is(err, ErrNoJob) {
		t.Errorf("DroppedCount returned %v, want ErrNoJob", err)
	}
}

func TestProgress_CarriesTheFilterAndWhatItDropped(t *testing.T) {
	// The number is only readable beside the filter that made it, and both have
	// to reach the page from the one read the page already does.
	s := testStore(t)
	id := jobWithFilter(t, s, UniqueHost, "a")
	mustRecordURLs(t, s, id, 0, "https://example.com/one", "https://example.com/two")

	sum, err := s.Progress(context.Background(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.UniqueBy != UniqueHost {
		t.Errorf("the job reads back as filtered by %q, want %q", sum.UniqueBy, UniqueHost)
	}
	if sum.Dropped != 1 {
		t.Errorf("the job reads back with %d dropped, want 1", sum.Dropped)
	}
}

func TestCreateJob_FilesTheFilterTheJobWasAskedFor(t *testing.T) {
	// A filter that did not reach the database would be an option the form
	// offers and the run ignores.
	s := testStore(t)
	id := jobWithFilter(t, s, UniqueHost, "a")

	var by string
	if err := s.db.QueryRow(`SELECT unique_by FROM jobs WHERE id = ?`, id).Scan(&by); err != nil {
		t.Fatalf("query: %v", err)
	}
	if UniqueBy(by) != UniqueHost {
		t.Errorf("the job is filed as filtered by %q, want %q", by, UniqueHost)
	}
}

func TestOpenPlan_FilesTheFilterAJobUploadedAsAFileWasAskedFor(t *testing.T) {
	// A list arriving as a file is the one that runs to ten million results,
	// which is the run the filter was asked for in the first place.
	s := testStore(t)
	plan, err := s.OpenPlan(context.Background(), JobSpec{Name: "j", Pages: 1, UniqueBy: UniqueURL})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	if err := plan.Add(context.Background(), "a"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := plan.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	var by string
	if err := s.db.QueryRow(`SELECT unique_by FROM jobs WHERE id = ?`, plan.JobID()).Scan(&by); err != nil {
		t.Fatalf("query: %v", err)
	}
	if UniqueBy(by) != UniqueURL {
		t.Errorf("the uploaded job is filed as filtered by %q, want %q", by, UniqueURL)
	}
}

func TestLastUnfinished_CarriesTheFilterTheJobWasCreatedUnder(t *testing.T) {
	// A run carried on has to be the run it was: results gathered under two
	// rules with nothing to say which is which is a history nobody can read.
	s := testStore(t)
	want := jobWithFilter(t, s, UniqueHost, "a")

	got, err := s.LastUnfinished(context.Background(), "j")
	if err != nil {
		t.Fatalf("LastUnfinished: %v", err)
	}
	if got.ID != want {
		t.Fatalf("LastUnfinished chose job %d, want %d", got.ID, want)
	}
	if got.Spec.UniqueBy != UniqueHost {
		t.Errorf("the job comes back filtered by %q, want %q", got.Spec.UniqueBy, UniqueHost)
	}
}

func TestRecord_FiltersByTheJobItIsWritingRatherThanByAnyOtherJobAround(t *testing.T) {
	// The filter that is applied has to be the one belonging to the job the
	// results are being filed under. A history holds many jobs at once and they
	// do not agree: reading the filter off whichever job is newest, or oldest,
	// would run one operator's job under another operator's rule, and neither
	// page would say so.
	//
	// Both orders are put through, so a wrong job of any description is caught
	// rather than only a wrong job that happens to be later. Both counts are
	// read as well, because a repeat charged to the wrong job is the same fault
	// told in a different column.
	for _, c := range []struct {
		name          string
		filteredFirst bool
	}{
		{"the filtered job written first", true},
		{"the filtered job written second", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testStore(t)
			var filtered, plain int64
			if c.filteredFirst {
				filtered = jobWithFilter(t, s, UniqueURL, "a")
				plain = jobWithFilter(t, s, UniqueOff, "a")
			} else {
				plain = jobWithFilter(t, s, UniqueOff, "a")
				filtered = jobWithFilter(t, s, UniqueURL, "a")
			}
			// Both jobs are written to, and each has to behave as itself whichever
			// of the two was written down second.
			mustRecordURLs(t, s, filtered, 0, "https://example.com/one", "https://example.com/one")
			mustRecordURLs(t, s, plain, 0, "https://other.test/x", "https://other.test/x")

			if n := rowCount(t, s, filtered); n != 1 {
				t.Errorf("the filtered job kept %d rows, want 1", n)
			}
			if n := rowCount(t, s, plain); n != 2 {
				t.Errorf("the unfiltered job kept %d rows, want both", n)
			}
			if n := mustDroppedCount(t, s, filtered); n != 1 {
				t.Errorf("the filtered job counted %d dropped, want 1", n)
			}
			if n := mustDroppedCount(t, s, plain); n != 0 {
				t.Errorf("the unfiltered job counted %d dropped, want 0", n)
			}
		})
	}
}

func TestRecord_DropsARepeatThatArrivedOnALaterPageOfTheSameQuery(t *testing.T) {
	// A query is taken to several pages in one call, and a repeat turns up on a
	// later page far more often than on the first: that is what depth is for. A
	// filter that looked at the opening page alone would let through nearly
	// every repeat a deep run finds and still report itself as working.
	page := func(urls ...string) google.SERP {
		rs := make([]google.Result, 0, len(urls))
		for _, u := range urls {
			rs = append(rs, google.Result{Title: u, URL: u, Host: google.CanonicalHost(u)})
		}
		return google.SERP{Origin: "https://www.google.com", Results: rs}
	}
	for _, c := range []struct {
		by   UniqueBy
		kept int
	}{
		{UniqueURL, 3},
		{UniqueHost, 2},
	} {
		t.Run(string(c.by), func(t *testing.T) {
			s := testStore(t)
			id := jobWithFilter(t, s, c.by, "a")
			err := s.Record(context.Background(), id, QueryOutcome{
				Ordinal: 0,
				Pages: []google.SERP{
					page("https://example.com/one", "https://other.test/x"),
					page("https://example.com/one", "https://other.test/y"),
				},
			})
			if err != nil {
				t.Fatalf("Record: %v", err)
			}
			if n := rowCount(t, s, id); n != c.kept {
				t.Errorf("%d rows kept, want %d — the second page went unfiltered", n, c.kept)
			}
			if n := mustDroppedCount(t, s, id); n != 4-c.kept {
				t.Errorf("DroppedCount=%d, want %d", n, 4-c.kept)
			}
		})
	}
}
