// SPDX-License-Identifier: MIT

package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpen_CreatesAUsableDatabaseWhereThereWasNone(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "gserp.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatalf("the schema was not applied: %v", err)
	}
	if n != 0 {
		t.Errorf("a fresh database holds %d jobs, want none", n)
	}
}

func TestOpen_LeavesAnExistingDatabaseAlone(t *testing.T) {
	// Applying the schema a second time must not wipe what is there. A user
	// who opens their history twice has to still have it.
	path := filepath.Join(t.TempDir(), "gserp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO jobs(name, created_at, pages) VALUES('one', '2026-08-16T10:00:00Z', 1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = again.Close() }()

	var n int
	if err := again.db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Errorf("%d jobs survived the reopen, want 1", n)
	}
}

func TestOpen_DoesNotRebuildADatabaseAlreadyAtThisVersion(t *testing.T) {
	// A database at the version this build writes is opened, not rebuilt.
	// Dropping an index and finding it still gone after a reopen shows that:
	// a schema that ran a second time would have put it back.
	path := filepath.Join(t.TempDir(), "gserp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec(`DROP INDEX results_by_host`); err != nil {
		t.Fatalf("dropping the index: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = again.Close() }()

	var n int
	if err := again.db.QueryRow(
		`SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name = 'results_by_host'`,
	).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Error("the schema ran again over a database that was already current")
	}
}

func TestOpen_KeepsTheHistoryOnASingleConnection(t *testing.T) {
	// SQLite takes one writer. A pool that hands out several turns concurrent
	// writes into a lock error every caller then has to reason about, so the
	// store queues them on one connection instead. The busy timeout hides the
	// difference from a test that merely writes from several goroutines, which
	// is why the pool size itself is what is checked.
	s, err := Open(filepath.Join(t.TempDir(), "gserp.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("the pool may open %d connections, want 1", got)
	}
}

func TestOpen_RefusesADatabaseFromANewerVersion(t *testing.T) {
	// Opening a database this build does not understand and writing to it
	// anyway is how history gets corrupted. Refusing names the reason.
	path := filepath.Join(t.TempDir(), "gserp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatalf("bumping the version: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := Open(path); !errors.Is(err, ErrTooNew) {
		t.Errorf("Open returned %v, want ErrTooNew", err)
	}
}

func TestOpen_KeepsTheJournalWriteAhead(t *testing.T) {
	// A run is written down as it goes so that losing the process does not
	// lose the run, and a write-ahead log is what leaves the rows committed
	// before the loss still readable afterwards.
	s, err := Open(filepath.Join(t.TempDir(), "gserp.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("the journal is %q, want a write-ahead log", mode)
	}
}

func TestOpen_TurnsOnForeignKeys(t *testing.T) {
	// The schema leans on cascading deletes to keep a removed job from
	// leaving its pages and results behind. SQLite ignores them unless asked.
	s, err := Open(filepath.Join(t.TempDir(), "gserp.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var on int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatalf("query: %v", err)
	}
	if on != 1 {
		t.Error("foreign keys are off, so a deleted job would leave its results behind")
	}
}
