// SPDX-License-Identifier: MIT

package export

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
)

type csvWriter struct {
	w      *csv.Writer
	header bool
	closed bool
}

// NewCSV writes rows as comma-separated values.
//
// Quoting is left to encoding/csv rather than done by hand. A snippet holding a
// comma, a quote or a newline is ordinary, and a file written without quoting
// still opens — with every column after it shifted, which is the kind of wrong
// nobody notices until the numbers have been used.
func NewCSV(w io.Writer) Writer { return &csvWriter{w: csv.NewWriter(w)} }

func (c *csvWriter) writeHeader() error {
	if c.header {
		return nil
	}
	c.header = true
	return c.w.Write([]string{"ordinal", "query", "page", "rank", "title", "url", "host", "snippet"})
}

func (c *csvWriter) Write(r Row) error {
	if c.closed {
		return ErrClosed
	}
	if err := c.writeHeader(); err != nil {
		return fmt.Errorf("export: writing the header: %w", err)
	}
	err := c.w.Write([]string{
		strconv.Itoa(r.Ordinal), r.Query, strconv.Itoa(r.Page), strconv.Itoa(r.Rank),
		r.Title, r.URL, r.Host, r.Snippet,
	})
	if err != nil {
		return fmt.Errorf("export: writing a row: %w", err)
	}
	return nil
}

// Close writes the header if no row ever arrived, so an export that found
// nothing is an empty table rather than an empty file that reads as a failure.
//
// Calling Close again writes nothing more — the header is already out and there
// is nothing left to flush — and answers the same as the first call. It is not
// short-circuited on a flag, because a caller that checks only its deferred
// Close must not be told a file was finished when it was not.
func (c *csvWriter) Close() error {
	c.closed = true
	if err := c.writeHeader(); err != nil {
		return fmt.Errorf("export: writing the header: %w", err)
	}
	c.w.Flush()
	if err := c.w.Error(); err != nil {
		return fmt.Errorf("export: finishing the file: %w", err)
	}
	return nil
}
