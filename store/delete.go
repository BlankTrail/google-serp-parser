// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DeleteJob removes a job and everything gathered under it.
//
// Everything means everything: the queries, the pages, the results, the paid
// placements, the suggested searches and what the filter had seen. They go
// because every one of them points at the job with ON DELETE CASCADE, so this
// is one statement rather than seven — and seven statements is seven chances to
// leave a table behind, which is how a history ends up holding ten million rows
// belonging to a job nobody can see.
//
// What it does not do is make the file smaller. SQLite hands the space back to
// itself rather than to the disk: the pages are free for whatever is written
// next, so a machine that deletes a job and runs another does not grow, but one
// that only deletes stays the size it reached. Shrinking the file is a VACUUM,
// which rewrites the whole history and takes as long as the history is large,
// and doing that inside a delete would make a button somebody presses take
// minutes with nothing on the screen saying why.
func (s *Store) DeleteJob(ctx context.Context, jobID int64) error {
	// The cascade is enforced rather than assumed. Every connection this package
	// opens has foreign keys on, but a delete that quietly left the results
	// behind would look exactly like one that worked.
	var on int
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&on); err != nil {
		return fmt.Errorf("store: asking whether the cascade is on: %w", err)
	}
	if on != 1 {
		return errors.New("store: the cascade is off, so deleting a job would leave its results behind")
	}

	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, jobID)
	if err != nil {
		return fmt.Errorf("store: deleting job %d: %w", jobID, err)
	}
	gone, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: deleting job %d: %w", jobID, err)
	}
	if gone == 0 {
		return fmt.Errorf("%w: %d", ErrNoJob, jobID)
	}
	return nil
}

// CountsUnder is how many rows of each kind a job holds.
//
// It exists for the tests that prove a delete takes them: counting after the
// job is gone is the only way to tell a cascade that fired from one that was
// never asked to.
func (s *Store) CountsUnder(ctx context.Context, jobID int64) (map[string]int, error) {
	counts := map[string]int{}
	for table, query := range map[string]string{
		"queries": `SELECT count(*) FROM queries WHERE job_id = ?`,
		"seen":    `SELECT count(*) FROM seen WHERE job_id = ?`,
		"pages": `SELECT count(*) FROM pages p
		             JOIN queries q ON q.id = p.query_id WHERE q.job_id = ?`,
		"results": `SELECT count(*) FROM results r
		               JOIN pages p   ON p.id = r.page_id
		               JOIN queries q ON q.id = p.query_id WHERE q.job_id = ?`,
		"ads": `SELECT count(*) FROM ads a
		           JOIN pages p   ON p.id = a.page_id
		           JOIN queries q ON q.id = p.query_id WHERE q.job_id = ?`,
		"related": `SELECT count(*) FROM related r
		               JOIN pages p   ON p.id = r.page_id
		               JOIN queries q ON q.id = p.query_id WHERE q.job_id = ?`,
	} {
		var n int
		if err := s.db.QueryRowContext(ctx, query, jobID).Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store: counting %s under job %d: %w", table, jobID, err)
		}
		counts[table] = n
	}
	return counts, nil
}
