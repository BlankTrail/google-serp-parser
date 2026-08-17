// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
)

// The last few of what a job has gathered, newest first.
//
// A page cannot draw a job of ten million results, and it should not try: what
// somebody watching a run wants is proof that results are still arriving and a
// look at what they are. The whole lot is what the export is for, and it is the
// only thing that can carry it.
//
// Newest first, and by the order the rows were written rather than by the order
// the queries were listed. Those two agree at the start of a run and part
// company the moment more than one thread is going, and it is arrival that the
// reader is asking about — "is it still working" is a question about now.
//
// The limit is applied by the database. Read whole and trimmed afterwards, a
// job of ten million rows would be ten million rows read to show twenty.

// LatestRows is the last results a job captured, newest first.
func (s *Store) LatestRows(ctx context.Context, jobID int64, limit int) ([]Row, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT q.ordinal, q.text, p.number, r.rank, r.title, r.url, r.host,
		        r.snippet, r.link, r.display_path
		   FROM results r
		   JOIN pages   p ON p.id = r.page_id
		   JOIN queries q ON q.id = p.query_id
		  WHERE q.job_id = ?
		  ORDER BY r.id DESC
		  LIMIT ?`, jobID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: reading the last results of job %d: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Ordinal, &r.Query, &r.Page, &r.Rank,
			&r.Title, &r.URL, &r.Host, &r.Snippet, &r.Link, &r.DisplayPath); err != nil {
			return nil, fmt.Errorf("store: reading a result: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading the last results of job %d: %w", jobID, err)
	}
	return out, nil
}

// LatestVerdicts is the last addresses an index job settled, newest first.
//
// Ordered by when the query settled rather than by where it stood in the list,
// for the reason the results are: the reader is asking what has just happened.
// A query settled before this program kept the moment sorts by its ordinal
// instead, which is the order it would have had anyway.
func (s *Store) LatestVerdicts(ctx context.Context, jobID int64, limit int) ([]Verdict, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT q.ordinal, q.text, count(r.id)
		   FROM queries q
		   LEFT JOIN pages   p ON p.query_id = q.id
		   LEFT JOIN results r ON r.page_id  = p.id
		  WHERE q.job_id = ? AND q.state = 'done'
		  GROUP BY q.id
		  ORDER BY q.settled_at DESC, q.ordinal DESC
		  LIMIT ?`, jobID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: reading the last verdicts of job %d: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Verdict
	for rows.Next() {
		var v Verdict
		var found int
		if err := rows.Scan(&v.Ordinal, &v.Target, &found); err != nil {
			return nil, fmt.Errorf("store: reading a verdict: %w", err)
		}
		v.Held = found > 0
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading the last verdicts of job %d: %w", jobID, err)
	}
	return out, nil
}

// LatestStandings is the last phrases a position check settled, newest first.
func (s *Store) LatestStandings(ctx context.Context, jobID int64, limit int) ([]Standing, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT q.ordinal, q.text, coalesce(min(r.rank), 0)
		   FROM queries q
		   LEFT JOIN pages   p ON p.query_id = q.id
		   LEFT JOIN results r ON r.page_id  = p.id
		  WHERE q.job_id = ? AND q.state = 'done'
		  GROUP BY q.id
		  ORDER BY q.settled_at DESC, q.ordinal DESC
		  LIMIT ?`, jobID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: reading the last standings of job %d: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Standing
	for rows.Next() {
		var st Standing
		if err := rows.Scan(&st.Ordinal, &st.Query, &st.Rank); err != nil {
			return nil, fmt.Errorf("store: reading a standing: %w", err)
		}
		st.Found = st.Rank > 0
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading the last standings of job %d: %w", jobID, err)
	}
	return out, nil
}
