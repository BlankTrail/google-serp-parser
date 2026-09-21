// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Session is one of the sessions this program searches through, as the history
// keeps it: the fingerprint it wears, what it was made for, and the cookies the
// search engine has handed it.
type Session struct {
	ID int64
	// Profile is the fingerprint, by the name the proxy service holds it
	// under. It is what is put on a port when the session is taken up there.
	Profile string
	Browser string
	OS      string
	// Device is which kind of result page the session was made for. A phone's
	// cookies are a phone's, and a session is never handed to the other kind.
	Device string
	// Cookies is the session's jar written down, every host it touched. Empty
	// is a session nothing has been asked through yet.
	Cookies []byte

	CreatedAt time.Time
	// UsedAt is the last time the session was asked through, and what the time
	// an unused session is kept for is counted from.
	UsedAt time.Time
	// Failures is the refusals in a row. It goes back to nought on an answer.
	Failures int
}

// SessionFailuresAllowed is how many refusals in a row a session survives.
//
// Two. One refusal is the address under the session as often as it is the
// session: the address drops the connection, the next one does not, and the
// session was never the question. Two in a row, across what is usually two
// different addresses, is the session.
const SessionFailuresAllowed = 2

// ErrNoSession is returned for a session the history does not hold.
var ErrNoSession = errors.New("store: no such session")

// NewSession writes a session down and hands back its id.
func (s *Store) NewSession(ctx context.Context, sess Session) (int64, error) {
	now := time.Now().UTC()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	if sess.UsedAt.IsZero() {
		sess.UsedAt = sess.CreatedAt
	}
	cookies := sess.Cookies
	if len(cookies) == 0 {
		cookies = []byte("[]")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(profile, browser, os, device, cookies, created_at, used_at, failures)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.Profile, sess.Browser, sess.OS, sess.Device, string(cookies),
		sess.CreatedAt.UTC().Format(time.RFC3339Nano), sess.UsedAt.UTC().Format(time.RFC3339Nano),
		sess.Failures)
	if err != nil {
		return 0, fmt.Errorf("store: writing a session down: %w", err)
	}
	return res.LastInsertId()
}

// Sessions are the sessions made for one kind of result page and used at or
// after since, the most recently used first.
//
// The most recently used first, because that is the one most likely to still
// be what Google remembers: a session that answered a minute ago is warmer than
// one that answered eleven hours ago, and a run taking the first free one should
// be taking the warm one.
func (s *Store) Sessions(ctx context.Context, device string, since time.Time) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, profile, browser, os, device, cookies, created_at, used_at, failures
		   FROM sessions
		  WHERE device = ? AND used_at >= ?
		  ORDER BY used_at DESC, id DESC`,
		device, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("store: reading sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Session
	for rows.Next() {
		var one Session
		var cookies, created, used string
		if err := rows.Scan(&one.ID, &one.Profile, &one.Browser, &one.OS, &one.Device,
			&cookies, &created, &used, &one.Failures); err != nil {
			return nil, fmt.Errorf("store: reading a session: %w", err)
		}
		one.Cookies = []byte(cookies)
		one.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		one.UsedAt, _ = time.Parse(time.RFC3339Nano, used)
		out = append(out, one)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading sessions: %w", err)
	}
	return out, nil
}

// SessionAnswered records that a session was asked through and answered: the
// cookies it holds now, when, and its refusals in a row back to nought.
//
// The cookies are written on every answer rather than when the session is let
// go of, because a run can end between the two — stopped, or the machine gone
// — and a session whose last clearance was never written down is a session
// that pays for it again.
func (s *Store) SessionAnswered(ctx context.Context, id int64, cookies []byte, at time.Time) error {
	if len(cookies) == 0 {
		cookies = []byte("[]")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET cookies = ?, used_at = ?, failures = 0 WHERE id = ?`,
		string(cookies), at.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("store: recording an answer on session %d: %w", id, err)
	}
	return oneRow(res, id)
}

// SessionFailed counts one more refusal in a row on a session, and gives the
// session up at SessionFailuresAllowed. It says whether it did.
//
// The count and the giving up are one statement each inside one transaction,
// so two threads failing the same session at once cannot both see one failure
// and both leave it standing.
func (s *Store) SessionFailed(ctx context.Context, id int64, at time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var failures int
	err = tx.QueryRowContext(ctx,
		`UPDATE sessions SET failures = failures + 1, used_at = ? WHERE id = ? RETURNING failures`,
		at.UTC().Format(time.RFC3339Nano), id).Scan(&failures)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: %d", ErrNoSession, id)
	}
	if err != nil {
		return false, fmt.Errorf("store: counting a refusal on session %d: %w", id, err)
	}
	dropped := failures >= SessionFailuresAllowed
	if dropped {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
			return false, fmt.Errorf("store: giving up session %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit: %w", err)
	}
	return dropped, nil
}

// DropStaleSessions gives up every session last used before the given moment,
// and says how many.
func (s *Store) DropStaleSessions(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE used_at < ?`, before.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("store: giving up stale sessions: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// oneRow says whether a statement meant for one session found it.
func oneRow(res sql.Result, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %d", ErrNoSession, id)
	}
	return nil
}
