// SPDX-License-Identifier: MIT

package export

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// TabSeparator is what a text file separates its columns with when nobody says
// otherwise.
//
// A tab, because that is what a column of addresses is pasted into a spreadsheet
// with, and because an address, a title and a snippet all commonly hold commas
// and semicolons and none of them holds a tab.
const TabSeparator = '\t'

// ErrSeparator is returned for a separator this package will not write with.
var ErrSeparator = errors.New("export: that cannot separate columns")

// SeparatorOf reads the separator somebody asked for.
//
// Empty is the tab. The word "tab" is taken as one too, and so is the escape a
// person types when they mean one: a box on a form cannot hold a tab, because
// pressing it leaves the box.
//
// Anything else has to be exactly one character. Two would be a separator this
// package cannot write — the writer takes a single rune — and a reader who
// typed two would otherwise get a file separated by the first of them with no
// word said about the second.
func SeparatorOf(text string) (rune, error) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "", "tab", `\t`:
		return TabSeparator, nil
	}
	sep, size := utf8.DecodeRuneInString(text)
	if sep == utf8.RuneError || size != len(text) {
		return 0, fmt.Errorf("%w: %q, want one character or nothing for a tab", ErrSeparator, text)
	}
	// A file separated by a quote is one nothing can read back, including this
	// program: the quote is what the quoting around a field is made of. The two
	// line endings need no rule of their own — they are spaces, and anything made
	// only of spaces was answered above with the tab.
	if sep == '"' {
		return 0, fmt.Errorf("%w: %q is what a field is quoted with", ErrSeparator, text)
	}
	return sep, nil
}

// separated is a comma-separated writer that separates by something else.
//
// It is one function because four kinds of file are written this way — results,
// verdicts, paid placements and suggested searches — and a separator set in
// four places is a separator that will one day be set in three.
func separated(w io.Writer, sep rune) *csv.Writer {
	out := csv.NewWriter(w)
	out.Comma = sep
	return out
}

// NewText writes rows as text separated by the given character, carrying only
// the columns named. Nil is every column.
//
// It is the comma-separated writer with another separator, quoting and all.
// Written without quoting, a snippet holding the separator would shift every
// column after it — which is the kind of wrong nobody notices until the numbers
// have been used.
func NewText(w io.Writer, cols []string, sep rune) Writer {
	if cols == nil {
		cols = everyColumn()
	}
	return &csvWriter{w: separated(w, sep), cols: cols}
}
