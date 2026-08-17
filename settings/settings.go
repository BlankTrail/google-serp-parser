// SPDX-License-Identifier: MIT

// Package settings keeps what the program was told to connect to, in a file of
// its own beside the history.
//
// It is a separate file because the key in it cannot be stored as a hash — the
// program has to use it — and a database that carries no key can be copied,
// handed over or backed up without handing the key over with it.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrUnreadable is returned for a file that exists and is not settings.
var ErrUnreadable = errors.New("settings: the file is there and cannot be read as settings")

// tailLen is how much of the key is shown so a person can tell which one is
// set. Four characters name a key and are useless as one.
const tailLen = 4

// ProxySource is where the addresses come from and how often to look again.
type ProxySource struct {
	Kind     string        `json:"kind"`     // "file" | "url" | ""
	Location string        `json:"location"` // path or address
	Refresh  time.Duration `json:"refresh"`
}

// Settings is everything the interface can set.
type Settings struct {
	ControlURL string `json:"control_url"`
	APIKey     string `json:"api_key"`

	// The pause one identity keeps between two requests is not here. It was, and
	// it was wrong: how hard a list may be pushed depends on the list and on what
	// is being asked of it, and one machine runs a careful job and a fast one on
	// the same afternoon. It belongs to the job, is set on the job's own form,
	// and a file written by an older version keeps the number harmlessly — this
	// program no longer reads it.

	// HotPorts is how many identities this machine keeps open and warm between
	// jobs, and HotDevice is which kind of result page they are opened for.
	//
	// They are also the one pool this machine keeps standing, so the search
	// answered inside a request goes through them: a job's pool is gone between
	// jobs, and that address would otherwise have nothing to answer on. None kept
	// warm is that address turned off, which it says rather than waiting.
	//
	// The field that used to size that pool separately is gone. It sized nothing:
	// the pool was opened from the flags this server was started with, and two
	// numbers for one set of identities were two answers to one question.
	//
	// Nought is off, and off is what this program did until now: every job opened
	// its own identities from cold, met a challenge on the first request of each,
	// and produced nothing for the minutes that took. Kept warm, they answer at
	// once — measured at seconds against minutes — at the price of a few ports
	// standing open all day and one dull request through each every quarter of an
	// hour.
	//
	// A job of the same kind of page grows this set to its own size and gives the
	// growth back when it ends; a job of the other kind opens its own from cold,
	// because a phone's results are not a desktop's and an identity cannot be
	// both.
	HotPorts  int         `json:"hot_ports"`
	HotDevice string      `json:"hot_device"`
	Proxy     ProxySource `json:"proxy"`
	Language  string      `json:"language"`
}

// DefaultControlURL is where the identities are asked for on a machine where
// nobody has said otherwise.
//
// It is filled in rather than left blank because it is right for nearly
// everybody: the service runs on the same machine as this program and answers
// there. A blank box would make every reader look up an address they already
// have, and one they cannot check without leaving the page.
const DefaultControlURL = "http://127.0.0.1:8891/"

// Defaults are what a program that has never been configured runs on.
func Defaults() Settings {
	return Settings{ControlURL: DefaultControlURL}
}

// Load reads the settings, or the defaults when there is no file yet.
//
// A missing file is not a fault: it is somebody who has not opened the settings
// yet. A file that exists and cannot be read is a different thing entirely, and
// answering it with the defaults would send the program somewhere its owner did
// not tell it to go.
func Load(path string) (Settings, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Defaults(), nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("settings: reading %s: %w", filepath.Base(path), err)
	}
	s := Defaults()
	if err := json.Unmarshal(raw, &s); err != nil {
		return Settings{}, fmt.Errorf("%w: %v", ErrUnreadable, err)
	}
	return s, nil
}

// Save writes the settings so that a save cut off half way leaves the previous
// ones intact.
//
// The file is written beside its own name and renamed over it, because a rename
// within one directory is a single step: there is no moment at which the file
// holds half of the new settings and half of the old.
func Save(path string, s Settings) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("settings: writing out the settings: %w", err)
	}
	tmp := path + ".tmp"
	// The key is in here, so the file is the owner's alone from the moment it
	// exists rather than from the moment it is renamed.
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("settings: writing %s: %w", filepath.Base(tmp), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("settings: putting %s in place: %w", filepath.Base(path), err)
	}
	return nil
}

// Redacted is what may be shown or logged: the same settings with the key
// reduced to a tail that names it and cannot be used as it.
func (s Settings) Redacted() Settings {
	if s.APIKey != "" {
		tail := s.APIKey
		if len(tail) > tailLen {
			tail = tail[len(tail)-tailLen:]
		}
		s.APIKey = "…" + tail
	}
	return s
}
