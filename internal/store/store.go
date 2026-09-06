// SPDX-License-Identifier: MIT

// Package store keeps jobs and what they captured, so a run that is
// interrupted is not a run that is lost.
//
// It knows nothing about searching: it is handed queries and results and it
// writes them down.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver, which needs no C toolchain
)

//go:embed schema.sql
var schemaStep1 string

//go:embed schema_v2.sql
var schemaStep2 string

//go:embed schema_v3.sql
var schemaStep3 string

//go:embed schema_v4.sql
var schemaStep4 string

//go:embed schema_v5.sql
var schemaStep5 string

//go:embed schema_v6.sql
var schemaStep6 string

//go:embed schema_v7.sql
var schemaStep7 string

//go:embed schema_v8.sql
var schemaStep8 string

//go:embed schema_v9.sql
var schemaStep9 string

//go:embed schema_v10.sql
var schemaStep10 string

//go:embed schema_v11.sql
var schemaStep11 string

//go:embed schema_v12.sql
var schemaStep12 string

//go:embed schema_v13.sql
var schemaStep13 string

//go:embed schema_v14.sql
var schemaStep14 string

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
// To add version 15: write the file, embed it, append it here. Nothing else —
// with one thing worth knowing before writing it, which step five is the first
// to have needed. A step runs with foreign keys held off, because a step that
// builds a table again has to drop the old one, and dropping a table other
// tables point at deletes every row that pointed at it. See upgrade.
var steps = []string{schemaStep1, schemaStep2, schemaStep3, schemaStep4, schemaStep5,
	schemaStep6, schemaStep7, schemaStep8, schemaStep9, schemaStep10, schemaStep11,
	schemaStep12, schemaStep13, schemaStep14}

// schemaVersion is what this build writes and understands. It counts the steps,
// so a step cannot be added without the version following it.
var schemaVersion = len(steps)

// ErrTooNew is returned when the database was written by a later build.
var ErrTooNew = errors.New("store: the database was written by a newer version")

// Store is an open history.
type Store struct {
	db *sql.DB
	// now is the clock the history reads when it has to judge how long ago
	// something happened. It is a field so a test can hold time still; nothing
	// but a test ever sets it.
	now func() time.Time
}

// clock is the time this history reads, which is the real one unless a test has
// said otherwise.
func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
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
//
// The whole upgrade runs on one connection taken for it. The steps turn a
// setting of the connection off and back on, and a setting is only ever a
// setting of the connection it was made on: run over the pool, a step could be
// handed a different connection from the one that was prepared, and the
// difference between the two is whether dropping a table takes the history with
// it.
func (s *Store) migrate() error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: taking the connection to upgrade on: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var have int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&have); err != nil {
		return fmt.Errorf("store: reading the schema version: %w", err)
	}
	if have > schemaVersion {
		return fmt.Errorf("%w: found version %d, this build writes %d", ErrTooNew, have, schemaVersion)
	}
	if have == schemaVersion {
		// Every open of an up-to-date database goes this way, which is nearly
		// every open there is. It touches no setting and reads nothing further.
		return nil
	}
	return s.upgrade(ctx, conn, have)
}

// upgrade runs the steps a database is short of, with foreign keys held off for
// the length of them.
//
// They are held off because a step may have to build a table again — SQLite can
// widen a column but not a constraint — and that means dropping the old one.
// With foreign keys on, dropping a table runs an implicit delete of its rows,
// and every ON DELETE CASCADE pointing at it fires: an upgrade of the jobs table
// would take every query, page and result in the database with it. Off, the drop
// is a drop, and the tables pointing at the old name find the new one under it.
//
// What that costs is that nothing is enforced while the steps run, so the
// database is asked afterwards whether anything came out of it pointing at
// nothing. That check reads every row that has a parent, which is the whole
// history — paid once, on the open that upgrades, and not on any other.
func (s *Store) upgrade(ctx context.Context, conn *sql.Conn, have int) (err error) {
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("store: holding the foreign keys off to upgrade: %w", err)
	}
	defer func() {
		// The connection goes back to the pool after this, and a connection that
		// kept them off would be handed to the program to work on.
		if _, onErr := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); onErr != nil && err == nil {
			err = fmt.Errorf("store: putting the foreign keys back after the upgrade: %w", onErr)
		}
	}()

	for v := have; v < schemaVersion; v++ {
		if err := s.step(ctx, conn, v); err != nil {
			return err
		}
	}
	return orphaned(ctx, conn)
}

// orphaned reports rows left pointing at a parent that is not there.
//
// It is asked after an upgrade because the upgrade ran with nothing enforcing
// it. A database that comes out of a step with its results hanging off a jobs
// table that no longer exists opens perfectly well and answers every question
// wrongly, and the day that is noticed is a long way from the day it happened.
func orphaned(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("store: checking what the upgrade left behind: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var table string
	var loose int
	for rows.Next() {
		// The columns are the child table, the row, the parent table and which
		// foreign key it was. Only the first is worth reporting: it names where to
		// look, and a list of every row would be as long as the damage.
		var rowid, parent, key any
		if err := rows.Scan(&table, &rowid, &parent, &key); err != nil {
			return fmt.Errorf("store: reading what the upgrade left behind: %w", err)
		}
		loose++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: checking what the upgrade left behind: %w", err)
	}
	if loose > 0 {
		return fmt.Errorf("store: the upgrade left %d rows in %q pointing at a job that is not there", loose, table)
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
func (s *Store) step(ctx context.Context, conn *sql.Conn, from int) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin schema step %d: %w", from+1, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, steps[from]); err != nil {
		return fmt.Errorf("store: applying schema step %d: %w", from+1, err)
	}
	// PRAGMA user_version takes no bound parameter, so the version is formatted
	// in. It is this package's own count of its steps, never anything a caller
	// supplies.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, from+1)); err != nil {
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
