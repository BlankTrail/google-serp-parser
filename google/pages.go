// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"errors"
	"fmt"
)

// Searcher is what pagination needs from a session: one page at a time.
//
// It is declared here rather than beside Session because this is where it is
// consumed, and because narrowing it to one method is what lets the walk be
// tested without a network and without a page of markup per case.
type Searcher interface {
	Search(ctx context.Context, q Query) (SERP, error)
}

// ErrBadDepth is returned when a walk is asked for fewer than one page.
var ErrBadDepth = errors.New("google: depth must be at least one page")

// SearchUntil walks pages 1..pages of one query, handing each to fn, and stops
// as soon as fn says it has what it came for.
//
// The callback is what keeps a lookup from paying for pages it does not need.
// A caller looking for one site usually finds it on the first page; taking the
// rest anyway spends a request each to learn nothing. Collecting every page
// first and searching afterwards reads more simply and costs exactly that.
//
// Two conditions end the walk regardless of fn: a page with no results at all,
// and a page shorter than the one before it, which is Google saying it has run
// out. Google thins out before a requested depth far more often than it fills
// it.
func SearchUntil(ctx context.Context, s Searcher, q Query, pages int, fn func(page int, serp SERP) bool) error {
	if pages < 1 {
		return fmt.Errorf("%w: got %d", ErrBadDepth, pages)
	}

	prev := -1
	for n := 1; n <= pages; n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		q.Page = n
		serp, err := s.Search(ctx, q)
		if err != nil {
			return fmt.Errorf("google: page %d of %q: %w", n, q.Text, err)
		}
		if len(serp.Results) == 0 {
			return nil
		}
		if fn != nil && fn(n, serp) {
			return nil
		}
		if prev >= 0 && len(serp.Results) < prev {
			return nil
		}
		prev = len(serp.Results)
	}
	return nil
}

// SearchDepth takes pages 1..pages of one query and returns them in order.
//
// A failure returns the pages already taken alongside the error. A position
// found on page one is still a position, and discarding it would mean asking
// for page one a second time.
func SearchDepth(ctx context.Context, s Searcher, q Query, pages int) ([]SERP, error) {
	var out []SERP
	err := SearchUntil(ctx, s, q, pages, func(_ int, serp SERP) bool {
		out = append(out, serp)
		return false
	})
	return out, err
}
