// SPDX-License-Identifier: MIT

package export

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func sample() []Row {
	return []Row{
		{Ordinal: 0, Query: "iphone 13", Page: 1, Rank: 1,
			Title: "Apple", URL: "https://apple.com/", Host: "apple.com", Snippet: "a phone"},
		{Ordinal: 0, Query: "iphone 13", Page: 1, Rank: 2,
			Title: `He said "hi", then left`, URL: "https://b.test/x?a=1&b=2", Host: "b.test",
			Snippet: "line one\nline two"},
	}
}

// brokenWriter stands in for a disk that has run out.
type brokenWriter struct{}

var errNoRoom = errors.New("no room left")

func (brokenWriter) Write([]byte) (int, error) { return 0, errNoRoom }

func TestCSV_QuotesAValueThatWouldOtherwiseBreakTheColumns(t *testing.T) {
	// A snippet with a comma, a quote or a newline is ordinary. Writing it raw
	// shifts every column after it, and the file still opens — wrongly.
	var buf bytes.Buffer
	w := NewCSV(&buf)
	for _, r := range sample() {
		if err := w.Write(r); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	recs, err := csv.NewReader(bytes.NewReader(buf.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("the file this wrote does not read back as CSV: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("%d records, want a header and two rows", len(recs))
	}
	if recs[0][0] != "ordinal" {
		t.Errorf("first column is %q, want a header", recs[0][0])
	}
	// The columns are found by their heading rather than by counting: a file that
	// grew a column would otherwise fail this test for the wrong reason, and one
	// that lost a column would pass it for the wrong reason.
	if got := column(t, recs, 2, "title"); got != `He said "hi", then left` {
		t.Errorf("the title came back as %q", got)
	}
	if got := column(t, recs, 2, "snippet"); got != "line one\nline two" {
		t.Errorf("the snippet came back as %q", got)
	}
}

func TestCSV_WritesTheHeaderEvenWhenThereAreNoRows(t *testing.T) {
	// An empty export that is an empty file looks like a failed export. A
	// header says the run happened and found nothing.
	var buf bytes.Buffer
	w := NewCSV(&buf)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "ordinal,") {
		t.Errorf("an empty export wrote %q, want the header", buf.String())
	}
}

func TestJSONL_WritesOneSelfContainedObjectPerLine(t *testing.T) {
	// One object per line is what lets a reader stream the file and what keeps
	// a truncated file mostly readable.
	var buf bytes.Buffer
	w := NewJSONL(&buf)
	for _, r := range sample() {
		if err := w.Write(r); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want one per row", len(lines))
	}
	var got Row
	if err := json.Unmarshal([]byte(lines[1]), &got); err != nil {
		t.Fatalf("line two is not an object: %v", err)
	}
	if got.Title != `He said "hi", then left` || got.Snippet != "line one\nline two" {
		t.Errorf("line two came back as %+v", got)
	}
}

func TestJSONL_LeavesAnAddressLookingLikeTheAddressItIs(t *testing.T) {
	// An ampersand in a query string survives a round trip either way, so only
	// the bytes on the line say whether the file is one a person can read and
	// grep.
	var buf bytes.Buffer
	w := NewJSONL(&buf)
	if err := w.Write(sample()[1]); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(buf.String(), "https://b.test/x?a=1&b=2") {
		t.Errorf("the address was written as %q", buf.String())
	}
}

func TestNew_RefusesAFormatItCannotWrite(t *testing.T) {
	if _, err := New("xlsx", &bytes.Buffer{}); !errors.Is(err, ErrUnknownFormat) {
		t.Errorf("New returned %v, want ErrUnknownFormat", err)
	}
}

func TestNew_BuildsEveryFormatItAdvertises(t *testing.T) {
	// A format listed in help that cannot be built is a bug report waiting to
	// be filed.
	for _, f := range Formats() {
		w, err := New(f, &bytes.Buffer{})
		if err != nil {
			t.Errorf("New(%q): %v", f, err)
			continue
		}
		if err := w.Close(); err != nil {
			t.Errorf("Close for %q: %v", f, err)
		}
	}
}

func TestClose_IsSafeToCallASecondTime(t *testing.T) {
	// A deferred Close beside an explicit one is how this gets used, and the
	// second one must neither fail nor append a second header.
	for _, f := range Formats() {
		var buf bytes.Buffer
		w, err := New(f, &buf)
		if err != nil {
			t.Fatalf("New(%q): %v", f, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("%s: first Close: %v", f, err)
		}
		once := buf.String()
		if err := w.Close(); err != nil {
			t.Errorf("%s: second Close: %v", f, err)
		}
		if buf.String() != once {
			t.Errorf("%s: closing twice wrote %q, want %q", f, buf.String(), once)
		}
	}
}

func TestWrite_AfterCloseIsRefusedRatherThanAppendedToAFinishedFile(t *testing.T) {
	// The file is gone by then — handed on, or shut. A row accepted here would
	// be a row the caller believes was recorded.
	for _, f := range Formats() {
		var buf bytes.Buffer
		w, err := New(f, &buf)
		if err != nil {
			t.Fatalf("New(%q): %v", f, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("%s: Close: %v", f, err)
		}
		closed := buf.String()
		if err := w.Write(sample()[0]); !errors.Is(err, ErrClosed) {
			t.Errorf("%s: Write after Close returned %v, want ErrClosed", f, err)
		}
		if buf.String() != closed {
			t.Errorf("%s: Write after Close appended %q", f, buf.String()[len(closed):])
		}
	}
}

func TestCSV_KeepsSayingAFileCouldNotBeFinished(t *testing.T) {
	// A caller that checks only its deferred Close, after an explicit one it
	// ignored, would otherwise be told a lost file was written.
	w := NewCSV(brokenWriter{})
	if err := w.Close(); err == nil {
		t.Fatalf("the first Close reported success on a file that could not be written")
	}
	if err := w.Close(); err == nil {
		t.Errorf("the second Close reported success on the same file")
	}
}

func TestExport_ReportsAFileThatCouldNotBeWritten(t *testing.T) {
	// A full disk that is reported as a finished export costs the whole run.
	for _, f := range Formats() {
		w, err := New(f, brokenWriter{})
		if err != nil {
			t.Fatalf("New(%q): %v", f, err)
		}
		wErr := w.Write(sample()[0])
		cErr := w.Close()
		if wErr == nil && cErr == nil {
			t.Errorf("%s: a file that could not be written reported success", f)
		}
	}
}

// column is one field of one record, found by what the header calls it.
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
