// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Unscored is a suggestion nobody has measured yet, with the key it belongs to.
type Unscored struct {
	ID        int64
	Key, Text string
}

// Both queries start from the job — its queries by their (job_id, ordinal)
// index, their pages by the unique (query_id, number) one — and reach the
// results through the partial index of unmeasured rows keyed by page. The index
// used to be keyed by id and named in the query with INDEXED BY, so that the
// walk read unmeasured rows in id order; but every search, position and index
// job's results are unmeasured forever, and that index made a count on a
// database with two million of them read all two million (a third of a second
// per job page). Keyed by page, a job's walk reads only its own unmeasured rows
// and sorts them: a batch costs the rows still to do, which the backfill
// shrinks as it goes, and the rows of other jobs are never touched.
//
// Two things keep the walk on that path, whatever statistics the database has;
// the count, with no order and no limit to tempt the planner, takes it freely.
// CROSS JOIN fixes the order of the tables, which SQLite takes as an
// instruction not to reorder them: statistics that make a page look like a
// million rows (a database whose few pages are huge) otherwise send it to read
// the results table whole and join each row to its page and query. And the
// unary plus on r.id > after keeps that term from being used to reach the
// rows: as a range on the rowid it would be applied to every page in turn,
// reading every result above after for each, and once the database has been
// analysed the walk's ORDER BY r.id ... LIMIT tempts the planner to take it.
// Which index each table is reached by is still the planner's choice, and a
// test asserts it with no statistics, with real ones and with skewed ones,
// with another job's unmeasured rows in the table.

const unscoredSQL = `SELECT r.id, q.text, r.title
		   FROM queries q
		  CROSS JOIN pages   p ON p.query_id = q.id
		  CROSS JOIN results r ON r.page_id = p.id
		  WHERE q.job_id = ? AND r.similarity IS NULL AND +r.id > ?
		  ORDER BY r.id
		  LIMIT ?`

const unscoredCountSQL = `SELECT count(*) FROM queries q
		   JOIN pages   p ON p.query_id = q.id
		   JOIN results r ON r.page_id = p.id
		  WHERE q.job_id = ? AND r.similarity IS NULL`

// Unscored hands over up to limit of a job's suggestions with no closeness
// score, after the result id after, in id order. It is a walk that a job of
// half a million rows can be taken through in batches, and resumed where it
// stopped, because every batch is asked for by the last id of the one before:
// an offset would be re-counted from the start each time, and rows scored in
// between would shift it and skip some.
func (s *Store) Unscored(ctx context.Context, jobID, after int64, limit int) ([]Unscored, error) {
	rows, err := s.db.QueryContext(ctx, unscoredSQL, jobID, after, limit)
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
	err := s.db.QueryRowContext(ctx, unscoredCountSQL, jobID).Scan(&n)
	return n, err
}

// FilterCount is how many of a job's suggestions there are and how many a
// filter leaves out: the ones marked as having nothing of their key in them,
// and, filtering by meaning, the measured ones below floor. A suggestion never
// measured is left out only by its words. It is the same rule the export
// applies row by row, so the number the tab shows is the number the download
// drops. It reads the whole job once, with no index of its own: the screen asks
// for it when the filter or the threshold changes, not per row.
func (s *Store) FilterCount(ctx context.Context, jobID int64, meaning bool, floor float64) (total, cut int, err error) {
	byMeaning := 0
	if meaning {
		byMeaning = 1
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT count(*),
		        coalesce(sum(CASE WHEN r.offtopic = 1
		                           OR (? = 1 AND r.similarity IS NOT NULL AND r.similarity < ?)
		                          THEN 1 ELSE 0 END), 0)
		   FROM results r
		   JOIN pages p ON p.id = r.page_id
		   JOIN queries q ON q.id = p.query_id
		  WHERE q.job_id = ?`, byMeaning, floor, jobID).Scan(&total, &cut)
	return total, cut, err
}

// HasScores says whether any of a job's suggestions was measured. Only then is
// a filter by meaning on offer: without a single score it would be the filter
// by words under another name.
func (s *Store) HasScores(ctx context.Context, jobID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM results r
		   JOIN pages p ON p.id = r.page_id
		   JOIN queries q ON q.id = p.query_id
		  WHERE q.job_id = ? AND r.similarity IS NOT NULL LIMIT 1`, jobID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
