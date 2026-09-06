//go:build live

// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/export"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/run"
	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// Run this with -count=1. Nothing a live run depends on is an input Go can see,
// so a second run of an unchanged binary against unchanged environment
// variables is served from the test cache: it reprints the first run's numbers,
// passes, and measures nothing.

// liveEnv reads what a job needs, or skips.
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
	// ends of the connection it failed on.
	keepOut(control, key, listURL, "127.0.0.1", "localhost", "[::1]")
	return control, key, listURL
}

// withheld holds the values that must not reach a log, a file or a report.
//
// A failure quotes the address it failed on, so an unfiltered line publishes
// whatever was in that address — and the list address carries a key inside it.
// Nothing printed by this file is trusted to be free of them.
var withheld []string

// keepOut registers values that must never be printed, along with the parts of
// them a message is likely to quote on its own.
//
// Each is registered twice: as it is, and as a page carries it. A failure here
// quotes the page it failed on, the templates write an address with its
// ampersands escaped, and a filter looking only for the raw string would let a
// list address — which carries a key inside it — through in the one form it is
// most likely to appear in.
func keepOut(values ...string) {
	add := func(v string) {
		withheld = append(withheld, v)
		if escaped := html.EscapeString(v); escaped != v {
			withheld = append(withheld, escaped)
		}
	}
	for _, v := range values {
		if v == "" {
			continue
		}
		add(v)
		if _, rest, ok := strings.Cut(v, "://"); ok {
			add(rest)
			if host, _, ok := strings.Cut(rest, "/"); ok {
				add(host)
			}
		}
	}
	// Longest first, so a whole address is replaced before the host inside it
	// leaves a half-substituted line behind.
	sort.Slice(withheld, func(i, j int) bool { return len(withheld[i]) > len(withheld[j]) })
}

// hide removes the registered values, and any credentials carried inside an
// address, from one line of output.
func hide(s string) string {
	for _, v := range withheld {
		s = strings.ReplaceAll(s, v, "«withheld»")
	}
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

func logf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Log(hide(fmt.Sprintf(format, args...)))
}

func errorf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Error(hide(fmt.Sprintf(format, args...)))
}

func fatalf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Fatal(hide(fmt.Sprintf(format, args...)))
}

// filtered is where the server says what went wrong.
//
// It goes through the same filter as everything else this file prints, because
// the server is handed a history and a logger and knows neither what is secret
// nor where on this machine anything lives — so the filtering belongs on this
// side of it, exactly as it does in the command.
type filtered struct{ t *testing.T }

func (f filtered) Write(p []byte) (int, error) {
	f.t.Log(hide(strings.TrimRight(string(p), "\n")))
	return len(p), nil
}

// The job. Ten queries is enough to be cut in half and leave both halves worth
// counting, and every query is different so a place in the list is also a name.
var liveQueries = []string{
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

const (
	livePages   = 2
	liveThreads = 4
	// livePorts is what the supervisor is handed and what the form is told, so
	// the estimate the page shows is an estimate of the job that then runs.
	livePorts    = 8
	liveJobName  = "live from the browser"
	liveWatchGap = 500 * time.Millisecond
)

// browser is an http.Client that stops at a redirect instead of following it.
//
// Both buttons and the form answer with one, and the whole of what they promise
// is in that answer: which page the reader lands on, and that it is an answer to
// a press rather than a page rendered into it.
func browser() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// body reads a whole answer and closes it.
func body(t *testing.T, res *http.Response) string {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		fatalf(t, "reading the answer: %v", err)
	}
	return string(b)
}

// fetch reads one page as a reader would.
func fetch(t *testing.T, cl *http.Client, at string) (int, string) {
	t.Helper()
	res, err := cl.Get(at)
	if err != nil {
		fatalf(t, "fetching a page: %v", err)
	}
	return res.StatusCode, body(t, res)
}

// submit fills a form in and sends it.
func submit(t *testing.T, cl *http.Client, at string, form url.Values) *http.Response {
	t.Helper()
	res, err := cl.PostForm(at, form)
	if err != nil {
		fatalf(t, "submitting a form: %v", err)
	}
	return res
}

// liveForm is the new-job form as a reader fills it in.
//
// The threads and the ports are the ones the supervisor was built with, so the
// estimate the page shows is an estimate of the job that then runs. They change
// the estimate and nothing else, which is what the page says of them.
func liveForm() url.Values {
	return url.Values{
		"name":     {liveJobName},
		"queries":  {strings.Join(liveQueries, "\n")},
		"pages":    {strconv.Itoa(livePages)},
		"country":  {"us"},
		"language": {"en"},
		"threads":  {strconv.Itoa(liveThreads)},
		"ports":    {strconv.Itoa(livePorts)},
		fromField:  {fromBox},
	}
}

// watching says whether the page is still asking for more, which is the one
// thing on it that is decided by the server and acted on by the browser.
func watching(html string) bool {
	for _, tag := range tagsOf(html, "section") {
		if strings.Contains(tag, `id="progress"`) {
			return strings.Contains(tag, "data-poll")
		}
	}
	return false
}

// offered is what a reader can press, by the address each button sends to.
func offered(html string) []string {
	var buttons []string
	for _, tag := range tagsOf(html, "form") {
		_, action, ok := strings.Cut(tag, `action="`)
		if !ok {
			continue
		}
		action, _, _ = strings.Cut(action, `"`)
		buttons = append(buttons, action)
	}
	sort.Strings(buttons)
	return buttons
}

// reading is the job page as a reader sees it: the counts in the cells, what
// the state is called, what may be pressed, and whether it is still watching.
type reading struct {
	total, done, failed, pending string
	state                        string
	buttons                      []string
	watching                     bool
}

func (r reading) String() string {
	return fmt.Sprintf("queries=%s done=%s failed=%s left=%s state=%q buttons=%v watching=%v",
		r.total, r.done, r.failed, r.pending, r.state, r.buttons, r.watching)
}

func jobPageAt(t *testing.T, cl *http.Client, at string) reading {
	t.Helper()
	code, html := fetch(t, cl, at)
	if code != http.StatusOK {
		fatalf(t, "the job page came back %d", code)
	}
	return reading{
		total:    shown(t, html, "count-total"),
		done:     shown(t, html, "count-done"),
		failed:   shown(t, html, "count-failed"),
		pending:  shown(t, html, "count-pending"),
		state:    shown(t, html, "state"),
		buttons:  offered(html),
		watching: watching(html),
	}
}

// polled is what the page's own script is answered with.
func polled(t *testing.T, cl *http.Client, base string, id int64) progressJSON {
	t.Helper()
	res, err := cl.Get(base + "/api/progress?job=" + strconv.FormatInt(id, 10))
	if err != nil {
		fatalf(t, "asking how far the job has got: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		fatalf(t, "the poll came back %d", res.StatusCode)
	}
	var at progressJSON
	if err := json.NewDecoder(res.Body).Decode(&at); err != nil {
		fatalf(t, "reading the answer to a poll: %v", err)
	}
	return at
}

// awaitPoll watches the job until the answer satisfies want, and says how long
// that took.
func awaitPoll(t *testing.T, cl *http.Client, base string, id int64, what string,
	deadline time.Time, want func(progressJSON) bool) (progressJSON, time.Duration) {
	t.Helper()
	started := time.Now()
	for {
		at := polled(t, cl, base, id)
		if want(at) {
			return at, time.Since(started)
		}
		if time.Now().After(deadline) {
			fatalf(t, "waited %v for %s and it never happened: %+v",
				time.Since(started).Round(time.Second), what, at)
		}
		time.Sleep(liveWatchGap)
	}
}

// rowsByOrdinal counts what the history holds for each query of a job.
func rowsByOrdinal(ctx context.Context, t *testing.T, st *store.Store, jobID int64) (map[int]int, int) {
	t.Helper()
	counts := map[int]int{}
	total := 0
	if err := st.Rows(ctx, jobID, func(r store.Row) error {
		counts[r.Ordinal]++
		total++
		return nil
	}); err != nil {
		fatalf(t, "reading the job's rows: %v", err)
	}
	return counts, total
}

// pendingPlaces is what a job has left, as places in the list somebody can read.
func pendingPlaces(ctx context.Context, t *testing.T, st *store.Store, jobID int64) (map[int]bool, []int) {
	t.Helper()
	left, err := st.Pending(ctx, jobID)
	if err != nil {
		fatalf(t, "reading what the job has left: %v", err)
	}
	set := map[int]bool{}
	var places []int
	for _, q := range left {
		set[q.Ordinal] = true
	}
	for i := range liveQueries {
		if set[i] {
			places = append(places, i)
		}
	}
	return set, places
}

// livePool opens the identities the supervisor will run every job on.
func livePool(ctx context.Context, t *testing.T, ports, threads int, device string) *blanktrail.Pool {
	t.Helper()
	control, key, listURL := liveEnv(t)

	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		fatalf(t, "reaching the control API: %v", err)
	}
	pre := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: []string{"www.google.com", "google.com"},
		Ports:   ports,
	})
	for _, f := range pre.Findings {
		logf(t, "[%s] %s — %s → %s", f.Severity, f.Title, f.Detail, f.Action)
	}
	if !pre.OK() {
		t.Skip("the preflight refused this run; the findings above say why")
	}

	ups, unusable, err := (blanktrail.Source{Kind: "url", Location: listURL, DefaultScheme: "socks5"}).Load(ctx)
	if err != nil {
		// The error carries where it was loading from, and that address carries
		// a key inside it.
		fatalf(t, "loading the addresses: %v", err)
	}
	logf(t, "MEASUREMENT addresses: %d parsed, %d lines unusable", len(ups), len(unusable))

	started := time.Now()
	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: threads, PortsPerThread: max(ports/threads, 1),
		Spec: blanktrail.DefaultPortSpec(), Specs: blanktrail.SpecsFor(device), CA: pre.CA,
		Channels:    []blanktrail.Channel{blanktrail.NewListChannel("list", blanktrail.NewStaticRotor(ups))},
		DelayMin:    2 * time.Second,
		DelayMax:    5 * time.Second,
		ReviveAfter: time.Minute,
	})
	if err != nil {
		fatalf(t, "opening the identities: %v", err)
	}
	logf(t, "MEASUREMENT identities: %d held, opened in %v",
		pool.Size(), time.Since(started).Round(time.Millisecond))
	return pool
}

// liveRaise is how the supervisor puts up the pool a job asked for.
//
// A fresh pool per job is what the product does now, so a live run that handed
// one standing pool to every job would be measuring something the product no
// longer is. It costs a warm-up per job, and that cost is one of the things
// this file exists to report.
func liveRaise(t *testing.T) OpenPool {
	return func(ctx context.Context, _ store.Profile, ports, threads int, device string,
		_ time.Duration) (*blanktrail.Pool, error) {
		return livePool(ctx, t, ports, threads, device), nil
	}
}

// liveServer is the whole interface over a real socket: a history, the
// identities every job runs on, and the pages in front of both.
//
// It is served over a socket rather than driven as a handler, because what is
// being asked is whether a browser can do this — and a browser sends a form,
// follows a redirect and asks for a file down a connection.
func liveServer(ctx context.Context, t *testing.T) (base string, st *store.Store) {
	t.Helper()
	st = testStore(t)
	v := NewSupervisor(st, liveRaise(t), livePorts, liveThreads)
	// Registered after the history's own cleanup and so run before it: a job
	// this ends is written down as it lets go of it.
	t.Cleanup(func() { _ = v.Close() })

	s, err := New(Config{Store: st, Supervisor: v,
		Logger: slog.New(slog.NewTextHandler(filtered{t}, nil))})
	if err != nil {
		fatalf(t, "building the interface: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, st
}

// exported downloads a job in one format and counts what came back.
func exportedRows(t *testing.T, cl *http.Client, base string, id int64, format string) int {
	t.Helper()
	code, text := fetch(t, cl,
		base+"/export?job="+strconv.FormatInt(id, 10)+"&format="+format)
	if code != http.StatusOK {
		fatalf(t, "the %s export came back %d", format, code)
	}
	switch format {
	case "csv":
		r := csv.NewReader(strings.NewReader(text))
		records, err := r.ReadAll()
		if err != nil {
			fatalf(t, "reading the csv back: %v", err)
		}
		if len(records) == 0 {
			fatalf(t, "the csv holds not even a header")
		}
		return len(records) - 1
	default:
		dec := json.NewDecoder(strings.NewReader(text))
		count := 0
		for {
			var row export.Row
			err := dec.Decode(&row)
			if errors.Is(err, io.EOF) {
				return count
			}
			if err != nil {
				fatalf(t, "reading line %d of the %s back: %v", count+1, format, err)
			}
			count++
		}
	}
}

func TestLiveBrowser_SetsAJobUpStopsItTakesItUpAgainAndHandsItOver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()

	base, st := liveServer(ctx, t)
	cl := browser()

	// The costing that used to stand on this page is gone with the button that
	// asked for it; what the run reports is what this file measures.

	pressedStart := time.Now()
	res := submit(t, cl, base+"/new", liveForm())
	_ = body(t, res)
	if res.StatusCode != http.StatusSeeOther {
		fatalf(t, "pressing start came back %d, want 303", res.StatusCode)
	}
	where := res.Header.Get("Location")
	id, err := strconv.ParseInt(strings.TrimPrefix(where, "/job/"), 10, 64)
	if err != nil {
		fatalf(t, "start sent the browser to %q, which names no job", where)
	}
	jobAt := base + where

	// The plan is written before the first request, so the job is there to be
	// read the instant the browser lands on its page.
	if sum, err := st.Progress(ctx, id); err != nil {
		fatalf(t, "the job the browser was sent to cannot be read: %v", err)
	} else if sum.Total != len(liveQueries) {
		errorf(t, "the job holds %d queries, and %d were typed into the form",
			sum.Total, len(liveQueries))
	}

	// The first thing anyone watches for. It is looked for in the history
	// rather than on the page, because the page shows what the history holds
	// and the question is when the history first held anything.
	var firstRow time.Duration
	for waitUntil := time.Now().Add(30 * time.Minute); firstRow == 0; {
		if _, rows := rowsByOrdinal(ctx, t, st, id); rows > 0 {
			firstRow = time.Since(pressedStart)
			break
		}
		if time.Now().After(waitUntil) {
			fatalf(t, "nothing at all was written down in %v", time.Since(pressedStart).Round(time.Second))
		}
		time.Sleep(liveWatchGap)
	}
	logf(t, "MEASUREMENT first row in the history: %v after the form was sent",
		firstRow.Round(time.Millisecond))

	// Stopped in the middle, which is the only place a stop is worth measuring:
	// something recorded behind it and something still to do in front.
	moved, waited := awaitPoll(t, cl, base, id, "the job to get somewhere with queries still left",
		time.Now().Add(60*time.Minute), func(at progressJSON) bool {
			return at.Done+at.Failed > 0 && at.Pending > 0
		})
	logf(t, "MEASUREMENT the job moved: done=%d failed=%d left=%d after %v",
		moved.Done, moved.Failed, moved.Pending, waited.Round(time.Second))

	beforeStop := jobPageAt(t, cl, jobAt)
	stopPressed := time.Now()
	stopped := submit(t, cl, base+"/api/stop", url.Values{"job": {strconv.FormatInt(id, 10)}})
	_ = body(t, stopped)
	if stopped.StatusCode != http.StatusSeeOther {
		fatalf(t, "the stop came back %d, want 303", stopped.StatusCode)
	}
	if got := stopped.Header.Get("Location"); got != where {
		errorf(t, "the stop sent the reader to %q rather than back to %q", got, where)
	}

	atTheStop := jobPageAt(t, cl, jobAt)
	logf(t, "MEASUREMENT the page just before the stop: %s", beforeStop)
	logf(t, "MEASUREMENT the page the stop lands on: %s", atTheStop)

	// A stop returns as soon as the job has been told. What it was holding is
	// written down as it lets go of it, and this is that.
	after, _ := awaitPoll(t, cl, base, id, "the job to let go of what it was holding",
		time.Now().Add(10*time.Minute), func(at progressJSON) bool {
			return !at.Running && !at.Queued
		})
	logf(t, "MEASUREMENT the job let go %v after the stop was pressed: done=%d failed=%d left=%d",
		time.Since(stopPressed).Round(time.Second), after.Done, after.Failed, after.Pending)

	stillToDo, leftPlaces := pendingPlaces(ctx, t, st, id)
	rowsBefore, totalBefore := rowsByOrdinal(ctx, t, st, id)
	logf(t, "MEASUREMENT after the stop: %d of %d queries left %v, %d rows recorded, ran for %v",
		len(leftPlaces), len(liveQueries), leftPlaces, totalBefore,
		time.Since(pressedStart).Round(time.Second))

	if after.Finished {
		fatalf(t, "the job was stamped finished by a stop, so nothing was left unfinished to take up")
	}
	if len(leftPlaces) == 0 {
		fatalf(t, "the stop left nothing to take up, so there is no resume to measure")
	}

	// A job nobody is running now reads at three in the morning exactly as it
	// reads now, and the page says so by not asking again.
	restingPage := jobPageAt(t, cl, jobAt)
	logf(t, "MEASUREMENT the page once the job let go: %s", restingPage)
	if restingPage.watching {
		errorf(t, "the page goes on asking about a job nobody is running")
	}
	// What is asked of the page is that it offers carrying on and does not offer
	// stopping, rather than that it offers exactly one thing: the page has grown
	// a form for changing the pool since this was written, and pinning the whole
	// list makes every addition a failure here while catching nothing more.
	if !slices.Contains(restingPage.buttons, "/api/resume") {
		errorf(t, "a job that was stopped part way does not offer to carry on: %v", restingPage.buttons)
	}
	if slices.Contains(restingPage.buttons, "/api/stop") {
		errorf(t, "a job that has already let go still offers to be stopped: %v", restingPage.buttons)
	}

	resumePressed := time.Now()
	resumed := submit(t, cl, base+"/api/resume", url.Values{"job": {strconv.FormatInt(id, 10)}})
	_ = body(t, resumed)
	if resumed.StatusCode != http.StatusSeeOther {
		fatalf(t, "the resume came back %d, want 303", resumed.StatusCode)
	}

	end, _ := awaitPoll(t, cl, base, id, "the job to finish", time.Now().Add(60*time.Minute),
		func(at progressJSON) bool { return at.Finished })
	resumeTook := time.Since(resumePressed)
	logf(t, "MEASUREMENT the resume finished in %v: done=%d failed=%d left=%d",
		resumeTook.Round(time.Second), end.Done, end.Failed, end.Pending)

	rowsAfter, totalAfter := rowsByOrdinal(ctx, t, st, id)
	for i := range liveQueries {
		logf(t, "MEASUREMENT query %d: %d rows before the resume, %d after, %s",
			i, rowsBefore[i], rowsAfter[i],
			map[bool]string{true: "was left to do", false: "was settled"}[stillToDo[i]])
	}

	// Nothing already recorded may be run again. A query taken twice writes its
	// pages twice, and a ranking counted twice is worse than one missing: it
	// reads as data.
	for i := range liveQueries {
		if stillToDo[i] {
			continue
		}
		if rowsAfter[i] != rowsBefore[i] {
			errorf(t, "query %d was settled before the resume with %d rows and holds %d after it",
				i, rowsBefore[i], rowsAfter[i])
		}
	}
	if end.Pending != 0 {
		errorf(t, "the resume left %d queries unfinished", end.Pending)
	}

	whole := time.Since(pressedStart)
	logf(t, "MEASUREMENT the whole job: %v from the form to the stamp",
		whole.Round(time.Second))

	// What the export hands over against what the history holds. Both formats,
	// from the same history through the same walk, so a count that differs
	// between the two is the writing and not the job.
	for _, format := range export.Formats() {
		got := exportedRows(t, cl, base, id, format)
		logf(t, "MEASUREMENT export: the history holds %d rows and the %s download holds %d",
			totalAfter, format, got)
		if got != totalAfter {
			errorf(t, "the history holds %d rows and the %s download holds %d", totalAfter, format, got)
		}
	}

	finalPage := jobPageAt(t, cl, jobAt)
	logf(t, "MEASUREMENT the page at the end: %s", finalPage)
	if finalPage.watching {
		errorf(t, "the page goes on asking about a job that is finished")
	}
	if len(finalPage.buttons) != 0 {
		errorf(t, "a finished job offers %v, and there is nothing left to press", finalPage.buttons)
	}
}

// ---------------------------------------------------------------------------
// What is measured without a connection.
//
// The two below ask nothing of the network and skip nothing, and they are here
// rather than beside the ordinary tests because of what they cost: each writes
// hundreds of thousands of rows to say what something takes, and a suite paying
// that on every run is a suite nobody runs. They stand behind the same tag as
// the rest of this file, so one command asks for measurements and the ordinary
// command does not.
// ---------------------------------------------------------------------------

// weighedStore is a history this test can put on the scales.
//
// The size is read off the files after the history is closed, because a
// database in write-ahead mode keeps part of itself in a second file until
// then, and a reading taken with that file still open is the size of whatever
// happened to have been checkpointed.
func weighedStore(t *testing.T) (*store.Store, func() int64) {
	t.Helper()
	at := filepath.Join(t.TempDir(), "gserp.db")
	st, err := store.Open(at)
	if err != nil {
		fatalf(t, "opening a history: %v", err)
	}
	open := true
	t.Cleanup(func() {
		if open {
			_ = st.Close()
		}
	})
	return st, func() int64 {
		if open {
			if err := st.Close(); err != nil {
				fatalf(t, "closing the history: %v", err)
			}
			open = false
		}
		var total int64
		for _, part := range []string{"", "-wal", "-shm"} {
			if info, err := os.Stat(at + part); err == nil {
				total += info.Size()
			}
		}
		return total
	}
}

// peakHeap is the largest heap seen while fn ran.
//
// It is sampled rather than worked out, because what it is for is the claim the
// streaming reader makes about itself вЂ” that a file of a million lines costs no
// more to hold than a file of ten вЂ” and only a reading taken while the file is
// being read says anything about that.
func peakHeap(fn func()) uint64 {
	runtime.GC()
	stop := make(chan struct{})
	most := make(chan uint64, 1)
	go func() {
		var seen uint64
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > seen {
				seen = m.HeapAlloc
			}
			select {
			case <-stop:
				most <- seen
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	fn()
	close(stop)
	return <-most
}

// The fixture the streaming reader is measured on. The blank lines and the
// notes are counted here rather than recognised afterwards: a test that worked
// out how many queries it had sent by the same rule the server reads them with
// would agree with that rule whatever either of them did.
const (
	uploadQueries = 100000
	uploadBlanks  = 1000
	uploadNotes   = 1000
	// uploadSmall is the same upload at a size nobody would stream, so what the
	// large one costs has something to be a multiple of.
	uploadSmall = 100
)

// pipedUpload sends a list that is never held on either side: the lines are
// made as the server reads them.
//
// A fixture built in a buffer first would be a megabyte on this side of the
// connection while the server was proving it holds nothing on the other, and
// the reading taken during it would be of the fixture.
func pipedUpload(t *testing.T, s *Server, boxes map[string]string,
	queries, blanks, notes int) *httptest.ResponseRecorder {
	t.Helper()
	pr, pw := io.Pipe()
	form := multipart.NewWriter(pw)
	kind := form.FormDataContentType()

	go func() {
		var err error
		defer func() { _ = pw.CloseWithError(err) }()
		// The boxes stand before the file, exactly as they do in the markup: the
		// server counts on knowing the name and the depth before the first line of
		// the list arrives.
		for _, box := range []string{"name", "kind", "pages", "country", "language", "unique"} {
			value, filled := boxes[box]
			if !filled {
				continue
			}
			if err = form.WriteField(box, value); err != nil {
				return
			}
		}
		var part io.Writer
		if part, err = form.CreateFormFile(listField, "queries.txt"); err != nil {
			return
		}
		emit := func(line string) bool {
			_, err = io.WriteString(part, line+"\n")
			return err == nil
		}
		blanked, noted := 0, 0
		for written := 0; written < queries; {
			if !emit("phrase number " + strconv.Itoa(written)) {
				return
			}
			written++
			if written%100 != 0 {
				continue
			}
			if blanked < blanks {
				if !emit("") {
					return
				}
				blanked++
			}
			if noted < notes {
				if !emit("# a note somebody left themselves") {
					return
				}
				noted++
			}
		}
		for ; blanked < blanks; blanked++ {
			if !emit("") {
				return
			}
		}
		for ; noted < notes; noted++ {
			if !emit("# a note somebody left themselves") {
				return
			}
		}
		err = form.Close()
	}()

	req := httptest.NewRequest(http.MethodPost, uploadAt, pr)
	req.Header.Set("Content-Type", kind)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// jobBehind is the job a redirect sent the reader to.
func jobBehind(t *testing.T, rec *httptest.ResponseRecorder) int64 {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		fatalf(t, "the form came back %d, want 303:\n%s", rec.Code, rec.Body.String())
	}
	where := rec.Header().Get("Location")
	id, err := strconv.ParseInt(strings.TrimPrefix(where, "/job/"), 10, 64)
	if err != nil {
		fatalf(t, "the form sent the browser to %q, which names no job", where)
	}
	return id
}

func TestUploadCost_TakesEveryLineOfALargeFileIntoThePlanWithoutHoldingIt(t *testing.T) {
	// The engine waits before every query, so a list that has just been uploaded
	// is still there to be counted exactly as it was written.
	st := testStore(t)
	v := newSupervisor(st, &heldEngine{hold: make(chan struct{})})
	t.Cleanup(func() { _ = v.Close() })
	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet()})
	if err != nil {
		fatalf(t, "building the interface: %v", err)
	}

	boxes := map[string]string{"name": "a list too large for the box", "pages": "1"}
	var small *httptest.ResponseRecorder
	smallHeap := peakHeap(func() { small = pipedUpload(t, s, boxes, uploadSmall, 0, 0) })
	smallID := jobBehind(t, small)

	var large *httptest.ResponseRecorder
	started := time.Now()
	largeHeap := peakHeap(func() {
		large = pipedUpload(t, s, boxes, uploadQueries, uploadBlanks, uploadNotes)
	})
	took := time.Since(started)
	largeID := jobBehind(t, large)

	ctx := t.Context()
	smallSum, err := st.Progress(ctx, smallID)
	if err != nil {
		fatalf(t, "reading the small job back: %v", err)
	}
	largeSum, err := st.Progress(ctx, largeID)
	if err != nil {
		fatalf(t, "reading the large job back: %v", err)
	}

	sent := uploadQueries + uploadBlanks + uploadNotes
	logf(t, "MEASUREMENT upload: %d lines sent вЂ” %d queries, %d blank, %d notes вЂ” and %d queries in the plan",
		sent, uploadQueries, uploadBlanks, uploadNotes, largeSum.Total)
	logf(t, "MEASUREMENT upload: %v from the first byte to the redirect, %.0f lines a second",
		took.Round(time.Millisecond), float64(sent)/took.Seconds())
	logf(t, "MEASUREMENT upload: the heap peaked at %d KB reading %d lines and at %d KB reading %d",
		smallHeap>>10, uploadSmall, largeHeap>>10, sent)
	logf(t, "MEASUREMENT upload: the plan of the large job is marked complete: %v", largeSum.PlanReady)

	if largeSum.Total != uploadQueries {
		errorf(t, "%d query lines were sent and the plan holds %d", uploadQueries, largeSum.Total)
	}
	if smallSum.Total != uploadSmall {
		errorf(t, "%d query lines were sent and the plan holds %d", uploadSmall, smallSum.Total)
	}
	if !largeSum.PlanReady {
		errorf(t, "the whole list arrived and the plan is not marked complete, so nothing will run it")
	}
}

// pageEngine hands every query a page of results and asks nothing of anything.
//
// It stands where the identities go, so a job driven through it takes the whole
// path a real job's results take вЂ” the form, the queue, the sink, the history вЂ”
// and the only thing missing from it is Google. That is what makes what it
// measures the cost of the write rather than the cost of a measurement written
// beside the write.
type pageEngine struct {
	// perQuery is how many results each query brings back.
	perQuery int
	// addresses is how many different addresses the whole job draws on. As many
	// as it will produce results, every one is new and the filter drops nothing;
	// fewer, and it has repeats to drop.
	addresses int

	// made counts the results handed out so far, and it is what brings an address
	// round again. One goroutine runs a job, and it is the only thing that
	// touches this.
	made int
}

func (e *pageEngine) Run(ctx context.Context, j run.Job, sink run.Sink) run.Report {
	rep := run.Report{Results: make([]run.QueryResult, len(j.Queries))}
	for i := range j.Queries {
		res := run.QueryResult{Query: j.Queries[i], Ordinal: i, Attempted: true}
		if len(j.Ordinals) != 0 {
			res.Ordinal = j.Ordinals[i]
		}
		res.Pages = []google.SERP{e.page(j.Queries[i].Text)}
		if err := sink.Record(ctx, res); err != nil {
			res.Err = err
			rep.Failed++
		} else {
			rep.Done++
		}
		rep.Results[i] = res
		if ctx.Err() != nil {
			break
		}
	}
	return rep
}

func (e *pageEngine) Close() error { return nil }

func (e *pageEngine) Pool() poolFacts { return poolFacts{Threads: 1, Cooldown: time.Second} }

func (e *pageEngine) page(query string) google.SERP {
	serp := google.SERP{Query: query, Results: make([]google.Result, 0, e.perQuery)}
	for k := 0; k < e.perQuery; k++ {
		n := e.made % e.addresses
		e.made++
		host := "s" + strconv.Itoa(n) + ".example"
		at := "https://" + host + "/a/" + strconv.Itoa(n)
		serp.Results = append(serp.Results, google.Result{
			Position: k + 1,
			Title:    "page " + strconv.Itoa(n),
			Host:     host,
			URL:      at,
			Link:     at,
			Snippet:  "a line of text standing where a snippet stands",
		})
	}
	return serp
}

// The job the filter is measured on: two hundred thousand results, which is the
// size it was first costed at.
const (
	filterQueries  = 1000
	filterPerQuery = 200
	filterResults  = filterQueries * filterPerQuery
)

// filterRun is one job written through the interface, and what it cost.
type filterRun struct {
	// Kept is what the history holds and Dropped what the filter refused.
	Kept    int
	Dropped int
	// Shown is what the job's own page put on the screen for what was dropped. It
	// is read off the page rather than out of the history, because a count nobody
	// can see is a count the operator does not have.
	Shown string
	Took  time.Duration
	Bytes int64
}

// filterCost writes one job of the same size through the interface under one
// filter, and answers with what that cost.
func filterCost(t *testing.T, by string, addresses int) filterRun {
	t.Helper()
	st, weigh := weighedStore(t)
	v := newSupervisor(st, &pageEngine{perQuery: filterPerQuery, addresses: addresses})
	t.Cleanup(func() { _ = v.Close() })
	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet()})
	if err != nil {
		fatalf(t, "building the interface: %v", err)
	}

	lines := make([]string, filterQueries)
	for i := range lines {
		lines[i] = "phrase number " + strconv.Itoa(i)
	}
	form := url.Values{
		"name":    {"what the filter costs"},
		"queries": {strings.Join(lines, "\n")},
		"pages":   {"1"},
		"unique":  {by},
		fromField: {fromBox},
	}
	req := httptest.NewRequest(http.MethodPost, newAt, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	started := time.Now()
	s.Handler().ServeHTTP(rec, req)
	id := jobBehind(t, rec)

	ctx := t.Context()
	var sum store.JobSummary
	for waitUntil := time.Now().Add(15 * time.Minute); ; {
		sum, err = st.Progress(ctx, id)
		if err != nil {
			fatalf(t, "reading how far the job has got: %v", err)
		}
		if sum.Finished {
			break
		}
		if time.Now().After(waitUntil) {
			fatalf(t, "the job never finished: done=%d failed=%d left=%d", sum.Done, sum.Failed, sum.Pending)
		}
		time.Sleep(50 * time.Millisecond)
	}
	took := time.Since(started)

	kept := 0
	if err := st.Rows(ctx, id, func(store.Row) error { kept++; return nil }); err != nil {
		fatalf(t, "counting what the job kept: %v", err)
	}
	page := get(t, s, jobPath(id))
	if page.Code != http.StatusOK {
		fatalf(t, "the job page came back %d", page.Code)
	}
	shownDropped := maybeShown(page.Body.String(), "count-dropped")

	// The supervisor is closed before the history is weighed, so nothing is still
	// writing to the file the size is read off.
	if err := v.Close(); err != nil {
		fatalf(t, "closing the supervisor: %v", err)
	}
	return filterRun{Kept: kept, Dropped: sum.Dropped, Shown: shownDropped, Took: took, Bytes: weigh()}
}

func TestFilterCost_WhatDroppingRepeatsCostsOnTheWayThroughTheInterface(t *testing.T) {
	// Three jobs of one size. The first two carry the same results and differ only
	// in whether the job asked for the filter, so what stands between them is the
	// mark and nothing else; the third brings every address twice, which is what
	// the filter is for.
	off := filterCost(t, string(store.UniqueOff), filterResults)
	on := filterCost(t, string(store.UniqueURL), filterResults)
	half := filterCost(t, string(store.UniqueURL), filterResults/2)

	last := strconv.Itoa(filterResults - 1)
	key := google.CanonicalURL("https://s" + last + ".example/a/" + last)
	for _, r := range []struct {
		what string
		run  filterRun
	}{
		{"no filter", off},
		{"by address, nothing to drop", on},
		{"by address, every address twice", half},
	} {
		logf(t, "MEASUREMENT filter (%s): %d results kept, %d dropped, %v, %d KB of history",
			r.what, r.run.Kept, r.run.Dropped, r.run.Took.Round(time.Millisecond), r.run.Bytes>>10)
	}
	logf(t, "MEASUREMENT filter: the page shows %q dropped where the history counted %d",
		half.Shown, half.Dropped)
	if off.Took > 0 {
		logf(t, "MEASUREMENT filter: the mark costs %+.1f%% of the time the same %d results take without it",
			(on.Took.Seconds()-off.Took.Seconds())/off.Took.Seconds()*100, filterResults)
	}
	logf(t, "MEASUREMENT filter: %d bytes of history for %d marks, %d bytes each, on a key of %d characters",
		on.Bytes-off.Bytes, filterResults, (on.Bytes-off.Bytes)/filterResults, len(key))

	if off.Kept != filterResults || on.Kept != filterResults {
		errorf(t, "the two jobs the comparison rests on kept %d and %d results, and both should hold %d",
			off.Kept, on.Kept, filterResults)
	}
	if off.Dropped != 0 {
		errorf(t, "a job that asked for no filter dropped %d results", off.Dropped)
	}
	// Every address in the third job was captured exactly twice, so what it keeps
	// and what it drops are both known before it runs. Checking only that the two
	// come to what was captured is satisfied by a filter that drops nothing.
	if half.Kept != filterResults/2 || half.Dropped != filterResults/2 {
		errorf(t, "every address was captured twice, so %d results were to be kept and %d dropped, "+
			"and the job kept %d and dropped %d",
			filterResults/2, filterResults/2, half.Kept, half.Dropped)
	}
	// The marks take room, and a history that grew by nothing is a history nothing
	// was written into.
	if on.Bytes <= off.Bytes {
		errorf(t, "the same results took %d bytes with the filter and %d without it, "+
			"so the marks the filter needs went nowhere", on.Bytes, off.Bytes)
	}
	if half.Shown != strconv.Itoa(half.Dropped) {
		errorf(t, "the history counted %d dropped and the page shows %q", half.Dropped, half.Shown)
	}
}

// ---------------------------------------------------------------------------
// What a connection is needed for.
// ---------------------------------------------------------------------------

// maybeShown reads a cell the page draws only when it has something to put in
// it, and answers with nothing when the page drew none.
//
// It is the forgiving twin of shown, because a screen with no job running has
// no job to describe, and a reading that failed the test over that would fail
// on exactly the screen this is here to record.
func maybeShown(html, id string) string {
	anchor := `id="` + id + `">`
	at := strings.Index(html, anchor)
	if at < 0 {
		return ""
	}
	rest := html[at+len(anchor):]
	end := strings.IndexByte(rest, '<')
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// stateReading is the screen an operator sits in front of, as it stands at one
// instant.
type stateReading struct {
	Job                      string
	Done, Failed, Pending    string
	Elapsed, Expected, Rest  string
	Share, Failures, Settled string
	Reasons                  []string
	Alive, Ports             string
	Rotations, Aside, Back   string
	Waiting                  string
}

func (r stateReading) String() string {
	return fmt.Sprintf("job=%q done=%s failed=%s left=%s elapsed=%s expected=%s rest=%s "+
		"refused=%s of %s (%s) %v ports=%s alive=%s aside=%s back=%s rotated=%s queue=%s",
		r.Job, r.Done, r.Failed, r.Pending, r.Elapsed, r.Expected, r.Rest,
		r.Failures, r.Settled, r.Share, r.Reasons, r.Ports, r.Alive, r.Aside, r.Back,
		r.Rotations, r.Waiting)
}

// readState reads the screen once, as a reader with no script sees it.
func readState(t *testing.T, cl *http.Client, base string) stateReading {
	t.Helper()
	code, html := fetch(t, cl, base+stateAt)
	if code != http.StatusOK {
		fatalf(t, "the state screen came back %d", code)
	}
	r := stateReading{
		Done:      maybeShown(html, "run-done"),
		Failed:    maybeShown(html, "run-failed"),
		Pending:   maybeShown(html, "run-pending"),
		Elapsed:   maybeShown(html, "run-elapsed"),
		Expected:  maybeShown(html, "run-expected"),
		Rest:      maybeShown(html, "run-rest"),
		Share:     maybeShown(html, "fail-share"),
		Failures:  maybeShown(html, "fail-count"),
		Settled:   maybeShown(html, "fail-settled"),
		Alive:     maybeShown(html, "pool-alive"),
		Ports:     maybeShown(html, "pool-ports"),
		Rotations: maybeShown(html, "pool-rotations"),
		Aside:     maybeShown(html, "pool-quarantined"),
		Back:      maybeShown(html, "pool-revived"),
		Waiting:   maybeShown(html, "queue-waiting"),
	}
	for _, reason := range reasons {
		if count := maybeShown(html, reason.Cell); count != "" {
			r.Reasons = append(r.Reasons, LangEN.T(reason.Key)+"="+count)
		}
	}
	// The name of the job in flight is the one thing here that is not a cell,
	// because it is a link to that job rather than a figure kept up to date.
	if _, rest, ok := strings.Cut(html, `<p class="lead"><a href="/job/`); ok {
		if _, name, ok := strings.Cut(rest, ">"); ok {
			r.Job, _, _ = strings.Cut(name, "<")
		}
	}
	return r
}

// wholeOf reads a figure the screen drew as a number, and says whether it was
// one. The mark the screen writes where there is nothing to work a figure out
// from is not a number and must not be counted as nought.
func wholeOf(cell string) (int, bool) {
	n, err := strconv.Atoi(cell)
	return n, err == nil
}

func TestLiveState_ShowsThePoolAndTheRefusalsWhileAJobIsInFlight(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	base, st := liveServer(ctx, t)
	cl := browser()

	res := submit(t, cl, base+"/new", liveForm())
	_ = body(t, res)
	if res.StatusCode != http.StatusSeeOther {
		fatalf(t, "pressing start came back %d, want 303", res.StatusCode)
	}
	where := res.Header.Get("Location")
	id, err := strconv.ParseInt(strings.TrimPrefix(where, "/job/"), 10, 64)
	if err != nil {
		fatalf(t, "start sent the browser to %q, which names no job", where)
	}

	// The screen is read on the schedule it asks the browser to keep, and every
	// reading is kept. What this is for is the shape of a run as it goes, and a
	// summary taken at the end is a different thing entirely.
	watching := time.Now()
	var readings, named, wentAside, deepest int
	last := ""
	for deadline := time.Now().Add(50 * time.Minute); ; {
		r := readState(t, cl, base)
		readings++
		if r.Job != "" {
			named++
		}
		// Every figure on this screen is worked out from one reading of one server,
		// and this is the arithmetic that says so: a port is either available or set
		// aside, and the two come to the number of ports at whatever instant the
		// screen was drawn.
		alive, aliveOK := wholeOf(r.Alive)
		aside, asideOK := wholeOf(r.Aside)
		ports, portsOK := wholeOf(r.Ports)
		if aliveOK && asideOK && portsOK && alive+aside != ports {
			errorf(t, "the screen shows %d ports, %d available and %d set aside, "+
				"which is two readings drawn as one", ports, alive, aside)
		}
		if asideOK && aside > 0 {
			if wentAside == 0 {
				logf(t, "MEASUREMENT state: the first port went aside %v in: %s",
					time.Since(watching).Round(time.Second), r)
			}
			wentAside++
			if aside > deepest {
				deepest = aside
			}
		}
		if now := r.String(); now != last {
			logf(t, "MEASUREMENT state at %v: %s", time.Since(watching).Round(time.Second), now)
			last = now
		}
		at := polled(t, cl, base, id)
		if at.Finished || (!at.Running && !at.Queued) {
			break
		}
		if time.Now().After(deadline) {
			fatalf(t, "the job was still going after %v and the screen had been read %d times",
				time.Since(watching).Round(time.Second), readings)
		}
		time.Sleep(stateRefresh)
	}

	logf(t, "MEASUREMENT state: %d readings %v apart over %v, %d naming a job in flight, "+
		"%d with a port set aside, %d aside at once at the deepest",
		readings, stateRefresh, time.Since(watching).Round(time.Second), named, wentAside, deepest)
	if wentAside == 0 {
		logf(t, "MEASUREMENT state: no port was set aside in this run, so what this screen "+
			"shows as a pool goes off is not something this run measured")
	}

	// Once nothing is running the screen says so, rather than going on describing
	// the job that was.
	final := readState(t, cl, base)
	logf(t, "MEASUREMENT state at rest: %s", final)
	sum, err := st.Progress(ctx, id)
	if err != nil {
		fatalf(t, "reading the job back: %v", err)
	}
	logf(t, "MEASUREMENT state: the job ended done=%d failed=%d left=%d finished=%v",
		sum.Done, sum.Failed, sum.Pending, sum.Finished)
	if named == 0 {
		errorf(t, "the screen never named the job that was running, so it was never read while it ran")
	}
}

// liveSettingsServer is the whole interface with somewhere to keep its settings
// and a way of opening what they describe.
//
// The file is seeded with the connection from the environment, so the form can
// be sent with an empty key box вЂ” which is how a reader who came to change a
// port count sends it, and the one path on that page everybody walks.
func liveSettingsServer(ctx context.Context, t *testing.T) (string, *store.Store, *Supervisor) {
	t.Helper()
	control, key, listURL := liveEnv(t)

	at := filepath.Join(t.TempDir(), "gserp-settings.json")
	if err := settings.Save(at, settings.Settings{
		ControlURL: control, APIKey: key,
		HotPorts: livePorts,
		Proxy:    settings.ProxySource{Kind: sourceURL, Location: listURL},
	}); err != nil {
		fatalf(t, "writing the settings down: %v", err)
	}

	st := testStore(t)
	v := NewSupervisor(st, liveRaise(t), livePorts, liveThreads)
	t.Cleanup(func() { _ = v.Close() })

	s, err := New(Config{Store: st, Supervisor: v, SettingsPath: at,
		Logger:  slog.New(slog.NewTextHandler(filtered{t}, nil)),
		Connect: liveConnect(t)})
	if err != nil {
		fatalf(t, "building the interface: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, st, v
}

// liveConnect opens the ports a connection just saved describes, doing what the
// command does: the check first, then the ports, then the list behind them.
func liveConnect(t *testing.T) Connect {
	return func(ctx context.Context, saved settings.Settings, _ store.Profile, ports, threads int,
		device string, _ time.Duration) (*blanktrail.Pool, error) {
		client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
		if err != nil {
			return nil, err
		}
		// The connection comes from the settings and the size from the job. This
		// is the seam the command has too, and a live run that took both from one
		// place would be measuring a program nobody runs.
		ports, threads = atLeastOne(ports), atLeastOne(threads)
		pre := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
			Domains: reachedDomains, Ports: ports,
		})
		if !pre.OK() {
			return nil, errors.New("the check refused this connection")
		}
		cfg := blanktrail.PoolConfig{
			Client: client, Threads: threads, PortsPerThread: max(ports/threads, 1),
			Spec: blanktrail.DefaultPortSpec(), CA: pre.CA,
			DelayMin: 2 * time.Second, DelayMax: 5 * time.Second, ReviveAfter: time.Minute,
		}
		if saved.Proxy.Kind != "" {
			ups, _, err := (blanktrail.Source{Kind: saved.Proxy.Kind, Location: saved.Proxy.Location,
				DefaultScheme: "socks5"}).Load(ctx)
			if err != nil {
				return nil, err
			}
			cfg.Channels = []blanktrail.Channel{
				blanktrail.NewListChannel("list", blanktrail.NewStaticRotor(ups)),
			}
		}
		started := time.Now()
		pool, err := blanktrail.NewPool(ctx, cfg)
		if err != nil {
			return nil, err
		}
		logf(t, "MEASUREMENT settings: %d identities opened in %v for a connection that was just saved",
			pool.Size(), time.Since(started).Round(time.Millisecond))
		return pool, nil
	}
}

// liveSettingsPost is the settings as a reader sends them, with the key box left
// empty: the page cannot show a key, and nobody retypes one to change a port
// count.
func liveSettingsPost(control, listURL string, ports int) url.Values {
	return url.Values{
		urlField:     {control},
		keyField:     {""},
		hotField:     {strconv.Itoa(ports)},
		sourceField:  {sourceURL},
		whereField:   {listURL},
		refreshField: {"0"},
		tongueField:  {""},
	}
}

// shortLiveJob is a job small enough to be watched twice over in one session:
// the settings are changed under one job that is going to be stopped and one
// that is going to finish, and both have to happen while somebody is waiting.
func shortLiveJob(name string) url.Values {
	form := liveForm()
	form.Set("name", name)
	form.Set("queries", strings.Join(liveQueries[:4], "\n"))
	form.Set("pages", "1")
	return form
}

func TestLiveSettings_LeavesTheJobInFlightOnThePoolItRaised(t *testing.T) {
	// The two answers this used to measure — «now» and «after this job» — were
	// the price of one pool shared by everything. A job raises its own now, so a
	// connection saved while one is running cannot reach it, and the next job
	// starts through what was saved. What is worth measuring is that: the job
	// goes on, and the one after it comes up on the new connection.
	//
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()

	base, _, sup := liveSettingsServer(ctx, t)
	cl := browser()
	control, _, listURL := liveEnv(t)

	started := time.Now()
	res := submit(t, cl, base+newAt, shortLiveJob("running while the settings change"))
	if res.StatusCode != http.StatusSeeOther {
		fatalf(t, "starting the job came back %d: %s", res.StatusCode, body(t, res))
	}
	id := jobIDIn(t, res)
	waitUntilLive(t, ctx, "the job to be running", func() bool {
		running, ok := sup.Running()
		return ok && running == id
	})
	logf(t, "MEASUREMENT settings: the job was running %v after the form went",
		time.Since(started).Round(time.Second))

	// One port where the job above came up on eight, so what the next job comes
	// up on cannot be mistaken for what this one is running on.
	saved := time.Now()
	res = submit(t, cl, base+settingsAt, liveSettingsPost(control, listURL, 1))
	page := body(t, res)
	if res.StatusCode != http.StatusSeeOther {
		fatalf(t, "saving the settings came back %d: %s", res.StatusCode, page)
	}
	logf(t, "MEASUREMENT settings: the save came back in %v and asked nothing",
		time.Since(saved).Round(time.Millisecond))

	if running, ok := sup.Running(); !ok || running != id {
		errorf(t, "running=%d,%v — saving the settings took the job down", running, ok)
	}
	if strings.Contains(page, "settings.running") {
		errorf(t, "the page still asks what to do about the running job")
	}

	at := base + jobPath(id)
	waitUntilLive(t, ctx, "the job to finish on the pool it raised", func() bool {
		return !jobPageAt(t, cl, at).watching
	})
	logf(t, "MEASUREMENT settings: the job ran to the end %v after the save: %s",
		time.Since(saved).Round(time.Second), jobPageAt(t, cl, at))
}

// liveIndexTargets are ten pages whose answer somebody can check by opening
// them, some held and some not.
//
// Both shapes of target are here on purpose. A bare host asks whether Google
// holds any page of the site, an address asks about that page and nothing else,
// and only the second can be answered wrongly in the direction this check was
// changed to stop: a site's other pages standing in for the one asked about.
//
// The absent ones are absent for two different reasons. Two are under a name
// nobody can register, so nothing about them can ever be held. Two are paths
// that do not exist under sites that are held, and those are the ones that
// matter: that is where a site: query answers with the site's other pages, and
// where a check reading В«something came backВ» reports a page Google has never
// seen.
var liveIndexTargets = []struct {
	Target string
	Held   bool
}{
	{"go.dev", true},
	{"en.wikipedia.org", true},
	{"github.com", true},
	{"go.dev/doc/effective_go", true},
	{"pkg.go.dev/net/http", true},
	{"github.com/golang/go", true},
	{"no-such-site-7f3ac21b.example", false},
	{"nothing-is-here-5d81ba94.example", false},
	{"go.dev/doc/there-is-no-such-page-4f19c7", false},
	{"github.com/golang/go/there-is-no-such-path-8ab3f2", false},
}

func TestLiveIndex_SaysHeldOnlyForThePageThatWasAskedAbout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	base, st := liveServer(ctx, t)
	cl := browser()

	lines := make([]string, len(liveIndexTargets))
	for i, target := range liveIndexTargets {
		lines[i] = target.Target
	}
	form := liveForm()
	form.Set("name", "is this page held")
	form.Set("kind", store.KindIndex)
	form.Set("queries", strings.Join(lines, "\n"))
	form.Set("pages", "1")

	res := submit(t, cl, base+"/new", form)
	_ = body(t, res)
	if res.StatusCode != http.StatusSeeOther {
		fatalf(t, "pressing start came back %d, want 303", res.StatusCode)
	}
	where := res.Header.Get("Location")
	id, err := strconv.ParseInt(strings.TrimPrefix(where, "/job/"), 10, 64)
	if err != nil {
		fatalf(t, "start sent the browser to %q, which names no job", where)
	}

	end, took := awaitPoll(t, cl, base, id, "the addresses to be checked",
		time.Now().Add(50*time.Minute), func(at progressJSON) bool { return at.Finished })
	logf(t, "MEASUREMENT index: %d addresses checked in %v вЂ” done=%d failed=%d",
		len(liveIndexTargets), took.Round(time.Second), end.Done, end.Failed)

	// What was recorded against each address, and whether those records carry an
	// address at all. Under the encrypted link form a result gives a host and no
	// address, and under that form a question asked about a path cannot be
	// answered yes by anything вЂ” so a run in which nothing carries an address
	// says nothing about the half of this list that names one.
	rows, withURL := map[int]int{}, map[int]int{}
	if err := st.Rows(ctx, id, func(r store.Row) error {
		rows[r.Ordinal]++
		if r.URL != "" {
			withURL[r.Ordinal]++
		}
		return nil
	}); err != nil {
		fatalf(t, "reading what the check recorded: %v", err)
	}

	verdicts := map[int]bool{}
	if err := st.Verdicts(ctx, id, func(v store.Verdict) error {
		verdicts[v.Ordinal] = v.Held
		return nil
	}); err != nil {
		fatalf(t, "reading the verdicts: %v", err)
	}

	var agreed, checked, heldWrongly int
	for i, target := range liveIndexTargets {
		held, answered := verdicts[i]
		if !answered {
			logf(t, "MEASUREMENT index: %s вЂ” no verdict; the check got no answer for it", target.Target)
			continue
		}
		checked++
		switch {
		case held == target.Held:
			agreed++
		case held:
			heldWrongly++
		}
		logf(t, "MEASUREMENT index: %s вЂ” held=%v against the held=%v anybody can see, "+
			"%d results recorded, %d of them carrying an address",
			target.Target, held, target.Held, rows[i], withURL[i])
	}
	logf(t, "MEASUREMENT index: %d of %d verdicts agreed with what is there to be seen, "+
		"and %d called a page nobody holds held",
		agreed, checked, heldWrongly)

	if checked == 0 {
		errorf(t, "not one address was checked, so this run says nothing about any verdict")
	}
	// The one direction that is wrong however the results came back. A page
	// nobody holds, reported as held, is the answer this check was changed to
	// stop giving, and it is the answer a reader has no way of disbelieving.
	if heldWrongly != 0 {
		errorf(t, "%d addresses nobody holds came back held", heldWrongly)
	}
}
