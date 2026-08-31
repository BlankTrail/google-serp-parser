//go:build live

// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/export"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// Run this with -count=1. Nothing a live run depends on is an input Go can see,
// so a second run of an unchanged binary against unchanged environment
// variables is served from the test cache: it reprints the first run's numbers,
// passes, and measures nothing.

// liveEnv skips unless everything the command needs is in the environment.
//
// Nothing here has a fallback. A live test that falls back to a well-known
// address measures whatever happens to be listening there and reports it as
// this test's result.
func liveEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{envControlURL, envAPIKey, envProxyList} {
		if os.Getenv(name) == "" {
			t.Skipf("%s is not set", name)
		}
	}
}

// learnt is what only a running test knows it must not print. The run says
// where it wrote its export, and that is a path on this machine.
var learnt []string

// keptOut is everything this file must not print. It is the command's own list,
// plus the two addresses it was pointed at — a failure quotes both ends of the
// connection it failed on, and one of those addresses carries a key inside it —
// and whatever the test has learnt since.
func keptOut() []string {
	out := append(secrets(), os.Getenv(envControlURL), "127.0.0.1", "localhost", "[::1]")
	return append(out, learnt...)
}

// hide puts a line through the same filter the command puts its own output
// through, so nothing this test prints is trusted to be clean on its own.
func hide(s string) string { return hideSecrets(s, keptOut()...) }

func logf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Log(hide(fmt.Sprintf(format, args...)))
}

func errorf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Error(hide(fmt.Sprintf(format, args...)))
}

func fatalf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Fatal(hide(fmt.Sprintf(format, args...)))
}

// replay prints what a run said, a line at a time. The filter joins on single
// spaces, so a whole buffer handed to it at once comes back as one line.
func replay(t *testing.T, label, out string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		logf(t, "%s | %s", label, line)
	}
}

// The job: twelve queries, each taken two pages deep. Twelve is enough to be
// cut in half and leave both halves worth counting, and every query is
// different so a place in the list is also a name.
var liveQueries = []string{
	"golang channels",
	"golang generics",
	"golang modules",
	"golang context",
	"golang testing",
	"golang interfaces",
	"golang goroutines",
	"golang slices",
	"golang errors",
	"golang embed",
	"golang json",
	"golang http server",
}

const (
	livePages          = 2
	liveThreads        = 4
	livePortsPerThread = 2
	liveJobName        = "live-resume"
)

// interruption is where the first run was cut off.
type interruption struct {
	at      time.Duration
	settled int
	cut     bool
	err     error
}

// cutWhenHalfIsSettled cancels a run once half its queries have been written
// down.
//
// Half is watched for in the history rather than waited out on a clock. A run
// this long is paced by what answers it, not by anything this test knows, so a
// wall-clock guess cuts a quick run after it has finished and a slow one before
// it has started — and neither is the interruption worth measuring, which is
// one taken in the middle of the list with queries still in the air.
func cutWhenHalfIsSettled(ctx context.Context, st *store.Store, total, half int,
	cancel context.CancelFunc, stop <-chan struct{}) <-chan interruption {
	out := make(chan interruption, 1)
	started := time.Now()
	go func() {
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				out <- interruption{at: time.Since(started)}
				return
			case <-ctx.Done():
				out <- interruption{at: time.Since(started), err: ctx.Err()}
				return
			case <-tick.C:
			}
			job, err := st.LastUnfinished(ctx, liveJobName)
			if errors.Is(err, store.ErrNoUnfinishedJob) {
				// The plan is written before the first request, so this is only
				// the stretch before the run has said what it means to do.
				continue
			}
			if err != nil {
				out <- interruption{at: time.Since(started), err: err}
				return
			}
			left, err := st.Pending(ctx, job.ID)
			if err != nil {
				out <- interruption{at: time.Since(started), err: err}
				return
			}
			if settled := total - len(left); settled >= half {
				cancel()
				out <- interruption{at: time.Since(started), settled: settled, cut: true}
				return
			}
		}
	}()
	return out
}

// rowsByOrdinal counts what the history holds for each query of a job.
func rowsByOrdinal(ctx context.Context, t *testing.T, st *store.Store, jobID int64) (map[int]int, int) {
	t.Helper()
	counts := map[int]int{}
	total := 0
	if err := st.Rows(ctx, jobID, func(r store.Row) error {
		counts[r.Ordinal]++
		total++
		return nil
	}); err != nil {
		fatalf(t, "reading the job's rows: %v", err)
	}
	return counts, total
}

// pendingSet turns what is left of a job into a set of places in the list.
func pendingSet(left []store.PendingQuery) map[int]bool {
	set := map[int]bool{}
	for _, q := range left {
		set[q.Ordinal] = true
	}
	return set
}

// sorted places, for a line somebody has to read.
func places(set map[int]bool, total int) []int {
	var out []int
	for i := 0; i < total; i++ {
		if set[i] {
			out = append(out, i)
		}
	}
	return out
}

func TestLiveRun_TakesUpAnInterruptedJobExactlyWhereItStopped(t *testing.T) {
	liveEnv(t)

	ctx, cancelAll := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancelAll()

	dir := t.TempDir()
	// The run prints the file it wrote, and that name carries the directory it
	// is in, which is a path on this machine.
	learnt = append(learnt, dir)
	db := filepath.Join(dir, "history.db")
	list := listOf(t, dir, strings.Join(liveQueries, "\n")+"\n")

	opts := runOptions{
		Queries:  list,
		DB:       db,
		Name:     liveJobName,
		Format:   "csv",
		Pages:    livePages,
		Threads:  liveThreads,
		Ports:    livePortsPerThread,
		Country:  "us",
		Language: "en",
	}

	// The watcher's own handle is opened first, so the schema is already in
	// place when the run opens the file and the two never race to create it.
	watcher, err := store.Open(db)
	if err != nil {
		fatalf(t, "opening the history to watch it: %v", err)
	}
	defer func() { _ = watcher.Close() }()

	cutCtx, cut := context.WithCancel(ctx)
	defer cut()
	stop := make(chan struct{})
	cuts := cutWhenHalfIsSettled(ctx, watcher, len(liveQueries), len(liveQueries)/2, cut, stop)

	var first bytes.Buffer
	started := time.Now()
	firstErr := runJob(cutCtx, &first, opts)
	firstTook := time.Since(started)
	close(stop)
	where := <-cuts

	replay(t, "first run", first.String())
	logf(t, "MEASUREMENT first run: %v elapsed, cut after %v with %d of %d settled (cut=%v, watch error %v), returned: %v",
		firstTook.Round(time.Second), where.at.Round(time.Second), where.settled,
		len(liveQueries), where.cut, where.err, firstErr)

	job, err := watcher.LastUnfinished(ctx, liveJobName)
	if err != nil {
		fatalf(t, "the interrupted job cannot be found to take up: %v", err)
	}
	leftBefore, err := watcher.Pending(ctx, job.ID)
	if err != nil {
		fatalf(t, "reading what the interrupted job has left: %v", err)
	}
	pendingBefore := pendingSet(leftBefore)
	rowsBefore, totalBefore := rowsByOrdinal(ctx, t, watcher, job.ID)

	settledBefore := map[int]bool{}
	for i := range liveQueries {
		if !pendingBefore[i] {
			settledBefore[i] = true
		}
	}
	logf(t, "MEASUREMENT after the interruption: %d of %d queries settled %v, %d left %v, %d rows recorded",
		len(settledBefore), len(liveQueries), places(settledBefore, len(liveQueries)),
		len(leftBefore), places(pendingBefore, len(liveQueries)), totalBefore)
	for i := range liveQueries {
		logf(t, "MEASUREMENT query %d: %d rows after the interruption, %s",
			i, rowsBefore[i], map[bool]string{true: "still to do", false: "settled"}[pendingBefore[i]])
	}

	if len(leftBefore) == 0 {
		fatalf(t, "the interruption left nothing to take up, so there is no resume to measure: %d of %d settled",
			len(settledBefore), len(liveQueries))
	}

	resume := opts
	resume.Resume = true
	resume.Queries = ""
	resume.Out = filepath.Join(dir, "results.csv")

	var second bytes.Buffer
	started = time.Now()
	resumeErr := runJob(ctx, &second, resume)
	resumeTook := time.Since(started)
	replay(t, "resume", second.String())
	logf(t, "MEASUREMENT resume: %v elapsed, returned: %v", resumeTook.Round(time.Second), resumeErr)
	if resumeErr != nil {
		errorf(t, "the resume did not finish: %v", resumeErr)
	}

	// What the resume set out to do. The estimate is printed before the first
	// request, so this is the plan it was working from rather than a count of
	// what happened to succeed.
	planned := fmt.Sprintf("%s: %d queries", liveJobName, len(leftBefore))
	if !strings.Contains(second.String(), planned) {
		errorf(t, "the resume did not take up exactly the %d queries that were left; it does not say %q",
			len(leftBefore), planned)
	}

	leftAfter, err := watcher.Pending(ctx, job.ID)
	if err != nil {
		fatalf(t, "reading what is left after the resume: %v", err)
	}
	rowsAfter, totalAfter := rowsByOrdinal(ctx, t, watcher, job.ID)
	logf(t, "MEASUREMENT after the resume: %d left %v, %d rows recorded (%d before the resume, %d added)",
		len(leftAfter), places(pendingSet(leftAfter), len(liveQueries)),
		totalAfter, totalBefore, totalAfter-totalBefore)
	for i := range liveQueries {
		logf(t, "MEASUREMENT query %d: %d rows before the resume, %d after", i, rowsBefore[i], rowsAfter[i])
	}

	// Nothing already recorded may be run again. A query taken twice writes its
	// pages twice, and a ranking counted twice is worse than one missing: it
	// reads as data.
	for i := range liveQueries {
		if !settledBefore[i] {
			continue
		}
		if rowsAfter[i] != rowsBefore[i] {
			errorf(t, "query %d was settled before the resume with %d rows and holds %d after it",
				i, rowsBefore[i], rowsAfter[i])
		}
	}
	// And nothing may be left behind. A resume that stops short leaves a job
	// nobody can tell from one that was finished.
	if len(leftAfter) != 0 {
		errorf(t, "the resume left %d queries unfinished: %v",
			len(leftAfter), places(pendingSet(leftAfter), len(liveQueries)))
	}

	if resumeErr != nil {
		t.Skip("the resume did not finish, so there is no export to read back")
	}

	// The export the command itself wrote, counted by the command itself. The
	// command names the file and not the directory it is in, which is why this
	// is the base name: where on the machine it sits is not printed.
	written := fmt.Sprintf("%d rows written to %s", totalAfter, filepath.Base(resume.Out))
	if !strings.Contains(second.String(), written) {
		errorf(t, "the history holds %d rows and the export does not say it wrote that many", totalAfter)
	}

	records := readBackCSV(t, resume.Out)
	logf(t, "MEASUREMENT export: history %d rows, csv %d records after the header", totalAfter, records)
	if records != totalAfter {
		errorf(t, "the history holds %d rows and the csv holds %d records", totalAfter, records)
	}

	// The second format, written from the same history through the same walk,
	// so a count that differs between the two is the writer's doing and not the
	// job's.
	jsonlPath := filepath.Join(dir, "results.jsonl")
	reported, err := exportJob(ctx, watcher, job.ID, jsonlPath, "jsonl")
	if err != nil {
		fatalf(t, "writing the second export: %v", err)
	}
	lines := readBackJSONL(t, jsonlPath)
	logf(t, "MEASUREMENT export: history %d rows, jsonl reported %d, read back %d", totalAfter, reported, lines)
	if reported != totalAfter || lines != totalAfter {
		errorf(t, "the history holds %d rows, the jsonl export reported %d and reads back as %d",
			totalAfter, reported, lines)
	}

	// The history has to be shut before it is weighed: what a run wrote is not
	// all in the file itself until the last reader lets go of it.
	if err := watcher.Close(); err != nil {
		errorf(t, "closing the history: %v", err)
	}
	logf(t, "MEASUREMENT history on disk: %s for %d rows over %d queries (%d bytes per row)",
		onDisk(t, db), totalAfter, len(liveQueries), sizeOf(t, db)/int64(max(totalAfter, 1)))
	logf(t, "MEASUREMENT exports on disk: csv %d bytes, jsonl %d bytes for %d rows",
		sizeOf(t, resume.Out), sizeOf(t, jsonlPath), totalAfter)
}

// readBackCSV reads the export the way a spreadsheet would and returns how many
// rows it holds, header aside. It also checks that no place in the list carries
// the same rank twice, which is what a query recorded a second time would look
// like from the outside.
func readBackCSV(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		fatalf(t, "opening the csv: %v", err)
	}
	defer func() { _ = f.Close() }()

	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		fatalf(t, "reading the csv header: %v", err)
	}
	logf(t, "MEASUREMENT csv header: %v", header)

	seen := map[string]bool{}
	count := 0
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fatalf(t, "reading the csv: %v", err)
		}
		if len(rec) != len(header) {
			errorf(t, "record %d has %d columns, the header has %d", count, len(rec), len(header))
		}
		count++
		// Columns nought and three are the place in the list and the rank.
		key := rec[0] + "/" + rec[3]
		if seen[key] {
			errorf(t, "query %s carries rank %s twice", rec[0], rec[3])
		}
		seen[key] = true
	}
	return count
}

// readBackJSONL decodes every line of the second export and returns how many
// there were.
func readBackJSONL(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		fatalf(t, "opening the jsonl: %v", err)
	}
	defer func() { _ = f.Close() }()

	dec := json.NewDecoder(f)
	count := 0
	for {
		var row export.Row
		err := dec.Decode(&row)
		if errors.Is(err, io.EOF) {
			return count
		}
		if err != nil {
			fatalf(t, "reading line %d of the jsonl: %v", count+1, err)
		}
		count++
	}
}

// onDisk says what the history costs, counting the files it keeps beside
// itself.
func onDisk(t *testing.T, db string) string {
	t.Helper()
	var parts []string
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(db + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errorf(t, "weighing the history: %v", err)
			continue
		}
		total += info.Size()
		parts = append(parts, fmt.Sprintf("%s%s=%d bytes", filepath.Base(db), suffix, info.Size()))
	}
	return fmt.Sprintf("%d bytes total (%s)", total, strings.Join(parts, ", "))
}

// sizeOf is one file's size, or nought if it cannot be weighed.
func sizeOf(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		errorf(t, "weighing %s: %v", filepath.Base(path), err)
		return 0
	}
	return info.Size()
}
