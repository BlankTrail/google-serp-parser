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
// A job that asked to drop repeats drops them here, as the results are written
// and never afterwards. This is the funnel everything captured already flows
// through, and it is the only place where a repeat can be refused before it
// costs anything to keep.
//
// Everything goes in one transaction. A query written half way — the second
// page present and the first missing — is worse than one not written at all,
// because it reads as data. What the job has seen is written in that same
// transaction, so a mark and the result it stands for are one act.
func (s *Store) Record(ctx context.Context, jobID int64, out QueryOutcome) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The filter is read once for the whole call, alongside the query it belongs
	// to, so a job of ten results per query does not ask ten times what it asked
	// for once — and so the filter that is applied cannot be a different job's.
	var queryID int64
	var by UniqueBy
	var keep Fields
	err = tx.QueryRowContext(ctx,
		`SELECT q.id, j.unique_by, j.fields
		   FROM queries q JOIN jobs j ON j.id = q.job_id
		  WHERE q.job_id = ? AND q.ordinal = ?`, jobID, out.Ordinal).Scan(&queryID, &by, &keep)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: job %d has no query at ordinal %d", jobID, out.Ordinal)
	}
	if err != nil {
		return fmt.Errorf("store: finding the query: %w", err)
	}

	// The rank runs across the whole walk. A position is what the caller asked
	// for, and the eleventh result is eleventh whichever page carried it.
	rank := 0
	// dropped counts the repeats this query brought, so the job can say how many
	// results it threw away rather than leaving a reader to wonder why there are
	// fewer than they expected.
	dropped := 0
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
			// The rank counts what arrived, not what was kept. A repeat dropped from
			// between two results does not move the one below it: a page held it at
			// position seven, and seven is where it stood whatever became of the one
			// above.
			rank++
			// A result that already knows where it stood is filed there. A check
			// looking for one site hands over that site and nothing else, and the
			// place it stood is the only thing the check produced: counting the rows
			// handed over would file a site that ranked seventh as first. A walk
			// cannot be moved by this, because a page numbers its results from one
			// again on every page, so what a walk carries is never further down than
			// the count already is.
			if r.Position > rank {
				rank = r.Position
			}
			// A job that asked for no filter never reaches the table of what it has
			// seen. It pays neither the write nor the room it would take.
			if key, ok := keyOf(by, r); ok {
				first, err := s.firstSeen(ctx, tx, jobID, key)
				if err != nil {
					return err
				}
				if !first {
					dropped++
					continue
				}
			}
			// Only what the job asked to keep is written. A part left out is left
			// out of the row rather than written empty: the room is the whole point,
			// and on a job of ten million results the snippet alone is most of it.
			_, err := tx.ExecContext(ctx,
				`INSERT INTO results(page_id, rank, title, url, link, host, snippet, display_path)
				 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
				pageID, rank,
				kept(keep, FieldTitle, r.Title), kept(keep, FieldURL, r.URL),
				kept(keep, FieldLink, r.Link), kept(keep, FieldHost, r.Host),
				kept(keep, FieldSnippet, r.Snippet), kept(keep, FieldPath, r.DisplayPath))
			if err != nil {
				return fmt.Errorf("store: recording result %d: %w", rank, err)
			}
		}
		if err := writeAside(ctx, tx, pageID, keep, serp); err != nil {
			return err
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
	// Written only when there was something to add, so a query that dropped
	// nothing touches no row it would not have touched before. It goes in this
	// transaction with the results it is about: a count that survived a write
	// that failed would say a job dropped results it never saw.
	if dropped > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET dropped = dropped + ? WHERE id = ?`, dropped, jobID); err != nil {
			return fmt.Errorf("store: counting what job %d dropped: %w", jobID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// kept is the value when the job keeps that part of a result, and nothing when
// it does not.
func kept(fields Fields, name, value string) string {
	if fields.Keeps(name) {
		return value
	}
	return ""
}

// writeAside writes what the page carried besides its results: the paid
// placements and the searches it suggested.
//
// Neither is filtered for repeats. A repeat among results is a second sighting
// of the same address, which is what the filter is about; the same advertiser
// on two queries is two facts about two pages, and dropping the second would
// answer "who advertises here" with a list that depends on the order the
// queries ran in.
func writeAside(ctx context.Context, tx *sql.Tx, pageID int64, keep Fields, serp google.SERP) error {
	if keep.Keeps(FieldAds) {
		for i, ad := range serp.Ads {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO ads(page_id, position, placement, title, host, url, snippet)
				 VALUES(?, ?, ?, ?, ?, ?, ?)`,
				pageID, i+1, string(ad.Placement), ad.Title, ad.Host, ad.URL, ad.Snippet); err != nil {
				return fmt.Errorf("store: recording a paid placement: %w", err)
			}
		}
	}
	if keep.Keeps(FieldRelated) {
		for i, query := range serp.Related {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO related(page_id, position, query) VALUES(?, ?, ?)`,
				pageID, i+1, query); err != nil {
				return fmt.Errorf("store: recording a related search: %w", err)
			}
		}
	}
	return nil
}
