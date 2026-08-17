// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrPlanDone is returned when a plan that has already been finished or
// abandoned is written to again.
var ErrPlanDone = errors.New("store: this plan has already been finished or abandoned")

// planBatch is how many lines one transaction of an upload carries, and how
// many this holds at once.
//
// The number is a price paid at both ends and a thousand is where the two meet.
// Every transaction costs a commit, and a batch of ten pays that on every
// hundred-thousandth of a million-line file. A batch also holds this store's one
// connection for as long as it is open, and everything else the program is doing
// waits behind it — the screen the operator is watching, the job that is running
// and writing down its results — so a batch of the whole file stops the
// interface for the length of the upload.
const planBatch = 1000

// queryInsert writes one line of a plan. It is shared by the two paths that
// write a plan, because the ordinal in it is the meaning of the row: two copies
// of this statement are two chances for one of them to start letting the row
// order stand for the list order.
const queryInsert = `INSERT INTO queries(job_id, ordinal, text) VALUES(?, ?, ?)`

// Plan is a job whose list of queries is being written down as it arrives.
//
// It is for lists too large to hold: hundreds of thousands of phrases, or a
// file of addresses to check, arriving over a connection while it is read. No
// caller of this ever has the whole list, and neither does this — what it holds
// is one batch, and the batch is a thousand lines however long the file is.
//
// The guarantee CreateJob gives in one transaction is kept here by the flag
// instead. A job is written first and marked complete after its last line, and
// until that mark nothing will run it and nothing will carry it on. So a plan
// that broke off part way is a job that is visibly unusable rather than one that
// is quietly short, which is the failure worth designing against: a run that
// took a fraction of a list, finished it, and stamped the job done.
//
// A batch is gathered first and written afterwards, rather than a transaction
// being held open across the reading of the file. The reading is the slow part —
// it is a file arriving over a connection somebody else controls — and a
// transaction open across it would hold this store's one connection for the
// whole upload, which is the interface frozen for as long as the file lasts.
// What the two arrangements lose on a crash is the same: the batch that had not
// been committed.
//
// One goroutine writes a plan. It is the one reading the file, and there is no
// second thing to be told what is in a file being read.
type Plan struct {
	s  *Store
	id int64

	// batch is the lines waiting to be written, and never longer than planBatch.
	batch []string
	// written is how many lines are in the database, which is also the ordinal
	// the next line written will take.
	written int
	// done is set by Ready and by Abandon. It is what makes an abandon on the way
	// out of a handler that already finished into nothing at all.
	done bool
}

// OpenPlan writes a job down and hands back the list it is waiting for.
//
// The job is committed here, on its own, before a line of the list has arrived.
// Reading a file of a million lines takes minutes, and a job that appeared only
// with its first batch would be minutes in which the operator who started the
// upload has nothing to look at. It is written without the flag, so for those
// minutes it is a job that can be seen and cannot be run.
func (s *Store) OpenPlan(ctx context.Context, spec JobSpec) (*Plan, error) {
	pages := spec.Pages
	if pages < 1 {
		pages = 1
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs(name, created_at, kind, unique_by, pages, spec_name, country, language, plan_ready)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		spec.Name, time.Now().UTC().Format(time.RFC3339), spec.kind(), string(spec.UniqueBy), pages,
		spec.SpecName, spec.Country, spec.Language)
	if err != nil {
		return nil, fmt.Errorf("store: recording the job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("store: reading the job id: %w", err)
	}
	return &Plan{s: s, id: id, batch: make([]string, 0, planBatch)}, nil
}

// JobID is the job this plan is being written for.
func (p *Plan) JobID() int64 { return p.id }

// Count is how many lines this plan has taken.
//
// It is kept as they go by, because counting them any other way means having
// them all, and having them all is the one thing this type exists not to do.
func (p *Plan) Count() int { return p.written + len(p.batch) }

// Add takes one query at the end of the plan, and writes the batch it completes.
//
// The ordinal written with each line is that line's place in the list as it
// arrived. It is not left to the row order: row order in SQL is not a promise,
// and the number is what a run that is carried on later files its results
// against.
func (p *Plan) Add(ctx context.Context, text string) error {
	if p.done {
		return fmt.Errorf("%w: job %d", ErrPlanDone, p.id)
	}
	p.batch = append(p.batch, text)
	if len(p.batch) < planBatch {
		return nil
	}
	return p.write(ctx, false)
}

// Ready marks the list complete, and it is the moment the job becomes work.
//
// The last batch and the mark go in together. A mark committed on its own would
// leave an instant in which the job says its list is whole and the last thousand
// lines of it are not there yet, and a run beginning in that instant is exactly
// the half-finished run the flag exists to prevent.
func (p *Plan) Ready(ctx context.Context) error {
	if p.done {
		return fmt.Errorf("%w: job %d", ErrPlanDone, p.id)
	}
	if p.Count() == 0 {
		// A list with nothing in it is not a job. Marked complete it would be
		// picked up, found to have nothing left, and stamped done: a run that never
		// happened, filed as one that did.
		return fmt.Errorf("%w: job %d", ErrNoQueries, p.id)
	}
	if err := p.write(ctx, true); err != nil {
		return err
	}
	p.done = true
	return nil
}

// Abandon gives up an upload that stopped part way.
//
// It drops the batch that had not been written and touches the flag, the job and
// the batches already committed not at all. What is left is a job in the history
// that can be seen and cannot be run: whoever spent twenty minutes sending a file
// is owed something to look at, and a job that disappeared is twenty minutes with
// nothing to ask about.
//
// After Ready it does nothing, so a caller can abandon on its way out of every
// path without deciding which path it is on.
func (p *Plan) Abandon() error {
	if p.done {
		return nil
	}
	p.done = true
	p.batch = p.batch[:0]
	return nil
}

// write puts the batch in the database in one transaction, and with it, when
// this is the last batch, the mark that says the list is complete.
//
// The connection is taken here and given back before this returns. Between two
// batches this store is as free as it was before the upload started, which is
// what lets the operator watch a screen while a file of a million lines is being
// read.
// It is called with a full batch or with the last one, and never otherwise, so
// there is no empty write to guard against.
func (p *Plan) write(ctx context.Context, last bool) error {
	tx, err := p.s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin a batch of the plan of job %d: %w", p.id, err)
	}
	defer func() { _ = tx.Rollback() }()

	if len(p.batch) > 0 {
		stmt, err := tx.PrepareContext(ctx, queryInsert)
		if err != nil {
			return fmt.Errorf("store: preparing the query insert: %w", err)
		}
		defer func() { _ = stmt.Close() }()

		for i, text := range p.batch {
			if _, err := stmt.ExecContext(ctx, p.id, p.written+i, text); err != nil {
				return fmt.Errorf("store: recording query %d of job %d: %w", p.written+i, p.id, err)
			}
		}
	}
	if last {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET plan_ready = 1 WHERE id = ?`, p.id); err != nil {
			return fmt.Errorf("store: finishing the plan of job %d: %w", p.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing a batch of the plan of job %d: %w", p.id, err)
	}
	p.written += len(p.batch)
	p.batch = p.batch[:0]
	return nil
}
