//go:build live

// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
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
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/export"
	"github.com/blanktrail/google-serp-parser/store"
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
func keepOut(values ...string) {
	for _, v := range values {
		if v == "" {
			continue
		}
		withheld = append(withheld, v)
		if _, rest, ok := strings.Cut(v, "://"); ok {
			withheld = append(withheld, rest)
			if host, _, ok := strings.Cut(rest, "/"); ok {
				withheld = append(withheld, host)
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
func liveForm(do string) url.Values {
	return url.Values{
		"name":      {liveJobName},
		"queries":   {strings.Join(liveQueries, "\n")},
		"pages":     {strconv.Itoa(livePages)},
		"country":   {"us"},
		"language":  {"en"},
		"threads":   {strconv.Itoa(liveThreads)},
		"ports":     {strconv.Itoa(livePorts)},
		actionField: {do},
	}
}

// estimateShown reads the estimate off the page it was shown on, as what each
// line is called and what it says.
//
// It reads the page rather than working the numbers out again. What this run is
// held against is what a reader was told before they pressed start, and a
// second calculation is not that.
func estimateShown(t *testing.T, html string) map[string]string {
	t.Helper()
	_, rest, ok := strings.Cut(html, `<section class="estimate">`)
	if !ok {
		fatalf(t, "the page shows no estimate:\n%s", html)
	}
	rest, _, _ = strings.Cut(rest, "</section>")

	shown := map[string]string{}
	var name string
	for _, part := range strings.Split(rest, "<")[1:] {
		tag, text, _ := strings.Cut(part, ">")
		switch tag {
		case "dt":
			name = strings.TrimSpace(text)
		case "dd":
			shown[name] = strings.TrimSpace(text)
		}
	}
	if len(shown) == 0 {
		fatalf(t, "the estimate has no lines in it:\n%s", rest)
	}
	return shown
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
func livePool(ctx context.Context, t *testing.T) *blanktrail.Pool {
	t.Helper()
	control, key, listURL := liveEnv(t)

	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		fatalf(t, "reaching the control API: %v", err)
	}
	pre := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: []string{"www.google.com", "google.com"},
		Ports:   livePorts,
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
		Client: client, Threads: liveThreads, PortsPerThread: livePorts / liveThreads,
		Spec: blanktrail.DefaultPortSpec(), CA: pre.CA,
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

// liveServer is the whole interface over a real socket: a history, the
// identities every job runs on, and the pages in front of both.
//
// It is served over a socket rather than driven as a handler, because what is
// being asked is whether a browser can do this — and a browser sends a form,
// follows a redirect and asks for a file down a connection.
func liveServer(ctx context.Context, t *testing.T) (base string, st *store.Store) {
	t.Helper()
	st = testStore(t)
	v := NewSupervisor(st, livePool(ctx, t), liveThreads)
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

	// What a reader is told the job will cost, before any of it is sent.
	estimate := estimateShown(t, body(t, submit(t, cl, base+"/new", liveForm(doEstimate))))
	for _, line := range []string{
		"estimate.searches", "estimate.requests", "estimate.worst",
		"estimate.expected", "estimate.floor", "form.ports",
	} {
		logf(t, "MEASUREMENT estimate — %s: %s", LangEN.T(line), estimate[LangEN.T(line)])
	}

	pressedStart := time.Now()
	res := submit(t, cl, base+"/new", liveForm(doStart))
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
	if want := []string{"/api/resume"}; fmt.Sprint(restingPage.buttons) != fmt.Sprint(want) {
		errorf(t, "a job that was stopped part way offers %v, want %v", restingPage.buttons, want)
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
	logf(t, "MEASUREMENT the whole job: %v from the form to the stamp, against an estimate of %s expected and %s at the floor",
		whole.Round(time.Second),
		estimate[LangEN.T("estimate.expected")], estimate[LangEN.T("estimate.floor")])

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
