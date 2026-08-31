// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// resultAt is one captured result, named after the site it belongs to and the
// place it stood in, so a line read back off the wire says which one it is.
func resultAt(host string, rank int) google.Result {
	return google.Result{
		Title:   fmt.Sprintf("%s at %d", host, rank),
		URL:     fmt.Sprintf("https://%s/%d", host, rank),
		Host:    host,
		Snippet: "what the page said about " + host,
	}
}

// recorded writes a job down and records the same page of results against every
// one of its queries.
//
// It counts the rows back before returning. A fixture that stored something
// else makes every count taken from it a count of the wrong thing, and the
// comparisons below would hold just as well.
func recorded(t *testing.T, st *store.Store, name string, queries []string, results []google.Result) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.CreateJob(ctx, store.JobSpec{Name: name, Pages: 1}, queries)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for ordinal := range queries {
		err := st.Record(ctx, id, store.QueryOutcome{
			Ordinal: ordinal,
			Pages:   []google.SERP{{Origin: "https://www.google.com", Results: results}},
		})
		if err != nil {
			t.Fatalf("recording query %d: %v", ordinal, err)
		}
	}
	if held, want := rowsHeld(t, st, id), len(queries)*len(results); held != want {
		t.Fatalf("the seeded job holds %d rows, want %d", held, want)
	}
	return id
}

// ranked runs one job for one query and puts the site at the rank given, with
// somebody else above it wherever the rank leaves room.
func ranked(t *testing.T, st *store.Store, name, query, host string, rank int) int64 {
	t.Helper()
	if rank < 1 {
		t.Fatalf("no site stands at rank %d", rank)
	}
	results := make([]google.Result, 0, rank)
	for above := 1; above < rank; above++ {
		results = append(results, resultAt(fmt.Sprintf("above%d.example", above), above))
	}
	return recorded(t, st, name, []string{query}, append(results, resultAt(host, rank)))
}

// rowsHeld is how many results the history has for a job, which is the number
// the stream has to match.
func rowsHeld(t *testing.T, st *store.Store, jobID int64) int {
	t.Helper()
	var held int
	if err := st.Rows(context.Background(), jobID, func(store.Row) error { held++; return nil }); err != nil {
		t.Fatalf("Rows: %v", err)
	}
	return held
}

// lineBody is one result as the wire carries it. The names are written out here
// so that renaming a field in the answer fails a test rather than somebody's
// program.
type lineBody struct {
	Ordinal int    `json:"ordinal"`
	Query   string `json:"query"`
	Page    int    `json:"page"`
	Rank    int    `json:"rank"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Host    string `json:"host"`
	Snippet string `json:"snippet"`
}

// positionBody is one place a site held, as the wire carries it.
type positionBody struct {
	JobID   int64  `json:"job_id"`
	JobName string `json:"job_name"`
	TakenAt string `json:"taken_at"`
	Query   string `json:"query"`
	Rank    int    `json:"rank"`
	URL     string `json:"url"`
}

// ndjsonLines splits a streamed answer into the lines it is made of, insisting
// that every one of them reads on its own.
//
// Reading each line by itself is the whole point of the form: a transfer that
// stopped part way is still worth as much as it got, and a body that only
// parses whole would be worth nothing.
func ndjsonLines(t *testing.T, body string) []string {
	t.Helper()
	if body == "" {
		return nil
	}
	if !strings.HasSuffix(body, "\n") {
		t.Errorf("the last line of the answer has no ending, so a reader cannot tell it is whole")
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if line == "" {
			t.Fatalf("the answer carries a blank line, which reads as an object that never arrived")
		}
		var object map[string]any
		if err := json.Unmarshal([]byte(line), &object); err != nil {
			t.Fatalf("a line does not read as JSON on its own: %v (line %q)", err, line)
		}
		out = append(out, line)
	}
	return out
}

func resultsAt(id int64) string {
	return "/api/v1/jobs/" + strconv.FormatInt(id, 10) + "/results"
}

func TestJobResults_CarryEveryRowTheHistoryHoldsForThatJobAndNoOther(t *testing.T) {
	// A count that disagrees is a stream losing or repeating rows, and nobody
	// spots that by eye in a million lines. A second job holds rows of its own,
	// so a handler streaming everything it can read fails here.
	s, st, _, secret := jobServer(t)
	id := recorded(t, st, "nightly", []string{"iphone 13", "golang"},
		[]google.Result{resultAt("first.test", 1), resultAt("second.test", 2)})
	recorded(t, st, "monday", []string{"pixel"}, []google.Result{resultAt("third.test", 1)})

	rec := call(t, s, secret, http.MethodGet, resultsAt(id), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("asking for a job's results gave %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	lines := ndjsonLines(t, rec.Body.String())
	held := rowsHeld(t, st, id)
	// A job holding nothing would make the comparison below true whatever the
	// handler did with the rows it was given.
	if held == 0 {
		t.Fatal("the seeded job holds no rows, so an empty body would pass this")
	}
	if len(lines) != held {
		t.Errorf("the stream carries %d lines and the history holds %d rows", len(lines), held)
	}
}

func TestJobResults_AreOneObjectToALineAndNeverOneArray(t *testing.T) {
	// A million rows in an array have to be built whole at both ends, and a
	// transfer cut short leaves a body no parser will read at all. The type is
	// checked with the shape: a reader told this is JSON hands the whole body to
	// a parser and gets a fault on the second line.
	s, st, _, secret := jobServer(t)
	id := recorded(t, st, "nightly", []string{"iphone 13"},
		[]google.Result{resultAt("first.test", 1), resultAt("second.test", 2)})

	rec := call(t, s, secret, http.MethodGet, resultsAt(id), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("asking for a job's results gave %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/x-ndjson") {
		t.Errorf("the stream is offered as %q, want application/x-ndjson", got)
	}
	body := rec.Body.String()
	if strings.HasPrefix(body, "[") {
		t.Fatalf("the answer opens as an array: %q", body)
	}
	lines := ndjsonLines(t, body)
	if len(lines) != 2 {
		t.Fatalf("%d lines, want the two results the query captured", len(lines))
	}
	for i, line := range lines {
		var row lineBody
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("a line does not read as a result: %v", err)
		}
		if row.Query != "iphone 13" {
			t.Errorf("line %d says it is about %q, want the query it was captured for", i+1, row.Query)
		}
		if row.Rank != i+1 || row.Page != 1 {
			t.Errorf("line %d stands at rank %d on page %d, want rank %d on page 1", i+1, row.Rank, row.Page, i+1)
		}
		if row.Host == "" || row.URL == "" || row.Title == "" {
			t.Errorf("line %d carries no site to act on: %q", i+1, line)
		}
	}
}

func TestJobResults_RefuseAJobNobodyStoredBeforeAnyByteOfTheBodyGoesOut(t *testing.T) {
	// Once a byte is out the status has gone with it and cannot be taken back:
	// the caller has a 200 with a short body, which reads as a job that captured
	// nothing rather than as a job that is not there. Other jobs hold rows, so a
	// handler answering because it could read nothing at all is not mistaken for
	// one that looked for this job.
	s, st, _, secret := jobServer(t)
	recorded(t, st, "monday", []string{"iphone 13"}, []google.Result{resultAt("first.test", 1)})
	recorded(t, st, "tuesday", []string{"golang"}, []google.Result{resultAt("second.test", 1)})

	rec := call(t, s, secret, http.MethodGet, "/api/v1/jobs/9999/results", "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("a job nobody stored gave %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); strings.Contains(got, "ndjson") {
		t.Errorf("the refusal was promised as a stream of results: %q", got)
	}
	refusalOf(t, rec)
	if strings.Contains(rec.Body.String(), "first.test") {
		t.Error("a captured result went out before the job was refused")
	}
}

func TestJobResults_TakeTheJobFromTheAddressAndNothingElse(t *testing.T) {
	// The address names the job, here as it does everywhere else in this
	// interface. A second place to name one is a second answer to the same
	// address, and a caller handed rows they did not ask for has no way to tell:
	// the lines carry no job of their own, only what was captured.
	s, st, _, secret := jobServer(t)
	named := recorded(t, st, "monday", []string{"iphone 13"}, []google.Result{resultAt("named.test", 1)})
	other := recorded(t, st, "tuesday", []string{"golang"}, []google.Result{resultAt("other.test", 1)})

	rec := call(t, s, secret, http.MethodGet,
		resultsAt(named)+"?job="+strconv.FormatInt(other, 10), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("asking for a job's results gave %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	lines := ndjsonLines(t, rec.Body.String())
	if len(lines) != 1 {
		t.Fatalf("%d lines, want the one row the job in the address holds", len(lines))
	}
	var row lineBody
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("a line does not read as a result: %v", err)
	}
	if row.Host != "named.test" {
		t.Errorf("the answer carries %q, which belongs to the job the address does not name", row.Host)
	}
}

func TestJobResults_AndSiteHistoryAreBehindTheKeyCheck(t *testing.T) {
	// An address left open is the whole check gone, and these two hand over
	// everything the history holds.
	s, st, _, _ := jobServer(t)
	id := recorded(t, st, "nightly", []string{"iphone 13"}, []google.Result{resultAt("first.test", 1)})

	for _, target := range []string{resultsAt(id), "/api/v1/history?host=first.test"} {
		rec := call(t, s, "", http.MethodGet, target, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s answered %d without a key, want 401 (body %q)",
				target, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "first.test") {
			t.Errorf("GET %s handed over a captured result without a key", target)
		}
	}
}

func TestSiteHistory_CarriesEveryPositionThatSiteHeldAndNobodyElses(t *testing.T) {
	// This is what a rank tracker is for: one site, and how its number moved.
	// Another site is ranked in the same runs, so a handler that ignores the
	// site it was asked about fails here rather than passing on a count.
	s, st, _, secret := jobServer(t)
	ranked(t, st, "monday", "iphone 13", "example.com", 3)
	ranked(t, st, "tuesday", "iphone 13", "example.com", 1)
	ranked(t, st, "wednesday", "golang", "elsewhere.test", 1)

	rec := call(t, s, secret, http.MethodGet, "/api/v1/history?host=example.com", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("asking for a site's history gave %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/x-ndjson") {
		t.Errorf("the stream is offered as %q, want application/x-ndjson", got)
	}
	lines := ndjsonLines(t, rec.Body.String())
	if len(lines) != 2 {
		t.Fatalf("%d lines, want the two runs the site was ranked in (body %q)", len(lines), rec.Body.String())
	}
	var runs []string
	var ranks []int
	for _, line := range lines {
		var p positionBody
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			t.Fatalf("a line does not read as a position: %v", err)
		}
		if p.URL != "https://example.com/"+strconv.Itoa(p.Rank) {
			t.Errorf("a line puts %q at rank %d, which is somebody else's position", p.URL, p.Rank)
		}
		if p.Query != "iphone 13" || p.JobID == 0 {
			t.Errorf("a line does not say what was asked or which run asked it: %q", line)
		}
		runs = append(runs, p.JobName)
		ranks = append(ranks, p.Rank)
	}
	// Oldest run first, which is what the history hands back and the order a
	// person reads a movement in.
	if runs[0] != "monday" || runs[1] != "tuesday" {
		t.Errorf("the runs arrive as %v, want monday then tuesday", runs)
	}
	if ranks[0] != 3 || ranks[1] != 1 {
		t.Errorf("the site is at %v, want 3 then 1", ranks)
	}
}

func TestSiteHistory_AnswersASiteThatNeverRankedWithNothingRatherThanARefusal(t *testing.T) {
	// A site with no positions is an answer, not a missing thing. A caller that
	// treats a refusal as a fault would report one where nothing is wrong, and a
	// rank tracker is asked about sites that do not rank yet as a matter of
	// course. The history holds positions for another site, so this is a site
	// that was looked for and not found rather than an empty history.
	s, st, _, secret := jobServer(t)
	ranked(t, st, "monday", "iphone 13", "example.com", 1)

	rec := call(t, s, secret, http.MethodGet, "/api/v1/history?host=nowhere.test", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("a site that never ranked gave %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("a site that never ranked was answered with %q, want nothing at all", body)
	}
}

func TestSiteHistory_RefusesARequestThatNamesNoSite(t *testing.T) {
	// Handing back nothing would be an answer the caller believes: they would
	// read it as a site that never ranked and never learn they asked wrongly.
	s, st, _, secret := jobServer(t)
	ranked(t, st, "monday", "iphone 13", "example.com", 1)

	rec := call(t, s, secret, http.MethodGet, "/api/v1/history", "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a request naming no site gave %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if msg := refusalOf(t, rec); !strings.Contains(msg, "host") {
		t.Errorf("the refusal %q does not say what to send instead", msg)
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

// long is a snippet long enough that one line of the stream is worth measuring.
const long = 3000

// wideResults is one page of results big enough that the stream they make is
// larger than anything on the way out holds at once.
func wideResults(host string, rows int) []google.Result {
	filler := strings.Repeat("x", long)
	out := make([]google.Result, 0, rows)
	for rank := 1; rank <= rows; rank++ {
		r := resultAt(host, rank)
		r.Snippet = filler
		out = append(out, r)
	}
	return out
}

func TestStreamedAnswers_ArriveInPiecesRatherThanAllAtOnce(t *testing.T) {
	// This cannot see memory, and says so: what it pins is the consequence a
	// reader feels. An answer gathered first and written at the end arrives as
	// one piece, and the caller sees nothing until the last row has been read.
	// The fixtures are large on purpose — on a job of six rows every
	// implementation writes once, and a test on that fixture would pass on any
	// of them.
	for _, c := range []struct {
		what  string
		build func(*testing.T, *store.Store) string
	}{
		{"a job's results", func(t *testing.T, st *store.Store) string {
			t.Helper()
			return resultsAt(recorded(t, st, "wide", []string{"iphone 13"},
				wideResults("wide.example", 40)))
		}},
		{"a site's history", func(t *testing.T, st *store.Store) string {
			t.Helper()
			// The line of a position carries no snippet, so the length has to
			// come from the query, which every line of this run repeats.
			recorded(t, st, "wide", []string{strings.Repeat("q", long)},
				wideResults("wide.example", 40))
			return "/api/v1/history?host=wide.example"
		}},
	} {
		t.Run(c.what, func(t *testing.T) {
			s, st, _, secret := jobServer(t)
			target := c.build(t, st)

			var body pieces
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.Header.Set("Authorization", "Bearer "+secret)
			s.Handler().ServeHTTP(&body, req)

			if body.total < 64*1024 {
				t.Fatalf("%s is %d bytes, too small to tell one write from many", c.what, body.total)
			}
			if body.largest > body.total/4 {
				t.Errorf("one write carried %d bytes of %d: %s was gathered before it was sent",
					body.largest, body.total, c.what)
			}
		})
	}
}
