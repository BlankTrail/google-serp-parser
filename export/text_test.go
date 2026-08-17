// SPDX-License-Identifier: MIT

package export

import (
	"bytes"
	"encoding/csv"
	"errors"
	"strings"
	"testing"
)

func TestSeparatorOf_ReadsWhatSomebodyTypedIntoABoxThatCannotHoldATab(t *testing.T) {
	// Pressing tab in a box leaves the box, so the tab is what an empty box
	// means — and so is a pasted one, and the two ways somebody writes one down
	// when they cannot type it.
	for _, asked := range []string{"", "  ", "tab", "TAB", `\t`, "\t", "\n"} {
		got, err := SeparatorOf(asked)
		if err != nil {
			t.Errorf("SeparatorOf(%q): %v", asked, err)
			continue
		}
		if got != TabSeparator {
			t.Errorf("SeparatorOf(%q) = %q, want a tab", asked, got)
		}
	}
	for _, asked := range []string{";", "|", ",", "§"} {
		got, err := SeparatorOf(asked)
		if err != nil {
			t.Errorf("SeparatorOf(%q): %v", asked, err)
			continue
		}
		if string(got) != asked {
			t.Errorf("SeparatorOf(%q) = %q, want what was asked for", asked, got)
		}
	}
}

func TestSeparatorOf_RefusesWhatWouldWriteAFileNothingCanReadBack(t *testing.T) {
	// Two characters is a separator this package cannot write — the writer takes
	// one — and a reader who typed two would otherwise get a file separated by
	// the first with no word said about the second. The quote is what a field is
	// quoted with, so a file separated by it reads back as one column.
	for _, asked := range []string{"::", "ab", `"`} {
		if _, err := SeparatorOf(asked); !errors.Is(err, ErrSeparator) {
			t.Errorf("SeparatorOf(%q) returned %v, want ErrSeparator", asked, err)
		}
	}
}

func TestNewText_SeparatesByWhatItWasGivenAndQuotesWhatHoldsIt(t *testing.T) {
	// Quoting is the whole reason this is the comma-separated writer with another
	// separator rather than fields joined by hand. A snippet holding the
	// separator would otherwise shift every column after it, which is the kind of
	// wrong nobody notices until the numbers have been used.
	var buf bytes.Buffer
	w := NewText(&buf, []string{ColQuery, ColTitle, ColURL}, ';')
	err := w.Write(Row{
		Query: "iphone 13",
		Title: "a title; with the separator in it",
		URL:   "https://example.com/",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "query;title;url") {
		t.Errorf("the header is not separated by what was asked for:\n%s", got)
	}
	if !strings.Contains(got, `"a title; with the separator in it"`) {
		t.Errorf("a field holding the separator was written unquoted:\n%s", got)
	}
	// And the file reads back as three columns, which is the only thing that
	// actually matters about the quoting.
	back := csv.NewReader(strings.NewReader(got))
	back.Comma = ';'
	rows, err := back.ReadAll()
	if err != nil {
		t.Fatalf("the file this wrote cannot be read back: %v", err)
	}
	if len(rows) != 2 || len(rows[1]) != 3 {
		t.Errorf("the file reads back as %d rows and %d columns on the first", len(rows), len(rows[1]))
	}
}

func TestNewSeparated_WritesATabFileWhenNobodySaidOtherwise(t *testing.T) {
	// The default matters more than it looks: it is what every link on the page
	// asks for, and a file separated by whatever the writer happened to hold
	// would be a different file each build.
	var buf bytes.Buffer
	w, err := NewWith("txt", &buf, []string{ColQuery, ColURL})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	if err := w.Write(Row{Query: "iphone 13", URL: "https://example.com/"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(buf.String(), "iphone 13\thttps://example.com/") {
		t.Errorf("a txt file with nothing asked for is not tab separated:\n%q", buf.String())
	}
}

func TestFormats_OffersTextEverywhereItOffersAnythingElse(t *testing.T) {
	// An operator picks a format, not a format and a kind of job. A menu offering
	// something that then refuses is a menu that lies.
	for _, format := range Formats() {
		if _, err := NewWith(format, &bytes.Buffer{}, nil); err != nil {
			NewWith := err
			t.Errorf("results in %q: %v", format, NewWith)
		}
		if _, err := NewVerdicts(format, &bytes.Buffer{}, TabSeparator); err != nil {
			t.Errorf("verdicts in %q: %v", format, err)
		}
		if _, err := NewAds(format, &bytes.Buffer{}, TabSeparator); err != nil {
			t.Errorf("paid placements in %q: %v", format, err)
		}
		if _, err := NewSuggestions(format, &bytes.Buffer{}, TabSeparator); err != nil {
			t.Errorf("related searches in %q: %v", format, err)
		}
	}
}
