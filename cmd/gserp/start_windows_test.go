// SPDX-License-Identifier: MIT

//go:build windows

package main

import (
	"errors"
	"testing"
)

func TestTold_PutsAFailureOnTheScreenBecauseTheConsoleIsGone(t *testing.T) {
	// A run started by double-click has had its console hidden, so the line this
	// program writes to standard error goes where nobody can look and is taken
	// away when the process exits. What the reader sees is an icon that never
	// appears — and the two things that most often stop it, a folder that cannot
	// be written to and a port already taken, both look exactly like that.
	var said []string
	saying = func(reason string) { said = append(said, reason) }
	t.Cleanup(func() { saying = sayWhy })

	boom := errors.New("open gserp.db: access is denied")
	if err := told(boom); !errors.Is(err, boom) {
		t.Errorf("told returned %v, want the error it was given", err)
	}
	if len(said) != 1 || said[0] != boom.Error() {
		t.Errorf("what reached the screen was %v, want the reason", said)
	}
}

func TestTold_SaysNothingAboutAStopOrASuccess(t *testing.T) {
	// Being stopped is not a failure, and a box that appeared every time
	// somebody closed the program from its own menu would be a box they learn to
	// dismiss without reading — including the once it matters.
	var said []string
	saying = func(reason string) { said = append(said, reason) }
	t.Cleanup(func() { saying = sayWhy })

	if err := told(nil); err != nil {
		t.Errorf("told(nil) returned %v", err)
	}
	if err := told(errStopped); !errors.Is(err, errStopped) {
		t.Errorf("told(errStopped) returned %v, want it back", err)
	}
	if len(said) != 0 {
		t.Errorf("%v reached the screen, want nothing", said)
	}
}
