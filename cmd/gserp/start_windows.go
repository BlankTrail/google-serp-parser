//go:build windows

// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
)

// noArguments is what this program does when it is started with nothing to do.
//
// Two people start it that way and they mean opposite things. Somebody at a
// prompt is asking what it does, and gets the usage. Somebody who
// double-clicked it is asking for the program itself: they get the interface,
// a browser tab open on it, and an icon in the notification area to close it
// with — because a window that prints a page of usage and vanishes is, to them,
// a program that did not start.
func noArguments(ctx context.Context) error {
	if !startedFromExplorer() {
		usage()
		return nil
	}
	hideConsole()
	return told(startFromExplorer(ctx))
}

// startFromExplorer is the whole of a double-click: serve, with an icon in the
// notification area and the browser opened at it.
func startFromExplorer(ctx context.Context) error {
	var opts serveOptions
	fs := serveFlags(&opts)
	if err := fs.Parse(nil); err != nil {
		return err
	}
	ctx, stop := interruptible(ctx, discard{})
	defer stop()
	return serveWithTray(ctx, discard{}, opts)
}

// told puts a failure on the screen and hands it back.
//
// A run started by double-click has had its console hidden, so the line this
// program writes to standard error goes somewhere nobody can look and is taken
// away when the process exits. What the reader sees is an icon that never
// appears — and the two things that most often stop it, a folder that cannot be
// written to and a port already taken, both look exactly like that.
//
// Being stopped on purpose is not a failure and says nothing.
func told(err error) error {
	if err == nil || errors.Is(err, errStopped) {
		return err
	}
	saying(err.Error())
	return err
}

// saying is how a failure reaches the screen, replaced in a test: a box that
// waits to be clicked would hold a test run until somebody noticed it.
var saying = sayWhy

// discard is where the interface's own lines go when nobody is looking at a
// console. What went wrong still reaches standard error, which is where the
// log has always been.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
