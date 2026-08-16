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
var schema string

// schemaVersion is what this build writes and understands.
const schemaVersion = 1

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

// migrate applies the schema to a database that has not seen it.
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
	if have == schemaVersion {
		return nil
	}
	// PRAGMA user_version takes no bound parameter, so the version is formatted
	// in. It is a package constant, never anything a caller supplies.
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("store: applying the schema: %w", err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("store: recording the schema version: %w", err)
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
