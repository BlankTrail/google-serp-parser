// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNoQueries is returned when a job is created with nothing to run.
var ErrNoQueries = errors.New("store: a job needs at least one query")

// ErrNoUnfinishedJob is returned when no job of that name is waiting to be
// taken up.
var ErrNoUnfinishedJob = errors.New("store: no unfinished job of this name")

// JobSpec is what a job was asked to do, kept so a later reader can tell one
// night's numbers from another's without guessing at the settings behind them.
type JobSpec struct {
	Name string
	// Pages is how many result pages each query is taken to. Non-positive
	// means one, because a job stored as fetching none would resume with
	// nothing to do.
	Pages    int
	SpecName string
	Country  string
	Language string
}

// PendingQuery is one piece of work a job has not finished.
type PendingQuery struct {
	// Ordinal is the query's place in the list the job was given. It is stored
	// rather than derived: row order in SQL is not a promise, and ordering by
	// id breaks the day queries are appended to an existing job.
	Ordinal int
	Text    string
}

// CreateJob writes a job and every query it intends to run, before any of them
// is run.
//
// The whole plan is written first, in one transaction, because that plan is
// what makes an interrupted run resumable. A crash halfway through writing it
// would leave a job that looks complete and is not, so it is all or nothing.
func (s *Store) CreateJob(ctx context.Context, spec JobSpec, queries []string) (int64, error) {
	if len(queries) == 0 {
		return 0, ErrNoQueries
	}
	pages := spec.Pages
	if pages < 1 {
		pages = 1
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO jobs(name, created_at, pages, spec_name, country, language)
		 VALUES(?, ?, ?, ?, ?, ?)`,
		spec.Name, time.Now().UTC().Format(time.RFC3339), pages,
		spec.SpecName, spec.Country, spec.Language)
	if err != nil {
		return 0, fmt.Errorf("store: recording the job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: reading the job id: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO queries(job_id, ordinal, text) VALUES(?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("store: preparing the query insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	// The ordinal is written here rather than left to the row it lands in,
	// which is what keeps a query someone listed twice as two pieces of work.
	for i, text := range queries {
		if _, err := stmt.ExecContext(ctx, id, i, text); err != nil {
			return 0, fmt.Errorf("store: recording query %d: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit: %w", err)
	}
	return id, nil
}

// Pending returns the queries of a job that have not finished, in the order
// they were given. A job that does not exist has nothing pending, which is the
// same answer as a job that is done — both mean there is no work here.
func (s *Store) Pending(ctx context.Context, jobID int64) ([]PendingQuery, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ordinal, text FROM queries
		 WHERE job_id = ? AND state = 'pending'
		 ORDER BY ordinal`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: reading pending queries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PendingQuery
	for rows.Next() {
		var p PendingQuery
		if err := rows.Scan(&p.Ordinal, &p.Text); err != nil {
			return nil, fmt.Errorf("store: reading a pending query: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading pending queries: %w", err)
	}
	return out, nil
}

// UnfinishedJob is a job that was never stamped as done, with the settings it
// was created under.
type UnfinishedJob struct {
	ID   int64
	Spec JobSpec
}

// LastUnfinished finds the newest job of a name that still has to be taken up.
//
// A job is looked up by name because that is what the person who started it
// knows it by; asking them for the id of a run that died in the night is asking
// them to read the database first.
//
// The settings come back with it. A job picked up part way has to run as the
// job it is: taking today's depth or today's country instead would mix results
// of two shapes into one run, and nothing in the history would say which rows
// were which.
//
// Jobs stamped in the same second are ordered by the one written last, so a
// name used twice in a minute resumes the later of the two rather than either.
func (s *Store) LastUnfinished(ctx context.Context, name string) (UnfinishedJob, error) {
	var j UnfinishedJob
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, pages, spec_name, country, language
		   FROM jobs
		  WHERE name = ? AND finished_at IS NULL
		  ORDER BY created_at DESC, id DESC
		  LIMIT 1`, name).
		Scan(&j.ID, &j.Spec.Name, &j.Spec.Pages, &j.Spec.SpecName, &j.Spec.Country, &j.Spec.Language)
	if errors.Is(err, sql.ErrNoRows) {
		return UnfinishedJob{}, fmt.Errorf("%w: %q", ErrNoUnfinishedJob, name)
	}
	if err != nil {
		return UnfinishedJob{}, fmt.Errorf("store: looking for an unfinished %q: %w", name, err)
	}
	return j, nil
}

// FinishJob stamps a job as done.
func (s *Store) FinishJob(ctx context.Context, jobID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET finished_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), jobID)
	if err != nil {
		return fmt.Errorf("store: finishing job %d: %w", jobID, err)
	}
	return nil
}
