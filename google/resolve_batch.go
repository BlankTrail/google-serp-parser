// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
)

// ErrNoOrigin is returned when a relative link has no page origin to join to.
var ErrNoOrigin = errors.New("google: page origin is unknown, cannot resolve a relative link")

// ResolveReport is what a batch of resolutions did.
//
// Failed is separate from Resolved because a link that will not resolve leaves
// a usable result behind — one with an exact host and no address — and a caller
// deciding what to record needs to tell that from a result nobody tried.
//
// The three counters do not have to add up. A cancelled batch leaves
// Attempted-Resolved-Failed links untouched, and that remainder is worth
// knowing: it is work still available to a later run, not work that was lost.
type ResolveReport struct {
	Attempted int
	Resolved  int
	Failed    int
	Errs      []error
}

// ResolveAll fills in the addresses missing from a page, in place.
//
// It runs the resolutions concurrently because they are independent of each
// other and of the session that captured the page: measured, a link resolves
// from any client, with no cookies, hours later. That independence is what
// makes this worth doing as a batch rather than inline with the capture.
//
// Results that already carry an address are left alone. Under the direct link
// form that is every result on the page, and asking again would spend a request
// each to learn what the page already said.
func (r *Resolver) ResolveAll(ctx context.Context, serp *SERP, workers int) ResolveReport {
	if serp == nil {
		return ResolveReport{}
	}
	return r.ResolveResults(ctx, serp.Origin, serp.Results, workers)
}

// ResolveResults fills in the addresses missing from a slice of results, in
// place, joining relative links to origin.
func (r *Resolver) ResolveResults(ctx context.Context, origin string, rs []Result, workers int) ResolveReport {
	if workers < 1 {
		workers = 1
	}

	var todo []int
	for i := range rs {
		if !rs[i].Resolved() && rs[i].Link != "" {
			todo = append(todo, i)
		}
	}
	rep := ResolveReport{Attempted: len(todo)}
	if len(todo) == 0 {
		return rep
	}

	// Each index reaches exactly one worker, so a result is written by the one
	// goroutine that took it. The mutex is there for the report, which every
	// worker shares.
	var mu sync.Mutex
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				target, err := absoluteLink(origin, rs[i].Link)
				if err == nil {
					var got string
					got, err = r.Resolve(ctx, target)
					if err == nil {
						mu.Lock()
						rs[i].URL = got
						rep.Resolved++
						mu.Unlock()
						continue
					}
				}
				mu.Lock()
				rep.Failed++
				rep.Errs = append(rep.Errs, fmt.Errorf("result %d (%s): %w", i+1, rs[i].Host, err))
				mu.Unlock()
			}
		}()
	}
	for n, i := range todo {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			// The counters stay as the workers left them. Links nobody was
			// handed are not failures, and calling them that would put Failed
			// out of step with the errors kept beside it. The cancellation is
			// reported once, on its own, so the caller can tell a batch that
			// stopped early from one that ran out of work.
			rep.Errs = append(rep.Errs, fmt.Errorf("google: resolve batch stopped with %d of %d links untried: %w", len(todo)-n, len(todo), ctx.Err()))
			return rep
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	return rep
}

// absoluteLink joins a page-relative link to the origin the page came from.
//
// The origin matters and cannot be assumed: a capture carrying a country axis
// lands on that country's Google domain, so a link resolved against a
// hardcoded host would be sent to the wrong one.
func absoluteLink(origin, link string) (string, error) {
	u, err := url.Parse(link)
	if err != nil {
		return "", fmt.Errorf("google: unusable link %q: %w", link, err)
	}
	if u.IsAbs() {
		return link, nil
	}
	if origin == "" {
		return "", ErrNoOrigin
	}
	base, err := url.Parse(origin)
	if err != nil {
		return "", fmt.Errorf("google: unusable origin %q: %w", origin, err)
	}
	return base.ResolveReference(u).String(), nil
}
