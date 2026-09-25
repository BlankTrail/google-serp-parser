// SPDX-License-Identifier: MIT

package export

import (
	"bytes"
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

func TestNew_WritesATabFileWhenNobodySaidOtherwise(t *testing.T) {
	// The default matters more than it looks: it is what every link on the page
	// asks for, and a file separated by whatever the writer happened to hold
	// would be a different file each build.
	var buf bytes.Buffer
	w, err := NewTable(&buf, Results, DefaultLayout("txt", []string{ColQuery, ColURL}))
	if err != nil {
		t.Fatalf("NewTable: %v", err)
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

func TestFormats_OffersEveryFormatForEveryPartOfAJob(t *testing.T) {
	// An operator picks a format, not a format and a kind of job. A menu offering
	// something that then refuses is a menu that lies.
	for _, format := range Formats() {
		if _, err := NewTable(&bytes.Buffer{}, Results, DefaultLayout(format, Results.Names())); err != nil {
			t.Errorf("results in %q: %v", format, err)
		}
		if _, err := NewTable(&bytes.Buffer{}, Ads, DefaultLayout(format, Ads.Names())); err != nil {
			t.Errorf("paid placements in %q: %v", format, err)
		}
		if _, err := NewTable(&bytes.Buffer{}, Suggestions, DefaultLayout(format, Suggestions.Names())); err != nil {
			t.Errorf("related searches in %q: %v", format, err)
		}
		if _, err := NewTable(&bytes.Buffer{}, Verdicts, DefaultLayout(format, Verdicts.Names())); err != nil {
			t.Errorf("verdicts in %q: %v", format, err)
		}
	}
}
