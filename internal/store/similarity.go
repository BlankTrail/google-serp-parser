// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
)

// Unscored is a suggestion nobody has measured yet, with the key it belongs to.
type Unscored struct {
	ID        int64
	Key, Text string
}

// Unscored hands over up to limit of a job's suggestions with no closeness
// score, after the result id after, in id order. It is a walk that a job of
// half a million rows can be taken through in batches, and resumed where it
// stopped, because every batch is asked for by the last id of the one before:
// an offset would be re-counted from the start each time, and rows scored in
// between would shift it and skip some.
func (s *Store) Unscored(ctx context.Context, jobID, after int64, limit int) ([]Unscored, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id, q.text, r.title
		   FROM results r
		   JOIN pages   p ON p.id = r.page_id
		   JOIN queries q ON q.id = p.query_id
		  WHERE q.job_id = ? AND r.similarity IS NULL AND r.id > ?
		  ORDER BY r.id
		  LIMIT ?`, jobID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: reading job %d's unscored suggestions: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Unscored
	for rows.Next() {
		var u Unscored
		if err := rows.Scan(&u.ID, &u.Key, &u.Text); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetSimilarity writes scores, all of them or none: a batch cut in half by a
// failure would leave rows scored by a run that then reports it stopped, and
// the next press would have to work out which half it was.
func (s *Store) SetSimilarity(ctx context.Context, scores map[int64]float64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `UPDATE results SET similarity = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for id, v := range scores {
		if _, err := stmt.ExecContext(ctx, v, id); err != nil {
			return fmt.Errorf("store: scoring result %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// UnscoredCount is how many of a job's suggestions have no score yet.
func (s *Store) UnscoredCount(ctx context.Context, jobID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM results r
		   JOIN pages p ON p.id = r.page_id
		   JOIN queries q ON q.id = p.query_id
		  WHERE q.job_id = ? AND r.similarity IS NULL`, jobID).Scan(&n)
	return n, err
}
