// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/blanktrail/google-serp-parser/google"
)

// UniqueBy is what a job counts as a repeat.
//
// The three values are the three words the column will hold, and the database
// refuses a fourth. A job filed under a filter nothing here reads would keep
// every result while its page said otherwise.
type UniqueBy string

const (
	// UniqueOff keeps every result. It is the empty string because that is what
	// a job created before there was a choice already carries, and because a job
	// that did not ask for a filter must not pay for one.
	UniqueOff UniqueBy = ""
	// UniqueURL keeps the first result at each address.
	UniqueURL UniqueBy = "url"
	// UniqueHost keeps the first result from each site.
	UniqueHost UniqueBy = "host"
)

// keyOf is what a result is remembered by under this filter, and whether there
// is anything to remember it by.
//
// The key is canonical, so one page written three ways is one page: the same
// rule the engines compare addresses by, because a filter on spelling would
// drop nothing anybody meant and keep everything they did.
//
// A result whose address says nothing has no key and is kept. This filter drops
// what it can prove it has already let through, and dropping is irreversible;
// throwing results away because they could not be told apart is exactly the
// silent loss the whole arrangement exists to avoid.
func keyOf(by UniqueBy, r google.Result) (string, bool) {
	var key string
	switch by {
	case UniqueURL:
		key = google.CanonicalURL(r.URL)
	case UniqueHost:
		// The site is read off the address, and off the host field only when the
		// address carries none. The address is what Google answered with, and the
		// two are one thing said twice.
		key = google.CanonicalHost(r.URL)
		if key == "" {
			key = google.CanonicalHost(r.Host)
		}
	default:
		return "", false
	}
	return key, key != ""
}

// firstSeen writes down that a job has let a key through, and answers whether
// this was the first time.
//
// The insert is the answer. INSERT OR IGNORE reports by its row count whether
// the key was new, so nothing is looked up before it and nothing is counted
// after it: at ten million results, a read before every write would be a second
// walk of the tree for an answer the write already has.
//
// It runs inside the caller's transaction, which is what makes the mark and the
// result it stands for one act. A mark that outlived the write it belongs to
// would drop that address from the job for good, and no later run of the job
// could tell that it had happened.
func (s *Store) firstSeen(ctx context.Context, tx *sql.Tx, jobID int64, key string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO seen(job_id, key) VALUES(?, ?)`, jobID, key)
	if err != nil {
		return false, fmt.Errorf("store: recording what job %d has seen: %w", jobID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: reading whether job %d had seen that: %w", jobID, err)
	}
	return n == 1, nil
}

// DroppedCount is how many results a job threw away as repeats.
//
// It reads a count that was kept as the run went, because a dropped result
// leaves nothing behind to count afterwards. That is the price of dropping at
// the moment of writing, and it is the price the filter is for: the rows are
// never written, so the history is smaller by however many repeats there were.
func (s *Store) DroppedCount(ctx context.Context, jobID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT dropped FROM jobs WHERE id = ?`, jobID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: %d", ErrNoJob, jobID)
	}
	if err != nil {
		return 0, fmt.Errorf("store: reading what job %d dropped: %w", jobID, err)
	}
	return n, nil
}
