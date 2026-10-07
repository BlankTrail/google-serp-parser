// SPDX-License-Identifier: MIT

package semantic

import (
	"io"
	"os"
	"sync"
)

// Holder is the one model of the program: read from its file the first time
// it is asked for and kept, at about 140 MB, for as long as the program runs.
// A model downloaded later is read the next time it is asked for after Reset.
type Holder struct {
	path  string
	mu    sync.Mutex
	model *Model
	err   error
	tried bool
}

// NewHolder holds the model at path, which need not exist yet.
func NewHolder(path string) *Holder { return &Holder{path: path} }

// Path is where the model is kept.
func (h *Holder) Path() string { return h.path }

// State is what can be said of the model file without reading all 140 MB of it.
type State int

const (
	// Missing is no file at the path.
	Missing State = iota
	// Ready is a file that looks like a model and has not failed to load.
	Ready
	// Damaged is a file that is there and is not a model, or one that was read
	// and failed.
	Damaged
)

// Status is a cheap look at the file, cheap enough for a page that redraws
// itself every few seconds: whether it is there, whether it begins with the
// signature of this format, and whether a read of it has already failed. It
// does not load the model, so a file damaged further in is called Ready until
// something reads it — and then Damaged, for as long as the failure is
// remembered. The size is a thing the release knows and this package's reader
// does not, so the caller that knows it checks it as well.
func (h *Holder) Status() State {
	h.mu.Lock()
	failed := h.tried && h.err != nil
	h.mu.Unlock()
	if failed {
		return Damaged
	}
	f, err := os.Open(h.path)
	if err != nil {
		// A file that cannot be opened is as good as not there: whatever stops
		// it being opened stops it being used, and the download that follows
		// will say if it cannot be written either.
		return Missing
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, len(signature))
	if _, err := io.ReadFull(f, head); err != nil || string(head) != signature {
		return Damaged
	}
	return Ready
}

// Get is the model, read once. A file that is not there is not an error worth
// remembering — it may be downloaded in a minute — and is looked for again;
// a file that is there and cannot be read is remembered until Reset, so a
// damaged 140 MB file is not read from disk again on every suggestion.
func (h *Holder) Get() (*Model, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.model != nil || (h.tried && h.err != nil) {
		return h.model, h.err
	}
	m, err := Load(h.path)
	if os.IsNotExist(err) {
		return nil, err
	}
	h.model, h.err, h.tried = m, err, true
	return m, err
}

// Reset forgets the model read, so the next Get reads the file again.
func (h *Holder) Reset() {
	h.mu.Lock()
	h.model, h.err, h.tried = nil, nil, false
	h.mu.Unlock()
}
