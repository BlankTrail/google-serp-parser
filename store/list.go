// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNoJob is returned for a job id nothing was stored under.
var ErrNoJob = errors.New("store: no such job")

// defaultJobLimit is how many jobs a listing returns when the caller names no
// number. A page that asks for none because a parameter went missing should
// still show something useful.
const defaultJobLimit = 50

// JobSummary is one job and how far it has got.
type JobSummary struct {
	ID         int64
	Name       string
	CreatedAt  time.Time
	FinishedAt time.Time
	Finished   bool

	Pages    int
	Country  string
	Language string
	SpecName string

	// PlanReady says the job's list of queries finished arriving. A job without
	// it is one whose upload broke off part way: it is here, it holds whatever
	// reached the database, and nothing will run it.
	PlanReady bool

	Total   int
	Done    int
	Failed  int
	Pending int
}

// jobSummaryQuery is the select a listing and a single job share, so the two
// cannot drift into reporting different things about the same job.
//
// The join is outer and the total counts query ids rather than rows, which is
// what keeps a job nothing was recorded against in the answer, with a count of
// nothing rather than a count of one.
const jobSummaryQuery = `
	SELECT j.id, j.name, j.created_at, coalesce(j.finished_at, ''),
	       j.pages, j.country, j.language, j.spec_name, j.plan_ready,
	       count(q.id),
	       sum(CASE WHEN q.state = 'done'    THEN 1 ELSE 0 END),
	       sum(CASE WHEN q.state = 'failed'  THEN 1 ELSE 0 END),
	       sum(CASE WHEN q.state = 'pending' THEN 1 ELSE 0 END)
	  FROM jobs j LEFT JOIN queries q ON q.job_id = j.id`

// Jobs lists jobs, newest first.
//
// The counts come from one pass over the queries rather than from a query per
// state. Three separate counts are three snapshots of three different moments,
// and on a job that is still running they will not add up to the total — which
// a progress bar would then render as nonsense.
//
// Jobs stamped in the same second are ordered by the one written last, so two
// runs started in the same minute read newest first like every other pair.
func (s *Store) Jobs(ctx context.Context, limit int) ([]JobSummary, error) {
	if limit < 1 {
		limit = defaultJobLimit
	}
	rows, err := s.db.QueryContext(ctx,
		jobSummaryQuery+` GROUP BY j.id ORDER BY j.created_at DESC, j.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []JobSummary
	for rows.Next() {
		sum, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sum)
	}
	// A listing cut short by a database that went away must not read as the
	// last few runs having never happened.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing jobs: %w", err)
	}
	return out, nil
}

// Progress returns one job and how far it has got.
func (s *Store) Progress(ctx context.Context, jobID int64) (JobSummary, error) {
	row := s.db.QueryRowContext(ctx, jobSummaryQuery+` WHERE j.id = ? GROUP BY j.id`, jobID)
	sum, err := scanSummary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return JobSummary{}, fmt.Errorf("%w: %d", ErrNoJob, jobID)
	}
	if err != nil {
		return JobSummary{}, err
	}
	return sum, nil
}

// scanner is what a listing and a single job have in common: a row that can be
// scanned.
type scanner interface {
	Scan(dest ...any) error
}

// scanSummary reads one row of jobSummaryQuery.
//
// It hands sql.ErrNoRows back unwrapped, because Progress tells "no such job"
// from "this row would not read" by looking for exactly that.
func scanSummary(row scanner) (JobSummary, error) {
	var sum JobSummary
	var created, finished string
	err := row.Scan(&sum.ID, &sum.Name, &created, &finished,
		&sum.Pages, &sum.Country, &sum.Language, &sum.SpecName, &sum.PlanReady,
		&sum.Total, &sum.Done, &sum.Failed, &sum.Pending)
	if errors.Is(err, sql.ErrNoRows) {
		return JobSummary{}, err
	}
	if err != nil {
		return JobSummary{}, fmt.Errorf("store: reading a job summary: %w", err)
	}
	// A stamp that cannot be read costs the reader a date, not the counts, so
	// it comes back zero rather than ending the listing.
	sum.CreatedAt, _ = time.Parse(time.RFC3339, created)
	if finished != "" {
		sum.FinishedAt, _ = time.Parse(time.RFC3339, finished)
		sum.Finished = true
	}
	return sum, nil
}
