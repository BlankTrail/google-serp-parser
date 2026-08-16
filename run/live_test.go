//go:build live

// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
)

// Run these with -count=1. Nothing a live run depends on is an input Go can
// see, so a second run of an unchanged binary against unchanged environment
// variables is served from the test cache: it reprints the first run's numbers,
// passes, and measures nothing. Repetition is the whole point of running this
// against a live target, and the cache silently takes it away.

// liveEnv reads what the run needs, or skips.
//
// Nothing here has a fallback. A live test that defaults to a well-known
// address measures whatever happens to be listening there and reports it as
// this test's result. The list address carries its own key inside it, which is a
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
	// The loopback address belongs to this machine, and a transport failure
	// quotes both ends of the connection it failed on.
	keepOut(control, key, listURL, "127.0.0.1", "localhost", "[::1]")
	return control, key, listURL
}

// withheld holds the values that must not reach a log, a file or a report.
//
// A failure quotes the address it failed on, so an unfiltered log of an error
// publishes whatever was in that address — and the list address carries a key
// inside it. Nothing printed by this file is trusted to be free of them.
var withheld struct {
	mu sync.Mutex
	of []string
}

// keepOut registers values that must never be printed, along with the parts of
// them a message is likely to quote on its own.
func keepOut(values ...string) {
	withheld.mu.Lock()
	defer withheld.mu.Unlock()
	for _, v := range values {
		if v == "" {
			continue
		}
		withheld.of = append(withheld.of, v)
		if _, rest, ok := strings.Cut(v, "://"); ok {
			withheld.of = append(withheld.of, rest)
			if host, _, ok := strings.Cut(rest, "/"); ok {
				withheld.of = append(withheld.of, host)
			}
		}
	}
	// Longest first, so a whole address is replaced before the host inside it
	// leaves a half-substituted line behind.
	sort.Slice(withheld.of, func(i, j int) bool { return len(withheld.of[i]) > len(withheld.of[j]) })
}

// hide removes the registered values and any credentials carried inside an
// address from one line of output.
func hide(s string) string {
	withheld.mu.Lock()
	for _, v := range withheld.of {
		s = strings.ReplaceAll(s, v, "«withheld»")
	}
	withheld.mu.Unlock()

	// An address from the list can carry a user and a password in it, and those
	// are nobody's to publish either. They are not known ahead of time, so they
	// are recognised by their position rather than by their value.
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

// The list is the same list for every test in this file and it does not change
// between them, so it is fetched once and the summary replayed into each test.
var (
	listOnce  sync.Once
	listUps   []blanktrail.Upstream
	listErr   error
	listLines []string
)

// addressList fetches the list once per process, unfiltered.
//
// Nothing checks these addresses before they are used. A separate check ran a
// plain fetch through each one, which is not the request the run makes: it
// carries none of the port's configuration, so it can approve an address the
// run cannot use and condemn one it can. The pool already sorts them out by the
// only standard that counts — a port that keeps failing is moved to another
// address, and one that keeps failing on all of them is set aside — so the good
// addresses accumulate through the work rather than ahead of it.
func addressList(ctx context.Context, t *testing.T, listURL string) []blanktrail.Upstream {
	t.Helper()
	listOnce.Do(func() {
		started := time.Now()
		ups, bad, err := blanktrail.Source{Kind: "url", Location: listURL, DefaultScheme: "socks5"}.Load(ctx)
		took := time.Since(started)
		if err != nil {
			listErr = err
			listLines = append(listLines, fmt.Sprintf("list: loading failed after %v: %v", took.Round(time.Millisecond), err))
			return
		}
		listUps = ups
		listLines = append(listLines,
			fmt.Sprintf("MEASUREMENT list: %d addresses parsed, %d lines unusable, fetched in %v",
				len(ups), len(bad), took.Round(time.Millisecond)))
	})
	for _, line := range listLines {
		t.Log(hide(line))
	}
	if listErr != nil {
		t.Fatal("the address list could not be loaded; the line above says how it failed")
	}
	return listUps
}

// livePorts is how many ports the whole run holds. One pool for the run rather
// than one per test, because the addresses that work accumulate in it: a pool
// thrown away and reopened between tests hands the later ones a list it has
// learned nothing about, which is not what a run does.
const livePorts = 8

var (
	poolOnce  sync.Once
	pool      *blanktrail.Pool
	poolSkip  string
	poolFatal string
	poolLines []string
)

// TestMain closes the run's pool once every test is finished with it.
func TestMain(m *testing.M) {
	code := m.Run()
	if pool != nil {
		_ = pool.Close()
	}
	os.Exit(code)
}

// livePool opens the run's pool straight onto the whole list, once.
//
// It also reports how long NewStaticRotor and NewPool took. Rotor.Next is linear
// in the list, and fifteen thousand addresses is the first time that is a real
// list rather than three lines from a test, so the cost of opening ports on one
// is worth a number rather than an opinion.
func livePool(ctx context.Context, t *testing.T) *blanktrail.Pool {
	t.Helper()
	control, key, listURL := liveEnv(t)
	ups := addressList(ctx, t, listURL)

	poolOnce.Do(func() { openLivePool(ctx, control, key, ups) })
	for _, line := range poolLines {
		t.Log(hide(line))
	}
	if poolSkip != "" {
		t.Skip(poolSkip)
	}
	if poolFatal != "" {
		t.Fatal(hide(poolFatal))
	}
	return pool
}

// openLivePool does the opening. It reports through package state rather than
// through the testing.T it was called under, because the pool belongs to the
// run and not to whichever test happened to ask for it first.
func openLivePool(ctx context.Context, control, key string, ups []blanktrail.Upstream) {
	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		poolFatal = fmt.Sprintf("control client: %v", err)
		return
	}
	pre := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: []string{"www.google.com", "google.com"},
		Ports:   livePorts,
	})
	for _, f := range pre.Findings {
		poolLines = append(poolLines, fmt.Sprintf("[%s] %s — %s → %s", f.Severity, f.Title, f.Detail, f.Action))
	}
	if !pre.OK() {
		poolSkip = "preflight refused the run; the findings above say why"
		return
	}

	rotorStarted := time.Now()
	rotor := blanktrail.NewStaticRotor(ups)
	rotorTook := time.Since(rotorStarted)

	poolStarted := time.Now()
	p, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: 4, PortsPerThread: livePorts / 4,
		Spec: blanktrail.DefaultPortSpec(), CA: pre.CA,
		Channels:    []blanktrail.Channel{blanktrail.NewListChannel("list", rotor)},
		DelayMin:    2 * time.Second,
		DelayMax:    5 * time.Second,
		ReviveAfter: time.Minute,
	})
	poolTook := time.Since(poolStarted)
	if err != nil {
		poolFatal = fmt.Sprintf("opening %d ports on a list of %d: %v", livePorts, len(ups), err)
		return
	}
	pool = p
	st := p.Stats()
	poolLines = append(poolLines, fmt.Sprintf(
		"MEASUREMENT pool opened: %d ports, %d available, cooldown %v — rotor built in %v, ports opened in %v (%v per port) over %d addresses",
		st.Ports, st.Available, p.Cooldown(),
		rotorTook.Round(time.Millisecond), poolTook.Round(time.Millisecond),
		(poolTook/livePorts).Round(time.Millisecond), len(ups)))
}

// watchPool samples the pool while a job runs, so the curve the run is meant to
// show — a pool converging on the addresses that work — is recorded as it
// happens rather than guessed at from the two ends.
type watchPool struct {
	stop    chan struct{}
	done    chan struct{}
	samples []poolSample
}

type poolSample struct {
	at    time.Duration
	stats blanktrail.Stats
}

func watch(pool *blanktrail.Pool, every time.Duration) *watchPool {
	w := &watchPool{stop: make(chan struct{}), done: make(chan struct{})}
	started := time.Now()
	go func() {
		defer close(w.done)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-w.stop:
				w.samples = append(w.samples, poolSample{time.Since(started), pool.Stats()})
				return
			case <-tick.C:
				w.samples = append(w.samples, poolSample{time.Since(started), pool.Stats()})
			}
		}
	}()
	return w
}

// report stops the watch and prints the curve. The samples are only read after
// the sampling goroutine has finished, so the slice has one writer at a time.
func (w *watchPool) report(t *testing.T, label string) {
	close(w.stop)
	<-w.done
	for _, s := range w.samples {
		logf(t, "MEASUREMENT %s at %v: available=%d/%d quarantined=%d identities-taken=%d rejections=%d egress-rotations=%d quarantines=%d revivals=%d renewals=%d",
			label, s.at.Round(time.Second), s.stats.Available, s.stats.Ports, s.stats.Quarantined,
			s.stats.Requests, s.stats.Rejections, s.stats.EgressRotations,
			s.stats.Quarantines, s.stats.Revivals, s.stats.Renewals)
	}
}

func TestLiveRun_AJobOverTheRawList(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	pool := livePool(ctx, t)

	queries := make([]google.Query, 20)
	for i := range queries {
		queries[i] = google.Query{
			Text:     fmt.Sprintf("golang %s", []string{"channels", "generics", "modules", "context", "testing"}[i%5]),
			Country:  "us",
			Language: "en",
		}
	}

	r := &Runner{Pool: pool, Threads: 4}

	est := r.Estimate(Job{Queries: queries, Pages: 2})
	// The estimate counts requests leaving the machine; the report counts
	// identities taken. Both are labelled because the two numbers answer
	// different questions and will not agree.
	logf(t, "MEASUREMENT estimate: searches=%d warmups=%d requests-leaving-the-machine=%d max-with-retries=%d floor=%v over %d ports",
		est.Searches, est.Warmups, est.Requests, est.MaxRequests, est.Floor.Round(time.Second), est.Ports)

	w := watch(pool, 30*time.Second)
	started := time.Now()
	rep := r.Run(ctx, Job{Queries: queries, Pages: 2})
	elapsed := time.Since(started)
	w.report(t, "pool during the job")

	var captured int
	for _, res := range rep.Results {
		captured += len(res.Pages)
	}
	logf(t, "MEASUREMENT job: done=%d failed=%d untried=%d identities-taken=%d pages-captured=%d in %v (estimated floor %v)",
		rep.Done, rep.Failed, rep.Untried, rep.Requests, captured,
		elapsed.Round(time.Second), est.Floor.Round(time.Second))

	st := pool.Stats()
	logf(t, "MEASUREMENT pool after the job: available=%d/%d quarantined=%d rejections=%d egress-rotations=%d quarantines=%d revivals=%d renewals=%d",
		st.Available, st.Ports, st.Quarantined, st.Rejections, st.EgressRotations,
		st.Quarantines, st.Revivals, st.Renewals)

	// How far each query got before it stopped. A walk resumes at the page it
	// was refused on, so the page counts are the only place a refusal that
	// landed on a resumed page can show itself.
	for i, res := range rep.Results {
		if res.Err == nil {
			continue
		}
		class, ok := google.ClassOf(res.Err)
		logf(t, "query %d (%q) failed after %d pages: class=%v known=%v: %v",
			i, res.Query.Text, len(res.Pages), class, ok, res.Err)
	}

	if n := rep.Done + rep.Failed + rep.Untried; n != len(queries) {
		errorf(t, "done+failed+untried=%d, want %d", n, len(queries))
	}
}

func TestLiveRun_TheFirstRequestOnAPortAndTheOnesAfterIt(t *testing.T) {
	// The first request a port makes and the ones after it are not the same
	// kind of request, and a cost model that treats them alike is wrong in
	// whichever direction the difference runs. Nothing here asserts a number:
	// this is a measurement, and the numbers are the output.
	//
	// Three things are being established. How long the first request through a
	// port takes. How long a request takes once that port has already answered
	// one. And whether the second is still true after the port has sat idle for
	// a while — because if it is not, a pool whose ports outlive their
	// usefulness is a pool that quietly pays the first cost over and over.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	pool := livePool(ctx, t)

	a := &Attempt{Pool: pool}
	q := func(text string) google.Query {
		return google.Query{Text: text, Country: "us", Language: "en"}
	}
	timed := func(s google.Searcher, label, text string) (time.Duration, int, error) {
		started := time.Now()
		serp, err := s.Search(ctx, q(text))
		d := time.Since(started)
		if err != nil {
			logf(t, "%s: %v after %v", label, err, d.Round(time.Millisecond))
			return d, 0, err
		}
		logf(t, "%s: %v, %d results", label, d.Round(time.Millisecond), len(serp.Results))
		return d, len(serp.Results), nil
	}

	// One lease held for the whole measurement, so every timing below belongs to
	// the same identity. Acquire returns whichever port is coldest, so a lease
	// taken per request would land the second one somewhere else and compare two
	// ports rather than two requests.
	//
	// An address from the list can be dead, and stopping at the first one that
	// is measures the list rather than the two lanes. So the first request is
	// carried to another port until one answers. Every failure moves that port
	// to another address, so the attempts it takes to get one answer are the
	// accumulation happening, and the count is reported rather than swallowed.
	//
	// The budget is a stretch of time rather than a number of ports. A pool of
	// eight ports needs more than eight attempts on a list this raw, and cutting
	// the search at the port count reports "nothing to measure" for what is
	// really "not yet".
	budget := 20 * time.Minute
	var (
		lease   *blanktrail.Lease
		first   time.Duration
		tried   int
		hunting = time.Now()
	)
	for tried = 1; ; tried++ {
		l, err := pool.Acquire(ctx)
		if err != nil {
			fatalf(t, "Acquire: %v", err)
		}
		d, _, err := timed(boundSearcher{attempt: a, lease: l},
			fmt.Sprintf("attempt %d: first request on port %d under a session that has not searched yet", tried, l.Port()),
			"golang channels")
		if err == nil {
			lease, first = l, d
			break
		}
		if _, classified := google.ClassOf(err); classified {
			_ = l.Reject(ctx)
		}
		l.Release()
		if time.Since(hunting) > budget {
			logf(t, "MEASUREMENT first answer: none in %d attempts over %v across %d ports",
				tried, time.Since(hunting).Round(time.Second), pool.Size())
			fatalf(t, "no port answered a first request in %d attempts, so there is nothing to compare against", tried)
		}
	}
	defer lease.Release()
	logf(t, "MEASUREMENT first answer: attempt %d of %d succeeded, %v spent finding a port that answers",
		tried, tried, time.Since(hunting).Round(time.Second))

	port := lease.Port()
	identity := lease.Session()
	bound := boundSearcher{attempt: a, lease: lease}
	logf(t, "MEASUREMENT measuring on port %d", port)

	second, _, err := timed(bound, "second request, same port, straight after", "golang generics")
	if err != nil {
		fatalf(t, "the second request failed: %v", err)
	}

	// Let the port sit. The pause is longer than the gap the pool keeps between
	// two requests on one port, so what is being measured is the identity ageing
	// rather than the pool pacing.
	idle := 3 * time.Minute
	logf(t, "leaving the port idle for %v (pool cooldown is %v)", idle, pool.Cooldown())
	select {
	case <-ctx.Done():
		t.Fatal("ran out of time before the idle measurement")
	case <-time.After(idle):
	}

	third, thirdResults, thirdErr := timed(bound, "third request, same port, after the idle spell", "golang modules")
	if thirdErr != nil {
		errorf(t, "the port stopped answering after %v idle: %v", idle, thirdErr)
	}

	logf(t, "MEASUREMENT identity on port %d: %q before, %q after the idle spell — unchanged=%v",
		port, identity, lease.Session(), identity == lease.Session())
	logf(t, "MEASUREMENT after the idle spell: request %s, %d results",
		map[bool]string{true: "worked", false: "failed"}[thirdErr == nil], thirdResults)

	logf(t, "MEASUREMENT SUMMARY port=%d first=%v second=%v third=%v (second/first=%.2f third/first=%.2f)",
		port, first.Round(time.Millisecond), second.Round(time.Millisecond), third.Round(time.Millisecond),
		float64(second)/float64(first), float64(third)/float64(first))

	st := pool.Stats()
	logf(t, "MEASUREMENT pool: identities-taken=%d rejections=%d egress-rotations=%d renewals=%d quarantines=%d revivals=%d",
		st.Requests, st.Rejections, st.EgressRotations, st.Renewals, st.Quarantines, st.Revivals)
}

func TestLiveRun_DeferredResolutionAndTheLinkForm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	pool := livePool(ctx, t)

	r := &Runner{Pool: pool, Threads: 2}
	rep := r.Run(ctx, Job{
		Queries: []google.Query{
			{Text: "site:go.dev", Country: "us", Language: "en"},
			{Text: "buy iphone", Country: "us", Language: "en"},
		},
		Pages: 2,
	})
	logf(t, "MEASUREMENT capture: done=%d failed=%d untried=%d identities-taken=%d",
		rep.Done, rep.Failed, rep.Untried, rep.Requests)
	for i, res := range rep.Results {
		logf(t, "capture: query %d (%q): %d pages, err=%v", i, res.Query.Text, len(res.Pages), res.Err)
	}

	forms := map[string]int{}
	byForm := map[google.LinkForm]int{}
	var pending, total int
	for _, res := range rep.Results {
		for _, serp := range res.Pages {
			for _, one := range serp.Results {
				total++
				byForm[one.Form]++
				switch {
				case one.Resolved():
					forms["direct"]++
				case one.Link != "":
					forms["needs resolving"]++
					pending++
				default:
					forms["no link at all"]++
				}
			}
		}
	}
	logf(t, "MEASUREMENT link forms across the capture: %v over %d results (by Form: %v)", forms, total, byForm)

	got := r.ResolveLinks(ctx, &rep, 4)
	logf(t, "MEASUREMENT resolve: attempted=%d resolved=%d failed=%d untried=%d errors=%d",
		got.Attempted, got.Resolved, got.Failed,
		got.Attempted-got.Resolved-got.Failed, len(got.Errs))
	for i, err := range got.Errs {
		if i == 3 {
			logf(t, "resolve: and %d more errors", len(got.Errs)-3)
			break
		}
		logf(t, "resolve error: %v", err)
	}

	// The question carried over from the previous milestone: does the same
	// result carry the same redirector path on two different result pages? If it
	// does not, the cross-page de-duplication in ListIndexed is correct but does
	// nothing for that link form. It can only be answered when the capture
	// actually holds such links.
	if pending == 0 {
		t.Log("MEASUREMENT link form: no result arrived needing resolution, so the redirector path is still unmeasured")
		return
	}
	for _, res := range rep.Results {
		if len(res.Pages) < 2 {
			continue
		}
		first := linksByTitle(res.Pages[0])
		var common, identical int
		for title, link := range linksByTitle(res.Pages[1]) {
			if other, ok := first[title]; ok {
				common++
				if other == link {
					identical++
				}
			}
		}
		logf(t, "MEASUREMENT link form for %q: %d results on both pages, %d carrying an identical link",
			res.Query.Text, common, identical)
	}
}

// TestLiveRun_ZZWhereThePoolEndedUp runs last, so the counters it prints cover
// everything the run put through one pool. The name carries the ordering
// because the file's order is the run's order.
func TestLiveRun_ZZWhereThePoolEndedUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	pool := livePool(ctx, t)
	st := pool.Stats()
	logf(t, "MEASUREMENT pool at the end of the run: available=%d/%d quarantined=%d identities-taken=%d rejections=%d egress-rotations=%d quarantines=%d revivals=%d renewals=%d",
		st.Available, st.Ports, st.Quarantined, st.Requests, st.Rejections,
		st.EgressRotations, st.Quarantines, st.Revivals, st.Renewals)
}

// linksByTitle indexes a page's results by title, which is the only handle a
// result carries before its address is known.
func linksByTitle(serp google.SERP) map[string]string {
	out := map[string]string{}
	for _, r := range serp.Results {
		if r.Title != "" {
			out[r.Title] = r.Link
		}
	}
	return out
}
