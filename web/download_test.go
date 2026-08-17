// SPDX-License-Identifier: MIT

package web

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/export"
	"github.com/blanktrail/google-serp-parser/google"
	"github.com/blanktrail/google-serp-parser/store"
)

// resultAt is one captured result, named after the site it belongs to and the
// place it stood in, so a test can read a row back and say which one it is.
func resultAt(host string, rank int) google.Result {
	return google.Result{
		Title:   fmt.Sprintf("%s at %d", host, rank),
		URL:     fmt.Sprintf("https://%s/%d", host, rank),
		Host:    host,
		Snippet: "what the page said about " + host,
	}
}

// seedJob writes a job of total queries, of which done finished and failed
// refused, leaving the rest waiting.
//
// It reads the job back and refuses to hand over one that does not hold what
// its arguments say. A seeder that quietly records something else makes every
// test built on it a test about nothing.
func seedJob(t *testing.T, s *Server, name string, total, done, failed int) int64 {
	t.Helper()
	if total < 1 || done < 0 || failed < 0 || done+failed > total {
		t.Fatalf("no job has %d queries of which %d are done and %d failed", total, done, failed)
	}
	queries := make([]string, total)
	for i := range queries {
		queries[i] = fmt.Sprintf("query %d", i+1)
	}
	ctx := t.Context()
	id, err := s.store.CreateJob(ctx, store.JobSpec{Name: name, Pages: 1}, queries)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for i := range done {
		err := s.store.Record(ctx, id, store.QueryOutcome{
			Ordinal: i,
			Pages: []google.SERP{{Origin: "https://www.google.com", Results: []google.Result{
				resultAt("first.test", 1), resultAt("second.test", 2),
			}}},
		})
		if err != nil {
			t.Fatalf("recording query %d as done: %v", i, err)
		}
	}
	for i := done; i < done+failed; i++ {
		err := s.store.Record(ctx, id, store.QueryOutcome{Ordinal: i, Err: errors.New("refused")})
		if err != nil {
			t.Fatalf("recording query %d as failed: %v", i, err)
		}
	}

	got, err := s.store.Progress(ctx, id)
	if err != nil {
		t.Fatalf("reading back the seeded job: %v", err)
	}
	if got.Name != name || got.Total != total || got.Done != done ||
		got.Failed != failed || got.Pending != total-done-failed {
		t.Fatalf("the seeded job reads back as %q %d/%d/%d/%d, want %q %d/%d/%d/%d",
			got.Name, got.Total, got.Done, got.Failed, got.Pending,
			name, total, done, failed, total-done-failed)
	}
	return id
}

// seedHistory runs one job for each rank given and puts the site at that rank
// in it, oldest run first.
//
// It reads the positions back before returning, for the same reason seedJob
// does: a history test whose fixture put the site somewhere else proves nothing
// about the page it draws.
func seedHistory(t *testing.T, s *Server, host string, ranks ...int) {
	t.Helper()
	ctx := t.Context()
	for run, rank := range ranks {
		if rank < 1 {
			t.Fatalf("no site stands at rank %d", rank)
		}
		id, err := s.store.CreateJob(ctx,
			store.JobSpec{Name: fmt.Sprintf("run %d", run+1), Pages: 1}, []string{"iphone 13"})
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		results := make([]google.Result, 0, rank)
		for above := 1; above < rank; above++ {
			results = append(results, resultAt(fmt.Sprintf("above%d.example", above), above))
		}
		results = append(results, resultAt(host, rank))
		err = s.store.Record(ctx, id, store.QueryOutcome{
			Ordinal: 0,
			Pages:   []google.SERP{{Origin: "https://www.google.com", Results: results}},
		})
		if err != nil {
			t.Fatalf("recording run %d: %v", run+1, err)
		}
	}

	var got []int
	err := s.store.History(ctx, host, func(p store.Position) error {
		got = append(got, p.Rank)
		return nil
	})
	if err != nil {
		t.Fatalf("reading back the seeded history: %v", err)
	}
	if !slices.Equal(got, ranks) {
		t.Fatalf("the seeded history puts %q at %v, want %v", host, got, ranks)
	}
}

func TestDownload_GivesBackEveryRowTheHistoryHolds(t *testing.T) {
	// A count that disagrees is an export losing or repeating rows, and nobody
	// spots that by eye in a million lines.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 3, 3, 0)

	rec := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=csv")
	if rec.Code != http.StatusOK {
		t.Fatalf("export gave %d, want 200", rec.Code)
	}

	records, err := csv.NewReader(bytes.NewReader(rec.Body.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("what came back does not read as CSV: %v", err)
	}
	var held int
	if err := s.store.Rows(t.Context(), id, func(store.Row) error { held++; return nil }); err != nil {
		t.Fatalf("Rows: %v", err)
	}
	// A job holding nothing would make the comparison below true whatever the
	// export did with the rows it was given.
	if held == 0 {
		t.Fatal("the seeded job holds no rows, so an empty file would pass this")
	}
	if len(records)-1 != held {
		t.Errorf("the file carries %d rows and the history holds %d", len(records)-1, held)
	}
}

func TestDownload_RefusesAFormatItCannotWriteBeforeAnyBytesGoOut(t *testing.T) {
	// Learning about a bad format half way down a file leaves a truncated file
	// that looks finished. The headers are checked as well as the body: a
	// refusal that has already promised an attachment is a browser saving the
	// refusal itself under the name of the export.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 1, 1, 0)

	rec := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=xlsx")
	if rec.Code == http.StatusOK {
		t.Fatalf("an unknown format was accepted")
	}
	if got := rec.Header().Get("Content-Disposition"); got != "" {
		t.Errorf("the refusal was promised as a file to save: %q", got)
	}
	if strings.Contains(rec.Body.String(), "ordinal,") {
		t.Error("a header went out before the format was refused")
	}
}

func TestDownload_AnswersNotFoundForAJobThatIsNotThere(t *testing.T) {
	// An empty file for a job that does not exist reads as a run that captured
	// nothing, which is a different and much worse answer.
	rec := get(t, testServer(t), "/export?job=4242&format=csv")
	if rec.Code != http.StatusNotFound {
		t.Errorf("exporting a job that is not there gave %d, want 404", rec.Code)
	}
}

func TestDownload_WritesEveryFormatTheExportKnows(t *testing.T) {
	// The route offers what export can write and nothing else. A format that
	// exists everywhere but here is a link on a page that answers with a
	// refusal.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 2, 2, 0)

	for _, format := range export.Formats() {
		rec := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format="+format)
		if rec.Code != http.StatusOK {
			t.Errorf("%s gave %d, want 200", format, rec.Code)
			continue
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s came back empty", format)
		}
		disposition := rec.Header().Get("Content-Disposition")
		if !strings.Contains(disposition, "nightly") || !strings.Contains(disposition, "."+format) {
			t.Errorf("%s is offered as %q, which names neither the job nor the format", format, disposition)
		}
	}
}

func TestDownload_WritesJSONLinesOneObjectPerLine(t *testing.T) {
	// The second format is written by a different writer, and a route that only
	// ever carried CSV would not say so.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 2, 2, 0)

	body := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=jsonl").Body.String()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 4 {
		t.Fatalf("%d lines, want the four rows the job holds", len(lines))
	}
	for _, line := range lines {
		var row export.Row
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("a line does not read as JSON: %v", err)
		}
		if row.Host == "" {
			t.Errorf("a row came back without the site it is about: %q", line)
		}
	}
}

// refusingWriter takes a fixed number of rows and then refuses, so a test can
// stand where a socket the reader walked away from stands.
type refusingWriter struct {
	takes int
	rows  int
}

func (w *refusingWriter) Write(export.Row) error {
	w.rows++
	if w.rows > w.takes {
		return errors.New("the reader is gone")
	}
	return nil
}

func (w *refusingWriter) Close() error { return nil }

func TestDownload_StopsReadingTheHistoryOnceTheFileWillTakeNoMore(t *testing.T) {
	// A browser that walked away mid-download must not leave the server reading
	// a million rows into a socket nobody is holding. Swallowing the refusal and
	// carrying on is invisible from the outside and costs the whole job.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 5, 5, 0)
	out := &refusingWriter{takes: 1}

	err := s.streamTo(t.Context(), out, id)
	if err == nil {
		t.Fatal("the walk ended quietly though the file refused a row")
	}
	if out.rows != 2 {
		t.Errorf("the file was handed %d rows, want the one it took and the one it refused", out.rows)
	}
}

// pieces is a response that remembers how the body arrived rather than what it
// said: how much in all, and the largest single write.
type pieces struct {
	header  http.Header
	total   int
	largest int
}

func (p *pieces) Header() http.Header {
	if p.header == nil {
		p.header = make(http.Header)
	}
	return p.header
}

func (p *pieces) WriteHeader(int) {}

func (p *pieces) Write(b []byte) (int, error) {
	p.total += len(b)
	p.largest = max(p.largest, len(b))
	return len(b), nil
}

func TestDownload_HandsTheFileOverInPiecesRatherThanAllAtOnce(t *testing.T) {
	// This cannot see memory, and says so: what it pins is the consequence a
	// reader feels. An export assembled first and written at the end arrives as
	// one piece, and the download shows nothing until the last row has been
	// read. The fixture is large on purpose — on a job of six rows every
	// implementation writes once, and a test on that fixture would pass on any
	// of them.
	s := testServer(t)
	id := seedJob(t, s, "wide", 2, 1, 0)
	fill(t, s, id, 1, 40, 3000)

	var body pieces
	s.Handler().ServeHTTP(&body, httptest.NewRequest(http.MethodGet,
		"/export?job="+strconv.FormatInt(id, 10)+"&format=csv", nil))

	if body.total < 64*1024 {
		t.Fatalf("the export is %d bytes, too small to tell one write from many", body.total)
	}
	if body.largest > body.total/4 {
		t.Errorf("one write carried %d bytes of %d: the file was assembled before it was sent",
			body.largest, body.total)
	}
}

// fill records one query's worth of results long enough that the export they
// make is bigger than anything on the way out holds at once.
func fill(t *testing.T, s *Server, jobID int64, ordinal, rows, snippet int) {
	t.Helper()
	long := strings.Repeat("x", snippet)
	results := make([]google.Result, 0, rows)
	for rank := 1; rank <= rows; rank++ {
		r := resultAt("wide.example", rank)
		r.Snippet = long
		results = append(results, r)
	}
	err := s.store.Record(t.Context(), jobID, store.QueryOutcome{
		Ordinal: ordinal,
		Pages:   []google.SERP{{Origin: "https://www.google.com", Results: results}},
	})
	if err != nil {
		t.Fatalf("filling the job: %v", err)
	}
}

func TestAttachmentName_CannotCarryAQuoteOrANewlineIntoTheHeader(t *testing.T) {
	// A job name is typed by a person, and it lands in a header where a quote
	// or a newline is not a character but a syntax.
	got := attachmentName(store.JobSummary{Name: "ni\"ght\r\nly"}, "csv")
	if strings.ContainsAny(got, "\"\r\n") {
		t.Errorf("attachment name %q still carries something the header cannot hold", got)
	}
	if !strings.HasSuffix(got, ".csv") {
		t.Errorf("attachment name %q does not end in the format it is", got)
	}
	if !strings.Contains(got, "ni") || !strings.Contains(got, "ly") {
		t.Errorf("attachment name %q kept nothing of the job it is for", got)
	}
}

func TestAttachmentName_KeepsNothingAHeaderCannotHold(t *testing.T) {
	// The quote and the line ending are the two that end up quoted in every
	// discussion of this header, and they are not the only ones: a semicolon
	// starts the next parameter, a backslash escapes whatever follows it, and a
	// control character is not text at all.
	got := attachmentName(store.JobSummary{Name: "a;b\\c\td\x00e ф"}, "jsonl")
	for _, bad := range []string{";", "\\", "\t", "\x00", "\""} {
		if strings.Contains(got, bad) {
			t.Errorf("attachment name %q carries %q", got, bad)
		}
	}
	if !strings.HasSuffix(got, ".jsonl") {
		t.Errorf("attachment name %q does not end in the format it is", got)
	}
}

func TestAttachmentName_StaysShortEnoughForAHeaderWhateverItIsGiven(t *testing.T) {
	// A job named with a pasted paragraph would otherwise put that paragraph in
	// a header, and every proxy between here and the reader has a limit on how
	// long one may be.
	got := attachmentName(store.JobSummary{Name: strings.Repeat("nightly ", 200)}, "csv")
	if len(got) > 120 {
		t.Errorf("attachment name is %d bytes long", len(got))
	}
}

func TestAttachmentName_NamesSomethingWhenTheJobNameSurvivesNothing(t *testing.T) {
	// A name made entirely of what a header cannot hold must still save as a
	// file, not as a bare extension.
	got := attachmentName(store.JobSummary{Name: "\"\r\n\t"}, "csv")
	if strings.HasPrefix(got, ".") || strings.HasPrefix(got, "-") {
		t.Errorf("attachment name %q begins where its name should be", got)
	}
}

func TestHistory_ShowsWhereASiteStoodOverTime(t *testing.T) {
	// This is what a rank tracker is for: one site, and how its number moved.
	// The addresses are looked for rather than the ranks alone, because a page
	// holding the digits 3 and 1 anywhere at all would pass on those.
	s := testServer(t)
	seedHistory(t, s, "example.com", 3, 1)

	rec := get(t, s, "/history?host=example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("history gave %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "example.com") {
		t.Error("the page does not name the site it is about")
	}
	if !strings.Contains(body, "iphone 13") {
		t.Error("the page does not say which query the site was found for")
	}
	third := strings.Index(body, "https://example.com/3")
	first := strings.Index(body, "https://example.com/1")
	if third < 0 || first < 0 {
		t.Fatalf("the page does not show both positions the site held:\n%s", body)
	}
	if third > first {
		t.Error("the later run is shown above the earlier one, so the page reads backwards")
	}
	for _, name := range []string{"run 1", "run 2"} {
		if !strings.Contains(body, name) {
			t.Errorf("the page does not say which run %q came from", name)
		}
	}
}

func TestHistory_SaysSoWhenASiteNeverRanked(t *testing.T) {
	// An empty table reads as a broken page; a sentence reads as an answer.
	rec := get(t, testServer(t), "/history?host=nowhere.test")
	if rec.Code != http.StatusOK {
		t.Fatalf("history gave %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<table") {
		t.Error("a site with no positions was shown an empty table")
	}
	if !strings.Contains(body, LangEN.T("history.none")) {
		t.Errorf("the page does not say the site has never ranked:\n%s", body)
	}
}

func TestHistory_ShowsNoBareKeyWhereAPhraseBelongs(t *testing.T) {
	// A key on the page is a phrase that was never looked up. The mistake lands
	// on whichever phrase nobody wrote a test about, so this one is over all of
	// them, in both languages, on a page with positions and a page without.
	s := testServer(t)
	seedHistory(t, s, "example.com", 2)

	for _, host := range []string{"example.com", "nowhere.test"} {
		for _, l := range Languages() {
			body := get(t, s, "/history?host="+host+"&lang="+string(l)).Body.String()
			for key := range catalogue[l] {
				if strings.Contains(body, key) {
					t.Errorf("the %s history of %s shows the key %q where its text belongs", l, host, key)
				}
			}
		}
	}
}

func TestDownload_CarriesTheAddressesAnIndexJobFoundNothingFor(t *testing.T) {
	// This is the half of the answer the job was run for. Exporting only what was
	// found leaves an address Google does not hold indistinguishable from one
	// that was never in the list, and the file looks complete either way.
	s := testServer(t)
	id := seedIndexJob(t, s, "is it indexed",
		[]string{"held.test/a", "missing.test/b", "held.test/c"},
		map[string]bool{"held.test/a": true, "held.test/c": true})

	rec := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=csv")
	if rec.Code != http.StatusOK {
		t.Fatalf("export gave %d, want 200", rec.Code)
	}
	records, err := csv.NewReader(bytes.NewReader(rec.Body.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("what came back does not read as CSV: %v", err)
	}
	if len(records)-1 != 3 {
		t.Fatalf("the file carries %d lines, want one for each of the 3 addresses checked: %v",
			len(records)-1, records)
	}
	if got := records[0]; got[1] != "address" || got[2] != "held" {
		t.Errorf("the header is %v, which is not a file of verdicts", got)
	}
	want := [][]string{
		{"0", "held.test/a", "true"},
		{"1", "missing.test/b", "false"},
		{"2", "held.test/c", "true"},
	}
	for i, line := range records[1:] {
		if !slices.Equal(line, want[i]) {
			t.Errorf("line %d came back as %v, want %v", i, line, want[i])
		}
	}
}

func TestDownload_LeavesOutAnAddressTheIndexJobNeverReached(t *testing.T) {
	// An address still waiting has no answer. Writing it as not held reports a
	// check that never happened, and a reader has no way to disbelieve it.
	s := testServer(t)
	ctx := t.Context()
	id, err := s.store.CreateJob(ctx,
		store.JobSpec{Name: "half run", Kind: store.KindIndex, Pages: 1},
		[]string{"held.test/a", "waiting.test/b"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.store.Record(ctx, id, store.QueryOutcome{Ordinal: 0,
		Pages: []google.SERP{{Origin: "https://www.google.com", Results: []google.Result{
			{Title: "held", URL: "https://held.test/a", Host: "held.test"},
		}}}}); err != nil {
		t.Fatalf("recording the address that was checked: %v", err)
	}

	rec := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=jsonl")
	if rec.Code != http.StatusOK {
		t.Fatalf("export gave %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "waiting.test/b") {
		t.Errorf("the export answered for an address nobody checked: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "held.test/a") {
		t.Errorf("the export left out the address that was checked: %q", rec.Body.String())
	}
}

func TestDownload_WritesAnIndexJobAsVerdictsInEveryFormatTheExportKnows(t *testing.T) {
	// An operator picks a format, not a format and a kind of job.
	s := testServer(t)
	id := seedIndexJob(t, s, "both formats",
		[]string{"held.test/a", "missing.test/b"}, map[string]bool{"held.test/a": true})

	for _, format := range export.Formats() {
		rec := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format="+format)
		if rec.Code != http.StatusOK {
			t.Errorf("%s gave %d, want 200", format, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), "missing.test/b") {
			t.Errorf("%s left out the address nothing was found for: %q", format, rec.Body.String())
		}
	}
}

func TestDownload_LeavesASearchJobsExportExactlyAsItWas(t *testing.T) {
	// The verdict shape answers a question a search job was never asked. A search
	// export that came back in it would break every reader already parsing one.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 2, 2, 0)

	rec := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=csv")
	if rec.Code != http.StatusOK {
		t.Fatalf("export gave %d, want 200", rec.Code)
	}
	records, err := csv.NewReader(bytes.NewReader(rec.Body.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("what came back does not read as CSV: %v", err)
	}
	// A job that kept everything carries every column. The list is read from the
	// export rather than written out here: what this test is about is that a
	// parse job's file is a file of results and not the shape a verdict comes in.
	if want := export.Columns(); !slices.Equal(records[0], want) {
		t.Errorf("a parse export's header is %v, want %v", records[0], want)
	}
	// Four rows: two queries, each holding the two results seedJob files.
	if len(records)-1 != 4 {
		t.Errorf("a search export carries %d rows, want the 4 the history holds", len(records)-1)
	}
}

// withAside is a parse job that captured a result, two ads and two suggestions.
//
// The two ads sit in different blocks and the two suggestions in a definite
// order, because a placement written into the wrong column and a list read back
// the other way round are exactly what these files exist to carry.
func withAside(t *testing.T, s *Server, keep string) int64 {
	t.Helper()
	id, err := s.store.CreateJob(t.Context(), store.JobSpec{
		Name: "with what the page carried", Pages: 1, Fields: store.Fields(keep),
	}, []string{"a phrase"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.store.Record(t.Context(), id, store.QueryOutcome{
		Ordinal: 0,
		Pages: []google.SERP{{
			Origin:  "https://www.google.com",
			Results: []google.Result{resultAt("first.test", 1)},
			Ads: []google.Ad{
				{Placement: google.PlacementTop, Title: "top ad", Host: "paid.test",
					URL: "https://paid.test/", Snippet: "what the ad said"},
				{Placement: google.PlacementBottom, Title: "bottom ad", Host: "other.test"},
			},
			// Named so that the order the page had and the order they would fall
			// into if sorted are different: with "first" and "second" a reader that
			// lost the order entirely would still answer this test correctly.
			Related: []string{"zebra came first", "apple came second"},
		}},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	return id
}

func TestDownload_HandsOverWhatThePageCarriedBesidesItsResultsInFilesOfTheirOwn(t *testing.T) {
	// An ad has a placement and no rank, a suggestion is a phrase and nothing
	// else. Folded into the results they would give every result columns that
	// are always empty, so each comes back in a file of its own.
	s := testServer(t)
	id := withAside(t, s, "")

	ads := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=csv&part=ads")
	if ads.Code != http.StatusOK {
		t.Fatalf("the paid placements came back %d", ads.Code)
	}
	recs, err := csv.NewReader(bytes.NewReader(ads.Body.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("what came back does not read as CSV: %v", err)
	}
	if len(recs)-1 != 2 {
		t.Fatalf("the file carries %d ads, want the two on the page: %v", len(recs)-1, recs)
	}
	// Found by heading rather than by counting, so a file that grew a column
	// does not fail this for the wrong reason.
	if got := column(t, recs, 1, "placement"); got != string(google.PlacementTop) {
		t.Errorf("the first ad's block is %q, want the one above the results", got)
	}
	if got := column(t, recs, 2, "placement"); got != string(google.PlacementBottom) {
		t.Errorf("the second ad's block is %q, want the one below them", got)
	}

	rel := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=csv&part=related")
	recs, err = csv.NewReader(bytes.NewReader(rel.Body.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("what came back does not read as CSV: %v", err)
	}
	if len(recs)-1 != 2 {
		t.Fatalf("the file carries %d suggestions, want the two on the page", len(recs)-1)
	}
	// The order is the page's own: a list read back the other way round says
	// something different about what Google offers first.
	if column(t, recs, 1, "text") != "zebra came first" ||
		column(t, recs, 2, "text") != "apple came second" {
		t.Errorf("the suggestions came back in another order: %v", recs)
	}
}

func TestDownload_RefusesAPartTheJobNeverKeptRatherThanHandingOverAnEmptyFile(t *testing.T) {
	// An empty file reads as a page that carried no advertising, which is a
	// different thing from a job that was never asked to keep any.
	s := testServer(t)
	id := withAside(t, s, store.FieldURL)

	for _, part := range []string{"ads", "related"} {
		rec := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=csv&part="+part)
		if rec.Code != http.StatusNotFound {
			t.Errorf("asking for the %s of a job that kept none came back %d, want 404",
				part, rec.Code)
		}
	}
	// And nothing was written down either. The refusal above is read off what the
	// job was asked to keep, so it fires whether or not the rows are there — this
	// is what says the room was actually saved.
	var ads int
	if err := s.store.Ads(t.Context(), id, func(store.Ad) error { ads++; return nil }); err != nil {
		t.Fatalf("Ads: %v", err)
	}
	var suggestions int
	if err := s.store.Suggestions(t.Context(), id, func(store.Suggestion) error {
		suggestions++
		return nil
	}); err != nil {
		t.Fatalf("Suggestions: %v", err)
	}
	if ads != 0 || suggestions != 0 {
		t.Errorf("the job kept %d ads and %d suggestions it was never asked for", ads, suggestions)
	}

	// And the job page does not offer what it cannot hand over.
	body := get(t, s, jobPath(id)).Body.String()
	if strings.Contains(body, "part=ads") || strings.Contains(body, "part=related") {
		t.Errorf("the page offers a file the job never captured:\n%s", body)
	}
}

// column is one field of one record, found by what the header calls it.
//
// By name and not by counting: a file that grew a column would otherwise fail
// every test for the wrong reason, and one that lost a column would pass for
// the wrong reason.
func column(t *testing.T, recs [][]string, row int, name string) string {
	t.Helper()
	for i, head := range recs[0] {
		if head == name {
			return recs[row][i]
		}
	}
	t.Fatalf("the file has no %q column: %v", name, recs[0])
	return ""
}

func TestDownload_SeparatesATextFileByWhatWasAskedFor(t *testing.T) {
	// The separator travels on the address, because a download is a link. If it
	// stopped anywhere between the link and the writer, every file would come
	// back tab-separated whatever the reader typed — and the box would be a box
	// that does nothing.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 2, 1, 0)
	fill(t, s, id, 1, 1, 1)

	rec := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=txt&sep=%3B")
	if rec.Code != http.StatusOK {
		t.Fatalf("the download came back %d:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, ";") {
		t.Errorf("nothing in the file is separated by the semicolon that was asked for:\n%s", body)
	}
	if strings.Contains(body, "\t") {
		t.Errorf("the file is separated by tabs although a semicolon was asked for:\n%s", body)
	}
}

func TestDownload_WritesATabFileWhenNothingWasAskedFor(t *testing.T) {
	// The other half of the same rule: the links on the page carry no separator,
	// and what they must produce is the tab this program documents.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 2, 1, 0)
	fill(t, s, id, 1, 1, 1)

	rec := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("the download came back %d:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\t") {
		t.Errorf("a txt file with nothing asked for is not tab separated:\n%q", rec.Body.String())
	}
}

func TestDownload_RefusesASeparatorItCannotWriteBeforeAnyBytesGoOut(t *testing.T) {
	// Refused before the header goes out, for the reason a format is: once a byte
	// of a file has gone, the only thing left to do with a mistake is stop
	// writing, and a file that stops looks finished to whoever downloaded it.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 2, 1, 0)
	fill(t, s, id, 1, 1, 1)

	rec := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=txt&sep=ab")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a separator of two characters came back %d, want a refusal", rec.Code)
	}
	if rec.Header().Get("Content-Disposition") != "" {
		t.Error("the refusal went out as a download, which a browser saves as a file")
	}
}
