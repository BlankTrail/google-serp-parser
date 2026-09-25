// SPDX-License-Identifier: MIT

package export

import (
	"errors"
	"fmt"
	"strconv"
)

// The columns the other parts of a job carry, beside the ones a result does.
const (
	ColPosition  = "position"
	ColPlacement = "placement"
	ColText      = "text"
	ColAddress   = "address"
	ColHeld      = "held"
)

// Field is one column a file can carry: what a header and a download call it,
// what a line of JSON calls it, and how it is read off a record.
type Field[T any] struct {
	// Name is the column's name in a header and in the list a download asks for.
	Name string
	// Key is what a line of JSON calls it, and empty where that is Name. It
	// differs in one place: a verdict's address has always been "target" in
	// JSON and "address" in a header, and a reader of either keeps working.
	Key string
	// Text is the column as a line of text carries it.
	Text func(T) string
	// Value is the column as a line of JSON carries it, and nil where that is
	// Text: a number stays a number and a yes stays a yes, so a program reading
	// the file can branch on them without parsing words.
	Value func(T) any
}

func (f Field[T]) key() string {
	if f.Key != "" {
		return f.Key
	}
	return f.Name
}

func (f Field[T]) value(rec T) any {
	if f.Value != nil {
		return f.Value(rec)
	}
	return f.Text(rec)
}

// Catalog is every column one kind of record can be written with, in the order
// a file carries them when nobody chose.
type Catalog[T any] struct {
	Fields []Field[T]
}

// Names is every column of the catalog, in its order.
func (c Catalog[T]) Names() []string {
	names := make([]string, len(c.Fields))
	for i, f := range c.Fields {
		names[i] = f.Name
	}
	return names
}

func (c Catalog[T]) field(name string) (Field[T], bool) {
	for _, f := range c.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field[T]{}, false
}

// LF and CRLF are the two ways a line of a file may end: the first is what
// everything but Windows' own tools reads, the second is what those expect.
const (
	LF   = "\n"
	CRLF = "\r\n"
)

// ErrLayout is returned for a layout a catalog cannot write.
var ErrLayout = errors.New("export: that file cannot be written")

// Layout is how a file is written: which columns, in what order, and what
// frames them.
type Layout struct {
	// Format is csv, txt or jsonl.
	Format string
	// Fields are the columns, in the order the file carries them.
	Fields []string
	// Header says a csv or txt file begins with the columns' names. A line of
	// JSON names its own fields and never has one.
	Header bool
	// EOL is what ends every line: LF or CRLF.
	EOL string
	// Sep separates the columns of a txt file and means nothing elsewhere.
	Sep rune
	// Unique drops a record whose chosen columns repeat one already written, so
	// one column chosen is a list of its distinct values.
	Unique bool
	// BOM starts a csv or txt file with the mark that tells a spreadsheet it is
	// UTF-8. Without it Excel opens a file of Cyrillic as mojibake.
	BOM bool
}

// DefaultLayout is the file written when nobody chose anything but the format:
// every column given, a header, lines ending the Unix way, tabs between the
// columns of a txt file, and nothing dropped.
func DefaultLayout(format string, fields []string) Layout {
	return Layout{Format: format, Fields: fields, Header: true, EOL: LF, Sep: TabSeparator}
}

// Check says whether this catalog can write a layout.
func (c Catalog[T]) Check(l Layout) error {
	if !Writes(l.Format) {
		return fmt.Errorf("%w: %q, want one of %v", ErrUnknownFormat, l.Format, Formats())
	}
	if len(l.Fields) == 0 {
		return fmt.Errorf("%w: no column was chosen", ErrLayout)
	}
	seen := make(map[string]bool, len(l.Fields))
	for _, name := range l.Fields {
		if _, ok := c.field(name); !ok {
			return fmt.Errorf("%w: %q is not a column of this file", ErrLayout, name)
		}
		// Twice is two columns saying the same thing, and a reader matching
		// columns by their heading finds whichever comes first.
		if seen[name] {
			return fmt.Errorf("%w: %q is chosen twice", ErrLayout, name)
		}
		seen[name] = true
	}
	if l.EOL != LF && l.EOL != CRLF {
		return fmt.Errorf("%w: a line ends with LF or CRLF", ErrLayout)
	}
	return nil
}

func itoa(n int) string { return strconv.Itoa(n) }

// Results is what a result can be written with.
var Results = Catalog[Row]{Fields: []Field[Row]{
	{Name: ColOrdinal, Text: func(r Row) string { return itoa(r.Ordinal) }, Value: func(r Row) any { return r.Ordinal }},
	{Name: ColQuery, Text: func(r Row) string { return r.Query }},
	{Name: ColPage, Text: func(r Row) string { return itoa(r.Page) }, Value: func(r Row) any { return r.Page }},
	{Name: ColRank, Text: func(r Row) string { return itoa(r.Rank) }, Value: func(r Row) any { return r.Rank }},
	{Name: ColTitle, Text: func(r Row) string { return r.Title }},
	{Name: ColURL, Text: func(r Row) string { return r.URL }},
	{Name: ColLink, Text: func(r Row) string { return r.Link }},
	{Name: ColHost, Text: func(r Row) string { return r.Host }},
	{Name: ColSnippet, Text: func(r Row) string { return r.Snippet }},
	{Name: ColPath, Text: func(r Row) string { return r.DisplayPath }},
}}

// Ads is what a paid placement can be written with.
var Ads = Catalog[Ad]{Fields: []Field[Ad]{
	{Name: ColOrdinal, Text: func(a Ad) string { return itoa(a.Ordinal) }, Value: func(a Ad) any { return a.Ordinal }},
	{Name: ColQuery, Text: func(a Ad) string { return a.Query }},
	{Name: ColPage, Text: func(a Ad) string { return itoa(a.Page) }, Value: func(a Ad) any { return a.Page }},
	{Name: ColPosition, Text: func(a Ad) string { return itoa(a.Position) }, Value: func(a Ad) any { return a.Position }},
	{Name: ColPlacement, Text: func(a Ad) string { return a.Placement }},
	{Name: ColTitle, Text: func(a Ad) string { return a.Title }},
	{Name: ColHost, Text: func(a Ad) string { return a.Host }},
	{Name: ColURL, Text: func(a Ad) string { return a.URL }},
	{Name: ColSnippet, Text: func(a Ad) string { return a.Snippet }},
}}

// Suggestions is what a related search can be written with.
var Suggestions = Catalog[Suggestion]{Fields: []Field[Suggestion]{
	{Name: ColOrdinal, Text: func(g Suggestion) string { return itoa(g.Ordinal) }, Value: func(g Suggestion) any { return g.Ordinal }},
	{Name: ColQuery, Text: func(g Suggestion) string { return g.Query }},
	{Name: ColPage, Text: func(g Suggestion) string { return itoa(g.Page) }, Value: func(g Suggestion) any { return g.Page }},
	{Name: ColPosition, Text: func(g Suggestion) string { return itoa(g.Position) }, Value: func(g Suggestion) any { return g.Position }},
	{Name: ColText, Text: func(g Suggestion) string { return g.Text }},
}}

// Verdicts is what an index check's answer can be written with. Held is written
// as true or false rather than a word: the file is read by programs as often as
// by people, and a word would have to be chosen in some language — which would
// make the meaning of the file depend on who downloaded it.
var Verdicts = Catalog[Verdict]{Fields: []Field[Verdict]{
	{Name: ColOrdinal, Text: func(v Verdict) string { return itoa(v.Ordinal) }, Value: func(v Verdict) any { return v.Ordinal }},
	{Name: ColAddress, Key: "target", Text: func(v Verdict) string { return v.Target }},
	{Name: ColHeld, Text: func(v Verdict) string { return strconv.FormatBool(v.Held) }, Value: func(v Verdict) any { return v.Held }},
}}
