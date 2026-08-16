// SPDX-License-Identifier: MIT

package export

import (
	"encoding/json"
	"fmt"
	"io"
)

type jsonlWriter struct {
	enc    *json.Encoder
	closed bool
}

// NewJSONL writes one self-contained object per line.
//
// One object per line rather than one array around everything: a reader can
// take it a line at a time without holding the file, and a file cut short by a
// full disk is still readable up to its last complete line.
//
// HTML escaping is off. An address carries ampersands and a snippet carries
// angle brackets, and a line nobody can read or grep is a worse trade than a
// line that has to be escaped again by whoever puts it in a page.
func NewJSONL(w io.Writer) Writer {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &jsonlWriter{enc: enc}
}

func (j *jsonlWriter) Write(r Row) error {
	if j.closed {
		return ErrClosed
	}
	if err := j.enc.Encode(r); err != nil {
		return fmt.Errorf("export: writing a row: %w", err)
	}
	return nil
}

// Close has nothing to finish: every line was complete when it was written. It
// marks the export done so a late row is refused rather than appended, and it
// stays quiet when called again.
func (j *jsonlWriter) Close() error {
	j.closed = true
	return nil
}
