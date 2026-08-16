// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"time"
)

// Row is one captured result, with enough of its query attached to stand on its
// own in an export.
type Row struct {
	Ordinal int
	Query   string
	Page    int
	Rank    int
	Title   string
	URL     string
	Host    string
	Snippet string
}

// Rows hands every result of a job to fn, in the order the job had.
//
// It streams rather than returning a slice: a job of ten thousand queries at
// ten results each is a million rows, and an export has no reason to hold them
// all in memory on the way to a file. An error from fn ends the walk and comes
// back unchanged, so a caller can stop it with its own sentinel.
func (s *Store) Rows(ctx context.Context, jobID int64, fn func(Row) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT q.ordinal, q.text, p.number, r.rank, r.title, r.url, r.host, r.snippet
		   FROM results r
		   JOIN pages   p ON p.id = r.page_id
		   JOIN queries q ON q.id = p.query_id
		  WHERE q.job_id = ?
		  ORDER BY q.ordinal, r.rank`, jobID)
	if err != nil {
		return fmt.Errorf("store: reading job %d: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Ordinal, &r.Query, &r.Page, &r.Rank,
			&r.Title, &r.URL, &r.Host, &r.Snippet); err != nil {
			return fmt.Errorf("store: reading a result: %w", err)
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	// A walk cut short by a database that went away must not read as a job that
	// held nothing more, or an export would be written short and called whole.
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: reading job %d: %w", jobID, err)
	}
	return nil
}

// Failures hands what each refused query of a job came back with to fn, in the
// order the job had.
//
// It streams for the same reason Rows does: every query of a job can refuse, and
// a screen counting what kind of refusals they were has no reason to hold ten
// thousand sentences at once. An error from fn ends the walk and comes back
// unchanged.
//
// Only queries settled as refused are handed over. A query nobody has reached
// yet carries no reason, and one that succeeded carries none either; passing
// those on would report work that has not failed among the failures.
func (s *Store) Failures(ctx context.Context, jobID int64, fn func(why string) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT err FROM queries
		  WHERE job_id = ? AND state = 'failed'
		  ORDER BY ordinal`, jobID)
	if err != nil {
		return fmt.Errorf("store: reading the refusals of job %d: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var why string
		if err := rows.Scan(&why); err != nil {
			return fmt.Errorf("store: reading a refusal: %w", err)
		}
		if err := fn(why); err != nil {
			return err
		}
	}
	// A walk cut short by a database that went away must not read as a job whose
	// refusals stopped there, or a breakdown drawn from it names the wrong share.
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: reading the refusals of job %d: %w", jobID, err)
	}
	return nil
}

// Position is where a site stood for one query on one run.
type Position struct {
	JobID   int64
	JobName string
	TakenAt time.Time
	Query   string
	Rank    int
	URL     string
}

// History hands every position a host held to fn, oldest run first.
//
// It is derived rather than kept in a table of its own. A second copy of the
// same truth is a second thing to keep right, and the day the two disagree
// there is no way to tell which one lied.
//
// Runs stamped in the same second are ordered by the job they belong to, so a
// history reads the same way twice.
func (s *Store) History(ctx context.Context, host string, fn func(Position) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT j.id, j.name, j.created_at, q.text, r.rank, r.url
		   FROM results r
		   JOIN pages   p ON p.id = r.page_id
		   JOIN queries q ON q.id = p.query_id
		   JOIN jobs    j ON j.id = q.job_id
		  WHERE r.host = ?
		  ORDER BY j.created_at, j.id, q.ordinal, r.rank`, host)
	if err != nil {
		return fmt.Errorf("store: reading the history of %q: %w", host, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var p Position
		var at string
		if err := rows.Scan(&p.JobID, &p.JobName, &at, &p.Query, &p.Rank, &p.URL); err != nil {
			return fmt.Errorf("store: reading a position: %w", err)
		}
		// A stamp that cannot be read costs the reader the date, not the number:
		// the site still stood where it stood. Ending the walk here would throw
		// away every later position too, and the zero time says what was lost.
		p.TakenAt, _ = time.Parse(time.RFC3339, at)
		if err := fn(p); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: reading the history of %q: %w", host, err)
	}
	return nil
}
