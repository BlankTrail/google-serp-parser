// SPDX-License-Identifier: MIT

package export

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// Ad is one paid placement, flat, for the same reason a Row is.
//
// It has a placement and no rank. An ad is not a result that happened to be
// paid for: it sits in a block of its own, above the results or below them or
// beside them, and the block it sat in is most of what somebody reading these
// wants to know.
type Ad struct {
	Ordinal   int    `json:"ordinal"`
	Query     string `json:"query"`
	Page      int    `json:"page"`
	Position  int    `json:"position"`
	Placement string `json:"placement"`
	Title     string `json:"title"`
	Host      string `json:"host"`
	URL       string `json:"url"`
	Snippet   string `json:"snippet"`
}

// Suggestion is one search a page offered beside its results.
type Suggestion struct {
	Ordinal  int    `json:"ordinal"`
	Query    string `json:"query"`
	Page     int    `json:"page"`
	Position int    `json:"position"`
	Text     string `json:"text"`
}

// AdWriter and SuggestionWriter take one at a time and finish the file on
// Close, on the same terms as Writer: a write after Close returns ErrClosed,
// and Close may be called more than once.
type AdWriter interface {
	Write(Ad) error
	Close() error
}

// SuggestionWriter is the same for the searches a page suggested.
type SuggestionWriter interface {
	Write(Suggestion) error
	Close() error
}

// NewAds and NewSuggestions build the writer for a named format.
//
// They are separate files rather than more columns on the results, because
// neither is a result: an ad has a placement and no rank, a suggestion is a
// phrase and nothing else, and folding either in would give every result
// several columns that are always empty.
func NewAds(format string, w io.Writer) (AdWriter, error) {
	switch format {
	case "csv":
		return &adCSV{w: csv.NewWriter(w)}, nil
	case "jsonl":
		return &adJSONL{enc: lines(w)}, nil
	default:
		return nil, fmt.Errorf("%w: %q, want one of %v", ErrUnknownFormat, format, Formats())
	}
}

// NewSuggestions builds the writer for a named format.
func NewSuggestions(format string, w io.Writer) (SuggestionWriter, error) {
	switch format {
	case "csv":
		return &suggestionCSV{w: csv.NewWriter(w)}, nil
	case "jsonl":
		return &suggestionJSONL{enc: lines(w)}, nil
	default:
		return nil, fmt.Errorf("%w: %q, want one of %v", ErrUnknownFormat, format, Formats())
	}
}

// lines is an encoder that writes one object per line, escaping nothing: an
// address carries ampersands and a snippet carries angle brackets, and a line
// nobody can read or grep is a worse trade than one that has to be escaped again
// by whoever puts it in a page.
func lines(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc
}

type adCSV struct {
	w      *csv.Writer
	header bool
	closed bool
}

func (c *adCSV) writeHeader() error {
	if c.header {
		return nil
	}
	c.header = true
	return c.w.Write([]string{"ordinal", "query", "page", "position", "placement",
		"title", "host", "url", "snippet"})
}

func (c *adCSV) Write(a Ad) error {
	if c.closed {
		return ErrClosed
	}
	if err := c.writeHeader(); err != nil {
		return fmt.Errorf("export: writing the header: %w", err)
	}
	err := c.w.Write([]string{
		strconv.Itoa(a.Ordinal), a.Query, strconv.Itoa(a.Page), strconv.Itoa(a.Position),
		a.Placement, a.Title, a.Host, a.URL, a.Snippet,
	})
	if err != nil {
		return fmt.Errorf("export: writing a paid placement: %w", err)
	}
	return nil
}

// Close writes the header if nothing ever arrived, so a job that captured no
// advertising is an empty table rather than an empty file that reads as a
// download that broke.
func (c *adCSV) Close() error {
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

type adJSONL struct {
	enc    *json.Encoder
	closed bool
}

func (j *adJSONL) Write(a Ad) error {
	if j.closed {
		return ErrClosed
	}
	if err := j.enc.Encode(a); err != nil {
		return fmt.Errorf("export: writing a paid placement: %w", err)
	}
	return nil
}

func (j *adJSONL) Close() error {
	j.closed = true
	return nil
}

type suggestionCSV struct {
	w      *csv.Writer
	header bool
	closed bool
}

func (c *suggestionCSV) writeHeader() error {
	if c.header {
		return nil
	}
	c.header = true
	return c.w.Write([]string{"ordinal", "query", "page", "position", "text"})
}

func (c *suggestionCSV) Write(g Suggestion) error {
	if c.closed {
		return ErrClosed
	}
	if err := c.writeHeader(); err != nil {
		return fmt.Errorf("export: writing the header: %w", err)
	}
	err := c.w.Write([]string{
		strconv.Itoa(g.Ordinal), g.Query, strconv.Itoa(g.Page), strconv.Itoa(g.Position), g.Text,
	})
	if err != nil {
		return fmt.Errorf("export: writing a related search: %w", err)
	}
	return nil
}

func (c *suggestionCSV) Close() error {
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

type suggestionJSONL struct {
	enc    *json.Encoder
	closed bool
}

func (j *suggestionJSONL) Write(g Suggestion) error {
	if j.closed {
		return ErrClosed
	}
	if err := j.enc.Encode(g); err != nil {
		return fmt.Errorf("export: writing a related search: %w", err)
	}
	return nil
}

func (j *suggestionJSONL) Close() error {
	j.closed = true
	return nil
}
