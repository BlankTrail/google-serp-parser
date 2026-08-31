// SPDX-License-Identifier: MIT

// Package export turns captured results into a file someone else can read.
//
// Everything here streams into an io.Writer rather than building a document in
// memory: an export is as large as the job behind it, and the same writers
// serve a file on disk and a response on a socket.
package export

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
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

// New builds the writer for a named format, carrying every column.
func New(format string, w io.Writer) (Writer, error) {
	return NewWith(format, w, nil)
}

// NewWith builds the writer for a named format, carrying only the columns
// named. Nil is every column.
//
// A column a job never kept is left out of the file rather than written empty.
// An empty column reads as a result that had none of that, which is a different
// thing from one nobody asked to keep — and on a job of ten million results it
// is also several hundred megabytes of separators.
func NewWith(format string, w io.Writer, cols []string) (Writer, error) {
	return NewSeparated(format, w, cols, TabSeparator)
}

// NewSeparated is NewWith, with the separator a text file uses. It is ignored
// by every other format: a comma-separated file is separated by commas and a
// file of one object per line is separated by nothing.
func NewSeparated(format string, w io.Writer, cols []string, sep rune) (Writer, error) {
	switch format {
	case "csv":
		return newCSVWith(w, cols), nil
	case "jsonl":
		return newJSONLWith(w, cols), nil
	case "txt":
		return NewText(w, cols, sep), nil
	default:
		return nil, fmt.Errorf("%w: %q, want one of %v", ErrUnknownFormat, format, Formats())
	}
}

// The columns a file can carry, in the order it writes them.
//
// The first four say which result this is — which query, which page of it, and
// where it stood — and they are always written: a file of addresses with
// nothing saying what was asked or in what order is a bag rather than a result
// page. The rest are what the job was asked to keep.
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

// everyColumn is what a file carries when nobody named the columns.
func everyColumn() []string {
	return []string{ColOrdinal, ColQuery, ColPage, ColRank,
		ColTitle, ColURL, ColLink, ColHost, ColSnippet, ColPath}
}

// Columns is every column a file may carry, for a caller building the list.
func Columns() []string { return everyColumn() }

// valueOf is one column of one row, as text.
//
// A name this does not know answers with nothing rather than refusing: the list
// of columns comes from a job written down some time ago, and a file missing a
// column is a smaller file, while a refusal here is an export that will not run
// at all.
func valueOf(r Row, col string) string {
	switch col {
	case ColOrdinal:
		return strconv.Itoa(r.Ordinal)
	case ColQuery:
		return r.Query
	case ColPage:
		return strconv.Itoa(r.Page)
	case ColRank:
		return strconv.Itoa(r.Rank)
	case ColTitle:
		return r.Title
	case ColURL:
		return r.URL
	case ColLink:
		return r.Link
	case ColHost:
		return r.Host
	case ColSnippet:
		return r.Snippet
	case ColPath:
		return r.DisplayPath
	}
	return ""
}
