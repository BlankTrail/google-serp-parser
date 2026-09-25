// SPDX-License-Identifier: MIT

package export

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// written is what a table made of these records looks like.
func written[T any](t *testing.T, c Catalog[T], l Layout, recs ...T) string {
	t.Helper()
	var buf bytes.Buffer
	out, err := NewTable(&buf, c, l)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	for _, r := range recs {
		if err := out.Write(r); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := out.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.String()
}

func two() []Row {
	return []Row{
		{Ordinal: 0, Query: "q", Page: 1, Rank: 1, Title: "One", URL: "https://a.test/1", Host: "a.test"},
		{Ordinal: 0, Query: "q", Page: 1, Rank: 2, Title: "Two", URL: "https://b.test/2", Host: "b.test"},
	}
}

func TestTable_WritesTheColumnsInTheOrderTheyWereChosen(t *testing.T) {
	// The order is the reader's: a file that put the rank first because the
	// catalog does would be the file they did not ask for.
	l := DefaultLayout("csv", []string{ColURL, ColRank})
	got := written(t, Results, l, two()...)
	want := "url,rank\nhttps://a.test/1,1\nhttps://b.test/2,2\n"
	if got != want {
		t.Errorf("the file is %q, want %q", got, want)
	}
}

func TestTable_LeavesTheHeaderOutWhenAskedTo(t *testing.T) {
	l := DefaultLayout("csv", []string{ColURL})
	l.Header = false
	if got := written(t, Results, l, two()...); got != "https://a.test/1\nhttps://b.test/2\n" {
		t.Errorf("the file is %q, want the two addresses and nothing above them", got)
	}
}

func TestTable_WritesOneFieldAsABareListOfLines(t *testing.T) {
	// What the whole builder was asked for: one field, one value a line, and
	// nothing else in the file.
	l := DefaultLayout("txt", []string{ColURL})
	l.Header = false
	if got := written(t, Results, l, two()...); got != "https://a.test/1\nhttps://b.test/2\n" {
		t.Errorf("the list is %q", got)
	}
}

func TestTable_EndsEveryLineTheWayItWasAskedTo(t *testing.T) {
	// A file meant for a Windows tool ends its lines there the Windows way, and
	// every line of it does — a header ending one way and the rows another is a
	// file that opens with one blank column.
	for _, format := range Formats() {
		l := DefaultLayout(format, []string{ColURL, ColRank})
		l.EOL = CRLF
		got := written(t, Results, l, two()...)
		if strings.Count(got, "\r\n") != strings.Count(got, "\n") || !strings.HasSuffix(got, "\r\n") {
			t.Errorf("%s ends its lines as %q", format, got)
		}
	}
}

func TestTable_WritesTextAsItIsAndKeepsEachRecordOnOneLine(t *testing.T) {
	// txt is for pasting and for reading by eye: no quotes around a title that
	// holds a quote. What cannot stay is what would break the file — the
	// separator inside a value would shift every column after it and a line
	// break would split the record — so those become spaces.
	l := DefaultLayout("txt", []string{ColTitle, ColURL})
	l.Sep = ';'
	rec := Row{Title: "He said \"hi\"; then\nleft\r", URL: "https://a.test/?q=1;2"}
	got := written(t, Results, l, rec)
	want := "title;url\nHe said \"hi\"  then left ;https://a.test/?q=1 2\n"
	if got != want {
		t.Errorf("the line is %q, want %q", got, want)
	}
}

func TestTable_WritesJSONWithTheChosenKeysInTheirOrderAndNumbersAsNumbers(t *testing.T) {
	// A program reading the line branches on a number without parsing it, and an
	// address stays readable: an ampersand is not written as &.
	l := DefaultLayout("jsonl", []string{ColURL, ColRank})
	rec := Row{URL: "https://a.test/?x=1&y=<2>", Rank: 3}
	want := `{"url":"https://a.test/?x=1&y=<2>","rank":3}` + "\n"
	if got := written(t, Results, l, rec); got != want {
		t.Errorf("the line is %q, want %q", got, want)
	}
}

func TestTable_WritesAVerdictAsAProgramCanBranchOnIt(t *testing.T) {
	// The address has always been "target" in JSON and "address" in a header,
	// and held is a yes a program can test rather than a word in some language.
	v := Verdict{Ordinal: 2, Target: "held.test/a", Held: true}
	if got := written(t, Verdicts, DefaultLayout("jsonl", Verdicts.Names()), v); got != `{"ordinal":2,"target":"held.test/a","held":true}`+"\n" {
		t.Errorf("the verdict is %q", got)
	}
	if got := written(t, Verdicts, DefaultLayout("csv", Verdicts.Names()), v); got != "ordinal,address,held\n2,held.test/a,true\n" {
		t.Errorf("the verdict is %q", got)
	}
}

func TestTable_DropsARecordWhoseChosenColumnsRepeat(t *testing.T) {
	// One column chosen with repeats dropped is a list of its distinct values —
	// the domains a job found, each once, in the order they first stood.
	l := DefaultLayout("txt", []string{ColHost})
	l.Header, l.Unique = false, true
	recs := []Row{{Host: "a.test"}, {Host: "b.test"}, {Host: "a.test"}}
	if got := written(t, Results, l, recs...); got != "a.test\nb.test\n" {
		t.Errorf("the list is %q, want each domain once", got)
	}
	// Two columns are compared as two: "ab" and "c" is not "a" and "bc", though
	// run together they spell the same.
	l = DefaultLayout("txt", []string{ColTitle, ColURL})
	l.Header, l.Unique = false, true
	recs = []Row{{Title: "ab", URL: "c"}, {Title: "a", URL: "bc"}}
	if got := written(t, Results, l, recs...); strings.Count(got, "\n") != 2 {
		t.Errorf("two different records were taken for one: %q", got)
	}
	// Nor does anything a value holds make two records one. Put between the
	// values as a fixed separator, even eight nought bytes can be matched by a
	// value that carries them; the length of each value cannot.
	nul := strings.Repeat("\x00", 8)
	recs = []Row{{Title: "a", URL: nul + "b"}, {Title: "a" + nul, URL: "b"}}
	if got := written(t, Results, l, recs...); strings.Count(got, "\n") != 2 {
		t.Errorf("two different records holding nought bytes were taken for one: %q", got)
	}
}

func TestTable_CountsOnlyWhatItWrote(t *testing.T) {
	// The preview stops at ten records written, and a repeat dropped is not one.
	l := DefaultLayout("csv", []string{ColHost})
	l.Unique = true
	out, err := NewTable(&bytes.Buffer{}, Results, l)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	for _, host := range []string{"a.test", "a.test", "b.test"} {
		if err := out.Write(Row{Host: host}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if out.Written() != 2 {
		t.Errorf("the table says it wrote %d records, want the 2 that were not repeats", out.Written())
	}
}

func TestTable_MarksAFileForASpreadsheetOnlyWhereOneOpensIt(t *testing.T) {
	// The mark is how Excel learns a file is UTF-8. A line of JSON is read by
	// programs, and there the mark is a stray character in front of the first
	// brace.
	for _, format := range Formats() {
		l := DefaultLayout(format, []string{ColURL})
		l.BOM = true
		got := written(t, Results, l, two()[0])
		marked := strings.HasPrefix(got, "\ufeff")
		if want := format != "jsonl"; marked != want {
			t.Errorf("%s begins %q: marked %v, want %v", format, got, marked, want)
		}
	}
}

func TestTable_WritesTheHeaderOfAFileWithNoRecords(t *testing.T) {
	// An empty table rather than an empty file, which reads as a download that
	// broke.
	if got := written[Row](t, Results, DefaultLayout("csv", []string{ColURL, ColHost})); got != "url,host\n" {
		t.Errorf("an empty file is %q, want its header", got)
	}
}

func TestTable_ClosesTwiceAndRefusesARecordAfterward(t *testing.T) {
	var buf bytes.Buffer
	out, err := NewTable(&buf, Results, DefaultLayout("csv", []string{ColURL}))
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	once := buf.String()
	if err := out.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := out.Write(two()[0]); !errors.Is(err, ErrClosed) {
		t.Errorf("Write after Close returned %v, want ErrClosed", err)
	}
	if buf.String() != once {
		t.Errorf("closing twice and writing after wrote %q, want %q", buf.String(), once)
	}
}

type fullDisk struct{}

func (fullDisk) Write([]byte) (int, error) { return 0, errors.New("no room left") }

func TestTable_KeepsSayingAFileCouldNotBeWritten(t *testing.T) {
	// A caller that checks only its deferred Close, after an explicit one it
	// ignored, would otherwise be told a lost file was written.
	for _, format := range Formats() {
		out, err := NewTable(fullDisk{}, Results, DefaultLayout(format, []string{ColURL}))
		if err != nil {
			t.Fatalf("NewTable(%s): %v", format, err)
		}
		wErr := out.Write(two()[0])
		if cErr := out.Close(); wErr == nil && cErr == nil {
			t.Errorf("%s: a file that could not be written reported success", format)
		}
		if err := out.Close(); err == nil {
			t.Errorf("%s: the second Close reported success on the same file", format)
		}
	}
}
