// SPDX-License-Identifier: MIT

// Package export turns captured results into a file someone else can read.
//
// Everything here streams into an io.Writer rather than building a document in
// memory: an export is as large as the job behind it, and the same writers
// serve a file on disk and a response on a socket.
package export

import (
	"errors"
	"io"
	"slices"
)

// Row is one captured result, flat, because that is the shape both a
// spreadsheet and a line-per-object file want.
//
// It is this package's own type rather than the store's. Today the rows come
// from a database; a caller that has them some other way should not have to
// build a database to write a file.
type Row struct {
	Ordinal int    `json:"ordinal"`
	Query   string `json:"query"`
	Page    int    `json:"page"`
	Rank    int    `json:"rank"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Host    string `json:"host"`
	Snippet string `json:"snippet"`
	// Link is the address exactly as the page carried it, which is not always a
	// URL, and DisplayPath is the path as the page drew it. Both are read by the
	// parser and both are kept only when the job asked for them.
	Link        string `json:"link"`
	DisplayPath string `json:"display_path"`
}

// Writer takes rows one at a time and finishes the file on Close.
//
// A Writer is not safe for concurrent use. A file has one order of lines, and
// the caller is the one who knows which order it wants.
//
// Close may be called more than once, so a deferred Close can sit beside an
// explicit one. The later calls write nothing more and answer as the first one
// did. Write after Close returns ErrClosed.
type Writer interface {
	Write(Row) error
	Close() error
}

// ErrUnknownFormat is returned for a name this package cannot write.
var ErrUnknownFormat = errors.New("export: unknown format")

// ErrClosed is returned by Write once the file has been finished. By then the
// file has been handed on or shut, and accepting the row would leave the caller
// believing it was recorded.
var ErrClosed = errors.New("export: the export is already finished")

// Formats lists what New accepts, for help text and for a menu.
func Formats() []string { return []string{"csv", "jsonl", "txt"} }

// Writes reports whether this package can write a named format.
//
// It exists so a caller can refuse a format before it has decided which shape
// of file it is writing. A refusal has to happen before a byte of the response
// has gone out, and by then the caller may not yet know whether it is writing
// rows or verdicts.
func Writes(format string) bool { return slices.Contains(Formats(), format) }

// New builds the writer of results for a named format, carrying every column
// with a header, lines ending the Unix way and tabs between the columns of a
// txt file. It is what a file written from the command line is.
func New(format string, w io.Writer) (Writer, error) {
	// Taken apart rather than returned whole: a nil *Table in a Writer is an
	// interface that is not nil, and a caller testing it would write to nothing.
	t, err := NewTable(w, Results, DefaultLayout(format, Results.Names()))
	if err != nil {
		return nil, err
	}
	return t, nil
}

// The columns a file of results can carry, in the order it writes them when
// nobody chose.
//
// The first four say which result this is — which query, which page of it, and
// where it stood. The rest are what the job was asked to keep.
const (
	ColOrdinal = "ordinal"
	ColQuery   = "query"
	ColPage    = "page"
	ColRank    = "rank"
	ColTitle   = "title"
	ColURL     = "url"
	ColLink    = "link"
	ColHost    = "host"
	ColSnippet = "snippet"
	ColPath    = "display_path"
)

// Columns is every column a file of results may carry, in the order it writes
// them when nobody chose.
func Columns() []string { return Results.Names() }
