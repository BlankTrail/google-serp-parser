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

	// Kind is what the job asks Google for — KindParse, KindPosition or
	// KindIndex. A reader that does not know it cannot say what a row of this job
	// means: the same captured result is one of a page's worth of findings under
	// the first, a position under the second and a verdict under the third.
	Kind string

	// Target is the site a position check is about, and empty under the other
	// kinds. It travels with the job because both the run and the page that
	// reports it are about that site and nothing else: a check read without it is
	// a column of numbers with no question above them.
	Target string

	// UniqueBy is what this job drops as a repeat, and Dropped is how many it
	// has dropped. They travel together because neither says much alone: a count
	// with no filter behind it reads as results lost to something unnamed, and a
	// filter with no count reads as one that found nothing to do.
	UniqueBy UniqueBy
	Dropped  int

	Pages    int
	Country  string
	Language string
	// Device is which kind of result page this job asked Google for. Empty is a
	// desktop — see JobSpec.Device.
	Device string
	// Cooldown is the gap this job leaves between two requests on one identity,
	// and nought is a job that named none — see JobSpec.Cooldown.
	Cooldown time.Duration

	// Ports and Threads are the pool this job asks to be run on. Zero in either
	// is a job that named no size rather than one asking for nothing at all — see
	// JobSpec.Ports — and it is handed on as the zero it is, because a page
	// offering to change the pool has to show which of the two it is looking at.
	Ports   int
	Threads int
	// Tries is how many identities one query may be taken to before it is
	// written off, and zero is a job that named none — the same reading as the
	// two above.
	Tries int
	// Fields is what each result of this job keeps, and empty is everything.
	Fields Fields
	// ProfileID is the proxy profile this job runs through, and nought is a job
	// that named none: it runs on whichever profile is default. The name behind
	// the number is read separately, by whoever is drawing it — a job listing
	// that joined the profiles would carry the same name a hundred times to
	// answer a question most screens do not ask.
	ProfileID int64

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
	SELECT j.id, j.name, j.created_at, coalesce(j.finished_at, ''), j.kind, j.target,
	       j.unique_by, j.dropped,
	       j.pages, j.country, j.language, j.device,
	       j.ports, j.threads, j.tries, j.cooldown_ms, j.fields, j.profile_id, j.plan_ready,
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
	// The gap is read as a number and turned back into a span below: a duration
	// in a database has to be a number, and this is one of the two places that
	// has to know which unit the column is written in.
	var cooldownMS int64
	err := row.Scan(&sum.ID, &sum.Name, &created, &finished, &sum.Kind, &sum.Target,
		&sum.UniqueBy, &sum.Dropped,
		&sum.Pages, &sum.Country, &sum.Language, &sum.Device,
		&sum.Ports, &sum.Threads, &sum.Tries, &cooldownMS, &sum.Fields, &sum.ProfileID, &sum.PlanReady,
		&sum.Total, &sum.Done, &sum.Failed, &sum.Pending)
	if errors.Is(err, sql.ErrNoRows) {
		return JobSummary{}, err
	}
	if err != nil {
		return JobSummary{}, fmt.Errorf("store: reading a job summary: %w", err)
	}
	// A stamp that cannot be read costs the reader a date, not the counts, so
	// it comes back zero rather than ending the listing.
	sum.Cooldown = time.Duration(cooldownMS) * time.Millisecond
	sum.CreatedAt, _ = time.Parse(time.RFC3339, created)
	if finished != "" {
		sum.FinishedAt, _ = time.Parse(time.RFC3339, finished)
		sum.Finished = true
	}
	return sum, nil
}
