// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
)

// ErrTooBusy is returned when as many searches are already under way as this
// server allows at once.
var ErrTooBusy = errors.New("api: too many searches at once")

// ErrTookTooLong is returned when the deadline came while the search was still
// going. It is not a refusal: nothing about the request was wrong, and the same
// one may well be answered next time.
var ErrTookTooLong = errors.New("api: the search did not finish in time")

// errNoSearcher is returned by a server that was given nothing to search with.
var errNoSearcher = errors.New("api: this server has nothing to search from")

// defaultSearchDeadline is how long one search may take when nobody says
// otherwise.
//
// Reaching an identity that answers has been measured at anything from half a
// minute to several, so a search with no bound at all can sit on a socket for
// as long as somebody else's program is prepared to hold it — and to that
// program a request that never comes back is indistinguishable from a program
// that has hung. A minute is long enough to be worth waiting through and short
// enough that a caller learns something either way.
const defaultSearchDeadline = time.Minute

// SearchConfig is what answering a search inside the request needs.
type SearchConfig struct {
	// Searcher takes one query to an identity and hands back what it read.
	// *run.Attempt is what satisfies it in the running program: a search that
	// moves to another identity when one is refused.
	//
	// It is named by what it does rather than by its type so a test can hold a
	// search half done without opening anything, which is the only way to ask
	// what happens when every place is taken. A server built without one serves
	// the history and searches nothing.
	Searcher google.Searcher
	// AtOnce is how many searches may be under way at the same time.
	// Non-positive means Ports.
	AtOnce int
	// Ports is how many identities stand behind Searcher, and it is what AtOnce
	// falls back to: a search competes for those identities with everything
	// else, so more searches at once than there are identities is not a limit.
	// Non-positive means startingPorts, the number a job is quoted against.
	Ports int
	// Deadline is how long one search may take before the caller is told it did
	// not finish. Non-positive means defaultSearchDeadline.
	Deadline time.Duration
}

// SearchLimit is how many searches may be under way at once.
//
// What it counts is searches holding, or about to hold, an identity: a place is
// taken before the search is asked for and given back after it has finished,
// however it finished. It does not count requests, and a request refused for
// want of a place never reaches an identity at all.
//
// The count is the number of values sitting in a buffered channel, and that
// channel is the whole of the state. Taking a place is a send that never waits;
// giving one back is a receive. There is no counter for two callers to read,
// add to and write back over each other, and nothing to lock: the channel is
// the guard, and a place is either in it or held by exactly one caller.
type SearchLimit struct {
	places chan struct{}
}

// NewSearchLimit builds a limiter with room for the given number of searches at
// once. Fewer than one is one: a server that answers no search at all is not
// what anybody configuring a limit meant to ask for.
func NewSearchLimit(places int) *SearchLimit {
	if places < 1 {
		places = 1
	}
	return &SearchLimit{places: make(chan struct{}, places)}
}

// Take holds a place and hands back the way to give it up.
//
// It refuses rather than waits, and that is the decision rather than an
// omission. A caller refused in a millisecond can retry, back off or go
// elsewhere; a caller held for a minute against a full limiter reaches its own
// timeout, gives up, and retries anyway — having spent the wait and the place
// it was finally handed on nobody.
//
// The release is safe to call more than once and gives back one place however
// often it is called. A second call that took a value out of the channel would
// be handing away a place another search is holding, which is the same failure
// as having no limiter and quieter to find.
func (l *SearchLimit) Take() (release func(), err error) {
	select {
	case l.places <- struct{}{}:
	default:
		return nil, ErrTooBusy
	}
	var once sync.Once
	return func() { once.Do(func() { <-l.places }) }, nil
}

// directSearch is everything a search inside the request is answered from.
//
// It goes to the identities directly rather than through the queue jobs wait
// in. That queue runs one job at a time on identities that answer better the
// longer they have been used, which is right for a job of ten thousand queries
// and impossible for a request somebody is holding a socket open for: the
// search would wait out the job in front of it. So it competes for the
// identities like any other consumer and is capped so that it cannot leave a
// running job with none.
type directSearch struct {
	finder   google.Searcher
	limit    *SearchLimit
	deadline time.Duration
}

// newDirectSearch settles what a search costs and how long it may take, once,
// before any request arrives.
func newDirectSearch(cfg SearchConfig) directSearch {
	places := cfg.AtOnce
	if places < 1 {
		places = cfg.Ports
	}
	if places < 1 {
		places = startingPorts
	}
	deadline := cfg.Deadline
	if deadline <= 0 {
		deadline = defaultSearchDeadline
	}
	return directSearch{
		finder:   cfg.Searcher,
		limit:    NewSearchLimit(places),
		deadline: deadline,
	}
}

// searchRoutes registers the search. It is behind the key check like everything
// else here, and it is the one address that spends identities on the strength
// of a single request.
func (s *Server) searchRoutes() {
	s.mux.HandleFunc("GET /api/v1/search", s.authed(s.search))
}

// searchJSON is one search as this interface hands it over.
type searchJSON struct {
	Query    string       `json:"query"`
	Country  string       `json:"country,omitempty"`
	Language string       `json:"language,omitempty"`
	Took     float64      `json:"took_seconds"`
	Results  []resultJSON `json:"results"`
	Related  []string     `json:"related,omitempty"`
}

// resultJSON is one organic result.
//
// Resolved is carried rather than left to be worked out from an empty address.
// Some results are read without their exact address, and a program that took
// the empty string for a fault would report a page this one read as a page it
// failed on.
type resultJSON struct {
	Position    int    `json:"position"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Host        string `json:"host"`
	DisplayPath string `json:"displayed_path"`
	Snippet     string `json:"snippet"`
	Resolved    bool   `json:"resolved"`
}

// search answers one search in the connection that asked for it.
//
// The answer carries the results themselves and not a number to come back for.
// A program calling this reads what it came for out of the reply it is already
// holding, and an answer of "started, ask again later" would be an endpoint
// nobody could use as one.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	asked := r.URL.Query()
	text := strings.TrimSpace(asked.Get("q"))
	if text == "" {
		writeError(w, http.StatusBadRequest,
			"the request names no query: ask with q=<what to search for>")
		return
	}
	q := google.Query{
		Text:     text,
		Country:  strings.TrimSpace(asked.Get("gl")),
		Language: strings.TrimSpace(asked.Get("hl")),
	}

	serp, took, err := s.searchNow(r.Context(), q)
	if err != nil {
		s.refuseSearch(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, searchAnswer(q, serp, took))
}

// searchNow takes one query to the identities under the cap and the deadline,
// and hands back how long it took, because a caller deciding whether to ask
// again this way or to set a job going has to compare something.
func (s *Server) searchNow(parent context.Context, q google.Query) (google.SERP, time.Duration, error) {
	if s.direct.finder == nil {
		return google.SERP{}, 0, errNoSearcher
	}

	// The place is taken before the search, because a place taken after it is a
	// count of searches that have already finished: every caller would reach the
	// identities first and be limited by nothing.
	release, err := s.direct.limit.Take()
	if err != nil {
		return google.SERP{}, 0, err
	}
	// And it is given back on the way out of this function whatever happens on
	// the way through it — an answer, a failure, or a panic unwinding past here.
	// A limiter that gives a place back only where the search worked loses one
	// per failure until it holds none, and nothing says so until nothing works.
	defer release()

	ctx, cancel := context.WithTimeout(parent, s.direct.deadline)
	defer cancel()

	began := time.Now()
	serp, err := s.direct.finder.Search(ctx, q)
	took := time.Since(began)
	if err == nil {
		return serp, took, nil
	}
	// The deadline is only this server's when the caller's own context is still
	// good. A caller who has hung up ended the search themselves, and reporting
	// that as this server running out of time would put a timeout in the log
	// every time somebody closed a connection.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil {
		return google.SERP{}, took, fmt.Errorf("%w after %s: %w", ErrTookTooLong, s.direct.deadline, err)
	}
	return google.SERP{}, took, err
}

// refuseSearch answers a search that produced nothing with the code it deserves.
//
// The code is what somebody else's program decides by. Too many at once, out of
// time, no identity free and an answer that could not be read are four
// different things to do next — retry now, retry the same request, come back
// later, and look at the query — and answering all four with a fault turns
// every one of them into a retry loop against a server with nothing wrong.
func (s *Server) refuseSearch(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrTooBusy):
		writeError(w, http.StatusTooManyRequests,
			"this server is already answering as many searches at once as it allows: try again")
	case errors.Is(err, ErrTookTooLong):
		writeError(w, http.StatusGatewayTimeout,
			"the search did not finish inside the time this server allows: it was not refused, and the same request may be answered next time")
	case errors.Is(err, errNoSearcher):
		writeError(w, http.StatusServiceUnavailable,
			"this server was started without identities and can search nothing")
	case errors.Is(err, blanktrail.ErrPoolExhausted):
		writeError(w, http.StatusServiceUnavailable,
			"no identity is able to answer at the moment: try again later")
	case errors.Is(err, google.ErrEmptyQuery):
		writeError(w, http.StatusBadRequest, "the request names no query")
	default:
		// The answer came back unusable, which is a failure between this server
		// and what it asked rather than a failure of this server's own records.
		s.log.Error("a search could not be answered", "path", r.URL.Path, "error", err)
		writeError(w, http.StatusBadGateway,
			"the search did not come back with an answer this server could read")
	}
}

// searchAnswer is the page as this interface hands it over.
func searchAnswer(q google.Query, serp google.SERP, took time.Duration) searchJSON {
	out := searchJSON{
		Query:    q.Text,
		Country:  q.Country,
		Language: q.Language,
		Took:     took.Seconds(),
		// A search that found nothing is an empty list and never nothing at all:
		// a caller walking the answer should not have to tell the two apart.
		Results: make([]resultJSON, 0, len(serp.Results)),
		Related: serp.Related,
	}
	for _, res := range serp.Results {
		out.Results = append(out.Results, resultJSON{
			Position:    res.Position,
			Title:       res.Title,
			URL:         res.URL,
			Host:        res.Host,
			DisplayPath: res.DisplayPath,
			Snippet:     res.Snippet,
			Resolved:    res.Resolved(),
		})
	}
	return out
}
