// SPDX-License-Identifier: MIT

package export

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// Verdict is what one index check settled about one address.
//
// It is a shape of its own rather than a Row with a column added. A row stands
// for something that was found and carries a title, a rank and a snippet; a
// verdict stands for an address that was asked about, and for half of them
// there is nothing to put in any of those columns. Bending one into the other
// would either put a column on every search export that has no business there,
// or fill a verdict's columns with blanks that read as missing data rather than
// as an answer.
type Verdict struct {
	// Ordinal is the address's place in the list the job was given, so a reader
	// can lay each answer against the line of their own file it came from.
	Ordinal int    `json:"ordinal"`
	Target  string `json:"target"`
	Held    bool   `json:"held"`
}

// VerdictWriter takes verdicts one at a time and finishes the file on Close.
//
// It mirrors Writer, and for the same reasons: a file has one order of lines,
// Close may be called more than once so a deferred Close can sit beside an
// explicit one, and a write after Close returns ErrClosed.
type VerdictWriter interface {
	Write(Verdict) error
	Close() error
}

// NewVerdicts builds the writer for a named format.
//
// It answers for the same names New does. An operator picks a format, not a
// format and a kind of job, and a menu offering something that then refuses is
// a menu that lies.
func NewVerdicts(format string, w io.Writer) (VerdictWriter, error) {
	switch format {
	case "csv":
		return NewVerdictCSV(w), nil
	case "jsonl":
		return NewVerdictJSONL(w), nil
	default:
		return nil, fmt.Errorf("%w: %q, want one of %v", ErrUnknownFormat, format, Formats())
	}
}

type verdictCSV struct {
	w      *csv.Writer
	header bool
	closed bool
}

// NewVerdictCSV writes verdicts as comma-separated values.
//
// Held is written as true or false rather than as a word like yes or held. The
// file is read by programs as often as by people, one spelling serves both
// formats this package writes, and a word would have to be chosen in some
// language — which would make the meaning of the file depend on who downloaded
// it.
func NewVerdictCSV(w io.Writer) VerdictWriter { return &verdictCSV{w: csv.NewWriter(w)} }

func (c *verdictCSV) writeHeader() error {
	if c.header {
		return nil
	}
	c.header = true
	return c.w.Write([]string{"ordinal", "address", "held"})
}

func (c *verdictCSV) Write(v Verdict) error {
	if c.closed {
		return ErrClosed
	}
	if err := c.writeHeader(); err != nil {
		return fmt.Errorf("export: writing the header: %w", err)
	}
	err := c.w.Write([]string{
		strconv.Itoa(v.Ordinal), v.Target, strconv.FormatBool(v.Held),
	})
	if err != nil {
		return fmt.Errorf("export: writing a verdict: %w", err)
	}
	return nil
}

// Close writes the header if no verdict ever arrived, so an export of a job
// that reached nothing is an empty table rather than an empty file that reads
// as a download that broke.
func (c *verdictCSV) Close() error {
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

type verdictJSONL struct {
	enc    *json.Encoder
	closed bool
}

// NewVerdictJSONL writes one self-contained object per line, for the reasons
// NewJSONL gives: a reader can take it a line at a time without holding the
// file, and a file cut short is still readable up to its last complete line.
func NewVerdictJSONL(w io.Writer) VerdictWriter {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &verdictJSONL{enc: enc}
}

func (j *verdictJSONL) Write(v Verdict) error {
	if j.closed {
		return ErrClosed
	}
	if err := j.enc.Encode(v); err != nil {
		return fmt.Errorf("export: writing a verdict: %w", err)
	}
	return nil
}

// Close has nothing to finish: every line was complete when it was written. It
// marks the export done so a late verdict is refused rather than appended.
func (j *verdictJSONL) Close() error {
	j.closed = true
	return nil
}
