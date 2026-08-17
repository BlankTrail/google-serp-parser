//go:build live

// SPDX-License-Identifier: MIT

package main

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestJobHealth_LiveReadsWhatTheHistorySaysAboutTheJobRunningNow reports what a
// job is actually doing, out of the history it is writing as it goes.
//
// It is a reader and changes nothing. What it answers is the question a screen
// of slow numbers cannot: whether the queries are failing or merely slow, what
// they are failing with, and how much of the last hour each answer took — which
// is the difference between a poor list of addresses and a program that is not
// asking fast enough.
func TestJobHealth_LiveReadsWhatTheHistorySaysAboutTheJobRunningNow(t *testing.T) {
	path := os.Getenv(envLiveDB)
	if path == "" {
		t.Skipf("%s is not set", envLiveDB)
	}
	// Read-only, and out of the way of the program that is writing it.
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening the history: %v", err)
	}
	defer func() { _ = db.Close() }()

	var id int64
	var name string
	err = db.QueryRow(`SELECT id, name FROM jobs WHERE finished_at IS NULL
	                    ORDER BY id DESC LIMIT 1`).Scan(&id, &name)
	if err != nil {
		t.Fatalf("finding the job that is running: %v", err)
	}
	t.Logf("job %d %q", id, name)

	var total, done, failed, pending int
	if err := db.QueryRow(`
		SELECT count(*),
		       sum(state = 'done'), sum(state = 'failed'),
		       sum(state NOT IN ('done','failed'))
		  FROM queries WHERE job_id = ?`, id).Scan(&total, &done, &failed, &pending); err != nil {
		t.Fatalf("counting the queries: %v", err)
	}
	t.Logf("queries: %d total, %d done, %d failed, %d left", total, done, failed, pending)

	// What settled in the last hour, and how it settled. A run whose failures
	// climb while its answers do not is being refused; one where neither moves is
	// not asking.
	since := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	var lastHourDone, lastHourFailed int
	_ = db.QueryRow(`SELECT sum(state = 'done'), sum(state = 'failed') FROM queries
	                  WHERE job_id = ? AND settled_at > ?`, id, since).
		Scan(&lastHourDone, &lastHourFailed)
	t.Logf("in the last hour: %d answered, %d given up on", lastHourDone, lastHourFailed)

	// The pages behind those answers, because a job taken deep makes many
	// requests per query and the queries alone hide that.
	var pages int
	_ = db.QueryRow(`SELECT count(*) FROM pages p JOIN queries q ON q.id = p.query_id
	                  WHERE q.job_id = ? AND q.settled_at > ?`, id, since).Scan(&pages)
	t.Logf("in the last hour: %d result pages captured", pages)

	// How the failures read, grouped, because a hundred of one thing and one of a
	// hundred things are opposite problems.
	rows, err := db.Query(`SELECT err FROM queries
	                        WHERE job_id = ? AND err <> '' ORDER BY id DESC LIMIT 500`, id)
	if err != nil {
		t.Fatalf("reading the failures: %v", err)
	}
	defer func() { _ = rows.Close() }()
	kinds := map[string]int{}
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatalf("reading a failure: %v", err)
		}
		kinds[shorten(text)]++
	}
	if len(kinds) == 0 {
		t.Log("no query has been written down as failed")
	}
	type pair struct {
		what string
		n    int
	}
	var sorted []pair
	for what, n := range kinds {
		sorted = append(sorted, pair{what, n})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].n > sorted[j].n })
	for _, p := range sorted {
		t.Logf("  %4d × %s", p.n, p.what)
	}

	// And the shape of the last hour minute by minute, so a run that stopped
	// producing at some moment says when.
	byMinute, err := db.Query(`SELECT substr(settled_at, 1, 16) AS minute, count(*)
	                             FROM queries
	                            WHERE job_id = ? AND settled_at > ?
	                            GROUP BY minute ORDER BY minute DESC LIMIT 20`, id, since)
	if err != nil {
		t.Fatalf("reading the last hour: %v", err)
	}
	defer func() { _ = byMinute.Close() }()
	for byMinute.Next() {
		var minute string
		var n int
		if err := byMinute.Scan(&minute, &n); err != nil {
			t.Fatalf("reading a minute: %v", err)
		}
		t.Logf("  %s — %d settled", minute, n)
	}
}

// shorten turns one failure into the kind of failure it is, so a hundred of them
// group instead of reading as a hundred different faults. The parts that differ
// between two of the same kind — the address, the port, the phrase — are what it
// cuts away.
func shorten(text string) string {
	if i := strings.Index(text, " (reporting"); i > 0 {
		text = text[:i]
	}
	for _, cut := range []string{"Get \"", "read tcp ", "dial tcp ", "127.0.0.1:"} {
		if i := strings.Index(text, cut); i > 0 {
			text = text[:i] + cut + "…"
			break
		}
	}
	if len(text) > 140 {
		text = text[:140] + "…"
	}
	return fmt.Sprintf("%s", text)
}
