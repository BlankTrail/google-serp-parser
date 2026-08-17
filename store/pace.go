// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"time"
)

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
// queries, and Settled is how many fell in it. A job that has settled fewer than
// two has no span to divide by, and says so with Known false rather than with a
// nought that reads as "stopped".
type Pace struct {
	Settled int
	Over    time.Duration
	Known   bool
}

// PerMinute is the speed, in queries a minute.
func (p Pace) PerMinute() float64 {
	if !p.Known || p.Over <= 0 {
		return 0
	}
	return float64(p.Settled) / p.Over.Minutes()
}

// Pace reads how fast a job is settling queries.
//
// The queries written before this program kept the moment carry none, and are
// passed over: a run resumed from such a history reports no speed until it has
// settled two of its own, which is the truth about what can be measured.
func (s *Store) Pace(ctx context.Context, jobID int64) (Pace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT settled_at
		   FROM queries
		  WHERE job_id = ? AND settled_at <> ''
		  ORDER BY settled_at DESC
		  LIMIT ?`, jobID, PaceSample)
	if err != nil {
		return Pace{}, fmt.Errorf("store: reading the pace of job %d: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	var moments []time.Time
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return Pace{}, fmt.Errorf("store: reading a settled moment: %w", err)
		}
		at, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			// A moment this program cannot read is one it did not write. It is
			// passed over rather than refused: a speed is worth less than a page
			// that draws, and the rest of the sample still measures something.
			continue
		}
		moments = append(moments, at)
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
	return Pace{Settled: len(moments) - 1, Over: span, Known: true}, nil
}
