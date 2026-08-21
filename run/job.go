// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

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

// Kind is what a job asks of Google.
//
// It is a number here and a word in the history, and the one place that turns
// one into the other is the one place that has both. A package that runs work
// has no business holding the vocabulary a database column accepts.
type Kind int

const (
	// Parse takes each query to the depth the job asks for and reports the pages
	// that came back, whole. It is the zero value because it is the ordinary
	// thing this program does and always was.
	Parse Kind = iota
	// Index reads each line as an address and reports whether Google holds it.
	Index
	// Position takes each query to the depth the job asks for and reports the one
	// result that was the job's target, or none. It is a walk that stops at the
	// answer, so a site found on the first page costs one page and not the depth.
	Position
)

// Job is a list of queries and how deep to take each one.
type Job struct {
	// Kind is what to ask about each line. The zero value is Parse, which is
	// what a job that names nothing has always been.
	Kind Kind
	// Target is the site a Position job is about. It is meaningless under the
	// other kinds and required under that one: a position job carrying no target
	// has no question, and every query of it is refused rather than answered
	// "not found" — see google.ErrNoSite.
	Target string
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
	//
	// An index job ignores it: presence is settled by the first page, and taking
	// a second would spend a request to re-answer a question already answered.
	Pages int
	// Mobile says this job runs on phones. It travels with the job because it is
	// what the job was set up as, and it reaches the header that has to differ.
	Mobile bool

	// SpecName asks for ports opened under a named template, so a run that wants
	// mobile results is not quietly answered from a desktop one. Empty takes any
	// port.
	SpecName string
	// Tries is how many identities one query may be taken to before it is
	// recorded as failed. Non-positive means defaultTries.
	Tries int

	// Asking, when set, is told the address of each search as it goes out, from
	// whichever thread is making it. It is what a screen showing "what is this
	// job doing now" is drawn from, and it is a job's rather than a runner's
	// because it is one job somebody is watching.
	Asking func(url string)
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
	// Starved says the run stopped because the pool had no identity left to
	// give, rather than because it ran out of queries. What is left is untried
	// and still pending: a query nobody could ask is not a query that failed,
	// and writing it down as one spends the whole list in the minutes it takes
	// to walk it and leaves nothing to pick up again.
	Starved bool
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
	// Watch, when set, is told every stage a thread passes through and how long
	// it took, so a slow run can be taken apart second by second. Nil is a run
	// nobody is watching, which costs nothing.
	Watch Watch
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
	attempt := &Attempt{Pool: r.Pool, SpecName: j.SpecName, Tries: j.Tries, Mobile: j.Mobile,
		Asking: j.Asking}

	queue := make(chan int)
	// Closed once, by whichever thread first finds the pool empty. Every thread
	// watches it, and so does the hand-out below: with fifty threads reading one
	// queue, a stop that only stopped the thread that noticed would let the
	// other forty-nine walk the rest of the list at the speed of a refusal.
	starved := make(chan struct{})
	var starveOnce sync.Once
	var wg sync.WaitGroup
	for w := 0; w < threads; w++ {
		wg.Add(1)
		go func(thread int) {
			defer wg.Done()
			first := true
			for i := range queue {
				text := j.Queries[i].Text
				began := time.Now()
				if !first {
					// The pause this thread owes its own last request. Taking it
					// before the query rather than after it means a job never
					// ends on a pause it had no request to pace, and a cancelled
					// job leaves the query it was holding untried rather than
					// half done.
					paused := time.Now()
					if err := r.Pool.Sleep(ctx, r.Pool.NextDelay()); err != nil {
						return
					}
					r.step(thread, StagePause, paused, text, nil)
				}
				first = false

				results[i].Attempted = true
				asked := time.Now()
				results[i].Pages, results[i].Err = take(ctx, attempt, j, j.Queries[i], pages)
				if errors.Is(results[i].Err, blanktrail.ErrPoolExhausted) {
					// There was nothing to ask through. That is the pool's
					// condition and not this query's, so the query is left as it
					// was found — untried, and pending in whatever is writing the
					// history — and the run stops rather than spending the rest
					// of the list on an empty pool. Measured on a live run: a
					// service that went away turned 24 787 queries into failures
					// in the time it took to walk them, and a job with nothing
					// left pending is a job with nothing to resume.
					results[i].Attempted = false
					results[i].Err = nil
					starveOnce.Do(func() { close(starved) })
					return
				}
				r.step(thread, StageAsk, asked, text, results[i].Err)
				if r.Sink != nil {
					// The failures go to the sink as well as the successes.
					// A query whose failure was never written down is one a
					// job picked up again takes up again, for as long as it
					// keeps failing.
					wrote := time.Now()
					sinkErr := r.Sink.Record(ctx, results[i])
					r.step(thread, StageRecord, wrote, text, sinkErr)
					if sinkErr != nil && results[i].Err == nil {
						// A job whose results are not being written is not a
						// job that succeeded, whatever the walk returned.
						results[i].Err = fmt.Errorf("run: recording %q: %w", j.Queries[i].Text, sinkErr)
					}
				}
				r.step(thread, StageQuery, began, text, results[i].Err)
			}
		}(w)
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
		case <-starved:
			// The pool has nothing to give. Everything still in hand stays
			// untried.
			break sending
		default:
		}
		select {
		case <-ctx.Done():
			// Threads stop taking work once ctx ends, so without this the last
			// send would wait for a reader that is never coming.
			break sending
		case <-starved:
			// The same reason, for the same kind of reader that is never
			// coming. The check above cannot stand in for this one: it reads
			// starved at an instant, and what matters is the window after it.
			// Every thread can find the pool empty and return between that
			// check and this send — they all do, when the pool was empty before
			// the job began — and then nothing is left reading the queue, ctx
			// is not done, and a send with no way out waits for ever. The run
			// hangs where it was meant to stop and say it had starved.
			break sending
		case queue <- i:
		}
	}
	close(queue)
	wg.Wait()

	rep := Report{Results: results, Requests: r.Pool.Stats().Requests - before}
	select {
	case <-starved:
		rep.Starved = true
	default:
	}
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

// take runs one line the way the job asked for it.
//
// The three kinds come back in the same shape, which is what lets everything
// downstream — the sink, the history, the export — stay one path. What a check
// found is dressed as a page of results because that is what it is: the results
// that were the thing asked about. The difference between the kinds is the
// question, not the shape of the answer.
//
// A page holding nothing is how "the answer is no" is written down, and it is
// written down rather than skipped. A query left with no page at all is a query
// the next resume takes up again, so an address genuinely absent, or a site
// genuinely outside the depth, would be checked on every run for as long as that
// stayed true.
//
// Neither check re-asks the question the engines answer. The rule for what
// counts as the site — a bare host is any page of it, an address with a path is
// that address and nothing else — lives in the engines and is applied there, so
// there is one answer to "is this the site we were looking for" rather than one
// per caller.
func take(ctx context.Context, a *Attempt, j Job, q google.Query, pages int) ([]google.SERP, error) {
	switch j.Kind {
	case Index:
		st, err := google.CheckIndexed(ctx, a, q, q.Text)
		if err != nil {
			return nil, err
		}
		return []google.SERP{{Query: q.Text, Results: st.Sample}}, nil
	case Position:
		pos, err := google.FindPosition(ctx, a, q, j.Target, pages)
		if err != nil {
			return nil, err
		}
		return []google.SERP{{Query: q.Text, Results: found(pos)}}, nil
	default:
		return a.Walk(ctx, q, pages)
	}
}

// found is the one result a position check produced, carrying the place it
// stood, or nothing at all.
//
// The place is written into the result because the result is all that is kept,
// and a single result filed by its position among what was kept would say first
// about a site that stood seventh. The walk counted across its pages; the page
// the result came from numbered it within itself; only the first of those is the
// answer, and it is the one that travels.
func found(pos google.Position) []google.Result {
	if !pos.Found {
		return nil
	}
	r := pos.Result
	r.Position = pos.Rank
	return []google.Result{r}
}
