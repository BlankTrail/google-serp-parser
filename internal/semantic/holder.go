// SPDX-License-Identifier: MIT

package semantic

import (
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

// Present says the file is there.
func (h *Holder) Present() bool {
	_, err := os.Stat(h.path)
	return err == nil
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
