// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/blanktrail/google-serp-parser/google"
)

// QueryOutcome is what became of one query.
type QueryOutcome struct {
	// Ordinal names which query of the job this is.
	Ordinal int
	// Pages are what was captured, in the order they were taken. A query that
	// failed part way still carries the pages it managed.
	Pages []google.SERP
	// Err is why the query produced nothing, or nothing further.
	Err error
}

// Record writes what became of one query, and stops it being pending.
//
// It is called as the job runs rather than at the end, which is the whole
// reason this package exists: a job of ten thousand queries that dies on the
// nine thousandth should lose one query, not nine thousand.
//
// Everything goes in one transaction. A query written half way — the second
// page present and the first missing — is worse than one not written at all,
// because it reads as data.
func (s *Store) Record(ctx context.Context, jobID int64, out QueryOutcome) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var queryID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM queries WHERE job_id = ? AND ordinal = ?`, jobID, out.Ordinal).Scan(&queryID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: job %d has no query at ordinal %d", jobID, out.Ordinal)
	}
	if err != nil {
		return fmt.Errorf("store: finding the query: %w", err)
	}

	// The rank runs across the whole walk. A position is what the caller asked
	// for, and the eleventh result is eleventh whichever page carried it.
	rank := 0
	for i, serp := range out.Pages {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO pages(query_id, number, origin) VALUES(?, ?, ?)`,
			queryID, i+1, serp.Origin)
		if err != nil {
			return fmt.Errorf("store: recording page %d: %w", i+1, err)
		}
		pageID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: reading the page id: %w", err)
		}
		for _, r := range serp.Results {
			rank++
			_, err := tx.ExecContext(ctx,
				`INSERT INTO results(page_id, rank, title, url, link, host, snippet)
				 VALUES(?, ?, ?, ?, ?, ?, ?)`,
				pageID, rank, r.Title, r.URL, r.Link, r.Host, r.Snippet)
			if err != nil {
				return fmt.Errorf("store: recording result %d: %w", rank, err)
			}
		}
	}

	// A query that was tried and refused is written as refused. Left pending it
	// would be handed out again by every resume, and a reader would never learn
	// why it produced nothing.
	state, msg := "done", ""
	if out.Err != nil {
		state, msg = "failed", out.Err.Error()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE queries SET state = ?, err = ? WHERE id = ?`, state, msg, queryID); err != nil {
		return fmt.Errorf("store: settling the query: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}
