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
func Formats() []string { return []string{"csv", "jsonl"} }

// New builds the writer for a named format.
func New(format string, w io.Writer) (Writer, error) {
	switch format {
	case "csv":
		return NewCSV(w), nil
	case "jsonl":
		return NewJSONL(w), nil
	default:
		return nil, fmt.Errorf("%w: %q, want one of %v", ErrUnknownFormat, format, Formats())
	}
}
