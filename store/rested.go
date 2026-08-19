// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"time"
)

// Rested is every address this machine has found dead, and when it found each
// one.
//
// What a rest means — how long it lasts, whether this one has run out — is the
// pool's to decide, so nothing is filtered here. This is the record; the reader
// applies its own rule to it.
func (s *Store) Rested(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, since FROM rested_upstreams`)
	if err != nil {
		return nil, fmt.Errorf("store: reading the addresses that are resting: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]time.Time{}
	for rows.Next() {
		var key, since string
		if err := rows.Scan(&key, &since); err != nil {
			return nil, fmt.Errorf("store: reading a resting address: %w", err)
		}
		at, err := time.Parse(time.RFC3339Nano, since)
		if err != nil {
			// A moment this program cannot read is one it did not write. It is
			// passed over rather than refused: the worst it costs is one address
			// tried again, and refusing would cost the whole record.
			continue
		}
		out[key] = at
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading the addresses that are resting: %w", err)
	}
	return out, nil
}

// Rest writes down that an address was found dead at a given moment.
//
// Writing it again moves the moment, because the second finding is the fresher
// one and a rest is counted from when it began.
func (s *Store) Rest(ctx context.Context, key string, since time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO rested_upstreams(key, since) VALUES(?, ?)
		    ON CONFLICT(key) DO UPDATE SET since = excluded.since`,
		key, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store: writing down a resting address: %w", err)
	}
	return nil
}

// ForgetRestsBefore drops the records of rests that began before the given
// moment.
//
// Without it the table grows for the life of the machine: a list of fifteen
// thousand addresses cycled through for a month leaves a row for every one that
// ever failed, and the rests among them expired long ago. The caller names the
// moment because how long a rest lasts is the pool's business, not this
// table's.
// ForgetAllRests drops every rest written down.
//
// It is what a release of the whole bench has to do as well as clearing it in
// memory: the rests are read back at the next start precisely so a restart does
// not walk into yesterday's dead addresses, and a release that left them there
// would last until the program was closed and no longer.
func (s *Store) ForgetAllRests(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM rested_upstreams`)
	if err != nil {
		return fmt.Errorf("store: forgetting the rests: %w", err)
	}
	return nil
}

func (s *Store) ForgetRestsBefore(ctx context.Context, cut time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM rested_upstreams WHERE since < ?`,
		cut.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store: forgetting rests that have run out: %w", err)
	}
	return nil
}
