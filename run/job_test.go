// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
)

// usQueries builds a numbered list, so a result can be traced back to the query
// that produced it.
func usQueries(n int) []google.Query {
	qs := make([]google.Query, n)
	for i := range qs {
		qs[i] = usQuery(fmt.Sprintf("q%02d", i))
	}
	return qs
}

// recordingSink stands in for whatever writes the history down. It is called
// from every thread of a job, so it holds a lock like any real one must.
//
// It also notes how much of the job the origin had served by the time each
// result reached it: a runner that kept everything and handed it over once the
// job was over would otherwise look exactly the same from out here.
type recordingSink struct {
	// served is the origin's search count, read at the moment of each call.
	served *atomic.Int64
	// fail is what this sink answers instead of writing anything down.
	fail error

	mu     sync.Mutex
	got    []QueryResult
	during []int64
}

func (s *recordingSink) Record(_ context.Context, res QueryResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.served != nil {
		s.during = append(s.during, s.served.Load())
	}
	if s.fail != nil {
		return s.fail
	}
	s.got = append(s.got, res)
	return nil
}

func (s *recordingSink) records() []QueryResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

func (s *recordingSink) ordinals() []int {
	out := make([]int, 0, len(s.got))
	for _, r := range s.records() {
		out = append(out, r.Ordinal)
	}
	slices.Sort(out)
	return out
}

// servedWhenRecording is how far along the job was at each call, in call order.
func (s *recordingSink) servedWhenRecording() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.during)
}

func TestRunner_HandsEveryQueryToTheSinkBeforeTheJobIsOver(t *testing.T) {
	// The point of a sink is that work is safe before the job ends. Collecting
	// it and handing it over at the end would lose everything on a crash,
	// which is the case the sink exists for.
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 4)

	sink := &recordingSink{served: &o.searches}
	queries := usQueries(3)
	r := &Runner{Pool: f.Pool, Threads: 2, Sink: sink}
	rep := r.Run(context.Background(), Job{Queries: queries, Pages: 1})

	if rep.Done != len(queries) {
		t.Fatalf("Done=%d, want %d (failed %d, untried %d)", rep.Done, len(queries), rep.Failed, rep.Untried)
	}
	if got := sink.ordinals(); !slices.Equal(got, []int{0, 1, 2}) {
		t.Errorf("the sink saw ordinals %v, want 0, 1 and 2", got)
	}
	// Two threads carry the first two queries, and the third is only handed out
	// once one of them comes back for it. So a sink called as its query lands
	// is called for the first time with a search still to come, and a sink
	// handed the whole job at the end sees every search already served.
	during := sink.servedWhenRecording()
	if len(during) == 0 || slices.Min(during) >= int64(len(queries)) {
		t.Errorf("the sink was called with %v of %d searches served, want the first one to arrive while the job was still running",
			during, len(queries))
	}
}

func TestRunner_TellsTheWatcherTheAddressOfEachSearchAsItGoesOut(t *testing.T) {
	// What a screen watching a run is drawn from. Without it the only honest
	// thing a page can say about a job between two settled queries is a row of
	// numbers that have not moved, and a job taken a hundred pages deep settles
	// one query for every hundred requests it makes.
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2)

	var mu sync.Mutex
	var seen []string
	r := &Runner{Pool: f.Pool, Threads: 1, Sink: &recordingSink{}}
	rep := r.Run(context.Background(), Job{
		Queries: []google.Query{usQuery("golang channels")},
		Pages:   1,
		Asking: func(url string) {
			mu.Lock()
			seen = append(seen, url)
			mu.Unlock()
		},
	})
	if rep.Err != nil {
		t.Fatalf("the job: %v", rep.Err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the watcher was told nothing, so a screen has nothing to show")
	}
	last := seen[len(seen)-1]
	if !strings.Contains(last, "q=golang+channels") {
		t.Errorf("the watcher was told %q, which is not the address of the query it ran", last)
	}
}

func TestRunner_CarriesTheOriginalNumberingThroughAJobPickedUpPartWay(t *testing.T) {
	// A resumed job holds only what is left. Numbering those from zero would
	// file every result against the wrong query.
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 4)

	sink := &recordingSink{}
	r := &Runner{Pool: f.Pool, Threads: 2, Sink: sink}
	rep := r.Run(context.Background(), Job{
		Queries:  []google.Query{usQuery("g"), usQuery("h")},
		Ordinals: []int{7, 9},
		Pages:    1,
	})

	if rep.Done != 2 {
		t.Fatalf("Done=%d, want 2 (failed %d, untried %d)", rep.Done, rep.Failed, rep.Untried)
	}
	if got := sink.ordinals(); !slices.Equal(got, []int{7, 9}) {
		t.Errorf("the sink saw ordinals %v, want the job's own 7 and 9", got)
	}
}

func TestRunner_RefusesNumberingThatDoesNotLineUpWithTheQueries(t *testing.T) {
	// A shorter list would file results against whichever queries happened to
	// line up, quietly and wrongly.
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2)

	sink := &recordingSink{}
	r := &Runner{Pool: f.Pool, Threads: 1, Sink: sink}
	rep := r.Run(context.Background(), Job{
		Queries:  usQueries(2),
		Ordinals: []int{3},
		Pages:    1,
	})

	if !errors.Is(rep.Err, ErrOrdinalsMismatch) {
		t.Errorf("Report.Err=%v, want ErrOrdinalsMismatch", rep.Err)
	}
	if rep.Done != 0 || rep.Failed != 0 || rep.Untried != 2 {
		t.Errorf("Done=%d Failed=%d Untried=%d, want nothing run", rep.Done, rep.Failed, rep.Untried)
	}
	// The refusal is worth having only if it comes before the work does.
	if got := o.searches.Load(); got != 0 {
		t.Errorf("%d searches, want none - the job ran before its numbering was checked", got)
	}
	if got := sink.records(); len(got) != 0 {
		t.Errorf("the sink was handed %d results by a job that was refused", len(got))
	}
}

func TestRunner_HandsAQueryThatFailedToTheSinkWithItsReason(t *testing.T) {
	// A failure nobody wrote down is a query that a job picked up again takes
	// up again, and again, for as long as it keeps failing.
	o := newOrigin(t, func(*http.Request, int) string { return shellBody })
	f := poolFacing(t, o.addr(), 3)

	sink := &recordingSink{}
	r := &Runner{Pool: f.Pool, Threads: 1, Sink: sink}
	rep := r.Run(context.Background(), Job{Queries: usQueries(1), Pages: 1, Tries: 2})

	if rep.Failed != 1 {
		t.Fatalf("Failed=%d, want 1 (done %d, untried %d)", rep.Failed, rep.Done, rep.Untried)
	}
	got := sink.records()
	if len(got) != 1 {
		t.Fatalf("the sink saw %d results, want the failed query among them", len(got))
	}
	if got[0].Err == nil {
		t.Error("the sink was handed the failed query as though it had produced results")
	}
}

func TestRunner_TreatsAResultTheSinkRefusedAsAFailedQuery(t *testing.T) {
	// A sink that cannot write is a job that is not being saved. Carrying on
	// silently would produce a run whose results exist only on screen.
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 3)

	sink := &recordingSink{fail: errors.New("disk is full")}
	r := &Runner{Pool: f.Pool, Threads: 1, Sink: sink}
	rep := r.Run(context.Background(), Job{Queries: usQueries(1), Pages: 1})

	if rep.Failed != 1 {
		t.Fatalf("Failed=%d, want 1 (done %d, untried %d)", rep.Failed, rep.Done, rep.Untried)
	}
	if rep.Results[0].Err == nil {
		t.Fatal("a query whose result was never written down is reported as done")
	}
	if !strings.Contains(rep.Results[0].Err.Error(), "disk is full") {
		t.Errorf("the recorded reason is %v, want it to name what the sink said", rep.Results[0].Err)
	}
}

func TestRunner_ReportsTheAnswersInTheOrderTheQueriesWereGiven(t *testing.T) {
	// A user who handed over a list expects the answers against it. The order
	// the threads finish in is the order of luck with the addresses, and lining
	// a report up with that puts every answer beside the wrong query.
	o := newOrigin(t, func(r *http.Request, _ int) string {
		return serpBody(r.URL.Query().Get("q") + ".test")
	})
	f := poolFacing(t, o.addr(), 4)

	queries := usQueries(12)
	r := &Runner{Pool: f.Pool, Threads: 4}
	rep := r.Run(context.Background(), Job{Queries: queries, Pages: 1})

	if rep.Done != len(queries) {
		t.Fatalf("Done=%d, want %d (failed %d, untried %d)", rep.Done, len(queries), rep.Failed, rep.Untried)
	}
	for i, q := range queries {
		res := rep.Results[i]
		if res.Query.Text != q.Text {
			t.Fatalf("Results[%d] stands against %q, want %q", i, res.Query.Text, q.Text)
		}
		if len(res.Pages) != 1 || len(res.Pages[0].Results) != 1 {
			t.Fatalf("Results[%d] carries no page to check", i)
		}
		if got, want := res.Pages[0].Results[0].Host, q.Text+".test"; got != want {
			t.Errorf("Results[%d] holds the answer to %q", i, strings.TrimSuffix(got, ".test"))
		}
	}
}

func TestRunner_LeavesAQueryNobodyReachedNeitherDoneNorFailed(t *testing.T) {
	// Counting untried work as failed tells a reader it was attempted and lost.
	// They then go looking for a fault in queries that were never sent.
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	queries := usQueries(10)
	r := &Runner{Pool: f.Pool, Threads: 2}
	rep := r.Run(ctx, Job{Queries: queries, Pages: 1, Tries: 1})

	if rep.Untried != len(queries) {
		t.Errorf("Untried=%d, want %d - nothing was sent after the caller had gone", rep.Untried, len(queries))
	}
	if rep.Failed != 0 || rep.Done != 0 {
		t.Errorf("Done=%d Failed=%d, want none of either", rep.Done, rep.Failed)
	}
	if rep.Done+rep.Failed+rep.Untried != len(queries) {
		t.Errorf("Done=%d Failed=%d Untried=%d, want them to add up to %d",
			rep.Done, rep.Failed, rep.Untried, len(queries))
	}
	for i, res := range rep.Results {
		if !res.Attempted && res.Err != nil {
			t.Errorf("Results[%d] was never attempted but carries an error", i)
		}
		if res.Query.Text != queries[i].Text {
			t.Errorf("Results[%d] stands against %q, want %q", i, res.Query.Text, queries[i].Text)
		}
	}
	if got := o.searches.Load(); got != 0 {
		t.Errorf("%d searches, want none", got)
	}
}

func TestRunner_CountsAQueryCutOffMidFlightAsAttempted(t *testing.T) {
	// A query that was in the air when the caller cancelled did produce nothing,
	// and it says why. The queries behind it were never sent, and claiming
	// otherwise would put a fault on work that never happened.
	started := make(chan struct{})
	var once sync.Once
	o := newOrigin(t, func(r *http.Request, _ int) string {
		once.Do(func() { close(started) })
		<-r.Context().Done()
		return shellBody
	})
	f := poolFacing(t, o.addr(), 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	queries := usQueries(3)
	r := &Runner{Pool: f.Pool, Threads: 1}
	rep := r.Run(ctx, Job{Queries: queries, Pages: 1, Tries: 1})

	if !rep.Results[0].Attempted {
		t.Error("the query that was in the air is reported as never sent")
	}
	if rep.Results[0].Err == nil {
		t.Error("the query that was cut off is reported as done")
	}
	if rep.Untried < 1 {
		t.Errorf("Untried=%d, want the queries behind it to be left untried", rep.Untried)
	}
	if rep.Done != 0 {
		t.Errorf("Done=%d, want none", rep.Done)
	}
	if rep.Done+rep.Failed+rep.Untried != len(queries) {
		t.Errorf("Done=%d Failed=%d Untried=%d, want them to add up to %d",
			rep.Done, rep.Failed, rep.Untried, len(queries))
	}
}

func TestRunner_PausesBetweenTheRequestsOfOneThread(t *testing.T) {
	// The pool derives the pause but does not apply it: it cannot know when the
	// caller is about to ask again. The runner is that caller, and without this
	// the delay a user configured does nothing at all.
	const pace = 90 * time.Second
	var paused atomic.Int64

	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2, func(c *blanktrail.PoolConfig) {
		c.DelayMin, c.DelayMax = pace, pace
		c.Sleep = func(ctx context.Context, d time.Duration) error {
			if d == pace {
				paused.Add(1)
			}
			return ctx.Err()
		}
	})

	queries := usQueries(4)
	r := &Runner{Pool: f.Pool, Threads: 1}
	rep := r.Run(context.Background(), Job{Queries: queries, Pages: 1})

	if rep.Done != len(queries) {
		t.Fatalf("Done=%d, want %d", rep.Done, len(queries))
	}
	if got, want := paused.Load(), int64(len(queries)-1); got != want {
		t.Errorf("%d pauses over %d queries on one thread, want %d - one between each pair",
			got, len(queries), want)
	}
}

func TestRunner_GivesUpOnWorkNobodyIsLeftToTakeWhenACancellationLandsInAPause(t *testing.T) {
	// A thread cancelled while it is pacing itself stops without taking the
	// query it was about to. Nothing else is coming to take it either, so a job
	// that only watched its threads would wait on a hand that has already gone.
	const pace = 90 * time.Second
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	paused := make(chan struct{})
	var once sync.Once
	f := poolFacing(t, o.addr(), 1, func(c *blanktrail.PoolConfig) {
		c.DelayMin, c.DelayMax = pace, pace
		c.Sleep = func(sctx context.Context, d time.Duration) error {
			if d == pace {
				once.Do(func() { close(paused) })
				<-sctx.Done()
			}
			return sctx.Err()
		}
	})
	go func() {
		<-paused
		cancel()
	}()

	queries := usQueries(3)
	r := &Runner{Pool: f.Pool, Threads: 1}

	// A job that waits on a thread that has gone never returns at all, so the
	// report is collected with a deadline. It is there only to make that
	// outcome visible.
	reports := make(chan Report, 1)
	go func() { reports <- r.Run(ctx, Job{Queries: queries, Pages: 1}) }()

	select {
	case rep := <-reports:
		if rep.Done != 1 {
			t.Errorf("Done=%d, want the 1 query that got through before the pause", rep.Done)
		}
		if rep.Untried != 2 {
			t.Errorf("Untried=%d, want 2 - neither the query left in the pause nor the one behind it was sent",
				rep.Untried)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the job never gave up on work no thread was left to take")
	}
}

func TestRunner_TakesANonPositiveThreadAndPageCountAsOne(t *testing.T) {
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2)

	// A thread count that is not clamped shows up as a job that never comes
	// back, so this one is bounded: it has to end by finishing the query, and
	// the deadline is there only to make the other outcome visible.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r := &Runner{Pool: f.Pool, Threads: 0}
	rep := r.Run(ctx, Job{Queries: usQueries(1)})

	if rep.Done != 1 {
		t.Fatalf("Done=%d, want 1 - a job with no threads to run on hands out work nobody takes (%v)",
			rep.Done, rep.Results[0].Err)
	}
	if n := len(rep.Results[0].Pages); n != 1 {
		t.Errorf("collected %d pages, want the one page a job that named no depth is worth", n)
	}
}

func TestRunner_FinishesAJobThatHasNoQueries(t *testing.T) {
	o := newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
	f := poolFacing(t, o.addr(), 2)

	r := &Runner{Pool: f.Pool, Threads: 4}
	rep := r.Run(context.Background(), Job{Pages: 1})

	if len(rep.Results) != 0 || rep.Done+rep.Failed+rep.Untried != 0 {
		t.Errorf("an empty job reported %d results (done %d, failed %d, untried %d)",
			len(rep.Results), rep.Done, rep.Failed, rep.Untried)
	}
}

func TestRunner_RecordsWhyAQueryFailedRatherThanOnlyThatItDid(t *testing.T) {
	// "Failed" on its own sends a reader to the query. The reason is what tells
	// them whether the query is unanswerable or the answers were unusable.
	o := newOrigin(t, func(*http.Request, int) string { return shellBody })
	f := poolFacing(t, o.addr(), 3)

	r := &Runner{Pool: f.Pool, Threads: 1}
	rep := r.Run(context.Background(), Job{Queries: usQueries(1), Pages: 1, Tries: 2})

	if rep.Failed != 1 {
		t.Fatalf("Failed=%d, want 1", rep.Failed)
	}
	if !rep.Results[0].Attempted {
		t.Error("a query that was sent is reported as never attempted")
	}
	if rep.Results[0].Err == nil {
		t.Fatal("a failed query carries no reason")
	}
	if !strings.Contains(rep.Results[0].Err.Error(), string(google.ClassShell)) {
		t.Errorf("the recorded reason %q does not say what came back", rep.Results[0].Err)
	}
}

func TestRunner_AsksForEveryPageTheJobRequestedOnOneIdentity(t *testing.T) {
	o := newOrigin(t, func(*http.Request, int) string { return serpBodyWithBar("example.com") })
	// Three ports, so a walk that changed identity between pages has somewhere to
	// change to and this test can see it happen.
	f := poolFacing(t, o.addr(), 3)

	r := &Runner{Pool: f.Pool, Threads: 1}
	rep := r.Run(context.Background(), Job{Queries: usQueries(1), Pages: 3})

	if rep.Done != 1 {
		t.Fatalf("Done=%d, want 1 (failed %d: %v)", rep.Done, rep.Failed, rep.Results[0].Err)
	}
	if n := len(rep.Results[0].Pages); n != 3 {
		t.Errorf("collected %d pages, want the 3 the job asked for", n)
	}
	if got := o.searches.Load(); got != 3 {
		t.Errorf("%d searches, want 3", got)
	}
	// One identity for the three pages: it visits the front page once, and the
	// job takes one identity rather than one per page.
	if got := o.homes.Load(); got != 1 {
		t.Errorf("%d visits to the front page, want 1 - the walk changed identity between pages", got)
	}
	if got := f.portsUsed(); got != 1 {
		t.Errorf("%d identities carried the walk, want 1", got)
	}
	if rep.Requests != 1 {
		t.Errorf("Requests=%d, want the 1 identity an unrefused walk costs", rep.Requests)
	}
}

// serpBodyOf builds a result page from a list of addresses, so an index case
// can be answered the way a site: query really is answered: with the address
// asked about among other people's.
func serpBodyOf(urls ...string) string {
	body := `<!doctype html><html><body><div id="search">`
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			panic("a case named an address that will not parse: " + raw)
		}
		body += `<div data-snc="x"><a href="` + raw + `" data-ved="2"><h3>Title</h3></a>` +
			`<cite>` + u.Host + `</cite></div>`
	}
	return body + `</div></body></html>`
}

func TestRunner_AnIndexJobKeepsOnlyWhatWasTheAddressAskedAbout(t *testing.T) {
	// The page answers with a stranger, the address itself, and another page of
	// the same site. Only the middle one is the answer to "is this address in
	// the index", and only it belongs in the history: what is recorded here is
	// what the verdict is later read off.
	var asked atomic.Value
	o := newOrigin(t, func(r *http.Request, _ int) string {
		asked.Store(r.URL.Query().Get("q"))
		return serpBodyOf("https://elsewhere.test/x",
			"https://example.com/page", "https://example.com/other")
	})
	f := poolFacing(t, o.addr(), 2)

	r := &Runner{Pool: f.Pool, Threads: 1}
	// The depth is set past one to pin that an index job ignores it: presence is
	// settled by the first page, and a second would be a request spent to
	// re-answer a question already answered.
	rep := r.Run(context.Background(), Job{
		Kind: Index, Queries: []google.Query{usQuery("example.com/page")}, Pages: 3})

	if rep.Done != 1 {
		t.Fatalf("Done=%d, want 1 (failed %d: %v)", rep.Done, rep.Failed, rep.Results[0].Err)
	}
	if got, _ := asked.Load().(string); got != "site:example.com/page" {
		t.Errorf("asked %q, want the operator for the address", got)
	}
	if got := o.searches.Load(); got != 1 {
		t.Errorf("%d searches for one address, want 1", got)
	}
	pages := rep.Results[0].Pages
	if len(pages) != 1 {
		t.Fatalf("recorded %d pages, want 1", len(pages))
	}
	if n := len(pages[0].Results); n != 1 {
		t.Fatalf("recorded %d results, want the 1 that was the address: %+v", n, pages[0].Results)
	}
	if got := pages[0].Results[0].URL; got != "https://example.com/page" {
		t.Errorf("recorded %q, want the address that was asked about", got)
	}
}

func TestRunner_AnIndexJobRecordsAnAddressNobodyHeldAsAnAnswer(t *testing.T) {
	// Google answering with somebody else's pages is a real answer and the
	// address is not in the index. It is written down as a page holding nothing
	// rather than left unwritten: a query with nothing recorded against it is
	// one every later resume takes up again, so an address genuinely absent
	// would be re-checked for as long as it stayed absent.
	o := newOrigin(t, func(*http.Request, int) string {
		return serpBodyOf("https://elsewhere.test/x", "https://another.test/y")
	})
	f := poolFacing(t, o.addr(), 2)

	r := &Runner{Pool: f.Pool, Threads: 1}
	rep := r.Run(context.Background(), Job{
		Kind: Index, Queries: []google.Query{usQuery("example.com/page")}, Pages: 1})

	if rep.Done != 1 {
		t.Fatalf("Done=%d, want 1 — an address nobody holds is not a failed query (%v)",
			rep.Done, rep.Results[0].Err)
	}
	pages := rep.Results[0].Pages
	if len(pages) != 1 {
		t.Fatalf("recorded %d pages, want the 1 that says the answer was taken", len(pages))
	}
	if n := len(pages[0].Results); n != 0 {
		t.Errorf("recorded %d results for an address nobody held: %+v", n, pages[0].Results)
	}
}

func TestRunner_AParseJobIsUnchangedByTheChecksExisting(t *testing.T) {
	// The kind a job names is the kind it runs. A parse asked for by name and a
	// parse asked for by naming nothing are the same job, and neither of them
	// goes near either check.
	for _, kind := range []Kind{Parse, Kind(0)} {
		o := newOrigin(t, func(r *http.Request, _ int) string {
			if strings.HasPrefix(r.URL.Query().Get("q"), "site:") {
				t.Error("a parse job asked the index operator")
			}
			return serpBody("example.com")
		})
		f := poolFacing(t, o.addr(), 2)

		r := &Runner{Pool: f.Pool, Threads: 1}
		rep := r.Run(context.Background(), Job{Kind: kind, Queries: usQueries(1), Pages: 1})
		if rep.Done != 1 {
			t.Fatalf("Done=%d, want 1 (%v)", rep.Done, rep.Results[0].Err)
		}
		if n := len(rep.Results[0].Pages[0].Results); n != 1 {
			t.Errorf("a parse job recorded %d results, want the 1 the page carried", n)
		}
	}
}
