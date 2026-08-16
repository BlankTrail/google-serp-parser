//go:build live

// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/run"
	"github.com/blanktrail/google-serp-parser/store"
	"github.com/blanktrail/google-serp-parser/web"
)

// Run this with -count=1. Nothing a live run depends on is an input Go can see,
// so a second run of an unchanged binary against unchanged environment
// variables is served from the test cache: it reprints the first run's numbers,
// passes, and measures nothing. The whole of what this file produces is
// numbers, and the cache silently takes them away.

// liveEnv reads what a live run needs, or skips.
//
// Nothing here has a fallback. A live test that defaults to a well-known
// address measures whatever happens to be listening there and reports it as
// this test's result. The list address carries a key inside it, which is a
// second reason it lives only in the environment.
func liveEnv(t *testing.T) (control, key, listURL string) {
	t.Helper()
	for _, v := range []struct {
		name string
		into *string
	}{
		{"BLANKTRAIL_URL", &control},
		{"BLANKTRAIL_API_KEY", &key},
		{"GSERP_PROXY_LIST_URL", &listURL},
	} {
		*v.into = os.Getenv(v.name)
		if *v.into == "" {
			t.Skipf("%s is not set", v.name)
		}
	}
	// The loopback address belongs to this machine, and a failure quotes both
	// ends of the connection it failed on. The server under test is itself on
	// loopback, so its own address is in every line this file prints.
	liveKeepOut(control, key, listURL, "127.0.0.1", "localhost", "[::1]")
	return control, key, listURL
}

// liveWithheld holds the values that must not reach a log, a file or a report.
//
// A failure quotes the address it failed on, so an unfiltered line publishes
// whatever was in that address — and the list address carries a key inside it.
// Nothing printed by this file is trusted to be free of them.
var liveWithheld struct {
	mu sync.Mutex
	of []string
}

// liveKeepOut registers values that must never be printed, along with the parts
// of them a message is likely to quote on its own.
func liveKeepOut(values ...string) {
	liveWithheld.mu.Lock()
	defer liveWithheld.mu.Unlock()
	for _, v := range values {
		if v == "" {
			continue
		}
		liveWithheld.of = append(liveWithheld.of, v)
		if _, rest, ok := strings.Cut(v, "://"); ok {
			liveWithheld.of = append(liveWithheld.of, rest)
			if host, _, ok := strings.Cut(rest, "/"); ok {
				liveWithheld.of = append(liveWithheld.of, host)
			}
		}
	}
	// Longest first, so a whole address is replaced before the host inside it
	// leaves a half-substituted line behind.
	sort.Slice(liveWithheld.of, func(i, j int) bool {
		return len(liveWithheld.of[i]) > len(liveWithheld.of[j])
	})
}

// liveHide removes the registered values, and any credentials carried inside an
// address, from one line of output.
func liveHide(s string) string {
	liveWithheld.mu.Lock()
	for _, v := range liveWithheld.of {
		s = strings.ReplaceAll(s, v, "«withheld»")
	}
	liveWithheld.mu.Unlock()

	// An address can carry a user and a password in it, and those are nobody's
	// to publish either. They are not known ahead of time, so they are
	// recognised by their position rather than by their value.
	fields := strings.Fields(s)
	for i, f := range fields {
		before, rest, ok := strings.Cut(f, "://")
		if !ok {
			continue
		}
		userinfo, host, ok := strings.Cut(rest, "@")
		if !ok || userinfo == "" {
			continue
		}
		fields[i] = before + "://«withheld»@" + host
	}
	return strings.Join(fields, " ")
}

func liveLogf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Log(liveHide(fmt.Sprintf(format, args...)))
}

func liveErrorf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Error(liveHide(fmt.Sprintf(format, args...)))
}

func liveFatalf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Fatal(liveHide(fmt.Sprintf(format, args...)))
}

// liveFiltered is where the server says what went wrong.
//
// It goes through the same filter as everything else this file prints, because
// the server is handed a history and a logger and knows neither what is secret
// nor where on this machine anything lives — so the filtering belongs on this
// side of it, exactly as it does in the command.
type liveFiltered struct{ t *testing.T }

func (f liveFiltered) Write(p []byte) (int, error) {
	f.t.Log(liveHide(strings.TrimRight(string(p), "\n")))
	return len(p), nil
}

// The job a program sets going. Ten queries at two pages is the same shape the
// browser was measured on, so the two milestones' numbers can be held against
// each other, and it is long enough that a search sent while it runs genuinely
// arrives during a job rather than after one.
var liveJobQueries = []string{
	"golang channels",
	"golang generics",
	"golang modules",
	"golang context",
	"golang testing",
	"golang interfaces",
	"golang goroutines",
	"golang slices",
	"golang errors",
	"golang embed",
}

// The searches sent one at a time through the entry point. Each is different,
// so no two of them can be answered from anything Google or this program kept
// from the one before.
var liveSearchQueries = []string{
	"iphone 13 review",
	"best coffee grinder",
	"typescript satisfies operator",
	"postgres index bloat",
	"kubernetes ingress tls",
}

const (
	livePages   = 2
	liveThreads = 4
	// livePorts is what the pool holds and what the search limiter is told, so
	// a search sent through this interface competes with the job for exactly
	// the identities the job is running on.
	livePorts   = 8
	liveJobName = "live from a program"
	// liveWatchGap is how often the job is asked how far it has got. It is the
	// gap the browser's own page polls at, so watching costs the server what
	// watching costs it in use.
	liveWatchGap = 500 * time.Millisecond
)

// livePool opens the identities every search and every job in this file runs on.
func livePool(ctx context.Context, t *testing.T) *blanktrail.Pool {
	t.Helper()
	control, key, listURL := liveEnv(t)

	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		liveFatalf(t, "reaching the control API: %v", err)
	}
	pre := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: []string{"www.google.com", "google.com"},
		Ports:   livePorts,
	})
	for _, f := range pre.Findings {
		liveLogf(t, "[%s] %s — %s → %s", f.Severity, f.Title, f.Detail, f.Action)
	}
	if !pre.OK() {
		t.Skip("the preflight refused this run; the findings above say why")
	}

	ups, unusable, err := (blanktrail.Source{
		Kind: "url", Location: listURL, DefaultScheme: "socks5",
	}).Load(ctx)
	if err != nil {
		// The error carries where it was loading from, and that address carries
		// a key inside it.
		liveFatalf(t, "loading the addresses: %v", err)
	}
	liveLogf(t, "MEASUREMENT addresses: %d parsed, %d lines unusable", len(ups), len(unusable))

	started := time.Now()
	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: liveThreads, PortsPerThread: livePorts / liveThreads,
		Spec: blanktrail.DefaultPortSpec(), CA: pre.CA,
		Channels:    []blanktrail.Channel{blanktrail.NewListChannel("list", blanktrail.NewStaticRotor(ups))},
		DelayMin:    2 * time.Second,
		DelayMax:    5 * time.Second,
		ReviveAfter: time.Minute,
	})
	if err != nil {
		liveFatalf(t, "opening the identities: %v", err)
	}
	liveLogf(t, "MEASUREMENT identities: %d held, opened in %v",
		pool.Size(), time.Since(started).Round(time.Millisecond))
	return pool
}

// liveClient is an http.Client with no timeout of its own.
//
// The whole question this file asks is how long the server takes and what it
// answers when it takes too long, and a client that gave up first would replace
// the server's answer with its own silence.
func liveClient() *http.Client { return &http.Client{} }

// liveAnswer is one call to the interface: what came back, and how long the
// caller waited for it.
type liveAnswer struct {
	code int
	body string
	took time.Duration
}

func (a liveAnswer) String() string {
	return fmt.Sprintf("%d in %v", a.code, a.took.Round(time.Millisecond))
}

// liveTryAsk makes one call carrying the key in the header, and times the whole
// of it: what a caller waits is the request and the answer, not the search
// alone.
//
// It hands a failure back rather than ending the test, because several callers
// at once is one of the things being measured and those callers are not the
// goroutine the test is running on. Ending a test from another goroutine leaves
// the run neither passed nor failed but stopped somewhere in between.
func liveTryAsk(cl *http.Client, secret, method, at, body string) (liveAnswer, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, at, reader)
	if err != nil {
		return liveAnswer{}, fmt.Errorf("building a request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	started := time.Now()
	res, err := cl.Do(req)
	if err != nil {
		return liveAnswer{}, fmt.Errorf("calling the interface: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	read, err := io.ReadAll(res.Body)
	took := time.Since(started)
	if err != nil {
		return liveAnswer{}, fmt.Errorf("reading the answer: %w", err)
	}
	return liveAnswer{code: res.StatusCode, body: string(read), took: took}, nil
}

// liveAsk is one call from the test's own goroutine, where a failure has
// nothing left to measure and may as well end the run.
func liveAsk(t *testing.T, cl *http.Client, secret, method, at, body string) liveAnswer {
	t.Helper()
	got, err := liveTryAsk(cl, secret, method, at, body)
	if err != nil {
		liveFatalf(t, "%v", err)
	}
	return got
}

// liveSearchShape is the part of a SerpApi-shaped answer this file counts. The
// names are written out here so that a renamed field fails a measurement rather
// than passing as a search that found nothing.
type liveSearchShape struct {
	SearchMetadata struct {
		ID             string  `json:"id"`
		Status         string  `json:"status"`
		TotalTimeTaken float64 `json:"total_time_taken"`
	} `json:"search_metadata"`
	OrganicResults []struct {
		Position      int    `json:"position"`
		Link          string `json:"link"`
		DisplayedLink string `json:"displayed_link"`
	} `json:"organic_results"`
	AdsOmitted int `json:"ads_omitted"`
}

// liveSearchAt is one search through the compatible entry point, reported
// whatever it answered.
//
// A refusal is a measurement too, and the one this file most expects to see: a
// search has a deadline it must answer inside, and whether a cold identity fits
// in it is the question.
func liveSearchAt(t *testing.T, cl *http.Client, base, secret, label, query string) (liveAnswer, liveSearchShape) {
	t.Helper()
	got := liveAsk(t, cl, secret, http.MethodGet,
		base+"/search?q="+url.QueryEscape(query)+"&gl=us&hl=en", "")

	var shape liveSearchShape
	if got.code != http.StatusOK {
		liveLogf(t, "MEASUREMENT %s: %s — %s", label, got, strings.TrimSpace(got.body))
		return got, shape
	}
	if err := json.Unmarshal([]byte(got.body), &shape); err != nil {
		liveFatalf(t, "%s: the answer is not the JSON it claims to be: %v", label, err)
	}
	liveLogf(t, "MEASUREMENT %s: %s, %d organic results, status %q, the server timed the search itself at %.2fs, %d paid placements not reported",
		label, got, len(shape.OrganicResults), shape.SearchMetadata.Status,
		shape.SearchMetadata.TotalTimeTaken, shape.AdsOmitted)
	return got, shape
}

// liveMedian is the middle of what was measured. With an even count it is the
// lower of the two middles rather than their mean: every number here is one
// observation of a live service, and an average of two of them is a number
// nothing was measured at.
func liveMedian(of []time.Duration) time.Duration {
	if len(of) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), of...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[(len(sorted)-1)/2]
}

// liveJobState is the part of a job's answer this file watches.
type liveJobState struct {
	ID       int64 `json:"id"`
	Total    int   `json:"total"`
	Done     int   `json:"done"`
	Failed   int   `json:"failed"`
	Pending  int   `json:"pending"`
	Finished bool  `json:"finished"`
	Running  bool  `json:"running"`
	Queued   bool  `json:"queued"`
}

func (j liveJobState) String() string {
	return fmt.Sprintf("total=%d done=%d failed=%d left=%d running=%v queued=%v finished=%v",
		j.Total, j.Done, j.Failed, j.Pending, j.Running, j.Queued, j.Finished)
}

// liveJobAt reads one job through the interface, as a program watching it would.
func liveJobAt(t *testing.T, cl *http.Client, base, secret string, id int64) liveJobState {
	t.Helper()
	got := liveAsk(t, cl, secret, http.MethodGet,
		base+"/api/v1/jobs/"+strconv.FormatInt(id, 10), "")
	if got.code != http.StatusOK {
		liveFatalf(t, "asking after the job came back %d: %s", got.code, got.body)
	}
	var state liveJobState
	if err := json.Unmarshal([]byte(got.body), &state); err != nil {
		liveFatalf(t, "the job's answer is not the JSON it claims to be: %v", err)
	}
	return state
}

// liveAwaitJob watches a job until the answer satisfies want, and says how long
// that took.
func liveAwaitJob(t *testing.T, cl *http.Client, base, secret string, id int64,
	what string, deadline time.Time, want func(liveJobState) bool) (liveJobState, time.Duration) {
	t.Helper()
	started := time.Now()
	for {
		state := liveJobAt(t, cl, base, secret, id)
		if want(state) {
			return state, time.Since(started)
		}
		if time.Now().After(deadline) {
			liveFatalf(t, "waited %v for %s and it never happened: %s",
				time.Since(started).Round(time.Second), what, state)
		}
		time.Sleep(liveWatchGap)
	}
}

// liveRowsHeld counts what the history holds for one job, which is what the
// stream is held against.
func liveRowsHeld(ctx context.Context, t *testing.T, st *store.Store, jobID int64) int {
	t.Helper()
	rows := 0
	if err := st.Rows(ctx, jobID, func(store.Row) error {
		rows++
		return nil
	}); err != nil {
		liveFatalf(t, "reading the job's rows: %v", err)
	}
	return rows
}

// TestLiveAPI_ASearchAnsweredInOneConnection is the whole live measurement, in
// one test on purpose.
//
// The first number it takes is what a search costs on identities that have
// never searched, and there is exactly one moment in a process when that can be
// measured. Split across test functions the order would be Go's to choose, and
// a cold measurement taken second is a warm measurement under another name.
func TestLiveAPI_ASearchAnsweredInOneConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Minute)
	defer cancel()

	pool := livePool(ctx, t)
	st := testStore(t)
	secret := issue(t, st, "live measurement")

	// The queue and the pool are the ones a job runs on, and the search goes to
	// the same pool without passing through the queue. That is the arrangement
	// the command builds, and measuring anything else would measure a program
	// nobody runs.
	sup := web.NewSupervisor(st, pool, liveThreads)
	// Registered after the history's own cleanup and so run before it: a job
	// this ends is written down as it lets go of it. It gives up the identities
	// as it closes, so nothing below may open a second pool.
	t.Cleanup(func() { _ = sup.Close() })

	log := slog.New(slog.NewTextHandler(liveFiltered{t}, nil))
	s, err := New(Config{Store: st, Logger: log, Supervisor: sup,
		Search: SearchConfig{Searcher: &run.Attempt{Pool: pool}, Ports: livePorts}})
	if err != nil {
		liveFatalf(t, "building the interface: %v", err)
	}
	// Served over a socket rather than driven as a handler. What is being asked
	// is what somebody else's program gets, and that program sends a request
	// down a connection and waits on it.
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	base := ts.URL
	liveKeepOut(base)
	cl := liveClient()

	liveLogf(t, "MEASUREMENT the search deadline this server allows: %v, and %d searches at once",
		s.direct.deadline, cap(s.direct.limit.places))

	// 1. Cold. Nothing has searched through these identities yet, and this is
	// the only moment in the process at which that is true.
	cold, coldShape := liveSearchAt(t, cl, base, secret, "cold — the first search on identities that have never searched",
		liveSearchQueries[0])

	// 2. Warm. The same entry point, one search at a time, on identities that
	// have now answered. Every query is different, so nothing is being served
	// out of anything the first search left behind.
	var warm []time.Duration
	for i, query := range liveSearchQueries[1:] {
		got, shape := liveSearchAt(t, cl, base, secret,
			fmt.Sprintf("warm search %d", i+1), query)
		if got.code == http.StatusOK {
			warm = append(warm, got.took)
			if len(shape.OrganicResults) > 0 {
				first := shape.OrganicResults[0]
				liveLogf(t, "MEASUREMENT warm search %d, first result: position=%d link=%q displayed_link=%q",
					i+1, first.Position, first.Link, first.DisplayedLink)
			}
		}
	}
	// A median over nothing is not a small number, it is no number. Printing
	// "0s" next to "0 answered" reads as a measurement to anyone skimming, and
	// the whole point of this file is that a figure stands for probes that were
	// actually taken.
	median := "not measured — no search was answered"
	if len(warm) > 0 {
		median = liveMedian(warm).Round(time.Millisecond).String()
	}
	liveLogf(t, "MEASUREMENT cold against warm: cold %s (%d results), warm median %s over %d answered searches of %d sent",
		cold, len(coldShape.OrganicResults), median,
		len(warm), len(liveSearchQueries)-1)

	// 3. Too many at once. The limit under test is one rather than the eight the
	// server above runs with: the question is whether a caller over the limit is
	// told so over a real socket, and eight identities spent to ask it would buy
	// nothing the same code path does not already answer. The searcher, the
	// limiter and the refusal are the ones the server above uses.
	narrow, err := New(Config{Store: st, Logger: log,
		Search: SearchConfig{Searcher: &run.Attempt{Pool: pool}, AtOnce: 1}})
	if err != nil {
		liveFatalf(t, "building a second interface: %v", err)
	}
	narrowServer := httptest.NewServer(narrow.Handler())
	t.Cleanup(narrowServer.Close)
	liveKeepOut(narrowServer.URL)

	const atOnce = 3
	var wg sync.WaitGroup
	crowd := make([]liveAnswer, atOnce)
	trouble := make([]error, atOnce)
	for i := range crowd {
		wg.Add(1)
		go func() {
			defer wg.Done()
			crowd[i], trouble[i] = liveTryAsk(cl, secret, http.MethodGet,
				narrowServer.URL+"/search?q="+url.QueryEscape("golang mutex")+"&gl=us&hl=en", "")
		}()
	}
	wg.Wait()
	for i, err := range trouble {
		if err != nil {
			liveFatalf(t, "caller %d of %d never got an answer: %v", i+1, atOnce, err)
		}
	}
	refused, admitted := 0, 0
	for i, got := range crowd {
		liveLogf(t, "MEASUREMENT %d searches at once against a limit of 1 — caller %d: %s %s",
			atOnce, i+1, got, strings.TrimSpace(firstLine(got.body)))
		if got.code == http.StatusTooManyRequests {
			refused++
			continue
		}
		admitted++
	}
	liveLogf(t, "MEASUREMENT %d searches at once against a limit of 1: %d refused with 429, %d let through",
		atOnce, refused, admitted)
	if refused != atOnce-1 {
		liveErrorf(t, "%d of %d callers were refused with 429 against a limit of 1, want %d",
			refused, atOnce, atOnce-1)
	}

	// 4. A search sent while a job is running. The job holds the identities and
	// the search competes for them, which is the arrangement this milestone
	// chose and the one number that says whether it is usable.
	created := liveAsk(t, cl, secret, http.MethodPost, base+"/api/v1/jobs", liveJobBody(t))
	if created.code != http.StatusCreated {
		liveFatalf(t, "setting a job going came back %d: %s", created.code, created.body)
	}
	var back struct {
		Job liveJobState `json:"job"`
	}
	if err := json.Unmarshal([]byte(created.body), &back); err != nil {
		liveFatalf(t, "the created job is not the JSON it claims to be: %v", err)
	}
	jobID := back.Job.ID
	liveLogf(t, "MEASUREMENT the job was accepted in %v: id=%d %s",
		created.took.Round(time.Millisecond), jobID, back.Job)

	// The search is sent once the job has recorded something, so the identities
	// are genuinely in use rather than still being handed out.
	moving, waited := liveAwaitJob(t, cl, base, secret, jobID, "the job to get somewhere",
		time.Now().Add(45*time.Minute), func(at liveJobState) bool {
			return at.Running && at.Done+at.Failed > 0
		})
	liveLogf(t, "MEASUREMENT the job was under way %v after it was accepted: %s",
		waited.Round(time.Second), moving)

	during, duringShape := liveSearchAt(t, cl, base, secret,
		"a search sent while the job is running", "golang sync pool")
	liveLogf(t, "MEASUREMENT a search during a job: %s, %d organic results, the job stood at %s when it was sent",
		during, len(duringShape.OrganicResults), moving)
	switch during.code {
	case http.StatusOK:
		liveLogf(t, "MEASUREMENT a search during a job was answered, and waited %v for an identity the job was using",
			during.took.Round(time.Millisecond))
	case http.StatusGatewayTimeout:
		liveLogf(t, "MEASUREMENT a search during a job ran out of time: the deadline is %v and the caller waited %v, so this server's own limit is what ended it and not the caller's patience",
			s.direct.deadline, during.took.Round(time.Millisecond))
	default:
		liveLogf(t, "MEASUREMENT a search during a job came back %d after %v: %s",
			during.code, during.took.Round(time.Millisecond), strings.TrimSpace(firstLine(during.body)))
	}

	// 5. What the stream hands over against what the history holds. The job is
	// let run to the end first, so the two counts are taken of a job nothing is
	// still writing to.
	finished, ran := liveAwaitJob(t, cl, base, secret, jobID, "the job to finish",
		time.Now().Add(90*time.Minute), func(at liveJobState) bool { return at.Finished })
	liveLogf(t, "MEASUREMENT the job finished in %v: %s", ran.Round(time.Second), finished)

	streamed := liveAsk(t, cl, secret, http.MethodGet,
		base+"/api/v1/jobs/"+strconv.FormatInt(jobID, 10)+"/results", "")
	if streamed.code != http.StatusOK {
		liveFatalf(t, "the results came back %d: %s", streamed.code, streamed.body)
	}
	lines := ndjsonLines(t, streamed.body)
	held := liveRowsHeld(ctx, t, st, jobID)
	liveLogf(t, "MEASUREMENT the stream carried %d lines in %v and the history holds %d rows",
		len(lines), streamed.took.Round(time.Millisecond), held)
	if len(lines) != held {
		liveErrorf(t, "the history holds %d rows and the stream carried %d lines", held, len(lines))
	}

	// 6. The refusal a program pointed here from another service will meet
	// first, over a real socket. Its whole worth is what it says, so what it
	// says is printed.
	unsupported := liveAsk(t, cl, secret, http.MethodGet,
		base+"/search?q=iphone&engine=google_images", "")
	liveLogf(t, "MEASUREMENT engine=google_images: %s — %s",
		unsupported, strings.TrimSpace(unsupported.body))
	if unsupported.code != http.StatusBadRequest {
		liveErrorf(t, "engine=google_images came back %d, want 400", unsupported.code)
	}
	for _, engine := range serpAPIEngines {
		if !strings.Contains(strings.ReplaceAll(unsupported.body, "google_images", "«asked»"), engine) {
			liveErrorf(t, "the refusal names no %q among the engines this server does search", engine)
		}
	}

	// The key in the query string is how somebody else's program already sends
	// it, so it is exercised the way they send it rather than trusted to work.
	inTheAddress, err := cl.Get(base + "/search?q=" +
		url.QueryEscape("golang errors is") + "&gl=us&hl=en&api_key=" + url.QueryEscape(secret))
	if err != nil {
		liveFatalf(t, "a search carrying the key in the address: %v", err)
	}
	_ = inTheAddress.Body.Close()
	liveLogf(t, "MEASUREMENT a search carrying the key in the address rather than the header: %d",
		inTheAddress.StatusCode)
	if inTheAddress.StatusCode == http.StatusUnauthorized {
		liveErrorf(t, "a key in the address was refused, and that is the way another service's clients send it")
	}

	stats := pool.Stats()
	liveLogf(t, "MEASUREMENT identities at the end: available=%d/%d quarantined=%d taken=%d rejections=%d egress-rotations=%d quarantines=%d revivals=%d renewals=%d",
		stats.Available, stats.Ports, stats.Quarantined, stats.Requests, stats.Rejections,
		stats.EgressRotations, stats.Quarantines, stats.Revivals, stats.Renewals)
}

// liveJobBody is the job a program asks for, in the shape this interface takes
// one in.
func liveJobBody(t *testing.T) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name":     liveJobName,
		"queries":  liveJobQueries,
		"pages":    livePages,
		"country":  "us",
		"language": "en",
		"ports":    livePorts,
		"threads":  liveThreads,
	})
	if err != nil {
		t.Fatalf("marshalling the job: %v", err)
	}
	return string(body)
}

// firstLine is as much of an answer as belongs on one line of a report.
func firstLine(body string) string {
	line, _, _ := strings.Cut(body, "\n")
	return line
}
