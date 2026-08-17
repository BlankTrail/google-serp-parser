// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoQueries is returned when a job is created with nothing to run.
var ErrNoQueries = errors.New("store: a job needs at least one query")

// ErrNoUnfinishedJob is returned when no job of that name is waiting to be
// taken up.
var ErrNoUnfinishedJob = errors.New("store: no unfinished job of this name")

// ErrPlanUnfinished is returned for a job whose list of queries was never
// finished being written.
var ErrPlanUnfinished = errors.New("store: the job's plan was never finished")

// KindParse, KindPosition and KindIndex are the three things a job can be, and
// they differ by what is asked rather than by what is written down: one hands
// Google phrases and writes down the whole of what comes back, one hands it the
// same phrases and reports where a named site stood in the answer, and one hands
// it addresses and reports whether they are held at all.
//
// They are the three words the column will hold, and the database refuses a
// fourth. A job filed under a kind no engine answers to would be picked up, found
// to be nothing anyone runs, and left in the queue.
const (
	// KindParse is the ordinary job: phrases in, the results they came back with
	// out.
	//
	// Its value is "search" and will stay "search". Every job this program has
	// ever recorded was this one — it took phrases and wrote down the whole
	// page, whatever the screen called it — and a rename in the database would
	// go back over that history and file it under a question none of those runs
	// asked. The word in the column is a fact about what was run; the name in Go
	// is what this program calls it now, and only the second was wrong.
	KindParse = "search"
	// KindPosition asks where one site stands for each phrase. It is the kind
	// that needs a target, and the only one that does.
	KindPosition = "position"
	// KindIndex asks whether Google holds each address in the list.
	KindIndex = "index"
)

// ErrNoTarget is returned when a position check is written down without the
// site it is supposed to be about.
//
// It is refused at the door rather than run: a position check with nothing to
// look for would answer "not found" for every phrase in the list, and that is a
// wrong answer nothing left in the database gives a reader any way to doubt.
var ErrNoTarget = errors.New("store: a position check needs the site it is about")

// JobSpec is what a job was asked to do, kept so a later reader can tell one
// night's numbers from another's without guessing at the settings behind them.
type JobSpec struct {
	Name string
	// Kind is what the job asks Google for. Empty means KindParse: every job
	// written before this field existed took phrases and wrote down what came
	// back, and one whose kind went missing has to run as the ordinary thing
	// rather than not at all.
	Kind string
	// Target is the site a position check is about, and it is meaningless under
	// the other two kinds. A check without one is refused rather than written
	// down; see ErrNoTarget.
	//
	// It is settled when the job is created and never afterwards, for the reason
	// UniqueBy is: a run half of whose phrases were measured against one site
	// and half against another is a run whose numbers stand for nothing, and
	// nothing in the history would say where the change fell.
	Target string
	// Pages is how many result pages each query is taken to. Non-positive
	// means one, because a job stored as fetching none would resume with
	// nothing to do.
	Pages int
	// UniqueBy is what this job drops as a repeat, and UniqueOff — the empty
	// string — keeps everything. It is settled when the job is created and never
	// afterwards: dropping happens as results are written and cannot be undone,
	// so a job whose filter changed part way would hold results gathered under
	// two rules with nothing to say which was which.
	UniqueBy UniqueBy
	SpecName string
	Country  string
	Language string

	// Ports is how many Google identities this job runs on at once, and Threads
	// is how many queries it keeps in flight over them. They belong to the job
	// and not to the machine because a pool is raised for one job and taken down
	// when that job lets go: no two jobs share one, so no two jobs have to agree
	// on its size.
	//
	// Zero means this job named no size. It does not mean a pool of nothing, and
	// the difference is the whole point: every job written before these fields
	// existed reads back as zero, and a job recorded last week has to go on
	// running rather than refuse to.
	//
	// What that zero becomes is deliberately decided elsewhere — where pools are
	// raised, not here. This package has no number it could honestly put in its
	// place: the size a job that named none should run at is whatever the program
	// raising pools was configured with, it changes when that configuration
	// changes, and a store that baked one in would be a second place claiming to
	// own it. Substituting one on the way in or out would also leave a job that
	// named nothing indistinguishable from one that named exactly that number,
	// and telling those two apart is precisely what a page offering to change the
	// pool has to do.
	//
	// A number below nothing is read as nothing named, for the same reason a
	// non-positive page count is read as one page: it is a fumbled field, not a
	// run somebody meant, and refusing the whole job over it costs more than it
	// saves.
	Ports   int
	Threads int
	// Tries is how many identities one query may be taken to before it is
	// written off. Nought is a job that said nothing, read the same way the two
	// above it are, and for the same reason: a job written before the column
	// existed carries it and has to go on running.
	//
	// It is the job's and not the machine's because the answer depends on the
	// list, and the list is somebody's: one operator's addresses are fresh this
	// morning and another's have been hammered for a week.
	Tries int
}

// kind is what to write in the column, which is never the empty string.
//
// It is filled in on the way to the database rather than left to the column's
// own default, so a job read straight back carries the kind the run will be
// judged by instead of a blank that every reader has to interpret again.
func (s JobSpec) kind() string {
	if s.Kind == "" {
		return KindParse
	}
	return s.Kind
}

// target is what to write in the target column: the site with the spaces a
// browser sends around it taken off, and nothing else changed. What counts as
// the same address is settled where addresses are compared, and a store that
// tidied one on the way in would be a second opinion about it.
func (s JobSpec) target() string { return strings.TrimSpace(s.Target) }

// refusal is what makes this job impossible to run, or nil.
//
// It is one function because a job is written down by two doors — the whole
// list in one transaction, or a list too large for that, arriving line by line —
// and a rule enforced at one of them is a rule the other lets past. The database
// refuses the same thing underneath, and this is here so that a caller is told
// which field is missing rather than handed a constraint.
func (s JobSpec) refusal() error {
	if s.kind() == KindPosition && s.target() == "" {
		return ErrNoTarget
	}
	return nil
}

// pool is what to write in the two pool columns.
//
// It is one function rather than a line at each of the two places a job is
// written, because the two must not drift: a job created from a list held in
// memory and a job created from a file arriving over a connection are the same
// job, and an operator who cannot tell which path wrote theirs cannot be told
// that only one of them keeps the numbers.
func (s JobSpec) pool() (ports, threads, tries int) {
	return atLeastNone(s.Ports), atLeastNone(s.Threads), atLeastNone(s.Tries)
}

// atLeastNone reads a size below nothing as a size nobody named.
func atLeastNone(n int) int {
	if n < 0 {
		return 0
	}
	return n
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
	if err := spec.refusal(); err != nil {
		return 0, err
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

	// The plan is marked ready in the same transaction that writes it, because
	// this call has the whole list in hand: either all of it is committed or
	// none of it is, and there is no moment in between for a reader to see. A
	// list too large to hold is written by a different path, which sets the flag
	// after its last batch.
	ports, threads, tries := spec.pool()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO jobs(name, created_at, kind, target, unique_by, pages, spec_name, country, language, ports, threads, tries, plan_ready)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		spec.Name, time.Now().UTC().Format(time.RFC3339), spec.kind(), spec.target(), string(spec.UniqueBy), pages,
		spec.SpecName, spec.Country, spec.Language, ports, threads, tries)
	if err != nil {
		return 0, fmt.Errorf("store: recording the job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: reading the job id: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, queryInsert)
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
// A job whose plan was never finished is refused rather than answered, and
// this is the one place every run and every resume passes through to learn what
// to do. Handing back the queries that did land would run a fraction of the
// list and stamp the job done; handing back none would read as a job with
// nothing left, which is the same loss said more quietly.
func (s *Store) Pending(ctx context.Context, jobID int64) ([]PendingQuery, error) {
	var ready bool
	err := s.db.QueryRowContext(ctx, `SELECT plan_ready FROM jobs WHERE id = ?`, jobID).Scan(&ready)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: reading the plan of job %d: %w", jobID, err)
	case !ready:
		return nil, fmt.Errorf("%w: job %d", ErrPlanUnfinished, jobID)
	}

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
// were which. The pool travels with them, because the run being taken up is the
// one that raises it, and so does the target: a check carried on against another
// site would be one column of positions standing for two questions.
//
// A job whose plan was never finished is passed over rather than chosen. It is
// the newest job of its name and it is not work, and choosing it would refuse
// the name outright while an older run of the same name waits to be taken up.
//
// Jobs stamped in the same second are ordered by the one written last, so a
// name used twice in a minute resumes the later of the two rather than either.
func (s *Store) LastUnfinished(ctx context.Context, name string) (UnfinishedJob, error) {
	var j UnfinishedJob
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, kind, target, unique_by, pages, spec_name, country, language, ports, threads, tries
		   FROM jobs
		  WHERE name = ? AND finished_at IS NULL AND plan_ready = 1
		  ORDER BY created_at DESC, id DESC
		  LIMIT 1`, name).
		Scan(&j.ID, &j.Spec.Name, &j.Spec.Kind, &j.Spec.Target, &j.Spec.UniqueBy, &j.Spec.Pages,
			&j.Spec.SpecName, &j.Spec.Country, &j.Spec.Language, &j.Spec.Ports, &j.Spec.Threads, &j.Spec.Tries)
	if errors.Is(err, sql.ErrNoRows) {
		return UnfinishedJob{}, fmt.Errorf("%w: %q", ErrNoUnfinishedJob, name)
	}
	if err != nil {
		return UnfinishedJob{}, fmt.Errorf("store: looking for an unfinished %q: %w", name, err)
	}
	return j, nil
}

// ErrJobFinished is returned when something a job has already finished is asked
// to change.
var ErrJobFinished = errors.New("store: this job has already finished")

// Reshape changes the pool a job will be run on.
//
// It changes nothing about the pool a job is running on this minute. The numbers
// are read when a pool is raised, so a job already under way keeps the one it
// has and takes the new size at the next raise — the caller has to say that, and
// this cannot say it for them.
//
// A finished job is refused. There is no next raise for it to take, so writing
// the numbers would leave a row nothing will ever read, and answering nothing at
// all is the same as answering "applied" to whoever asked.
//
// The reading and the write are one transaction because they are one decision:
// a job that finished between a check outside a transaction and the write after
// it would take a change this refuses, which is the exact case being refused.
func (s *Store) Reshape(ctx context.Context, jobID int64, ports, threads int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin reshaping job %d: %w", jobID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var finished bool
	err = tx.QueryRowContext(ctx,
		`SELECT finished_at IS NOT NULL FROM jobs WHERE id = ?`, jobID).Scan(&finished)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %d", ErrNoJob, jobID)
	case err != nil:
		return fmt.Errorf("store: reading job %d: %w", jobID, err)
	case finished:
		return fmt.Errorf("%w: %d", ErrJobFinished, jobID)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET ports = ?, threads = ? WHERE id = ?`,
		atLeastNone(ports), atLeastNone(threads), jobID); err != nil {
		return fmt.Errorf("store: reshaping job %d: %w", jobID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing the reshape of job %d: %w", jobID, err)
	}
	return nil
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
