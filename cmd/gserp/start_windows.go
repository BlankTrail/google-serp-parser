//go:build windows

// SPDX-License-Identifier: MIT

package main

import "context"

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
	var opts serveOptions
	fs := serveFlags(&opts)
	if err := fs.Parse(nil); err != nil {
		return err
	}
	ctx, stop := interruptible(ctx, discard{})
	defer stop()
	return serveWithTray(ctx, discard{}, opts)
}

// discard is where the interface's own lines go when nobody is looking at a
// console. What went wrong still reaches standard error, which is where the
// log has always been.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
