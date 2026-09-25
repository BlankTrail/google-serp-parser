// SPDX-License-Identifier: MIT

package export

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"strings"
)

// bom is the mark a spreadsheet reads as "this file is UTF-8": U+FEFF, written
// out as its three bytes.
const bom = "\xef\xbb\xbf"

// Table writes records of one kind as a layout says: the columns chosen, in
// their order, framed the way the reader asked for.
//
// It is one writer for every part of a job rather than one per part and per
// format. A choice added to the layout — a line ending, a mark for Excel — is
// then made once and holds for results, ads, related searches and verdicts
// alike; with a writer each, it would one day hold for three of them.
//
// A Table is not safe for concurrent use: a file has one order of lines.
type Table[T any] struct {
	l      Layout
	fields []Field[T]
	w      io.Writer
	// csv writes a csv file, quoting what needs it; out writes the other two,
	// which quote nothing.
	csv *csv.Writer
	out *bufio.Writer

	started bool
	closed  bool
	written int
	// seen holds a hash of every record written, when repeats are dropped:
	// eight bytes a distinct record, which is 80 MB on a job of ten million.
	seen map[uint64]struct{}
}

// NewTable builds the writer for a layout, or says why the layout cannot be
// written — before anything is.
func NewTable[T any](w io.Writer, c Catalog[T], l Layout) (*Table[T], error) {
	if err := c.Check(l); err != nil {
		return nil, err
	}
	t := &Table[T]{l: l, w: w}
	for _, name := range l.Fields {
		f, _ := c.field(name)
		t.fields = append(t.fields, f)
	}
	if l.Format == "csv" {
		t.csv = csv.NewWriter(w)
		t.csv.UseCRLF = l.EOL == CRLF
	} else {
		t.out = bufio.NewWriter(w)
	}
	if l.Unique {
		t.seen = map[uint64]struct{}{}
	}
	return t, nil
}

// start writes what comes before the first record: the mark, then the header.
func (t *Table[T]) start() error {
	if t.started {
		return nil
	}
	t.started = true
	if t.l.BOM && t.l.Format != "jsonl" {
		// Straight into the file for csv: its writer has buffered nothing yet,
		// so the mark lands first.
		var err error
		if t.csv != nil {
			_, err = io.WriteString(t.w, bom)
		} else {
			_, err = t.out.WriteString(bom)
		}
		if err != nil {
			return err
		}
	}
	if !t.l.Header || t.l.Format == "jsonl" {
		return nil
	}
	names := make([]string, len(t.fields))
	for i, f := range t.fields {
		names[i] = f.Name
	}
	return t.line(names)
}

// line writes one line of a csv or txt file.
func (t *Table[T]) line(values []string) error {
	if t.csv != nil {
		return t.csv.Write(values)
	}
	for i, v := range values {
		if i > 0 {
			if _, err := t.out.WriteRune(t.l.Sep); err != nil {
				return err
			}
		}
		if _, err := t.out.WriteString(t.plain(v)); err != nil {
			return err
		}
	}
	_, err := t.out.WriteString(t.l.EOL)
	return err
}

// plain is a value as a line of text carries it. Whatever would shift a column
// or break the line becomes a space, and nothing else is touched — no quotes
// round a value, because a txt file is read by eye and pasted, and a quote
// doubled inside quotes is noise to both.
func (t *Table[T]) plain(v string) string {
	return strings.Map(func(r rune) rune {
		if r == t.l.Sep || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, v)
}

// object writes one line of JSON with the chosen keys in their order. It is
// put together by hand because a map would sort the keys and a struct cannot
// know which of them were chosen.
func (t *Table[T]) object(rec T) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	// An address carries ampersands and a snippet angle brackets; escaped, the
	// line is one nobody can read or grep.
	enc.SetEscapeHTML(false)
	b.WriteByte('{')
	for i, f := range t.fields {
		if i > 0 {
			b.WriteByte(',')
		}
		// The encoder ends every value with a newline, which is cut each time.
		if err := enc.Encode(f.key()); err != nil {
			return err
		}
		b.Truncate(b.Len() - 1)
		b.WriteByte(':')
		if err := enc.Encode(f.value(rec)); err != nil {
			return err
		}
		b.Truncate(b.Len() - 1)
	}
	b.WriteByte('}')
	b.WriteString(t.l.EOL)
	_, err := t.out.Write(b.Bytes())
	return err
}

// hash is a record's chosen columns as one number. Each value goes in with its
// length in front, so "ab" and "c" do not collide with "a" and "bc".
func (t *Table[T]) hash(rec T) uint64 {
	h := fnv.New64a()
	var n [8]byte
	for _, f := range t.fields {
		v := f.Text(rec)
		binary.LittleEndian.PutUint64(n[:], uint64(len(v)))
		_, _ = h.Write(n[:])
		_, _ = io.WriteString(h, v)
	}
	return h.Sum64()
}

// Write adds a record, or passes over one whose chosen columns repeat one
// already written when repeats are dropped.
func (t *Table[T]) Write(rec T) error {
	if t.closed {
		return ErrClosed
	}
	if err := t.start(); err != nil {
		return fmt.Errorf("export: writing the header: %w", err)
	}
	if t.seen != nil {
		h := t.hash(rec)
		if _, again := t.seen[h]; again {
			return nil
		}
		t.seen[h] = struct{}{}
	}
	var err error
	if t.l.Format == "jsonl" {
		err = t.object(rec)
	} else {
		values := make([]string, len(t.fields))
		for i, f := range t.fields {
			values[i] = f.Text(rec)
		}
		err = t.line(values)
	}
	if err != nil {
		return fmt.Errorf("export: writing a record: %w", err)
	}
	t.written++
	return nil
}

// Written is how many records went into the file, repeats passed over not
// counted.
func (t *Table[T]) Written() int { return t.written }

// Flush hands on what has been written so far.
func (t *Table[T]) Flush() error {
	if t.csv != nil {
		t.csv.Flush()
		return t.csv.Error()
	}
	return t.out.Flush()
}

// Close writes the header of a file no record reached — an empty table rather
// than an empty file, which reads as a download that broke — and hands the
// rest on. Calling it again writes nothing more and answers as the first call
// did: both writers keep the error that stopped them.
func (t *Table[T]) Close() error {
	t.closed = true
	if err := t.start(); err != nil {
		return fmt.Errorf("export: writing the header: %w", err)
	}
	if err := t.Flush(); err != nil {
		return fmt.Errorf("export: finishing the file: %w", err)
	}
	return nil
}
