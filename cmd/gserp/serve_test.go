// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// listeningAt reads back the address the command says to open.
func listeningAt(t *testing.T, printed string) string {
	t.Helper()
	_, rest, ok := strings.Cut(printed, "http://")
	if !ok {
		t.Fatalf("nothing printed says where to open a browser:\n%s", printed)
	}
	return strings.Fields(rest)[0]
}

func TestServeCommand_SaysWhichPortItWasGivenAndStopsWhenTold(t *testing.T) {
	// Port zero is the only way to ask for a free one, and whoever asked cannot
	// reach the server unless the command says which one it got. Stopping is the
	// other half: a server that outlives its context holds the port and the
	// history, and the process never exits.
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- serveCommand(ctx, []string{
			"-addr", "127.0.0.1:0", "-db", filepath.Join(t.TempDir(), "h.db"),
		}, out)
	}()
	waitFor(t, "the command to say where to open a browser", func() bool {
		return strings.Contains(out.String(), "http://")
	})

	res, err := http.Get("http://" + listeningAt(t, out.String()) + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("the job list came back %d, want 200", res.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveCommand returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command did not return within ten seconds of its context being cancelled")
	}
}

func TestServeLog_DoesNotSayWhereOnThisMachineTheHistoryIsKept(t *testing.T) {
	// A log line is printed once and read for years. Which file could not be
	// read is the answer; the directories above it say where somebody keeps
	// their things, and an error quotes the path in whatever shape it likes.
	//
	// The directory is looked for by name rather than as a whole path: every
	// layer between the error and the line — the quoting in the error, the
	// quoting in the handler — doubles the separators again, so a whole path is
	// a substring of nothing while every name in it is still there to read.
	const marker = "where-nobody-should-look"
	db := filepath.Join(t.TempDir(), marker, "h.db")

	var out bytes.Buffer
	serveOptions{DB: db}.logger(&out).Error("a page could not be built",
		"error", fmt.Errorf("store: open %q: locked", db))

	got := out.String()
	if strings.Contains(got, marker) {
		t.Errorf("the log says which directory the history is kept in:\n%s", got)
	}
	if !strings.Contains(got, "h.db") {
		t.Errorf("the log no longer says which file it could not read:\n%s", got)
	}
}

func TestServeLog_SaysWhenEachLineWasWritten(t *testing.T) {
	// A server's log is read long after the fact and beside other logs. A line
	// with no time on it cannot be placed against anything that happened.
	var out bytes.Buffer
	serveOptions{}.logger(&out).Error("a page could not be built")

	if !strings.Contains(out.String(), "time=") {
		t.Errorf("the line carries no time:\n%s", out.String())
	}
}

func TestUsage_ListsEveryFlagTheServeCommandTakes(t *testing.T) {
	// Help that has drifted from the flags is the same defect as documentation
	// that is wrong: it is read instead of the code, and it is believed.
	var opts serveOptions
	fs := serveFlags(&opts)

	if !strings.Contains(usageText, "gserp serve") {
		t.Error("the help does not list the serve command")
	}
	fs.VisitAll(func(f *flag.Flag) {
		if !strings.Contains(usageText, "--"+f.Name) {
			t.Errorf("the help does not mention --%s", f.Name)
		}
	})
}
