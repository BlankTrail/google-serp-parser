// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

// A completions job is not walked query by query the way a search is. One key
// is hundreds of questions — the key with each letter of its alphabet before
// it, after it, joined to it — and a thread that took a key and asked them all
// would leave a job of three keys on three threads while ninety-seven stood
// idle. So the unit of work is one question: the keys are expanded as they come
// off the queue, every thread takes whichever question is next, and a key is
// settled by the thread that answers its last one.
//
// The questions go through the pool's ports with the job's identity and no kept
// session — the user's choice for completions: the box answers a fresh visitor,
// and a session's cookies and rests would cost more than they buy here.

// suggestion is one key's questions in flight and what has come back.
type suggestion struct {
	began time.Time
	// left counts the questions not yet answered or given up on.
	left int
	// answered counts the ones that came back with a list, empty or not; a key
	// none of whose questions came back has failed, and one with any has not.
	answered int
	last     error
	seen     map[string]bool
	found    []string
}

// question is one variant of one key, waiting for a thread.
type question struct {
	at int
	v  google.SuggestVariant
}

// completions runs a completions job on the given threads and fills results,
// settling each key as its last question comes back.
func (r *Runner) completions(ctx context.Context, j Job, a *Attempt, results []QueryResult, threads int,
	settle func(ctx context.Context, thread, at int, began time.Time)) (starved bool) {
	var mu sync.Mutex
	keys := map[int]*suggestion{}
	questions := make(chan question)
	stop := make(chan struct{})
	var stopOnce sync.Once
	starve := func() { stopOnce.Do(func() { close(stop) }) }

	// The expander: one key at a time, its questions generated as they are
	// handed out rather than written anywhere first.
	go func() {
		defer close(questions)
		for at, q := range j.Queries {
			alphabet := google.SuggestAlphabet(q.Language)
			vs := google.SuggestVariants(q.Text, alphabet, j.Multiword)
			if j.SuggestLimit > 0 && len(vs) > j.SuggestLimit {
				vs = vs[:j.SuggestLimit]
			}
			if len(vs) == 0 {
				continue
			}
			mu.Lock()
			keys[at] = &suggestion{began: time.Now(), left: len(vs), seen: map[string]bool{}}
			results[at].Attempted = true
			mu.Unlock()
			for _, v := range vs {
				select {
				case questions <- question{at: at, v: v}:
				case <-ctx.Done():
					return
				case <-stop:
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < threads; w++ {
		wg.Add(1)
		go func(thread int) {
			defer wg.Done()
			for one := range questions {
				if ctx.Err() != nil {
					return
				}
				asked := time.Now()
				got, err := a.Complete(ctx, j.Queries[one.at], one.v)
				if errors.Is(err, blanktrail.ErrPoolExhausted) {
					// Nothing to ask through: the pool's condition, not the
					// key's. The key stays untried, as a search's would.
					starve()
					return
				}
				if ctx.Err() != nil {
					return
				}
				r.step(thread, StageAsk, asked, one.v.Text, err)

				mu.Lock()
				k := keys[one.at]
				k.left--
				if err != nil {
					k.last = err
				} else {
					k.answered++
					for _, s := range got {
						// The question handed back is not a completion of the
						// key; see google.Echoes.
						if google.Echoes(one.v, j.Queries[one.at].Language, s) {
							continue
						}
						if !k.seen[s] {
							k.seen[s] = true
							k.found = append(k.found, s)
						}
					}
				}
				finished := k.left == 0
				if finished {
					results[one.at].Pages, results[one.at].Err = k.settled(j.Queries[one.at].Text, j.Similar)
					delete(keys, one.at)
				}
				mu.Unlock()
				if finished {
					settle(ctx, thread, one.at, k.began)
				}
			}
		}(w)
	}
	wg.Wait()

	// A key whose questions were not all asked — the job stopped, or the pool
	// ran out — is left as it was found: untried, and asked whole by the next
	// run. Half its completions kept would be a key the history calls done.
	mu.Lock()
	for at := range keys {
		results[at].Attempted = false
		results[at].Pages, results[at].Err = nil, nil
	}
	mu.Unlock()
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// settled is what a key comes to once every question is in: its completions as
// one page of results, numbered in the order they first came back, and a
// failure only if not one question was answered.
func (k *suggestion) settled(key string, similar func(string, []string) []float32) ([]google.SERP, error) {
	if k.answered == 0 {
		return nil, fmt.Errorf("run: no completion question for %q was answered: %w", key, k.last)
	}
	// One call for the key's completions, so the key is turned into a vector
	// once. A scorer that answers with another number of scores than it was
	// asked for has scored something else, and nothing of it is kept: the
	// completions go unmeasured, as with no model, rather than each being given
	// a neighbour's score.
	var scores []float32
	if similar != nil {
		if got := similar(key, k.found); len(got) == len(k.found) {
			scores = got
		}
	}
	page := google.SERP{Query: key, Results: make([]google.Result, 0, len(k.found))}
	for i, s := range k.found {
		// Marked, not left out: what has nothing of its key in it is for an
		// export to set aside, and for a reader to see that it was.
		r := google.Result{Position: i + 1, Title: s, Offtopic: !google.Related(key, s)}
		// Scored is set beside the number, not read off it: a suggestion the
		// model put at nought was measured, and one with no model was not.
		if scores != nil {
			r.Similarity, r.Scored = scores[i], true
		}
		page.Results = append(page.Results, r)
	}
	return []google.SERP{page}, nil
}

// Complete asks one completion question, moving to another identity for as
// long as the answers are refusals — the same bound and the same accounting as
// a search: an answer read and judged a refusal costs the address, a request
// that never arrived costs nothing but the try.
func (a *Attempt) Complete(ctx context.Context, q google.Query, v google.SuggestVariant) ([]string, error) {
	tries := a.Tries
	if tries < 1 {
		tries = defaultTries
	}
	var last error
	for i := 0; i < tries; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		got, err := a.completeOnce(ctx, q, v)
		if err == nil {
			return got, nil
		}
		if ctx.Err() != nil || errors.Is(err, blanktrail.ErrPoolExhausted) {
			return nil, err
		}
		last = err
	}
	return nil, fmt.Errorf("%w after %d: %w", ErrNoIdentityLeft, tries, last)
}

func (a *Attempt) completeOnce(ctx context.Context, q google.Query, v google.SuggestVariant) ([]string, error) {
	lease, err := a.lease(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	if err := a.Brake.Hold(ctx); err != nil {
		return nil, err
	}
	client := lease.Client()
	s := &google.Suggester{Client: &http.Client{Transport: client.Transport, Timeout: client.Timeout}}
	got, err := s.Complete(ctx, q, v)
	if err == nil {
		lease.Answered()
		// Told as a page with nothing on it: the request is what the screen
		// counts a minute of, and the completions are counted once, when the
		// key is written.
		a.caught(google.SERP{Query: v.Text}, nil)
		return got, nil
	}
	if _, classified := google.ClassOf(err); classified {
		if rejErr := lease.Reject(ctx); rejErr != nil {
			return nil, fmt.Errorf("%w (reporting it also failed: %v)", err, rejErr)
		}
	}
	return nil, err
}
