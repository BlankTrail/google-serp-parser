// SPDX-License-Identifier: MIT

// Package expand turns one query into the queries a job's formats make of it.
//
// A format is a line of text with the query's place marked in it — {query} —
// and, optionally, macros standing for a run of values:
//
//	{query}               the query as it is
//	site:{query}          the query with something said around it
//	"{query}"             the query as an exact phrase
//	{query} {ABC:a:z:2}   the query with every word of one and two letters,
//	                      a to z, after it: a … z, aa … zz
//	{query} {num:1:1000}  the query with every number from 1 to 1000 after it
//
// A format with two macros makes every pair of their values. A job's formats
// are all applied to every query, and what they make is kept once: the same
// query reached by two formats, or by two lines of the list, is asked once.
package expand

import (
	"errors"
	"fmt"
	"hash/maphash"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Default is the format a job that names none is run with: the query and
// nothing else, which is what a job always was.
const Default = "{query}"

// The macro of the query itself. {qery} is how the operator spelled it, and is
// read the same.
var queryMacros = []string{"{query}", "{qery}"}

// MaxPerQuery is the most one format may make of one query. A format past it is
// refused rather than run: {ABC:a:z:4} is 475 254 queries for every line of the
// list, and a job of that is a typing mistake far more often than a plan.
const MaxPerQuery = 1_000_000

var (
	// ErrNoQuery is a format that does not say where the query goes.
	ErrNoQuery = errors.New("expand: the format has no {query}")
	// ErrMacro is a macro that cannot be read: a bad range, a count of rounds
	// below one, a bound that is not a number.
	ErrMacro = errors.New("expand: a macro cannot be read")
	// ErrTooMany is a format that makes more than MaxPerQuery of one query.
	ErrTooMany = errors.New("expand: the format makes too many queries of each")
)

// Format is one line of text with the query's place and its macros read out.
type Format struct {
	text  string
	parts []part
}

// part is a piece of a format: literal text, the query, or a run of values.
type part struct {
	literal string
	query   bool
	values  []string
}

// Parse reads one format. Braces that are not a macro this package knows are
// kept as they are written: a query may well want a brace in it.
func Parse(text string) (Format, error) {
	f := Format{text: text}
	hasQuery := false
	rest := text
	for rest != "" {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			f.parts = append(f.parts, part{literal: rest})
			break
		}
		closing := strings.IndexByte(rest[open:], '}')
		if closing < 0 {
			f.parts = append(f.parts, part{literal: rest})
			break
		}
		macro := rest[open : open+closing+1]
		if open > 0 {
			f.parts = append(f.parts, part{literal: rest[:open]})
		}
		rest = rest[open+closing+1:]
		switch p, known, err := macroOf(macro); {
		case err != nil:
			return Format{}, fmt.Errorf("%w: %s: %v", ErrMacro, macro, err)
		case !known:
			f.parts = append(f.parts, part{literal: macro})
		default:
			if p.query {
				hasQuery = true
			}
			f.parts = append(f.parts, p)
		}
	}
	if !hasQuery {
		return Format{}, ErrNoQuery
	}
	if f.Count() > MaxPerQuery {
		return Format{}, fmt.Errorf("%w: %d, at most %d", ErrTooMany, f.Count(), MaxPerQuery)
	}
	return f, nil
}

// macroOf reads one {…}: the query, a run of letters or a run of numbers.
// Anything else is not a macro, and known says so.
func macroOf(macro string) (p part, known bool, err error) {
	for _, q := range queryMacros {
		if strings.EqualFold(macro, q) {
			return part{query: true}, true, nil
		}
	}
	fields := strings.Split(strings.TrimSuffix(strings.TrimPrefix(macro, "{"), "}"), ":")
	switch {
	case strings.EqualFold(fields[0], "ABC") && len(fields) == 4:
		values, err := letters(fields[1], fields[2], fields[3])
		return part{values: values}, true, err
	case strings.EqualFold(fields[0], "num") && len(fields) == 3:
		values, err := numbers(fields[1], fields[2])
		return part{values: values}, true, err
	}
	return part{}, false, nil
}

// letters is every word of one to rounds characters drawn from first to last,
// the shorter words first and each length in order: a … z, aa, ab … zz.
func letters(first, last, rounds string) ([]string, error) {
	from, fromSize := utf8.DecodeRuneInString(first)
	to, toSize := utf8.DecodeRuneInString(last)
	if fromSize != len(first) || toSize != len(last) || first == "" || last == "" {
		return nil, errors.New("the bounds are one character each")
	}
	if to < from {
		return nil, errors.New("the last character comes before the first")
	}
	n, err := strconv.Atoi(rounds)
	if err != nil || n < 1 {
		return nil, errors.New("the rounds are a whole number, one or more")
	}
	var alphabet []string
	for r := from; r <= to; r++ {
		alphabet = append(alphabet, string(r))
	}
	// Counted before it is made, so {ABC:a:z:9} is refused rather than built.
	total, width := 0, 1
	for round := 1; round <= n; round++ {
		width *= len(alphabet)
		total += width
		if total > MaxPerQuery {
			return nil, fmt.Errorf("%d rounds of %d characters is more than %d words", n, len(alphabet), MaxPerQuery)
		}
	}
	out := make([]string, 0, total)
	words := []string{""}
	for round := 1; round <= n; round++ {
		next := make([]string, 0, len(words)*len(alphabet))
		for _, w := range words {
			for _, a := range alphabet {
				next = append(next, w+a)
			}
		}
		out = append(out, next...)
		words = next
	}
	return out, nil
}

// numbers is every whole number from first to last.
func numbers(first, last string) ([]string, error) {
	from, err := strconv.Atoi(first)
	if err != nil {
		return nil, errors.New("the first number is not a whole number")
	}
	to, err := strconv.Atoi(last)
	if err != nil {
		return nil, errors.New("the last number is not a whole number")
	}
	if to < from {
		return nil, errors.New("the last number is below the first")
	}
	if to-from+1 > MaxPerQuery {
		return nil, fmt.Errorf("more than %d numbers", MaxPerQuery)
	}
	out := make([]string, 0, to-from+1)
	for v := from; v <= to; v++ {
		out = append(out, strconv.Itoa(v))
	}
	return out, nil
}

// Count is how many queries the format makes of one.
func (f Format) Count() int {
	n := 1
	for _, p := range f.parts {
		if p.values != nil {
			n *= len(p.values)
			if n > MaxPerQuery {
				return MaxPerQuery + 1
			}
		}
	}
	return n
}

// String is the format as it was written.
func (f Format) String() string { return f.text }

// each makes every query of one, in order: the first macro's values outermost.
// It stops when fn says to, and says whether it was let finish.
func (f Format) each(query string, fn func(string) bool) bool {
	var walk func(i int, made string) bool
	walk = func(i int, made string) bool {
		if i == len(f.parts) {
			return fn(strings.TrimSpace(made))
		}
		p := f.parts[i]
		switch {
		case p.query:
			return walk(i+1, made+query)
		case p.values != nil:
			for _, v := range p.values {
				if !walk(i+1, made+v) {
					return false
				}
			}
			return true
		}
		return walk(i+1, made+p.literal)
	}
	return walk(0, "")
}

// Expander applies a job's formats to its queries and keeps what they make
// once.
//
// What has been made is remembered as a 64-bit hash rather than as text, so a
// job of fourteen million queries costs a couple of hundred megabytes to sort
// out rather than over a gigabyte. Two different queries meeting on one hash is
// a chance of one in tens of thousands at that size and costs one query going
// unasked; it is a trade made knowingly.
type Expander struct {
	formats []Format
	seed    maphash.Seed
	seen    map[uint64]struct{}
}

// NewExpander reads a job's formats. None at all is the default, the query as
// it is; blank lines among them are passed over.
func NewExpander(formats []string) (*Expander, error) {
	e := &Expander{seed: maphash.MakeSeed(), seen: map[uint64]struct{}{}}
	for _, text := range formats {
		if strings.TrimSpace(text) == "" {
			continue
		}
		f, err := Parse(strings.TrimSpace(text))
		if err != nil {
			return nil, err
		}
		e.formats = append(e.formats, f)
	}
	if len(e.formats) == 0 {
		f, _ := Parse(Default)
		e.formats = []Format{f}
	}
	return e, nil
}

// Expand hands every query the formats make of one to fn, once across the
// whole job, and stops at fn's first error.
func (e *Expander) Expand(query string, fn func(string) error) error {
	var failed error
	for _, f := range e.formats {
		if !f.each(query, func(made string) bool {
			if made == "" {
				return true
			}
			h := maphash.String(e.seed, made)
			if _, ok := e.seen[h]; ok {
				return true
			}
			e.seen[h] = struct{}{}
			if err := fn(made); err != nil {
				failed = err
				return false
			}
			return true
		}) {
			return failed
		}
	}
	return nil
}

// PerQuery is how many queries the formats make of one at most, before repeats
// are taken out — what a form can say a list will become.
func (e *Expander) PerQuery() int {
	n := 0
	for _, f := range e.formats {
		n += f.Count()
	}
	return n
}
