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
	// SearchPorts is how many identities are held for a search answered inside
	// a request. It is the one pool this machine keeps standing: a job puts up
	// its own and gives it back as it ends, so between jobs there would
	// otherwise be nothing for that address to answer on. Nought turns the
	// address off, and it says so rather than waiting.
	//
	// The fields that used to stand here — how many ports and threads a job runs
	// on — belong to the job now. A machine-wide answer to that question would be
	// a second opinion about a number the job already carries.
	SearchPorts int           `json:"search_ports"`
	Cooldown    time.Duration `json:"cooldown"`
	Proxy       ProxySource   `json:"proxy"`
	Language    string        `json:"language"`
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
	return Settings{
		ControlURL:  DefaultControlURL,
		SearchPorts: 2,
		Cooldown:    2 * time.Second,
	}
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
