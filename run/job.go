// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
)

// Sink is where a finished query is written down while the job is still
// running.
//
// It is declared here because this is where it is consumed, and narrowing it to
// one method is what keeps this package clear of storage: whatever writes the
// history knows about databases and files, and this does not.
//
// Record is called from every thread of a job, and by more than one of them at
// once. An implementation has to be safe for concurrent use.
type Sink interface {
	Record(ctx context.Context, res QueryResult) error
}

// ErrOrdinalsMismatch is returned when a job carries a numbering that does not
// line up with its queries.
var ErrOrdinalsMismatch = errors.New("run: Ordinals must be empty or as long as Queries")

// Job is a list of queries and how deep to take each one.
type Job struct {
	// Queries are taken in this order and reported in it.
	Queries []google.Query
	// Ordinals gives each query its place in the list the job originally had,
	// which matters only for a job picked up part way: it holds what is left,
	// and numbering that from zero would file every result against the wrong
	// query. Empty means these queries are the whole list and the ordinal is
	// the index.
	Ordinals []int
	// Pages is how many result pages each query is taken to. Non-positive means
	// one. A walk stops earlier when the page says the results have run out.
	Pages int
	// SpecName asks for ports opened under a named template, so a run that wants
	// mobile results is not quietly answered from a desktop one. Empty takes any
	// port.
	SpecName string
	// Tries is how many identities one query may be taken to before it is
	// recorded as failed. Non-positive means defaultTries.
	Tries int
}

// QueryResult is what one query produced.
type QueryResult struct {
	Query google.Query
	// Pages are the result pages in page order, as far as the walk got.
	Pages []google.SERP
	// Err is why this query produced nothing. Nil on success.
	Err error
	// Ordinal is the query's place in the original list, so a sink can file the
	// result without knowing how the job was assembled.
	Ordinal int
	// Attempted says whether this query was ever sent. A cancelled job leaves
	// queries nobody reached, and reporting those as failures would tell a
	// reader they were tried and lost, sending them to look for a fault in work
	// that never happened.
	Attempted bool
}

// Report is the outcome of one job.
type Report struct {
	// Results stand against the queries one for one, in the order the queries
	// were given. The order the threads finish in is the order of luck with the
	// addresses, and a report lined up with that would put every answer beside
	// the wrong query.
	Results []QueryResult
	// Done, Failed and Untried add up to the number of queries. Untried is the
	// work nobody reached, which is neither of the other two.
	Done    int
	Failed  int
	Untried int
	// Requests is how many times this job took an identity from the pool: one
	// for every query, however deep it was taken, and one more for every further
	// identity a refused page was carried to. It is the difference between two
	// readings of the pool's own count rather than a tally kept here, because a
	// tally would count the queries the runner handed out and miss the retries
	// underneath them. A caller running two jobs on one pool at once sees both
	// in it.
	Requests int64
	// Err is a refusal that belongs to the job rather than to any one query: a
	// plan that does not add up, and nothing was run.
	Err error
}

// Runner spreads a job over threads.
type Runner struct {
	// Pool leases the identities. Required.
	Pool *blanktrail.Pool
	// Threads is how many queries are taken at once. Non-positive means one.
	Threads int
	// Sink is handed every query as it finishes, so a job that ends badly still
	// leaves behind everything it had established. Nil keeps the results in the
	// report and nowhere else.
	Sink Sink
}

// Run works through a job and reports what came of every query.
//
// Cancelling stops handing out work and returns what was already established.
// A query that was in the air comes back attempted, carrying the reason it
// produced nothing; the queries behind it come back neither done nor failed.
//
// Each thread leaves a pause between the queries it takes, from the range the
// pool was configured with. The pages of one query follow one another: the walk
// is one query's, and breaking it up would leave the caller holding half a
// ranking for the length of a pause.
func (r *Runner) Run(ctx context.Context, j Job) Report {
	if len(j.Ordinals) != 0 && len(j.Ordinals) != len(j.Queries) {
		// Refused before anything is sent. A numbering that runs short files
		// results against whichever queries happen to line up, and the job
		// would have to be taken apart afterwards to find out which.
		rep := Report{Results: make([]QueryResult, len(j.Queries)), Untried: len(j.Queries), Err: ErrOrdinalsMismatch}
		for i, q := range j.Queries {
			rep.Results[i].Query = q
		}
		return rep
	}

	threads := r.Threads
	if threads < 1 {
		threads = 1
	}
	if threads > len(j.Queries) {
		threads = len(j.Queries)
	}
	pages := j.Pages
	if pages < 1 {
		pages = 1
	}

	before := r.Pool.Stats().Requests

	// Every index is handed to exactly one thread, so every element has exactly
	// one writer and the results need no lock. The slice is allocated whole and
	// never appended to, so its header never moves either.
	results := make([]QueryResult, len(j.Queries))
	for i, q := range j.Queries {
		results[i].Query = q
		results[i].Ordinal = i
		if len(j.Ordinals) != 0 {
			results[i].Ordinal = j.Ordinals[i]
		}
	}

	// One Attempt for the whole job. It keeps a session per port, and a port is
	// leased to one thread at a time, so the threads never meet inside it.
	attempt := &Attempt{Pool: r.Pool, SpecName: j.SpecName, Tries: j.Tries}

	queue := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < threads; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			first := true
			for i := range queue {
				if !first {
					// The pause this thread owes its own last request. Taking it
					// before the query rather than after it means a job never
					// ends on a pause it had no request to pace, and a cancelled
					// job leaves the query it was holding untried rather than
					// half done.
					if err := r.Pool.Sleep(ctx, r.Pool.NextDelay()); err != nil {
						return
					}
				}
				first = false

				results[i].Attempted = true
				results[i].Pages, results[i].Err = attempt.Walk(ctx, j.Queries[i], pages)
				if r.Sink != nil {
					// The failures go to the sink as well as the successes.
					// A query whose failure was never written down is one a
					// job picked up again takes up again, for as long as it
					// keeps failing.
					if sinkErr := r.Sink.Record(ctx, results[i]); sinkErr != nil && results[i].Err == nil {
						// A job whose results are not being written is not a
						// job that succeeded, whatever the walk returned.
						results[i].Err = fmt.Errorf("run: recording %q: %w", j.Queries[i].Text, sinkErr)
					}
				}
			}
		}()
	}

sending:
	for i := range j.Queries {
		if ctx.Err() != nil {
			// A cancelled job hands out nothing further. The select below alone
			// would still pass work to a thread that happened to be free, and
			// the report would then blame the cancellation on queries it had
			// only just sent.
			break sending
		}
		select {
		case <-ctx.Done():
			// Threads stop taking work once ctx ends, so without this the last
			// send would wait for a reader that is never coming.
			break sending
		case queue <- i:
		}
	}
	close(queue)
	wg.Wait()

	rep := Report{Results: results, Requests: r.Pool.Stats().Requests - before}
	for i := range results {
		switch {
		case !results[i].Attempted:
			rep.Untried++
		case results[i].Err != nil:
			rep.Failed++
		default:
			rep.Done++
		}
	}
	return rep
}
