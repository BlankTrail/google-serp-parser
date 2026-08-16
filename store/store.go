// SPDX-License-Identifier: MIT

// Package store keeps jobs and what they captured, so a run that is
// interrupted is not a run that is lost.
//
// It knows nothing about searching: it is handed queries and results and it
// writes them down.
package store

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"

	_ "modernc.org/sqlite" // registers the "sqlite" driver, which needs no C toolchain
)

//go:embed schema.sql
var schemaStep1 string

//go:embed schema_v2.sql
var schemaStep2 string

// steps is the upgrade path, one step per version: steps[i] takes a database at
// version i to version i+1. A database that has never been written is version 0
// and walks the whole list.
//
// The mechanism is deliberate, and the next person to change the schema should
// read this before anything else. There is no second file describing the shape
// a new database should have. A new database is built by the same steps an old
// one is upgraded by, which means every test in this package runs the upgrade
// path: a step that only works on an empty database is caught the day it is
// written rather than the day a user upgrades. It also removes the failure the
// obvious alternative invites, where a current-shape file and a set of patches
// drift because someone edited one of them.
//
// To add version 3: write the file, embed it, append it here. Nothing else.
var steps = []string{schemaStep1, schemaStep2}

// schemaVersion is what this build writes and understands. It counts the steps,
// so a step cannot be added without the version following it.
var schemaVersion = len(steps)

// ErrTooNew is returned when the database was written by a later build.
var ErrTooNew = errors.New("store: the database was written by a newer version")

// Store is an open history.
type Store struct {
	db *sql.DB
}

// Open opens the history at path, creating it if it is not there.
//
// The connection pool is held to one. SQLite takes a single writer, and a pool
// that hands out several turns concurrent writes into a lock error the caller
// then has to reason about; one connection makes the queue explicit instead. At
// this size the reads it also serialises cost nothing.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// migrate walks a database up from whatever version it is at to this one.
//
// The version is kept in SQLite's own user_version rather than in a table of
// this package's own, because a table would itself need creating before it
// could be read, and that first step is exactly the one that has to work on a
// file that may hold anything.
func (s *Store) migrate() error {
	var have int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&have); err != nil {
		return fmt.Errorf("store: reading the schema version: %w", err)
	}
	if have > schemaVersion {
		return fmt.Errorf("%w: found version %d, this build writes %d", ErrTooNew, have, schemaVersion)
	}
	for v := have; v < schemaVersion; v++ {
		if err := s.step(v); err != nil {
			return err
		}
	}
	return nil
}

// step applies one upgrade and records the version it produced, both inside a
// single transaction.
//
// The transaction is the point: an upgrade that adds three columns and then
// cannot create a table must leave the database at the version it arrived with,
// because a database carrying half a step and the number of the whole one is a
// database no version describes, and the next open would skip the rest of the
// step forever.
func (s *Store) step(from int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin schema step %d: %w", from+1, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(steps[from]); err != nil {
		return fmt.Errorf("store: applying schema step %d: %w", from+1, err)
	}
	// PRAGMA user_version takes no bound parameter, so the version is formatted
	// in. It is this package's own count of its steps, never anything a caller
	// supplies.
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, from+1)); err != nil {
		return fmt.Errorf("store: recording schema version %d: %w", from+1, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing schema step %d: %w", from+1, err)
	}
	return nil
}

// Close closes the history.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}
