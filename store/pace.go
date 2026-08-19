// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"time"
)

// PaceWindow is how far back a speed is allowed to look.
//
// It is here because a speed measured over a sample alone lies after a pause. A
// job stopped overnight and taken up again has a newest query from a minute ago
// and an oldest from yesterday, and dividing twenty by twenty-three hours
// reported nothing a minute for a run that was answering ten — which is what a
// screen said while the history beside it said otherwise.
//
// Three minutes: long enough to hold several queries of a run of any size,
// short enough that what it reports is what is happening rather than what was.
// A run that has settled nothing inside it has no speed to report, and says so
// rather than averaging across the gap.
const PaceWindow = 3 * time.Minute

// PaceSample is how many settled queries a speed is measured over.
//
// Few enough that the figure follows a job that speeds up or slows down, and
// enough that one slow query does not halve it. At the measured pace of this
// program — a query every few seconds per thread — twenty covers roughly the
// last minute of a small job and rather less of a wide one, which is the window
// somebody watching a screen is asking about.
const PaceSample = 20

// Pace is how fast a job is settling queries now.
//
// It is measured from the moments written down beside the settled queries
// themselves rather than by counting twice and subtracting: nothing between one
// page and the next remembers when the first count was taken, and a figure that
// depended on how often somebody reloaded would be a different number for every
// reader.
//
// Over is the span between the oldest and newest of the last PaceSample settled
// queries that fall inside PaceWindow, and Settled is how many fell in it. A job
// that has settled fewer than two of them has no span to divide by, and says so
// with Known false rather than with a nought that reads as "stopped".
type Pace struct {
	Settled int
	Over    time.Duration
	Known   bool

	// Pages is how many result pages those settled queries took between them.
	//
	// It is a second figure and not a replacement, because the two answer
	// different questions. A job taken to a hundred pages settles one query for
	// every hundred requests it makes, so a screen showing only queries reads as
	// a job that has nearly stopped while it is working as hard as it ever does.
	Pages int
}

// PerMinute is the speed, in queries a minute.
func (p Pace) PerMinute() float64 {
	if !p.Known || p.Over <= 0 {
		return 0
	}
	return float64(p.Settled) / p.Over.Minutes()
}

// PagesPerMinute is the speed in result pages a minute, which is the number of
// requests this program is actually making.
func (p Pace) PagesPerMinute() float64 {
	if !p.Known || p.Over <= 0 {
		return 0
	}
	return float64(p.Pages) / p.Over.Minutes()
}

// Pace reads how fast a job is settling queries.
//
// The queries written before this program kept the moment carry none, and are
// passed over: a run resumed from such a history reports no speed until it has
// settled two of its own, which is the truth about what can be measured.
func (s *Store) Pace(ctx context.Context, jobID int64) (Pace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT q.settled_at, (SELECT count(*) FROM pages p WHERE p.query_id = q.id)
		   FROM queries q
		  WHERE q.job_id = ? AND q.settled_at <> ''
		  ORDER BY q.settled_at DESC
		  LIMIT ?`, jobID, PaceSample)
	if err != nil {
		return Pace{}, fmt.Errorf("store: reading the pace of job %d: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	// Anything older than the window is left out. The rows arrive newest first,
	// so the first one too old ends the sample: everything behind it is older
	// still.
	cut := s.clock().Add(-PaceWindow)
	var moments []time.Time
	var pages []int
	for rows.Next() {
		var text string
		var took int
		if err := rows.Scan(&text, &took); err != nil {
			return Pace{}, fmt.Errorf("store: reading a settled moment: %w", err)
		}
		at, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			// A moment this program cannot read is one it did not write. It is
			// passed over rather than refused: a speed is worth less than a page
			// that draws, and the rest of the sample still measures something.
			continue
		}
		if at.Before(cut) {
			break
		}
		moments = append(moments, at)
		pages = append(pages, took)
	}
	if err := rows.Err(); err != nil {
		return Pace{}, fmt.Errorf("store: reading the pace of job %d: %w", jobID, err)
	}
	if len(moments) < 2 {
		return Pace{}, nil
	}

	// Newest first, so the last is the oldest. The span covers the gaps between
	// them, which is one fewer than the count: measuring the count over the span
	// would report a speed a fifth higher than the job has ever run at.
	span := moments[0].Sub(moments[len(moments)-1])
	if span <= 0 {
		return Pace{}, nil
	}
	// The oldest query's pages are left out for the same reason its settling is:
	// the span begins where that query ended, so the pages it took were taken
	// before the span this speed is measured over.
	took := 0
	for _, n := range pages[:len(pages)-1] {
		took += n
	}
	return Pace{Settled: len(moments) - 1, Over: span, Pages: took, Known: true}, nil
}
