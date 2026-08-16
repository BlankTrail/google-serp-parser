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

// offsetPerPage is the stride between one page's start offset and the next. It
// has to agree with the offset a Query renders, because the pagination bar's
// links are expressed on that scale and the walk compares its own position
// against them.
const offsetPerPage = 10

// SearchUntil walks pages 1..pages of one query, handing each to fn, and stops
// as soon as fn says it has what it came for.
//
// The callback is what keeps a lookup from paying for pages it does not need.
// A caller looking for one site usually finds it on the first page; taking the
// rest anyway spends a request each to learn nothing. Collecting every page
// first and searching afterwards reads more simply and costs exactly that.
//
// Two conditions end the walk regardless of fn: a page with no results at all,
// and a pagination bar that offers nothing past the page in hand. The second is
// read from the page rather than inferred from how full it looked. A walk that
// stopped on a page shorter than the one before it stopped on an ordinary blip
// and reported sites further down as not ranking at all, which is a wrong
// answer that reads like a right one.
//
// A page whose bar this parser did not find is walked past rather than stopped
// at: absence of the signal is not the signal — see SERP.HasPagination.
func SearchUntil(ctx context.Context, s Searcher, q Query, pages int, fn func(page int, serp SERP) bool) error {
	if pages < 1 {
		return fmt.Errorf("%w: got %d", ErrBadDepth, pages)
	}

	for n := 1; n <= pages; n++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("google: page %d of %q: %w", n, q.Text, err)
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
		if serp.HasPagination && serp.MaxOffset <= (n-1)*offsetPerPage {
			return nil
		}
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
