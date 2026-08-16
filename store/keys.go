// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrBadKey is returned for a secret this store never issued, or issued and has
// since been told to forget.
var ErrBadKey = errors.New("store: no such key")

// secretBytes is how much randomness a key carries. Thirty-two bytes is past the
// point where guessing is a strategy, and the hex form is still short enough to
// paste.
const secretBytes = 32

// prefixLen is how much of a key is kept in the clear so a person can tell
// theirs from somebody else's in a listing. Eight characters name a key and are
// useless as one.
const prefixLen = 8

// APIKey is a key as anyone but its holder can see it.
type APIKey struct {
	ID         int64
	Name       string
	Prefix     string
	CreatedAt  time.Time
	LastUsedAt time.Time
	Revoked    bool
}

// CreateKey issues a key and returns the secret once.
//
// Only a hash of it is written down. The database is a file on somebody's disk;
// it goes into their backups and travels wherever those go, and a key readable
// out of it is a key that leaks with the file. There is deliberately no way to
// read a secret back afterwards: a lost key is replaced, not recovered.
func (s *Store) CreateKey(ctx context.Context, name string) (string, APIKey, error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", APIKey{}, fmt.Errorf("store: drawing a key: %w", err)
	}
	secret := hex.EncodeToString(raw)
	key := APIKey{
		Name:      name,
		Prefix:    secret[:prefixLen],
		CreatedAt: time.Now().UTC(),
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys(name, prefix, hash, created_at) VALUES(?, ?, ?, ?)`,
		key.Name, key.Prefix, hashSecret(secret), key.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return "", APIKey{}, fmt.Errorf("store: recording a key: %w", err)
	}
	if key.ID, err = res.LastInsertId(); err != nil {
		return "", APIKey{}, fmt.Errorf("store: reading the key id: %w", err)
	}
	return secret, key, nil
}

// CheckKey looks a secret up and notes that it was used.
//
// An empty secret is refused before the database is opened at all: a request
// carrying no key is the most ordinary request a public address receives, and it
// deserves no lookup. It is also the one input whose hash a reader could plant a
// row for, which the early return makes moot.
//
// The lookup is by hash, so what the database compares is a value derived from
// what the caller sent. Learning it a byte at a time by watching the clock would
// hand somebody a hash and no way back to the secret behind it.
func (s *Store) CheckKey(ctx context.Context, secret string) (APIKey, error) {
	if secret == "" {
		return APIKey{}, ErrBadKey
	}
	var key APIKey
	var created string
	var lastUsed sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, prefix, created_at, last_used_at FROM api_keys
		  WHERE hash = ? AND revoked = 0`, hashSecret(secret)).
		Scan(&key.ID, &key.Name, &key.Prefix, &created, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, ErrBadKey
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("store: checking a key: %w", err)
	}
	key.CreatedAt, _ = time.Parse(time.RFC3339, created)
	if lastUsed.Valid {
		key.LastUsedAt, _ = time.Parse(time.RFC3339, lastUsed.String)
	}

	// Failing to note the use is not a reason to refuse a key that is good: the
	// caller is entitled to their answer, and the note is for whoever audits
	// later.
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = ? WHERE id = ?`,
		now.Format(time.RFC3339), key.ID); err != nil {
		return key, nil
	}
	key.LastUsedAt = now
	return key, nil
}

// RevokeKey stops one key working, at once.
//
// An id nothing was stored under is an error rather than a silent success:
// somebody revoking a key after a leak would otherwise walk away believing a key
// that still works has been stopped.
func (s *Store) RevokeKey(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE api_keys SET revoked = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: revoking key %d: %w", id, err)
	}
	stopped, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: revoking key %d: %w", id, err)
	}
	if stopped == 0 {
		return fmt.Errorf("%w: %d", ErrBadKey, id)
	}
	return nil
}

// Keys lists what exists, carrying nothing anyone could use.
//
// Revoked keys stay in the answer, because a key that vanished from the listing
// looks like a key somebody deleted rather than one that was stopped.
func (s *Store) Keys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, prefix, created_at, last_used_at, revoked
		   FROM api_keys ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: listing keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []APIKey
	for rows.Next() {
		var k APIKey
		var created string
		var lastUsed sql.NullString
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &created, &lastUsed, &k.Revoked); err != nil {
			return nil, fmt.Errorf("store: reading a key: %w", err)
		}
		// A stamp that cannot be read costs the reader a date, not the listing.
		k.CreatedAt, _ = time.Parse(time.RFC3339, created)
		if lastUsed.Valid {
			k.LastUsedAt, _ = time.Parse(time.RFC3339, lastUsed.String)
		}
		out = append(out, k)
	}
	// A listing cut short by a database that went away must not read as the keys
	// it never reached having been revoked.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing keys: %w", err)
	}
	return out, nil
}

// hashSecret is what the database is allowed to hold. SHA-256 is chosen over a
// password hash on purpose: a key is thirty-two bytes drawn at random, so there
// is no dictionary to slow down, and this runs on every request.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
