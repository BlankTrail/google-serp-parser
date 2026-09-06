// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/export"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/run"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// The control address, the key and the address list are read from the
// environment and are not flags. A key given on a command line is a key in the
// shell history and in the process list of everyone on the machine.
const (
	envControlURL = "BLANKTRAIL_URL"
	envAPIKey     = "BLANKTRAIL_API_KEY"
	envProxyList  = "GSERP_PROXY_LIST_URL"
)

// defaultControlURL is where the proxy listens when it runs on this machine,
// which is where it usually runs.
const defaultControlURL = "http://127.0.0.1:8891"

// The pause a thread leaves between two of its own requests is drawn from this
// range. A request every N seconds exactly is a description of the program
// making them, so the gap is a range and not a number.
const (
	shortestPause = 2 * time.Second
	longestPause  = 5 * time.Second
)

// runOptions is everything the command was asked to do.
type runOptions struct {
	Queries  string
	DB       string
	Out      string
	Format   string
	Pages    int
	Threads  int
	Ports    int
	Country  string
	Language string
	Name     string
	Resume   bool
	DryRun   bool
}

// runFlags declares the flags. It is separate from the parsing so the help text
// can be checked against the flags themselves rather than against a list
// somebody has to remember to keep up.
func runFlags(opts *runOptions) *flag.FlagSet {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.StringVar(&opts.Queries, "queries", "", "file holding one query per line")
	fs.StringVar(&opts.DB, "db", "gserp.db", "history database to write")
	fs.StringVar(&opts.Out, "out", "", "file to export the results into")
	fs.StringVar(&opts.Format, "format", "csv", "export format: "+strings.Join(export.Formats(), " or "))
	fs.IntVar(&opts.Pages, "pages", 1, "result pages per query")
	fs.IntVar(&opts.Threads, "threads", 2, "queries taken at once")
	fs.IntVar(&opts.Ports, "ports", 3, "ports per thread")
	fs.StringVar(&opts.Country, "country", "", "two-letter country code, e.g. de")
	fs.StringVar(&opts.Language, "language", "", "language code, e.g. de")
	fs.StringVar(&opts.Name, "name", "", "name to file the job under (default: the query list's file name)")
	fs.BoolVar(&opts.Resume, "resume", false, "take up the last unfinished job of this name instead of starting one")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "print the estimate and send nothing")
	return fs
}

// runCommand works a list of queries, writing each one down as it lands.
func runCommand(ctx context.Context, args []string, out io.Writer) error {
	var opts runOptions
	fs := runFlags(&opts)
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The interruption is heard here rather than in main so that the whole of
	// what a stopped run does is inside the command under test: a test ends the
	// context this one is built on and gets the same path a user gets, without
	// raising a signal at the process running the tests.
	ctx, stop := interruptible(ctx, out)
	defer stop()
	return runJob(ctx, out, opts)
}

// stoppingNotice is what a user is told the moment they interrupt a run. It
// answers the two things anyone wants to know having just pressed Ctrl+C on an
// hour of work: that the program heard them, and that the hour is not being
// thrown away.
const stoppingNotice = "stopping; everything already recorded is kept"

// interruptible returns a context that ends when the user interrupts, and the
// function that gives the interrupt back.
//
// Stopping a long command over a network is the ordinary way a user ends one,
// so it is a shutdown and not a process kill: the query in flight finishes, the
// history is closed and the run says what it managed.
//
// The returned function waits for the watch to end, so a caller that has
// returned is a caller nothing is still printing behind.
func interruptible(parent context.Context, out io.Writer) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt)

	done := make(chan struct{})
	var watching sync.WaitGroup
	watching.Add(1)
	go func() {
		defer watching.Done()
		if !stopping(ctx, done) {
			return
		}
		_, _ = fmt.Fprintln(out, stoppingNotice)

		// Opened only now. signal.NotifyContext keeps listening after it has
		// cancelled, so a channel opened alongside it would already hold the
		// interrupt that got us here and would read as a second one.
		again := make(chan os.Signal, 1)
		signal.Notify(again, os.Interrupt)
		defer signal.Stop(again)
		insist(again, done, func() { os.Exit(interruptExit) })
	}()

	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			// Closed before the context is cancelled below, so a watch woken by
			// that cancellation always finds it closed and knows the run ended
			// of its own accord.
			close(done)
			stop()
		})
		watching.Wait()
	}
}

// stopping reports whether ctx ended because something outside the run ended
// it, rather than because the run finished and gave the interrupt back.
func stopping(ctx context.Context, done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	case <-ctx.Done():
	}
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// insist ends the process if a second interrupt arrives before the shutdown has
// finished.
//
// A graceful shutdown that cannot itself be interrupted is a hang, and a user
// pressing Ctrl+C twice has stopped asking politely.
func insist(again <-chan os.Signal, done <-chan struct{}, kill func()) {
	select {
	case <-again:
		kill()
	case <-done:
	}
}

// poolConfig is the pool a job will open. It is built here rather than inline
// so the estimate, the run, the browser interface and a test are all reasoning
// about the same one.
//
// It takes the two numbers it reads rather than a whole command's options,
// because the two commands that open a pool describe themselves with different
// options and neither is the other's.
func poolConfig(threads, ports int) blanktrail.PoolConfig {
	return blanktrail.PoolConfig{
		Threads:        threads,
		PortsPerThread: ports,
		Spec:           blanktrail.DefaultPortSpec(),
		DelayMin:       shortestPause,
		DelayMax:       longestPause,
		ReviveAfter:    time.Minute,
		// A job waits for an identity rather than spending a query on not having
		// one. Nobody is holding the line on a job the way a caller of the search
		// API is, and a phrase refused for want of a port is a phrase that has to
		// be found and asked again by hand.
		WaitForIdentity: true,
		// A fresh connection for every request, and the reason is measured. The
		// connection this program keeps alive ends at the proxy on this machine,
		// not at the address the work actually travels through — so it goes on
		// looking healthy long after the route behind it has died, and the next
		// request is handed a tunnel to nowhere. On a live list, keeping them
		// alive answered 1.1 queries a minute against 4.3 for opening one each
		// time, at a cost of one handshake — measured at a second and a half.
		//
		// Nothing about the identity is lost by it: the cookies and the profile
		// belong to the port, which the proxy keeps, and not to the connection.
		NoKeepAlives: true,
	}
}

// settledCooldown is the gap the pool will keep between two requests on one
// port.
//
// The estimate is printed before the ports are opened, and on a dry run they
// are never opened at all, so the number cannot be read off a pool and has to
// be arrived at the same way the pool arrives at it: a named gap stands, a
// described pause is what the gap is derived from, and a caller who said
// neither gets the documented default. Arrived at any other way, the estimate
// paces a job nobody is going to run.
func settledCooldown(cfg blanktrail.PoolConfig) time.Duration {
	switch {
	case cfg.Cooldown > 0:
		return cfg.Cooldown
	case cfg.DelayMin > 0 || cfg.DelayMax > 0:
		return blanktrail.DeriveCooldown(cfg.PortsPerThread, cfg.DelayMin, cfg.DelayMax)
	default:
		return blanktrail.DefaultCooldown
	}
}

// plan is the work a run is about to do, whether it was just read from a list
// or picked up from a job that stopped part way.
type plan struct {
	id       int64
	spec     store.JobSpec
	queries  []string
	ordinals []int // empty while the queries are the whole list
}

// runJob is the command itself: work out the plan, say what it costs, and —
// unless that was all that was asked — run it, saving as it goes.
func runJob(ctx context.Context, out io.Writer, opts runOptions) error {
	// The format is checked first of all. A job that runs for an hour and then
	// cannot be written out has spent the hour for nothing, and the writer
	// thrown away here is the one place that knows what the names are.
	if _, err := export.New(opts.Format, io.Discard); err != nil {
		return err
	}
	name := opts.Name
	if name == "" {
		if opts.Queries == "" {
			return errors.New("a query list is required (-queries), or the name of a job to take up (-name)")
		}
		name = filepath.Base(opts.Queries)
	}

	var st *store.Store
	defer func() {
		if st != nil {
			_ = st.Close()
		}
	}()

	var p plan
	if opts.Resume {
		var err error
		if st, err = store.Open(opts.DB); err != nil {
			return err
		}
		if p, err = resumedPlan(ctx, st, name); err != nil {
			return err
		}
	} else {
		queries, err := readQueries(opts.Queries)
		if err != nil {
			return err
		}
		p = plan{
			queries: queries,
			spec: store.JobSpec{
				Name:     name,
				Pages:    opts.Pages,
				Country:  opts.Country,
				Language: opts.Language,
			},
		}
	}

	cfg := poolConfig(opts.Threads, opts.Ports)
	job := run.Job{
		Queries:  searchQueries(p.queries, p.spec),
		Ordinals: p.ordinals,
		Pages:    p.spec.Pages,
		Mobile:   p.spec.Device == blanktrail.DeviceMobile,
		// Looked up only for a job that keeps the address: they cost a request
		// each, and a job keeping the title and the domain has nowhere to put
		// one.
		Addresses: p.spec.Fields.Keeps(store.FieldURL),
	}
	// The estimate is printed on every run, not only on a dry one: the number a
	// user is about to spend is worth a line whether or not they asked for it.
	//
	// The threads are named. They are the lanes the work is shared between, and
	// leaving them to be assumed equal to the ports quotes half the time for the
	// four threads over eight ports the live runs were measured at.
	printEstimate(out, p.spec.Name, run.EstimateWith(job, cfg.Size(), opts.Threads,
		settledCooldown(cfg), run.MeasuredPace))
	if opts.DryRun {
		return nil
	}

	if !opts.Resume {
		// The plan is written before the first request, so an interrupted run
		// leaves behind what it meant to do as well as what it managed.
		var err error
		if st, err = store.Open(opts.DB); err != nil {
			return err
		}
		if p.id, err = st.CreateJob(ctx, p.spec, p.queries); err != nil {
			return err
		}
	}

	// From here the job exists in the history, so however the run ends it is
	// wound up rather than abandoned: what was reached, what is left and the
	// command that takes that up are the whole reason the plan is written down
	// before the first request.
	runErr := work(ctx, out, cfg, job, opts.Threads, storeSink{st: st, jobID: p.id})
	return finish(ctx, out, st, p, opts, runErr)
}

// work opens the ports and takes the queries.
func work(ctx context.Context, out io.Writer, cfg blanktrail.PoolConfig, job run.Job,
	threads int, sink run.Sink) error {
	pool, err := openPool(ctx, out, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()

	report := (&run.Runner{Pool: pool, Threads: threads, Sink: sink}).Run(ctx, job)
	if report.Err != nil {
		return report.Err
	}
	printReport(out, report)
	return nil
}

// errStopped is a run's outcome when the user stopped it. It is not a failure
// of the job and it is not a success either: what was reached is recorded, and
// what is left is there to be taken up.
var errStopped = errors.New("stopped before the job was finished")

// settleGrace bounds the winding up of a run that was stopped. What it covers
// is local — a database on this machine and a file beside it — so it is
// generous for the work and short enough that a run the user stopped stops.
const settleGrace = 30 * time.Second

// settling returns the context a run is wound up on.
//
// A run the user interrupted still owes them a report, and every read behind
// that report takes a context — which is precisely what the interruption ended.
// Those reads get one of their own, carrying the job's values and a deadline of
// its own so a stopped run cannot hang in its own shutdown instead. A run
// nobody stopped keeps the context it ran on, and stays as interruptible while
// it writes its export as it was while it worked.
func settling(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), settleGrace)
}

// finish winds a run up, whatever ended it, and says what became of it.
func finish(ctx context.Context, out io.Writer, st *store.Store, p plan, opts runOptions, runErr error) error {
	if runErr != nil && ctx.Err() == nil {
		// A run that never got going has nothing to settle: no results to write
		// out, and a job the next resume finds by itself. An empty export and a
		// hint about resuming would bury the reason it failed.
		return runErr
	}

	settleCtx, done := settling(ctx)
	defer done()
	err := settle(settleCtx, out, st, p, opts)

	if ctx.Err() != nil {
		// The interruption is the run's own outcome and has to reach the exit
		// status: a job stopped half way that exits nought reads, to whatever
		// started it, as a job that finished. What the run was holding when it
		// was stopped goes with it, because that reason is the cancellation and
		// not anything a reader can act on.
		return errors.Join(errStopped, err)
	}
	return err
}

// settle stamps a job that has nothing left, says what to do about one that
// has, and writes the export.
func settle(ctx context.Context, out io.Writer, st *store.Store, p plan, opts runOptions) error {
	left, err := st.Pending(ctx, p.id)
	if err != nil {
		return err
	}
	if len(left) == 0 {
		if err := st.FinishJob(ctx, p.id); err != nil {
			return err
		}
	} else {
		// A job is stamped done only when nothing is pending, so a run that was
		// cut short stays available to be taken up rather than closing over the
		// queries nobody reached.
		say(out, opts, "still to do: %d; gserp run -resume -name %q takes them up",
			len(left), p.spec.Name)
	}

	if opts.Out == "" {
		return nil
	}
	rows, err := exportJob(ctx, st, p.id, opts.Out, opts.Format)
	if err != nil {
		// The reason names the file it could not open, which is the same path
		// by another route, and it goes on to whatever prints the error.
		return errors.New(scrub(err.Error(), opts))
	}
	say(out, opts, "%d rows written to %s", rows, opts.Out)
	return nil
}

// say prints a line with everything that must not be printed taken out of it.
func say(out io.Writer, opts runOptions, format string, args ...any) {
	_, _ = fmt.Fprintln(out, scrub(fmt.Sprintf(format, args...), opts))
}

// resumedPlan takes up the job a name was last left in the middle of.
//
// The settings come from the job rather than from today's flags: a run picked
// up part way has to be the run it was, or the history holds two shapes of
// result under one name.
func resumedPlan(ctx context.Context, st *store.Store, name string) (plan, error) {
	job, err := st.LastUnfinished(ctx, name)
	if err != nil {
		return plan{}, err
	}
	left, err := st.Pending(ctx, job.ID)
	if err != nil {
		return plan{}, err
	}
	if len(left) == 0 {
		return plan{}, fmt.Errorf("the last job named %q has nothing left to do", name)
	}
	p := plan{id: job.ID, spec: job.Spec}
	for _, q := range left {
		p.queries = append(p.queries, q.Text)
		// The query keeps the number it had. A resumed job holds only what is
		// left, and numbering that from zero would file every result against
		// the wrong query.
		p.ordinals = append(p.ordinals, q.Ordinal)
	}
	return p, nil
}

// readQueries reads a query list: one query per line, blank lines and lines
// starting with # passed over, so a list can carry notes and be edited by hand.
func readQueries(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the query list: %w", err)
	}
	defer func() { _ = f.Close() }()

	var out []string
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			// An editor on Windows marks the file it saves, and the mark is
			// invisible inside the query it would otherwise become part of.
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the query list: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s holds no queries", path)
	}
	return out, nil
}

// searchQueries dresses each line of the list in the settings the job was
// created with.
func searchQueries(texts []string, spec store.JobSpec) []google.Query {
	qs := make([]google.Query, len(texts))
	for i, text := range texts {
		qs[i] = google.Query{Text: text, Country: spec.Country, Language: spec.Language}
	}
	return qs
}

// printEstimate says what the job will cost, in numbers and units.
func printEstimate(out io.Writer, name string, est run.Estimate) {
	_, _ = fmt.Fprintf(out, "%s: %d queries × %d pages = %d searches, %d warm-ups, "+
		"%d requests leaving the machine, %d at worst\n",
		name, est.Queries, est.Pages, est.Searches, est.Warmups, est.Requests, est.MaxRequests)
	// The expected time is the one to plan around, and it is quoted with where
	// it came from. The costs behind it were measured on one list, on one day,
	// against one target, and the range the dominant one was taken from spans a
	// factor of twenty — so a bare number here would be believed, and should not
	// be. The caveat is a clause because a paragraph gets skipped.
	_, _ = fmt.Fprintf(out, "expected ≈ %v — from per-request costs measured elsewhere, "+
		"whose own spread is a factor of twenty\n", est.Expected.Round(time.Second))
	// The floor is the later of two bounds: waiting out the pauses, and doing the
	// work at the quickest each request was ever measured at. It counts no
	// retries and no challenge that has to be solved, so a run lands above it —
	// but it is a figure worth reading, which the pacing alone was not. A live
	// run was once quoted a floor of two seconds and then took 6m15s.
	_, _ = fmt.Fprintf(out, "not sooner than %v over %d ports, %v apart\n",
		est.Floor.Round(time.Second), est.Ports, est.Cooldown.Round(time.Second))
}

// failuresShown is how many reasons a run prints when queries fail. A job of
// ten thousand that fails wholesale has one fault behind it, and printing it
// ten thousand times buries the counts above it.
const failuresShown = 3

// printReport says what became of the job.
func printReport(out io.Writer, report run.Report) {
	_, _ = fmt.Fprintf(out, "%d done, %d failed, %d untried; %d identities taken\n",
		report.Done, report.Failed, report.Untried, report.Requests)

	shown := 0
	for _, res := range report.Results {
		if res.Err == nil {
			continue
		}
		if shown == failuresShown {
			_, _ = fmt.Fprintf(out, "  … and %d more, all of them in the history\n",
				report.Failed-shown)
			return
		}
		shown++
		// The reason is filtered. A failure that reached no further than the
		// transport quotes the address it was sent through, and an address this
		// program was handed can carry a key inside it.
		_, _ = fmt.Fprintf(out, "  %q: %s\n", res.Query.Text, hideSecrets(res.Err.Error(), secrets()...))
	}
}

// openPool opens the ports the job will run on.
func openPool(ctx context.Context, out io.Writer, cfg blanktrail.PoolConfig) (*blanktrail.Pool, error) {
	control := os.Getenv(envControlURL)
	if control == "" {
		control = defaultControlURL
	}
	client, err := blanktrail.NewClient(control, os.Getenv(envAPIKey))
	if err != nil {
		return nil, scrubbed(err)
	}

	// The same check gserp doctor runs, for the same reason: the most common
	// way a run is dead on arrival is one a single request would have shown.
	report := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: checkedDomains,
		Ports:   cfg.Size(),
	})
	for _, f := range report.Blocking() {
		_, _ = fmt.Fprintf(out, "[%s] %s\n      %s\n", f.Severity,
			hideSecrets(f.Title, secrets()...), hideSecrets(f.Detail, secrets()...))
	}
	if !report.OK() {
		return nil, errors.New("this run would not get through; gserp doctor prints the whole report")
	}
	cfg.Client = client
	cfg.CA = report.CA

	if listURL := os.Getenv(envProxyList); listURL != "" {
		ups, unusable, err := (blanktrail.Source{Kind: "url", Location: listURL, DefaultScheme: "socks5"}).Load(ctx)
		if err != nil {
			return nil, scrubbed(err)
		}
		if len(ups) == 0 {
			return nil, fmt.Errorf("the list named by %s holds no usable address", envProxyList)
		}
		_, _ = fmt.Fprintf(out, "%d addresses loaded, %d lines unusable\n", len(ups), len(unusable))
		cfg.Channels = []blanktrail.Channel{
			blanktrail.NewListChannel("list", blanktrail.NewStaticRotor(ups)),
		}
	}

	pool, err := blanktrail.NewPool(ctx, cfg)
	if err != nil {
		return nil, scrubbed(err)
	}
	_, _ = fmt.Fprintf(out, "%d ports open\n", pool.Size())
	return pool, nil
}

// exportJob writes every result of a job into a file.
//
// It streams: the rows come out of the history one at a time and go into the
// file one at a time, so an export is never as large in memory as it is on
// disk.
func exportJob(ctx context.Context, st *store.Store, jobID int64, path, format string) (int, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, fmt.Errorf("creating the export: %w", err)
	}
	w, err := export.New(format, f)
	if err != nil {
		_ = f.Close()
		return 0, err
	}

	rows := 0
	err = st.Rows(ctx, jobID, func(r store.Row) error {
		rows++
		return w.Write(exportRow(r))
	})
	// The file is finished and closed whatever happened above, and the first
	// failure is the one reported: the later ones are usually the same fault
	// meeting a file that was already being abandoned.
	if closeErr := w.Close(); err == nil {
		err = closeErr
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return rows, fmt.Errorf("writing the export: %w", err)
	}
	return rows, nil
}

// recorder is the part of the history a finished query goes into.
//
// It is named here rather than taken as *store.Store so that what the bridge
// below hands over can be looked at field by field. A bridge is exactly where a
// field goes missing without anything else noticing.
type recorder interface {
	Record(ctx context.Context, jobID int64, out store.QueryOutcome) error
}

// storeSink files a finished query in the history.
//
// It lives here rather than in either package because it is the only thing that
// has to know both shapes. A Record method on the store itself would make
// storage depend on the run layer, and then neither could be used without the
// other.
//
// It is safe for concurrent use, which the runner requires: it holds nothing
// that changes, and the history behind it takes one writer at a time.
type storeSink struct {
	st    recorder
	jobID int64
}

func (s storeSink) Record(ctx context.Context, res run.QueryResult) error {
	return s.st.Record(ctx, s.jobID, store.QueryOutcome{
		Ordinal: res.Ordinal,
		Pages:   res.Pages,
		Err:     res.Err,
	})
}

// exportRow turns a stored row into an exported one. The two are the same shape
// on purpose and the copy is the price of the two packages not knowing each
// other: the day an export is served straight out of a run, this file is the
// only one that changes.
func exportRow(r store.Row) export.Row {
	return export.Row{
		Ordinal: r.Ordinal,
		Query:   r.Query,
		Page:    r.Page,
		Rank:    r.Rank,
		Title:   r.Title,
		URL:     r.URL,
		Host:    r.Host,
		Snippet: r.Snippet,
	}
}

// withheld stands in for whatever was taken out of a printed line.
const withheld = "«withheld»"

// secrets are the values this run must not print. They are read afresh rather
// than kept, because a value that is never held is a value that cannot be
// printed by accident.
func secrets() []string {
	return []string{os.Getenv(envAPIKey), os.Getenv(envProxyList)}
}

// hideSecrets takes out of a line everything that must not be printed.
//
// A failure quotes the address it failed on, and an address this program is
// handed can carry a key inside it, so a message repeated as it arrived
// publishes that key to whatever the output is kept in. The values given are
// removed outright, along with the parts of them a message tends to quote on
// its own; a user and password inside any other address, and where on this
// machine a file is kept, are recognised by their shape, because neither is
// known ahead of time.
func hideSecrets(text string, of ...string) string {
	for _, v := range of {
		if v == "" {
			continue
		}
		text = strings.ReplaceAll(text, v, withheld)
		rest, ok := after(v, "://")
		if !ok {
			continue
		}
		text = strings.ReplaceAll(text, rest, withheld)
		if host, _, ok := strings.Cut(rest, "/"); ok {
			text = strings.ReplaceAll(text, host, withheld)
		}
	}

	fields := strings.Fields(text)
	for i, f := range fields {
		scheme, rest, ok := strings.Cut(f, "://")
		if !ok {
			continue
		}
		userinfo, host, ok := strings.Cut(rest, "@")
		if !ok || userinfo == "" {
			continue
		}
		fields[i] = scheme + "://" + withheld + "@" + host
	}
	return strings.Join(fields, " ")
}

// handed are the local paths this run was given on its command line.
//
// They are the only strings the command knows to be paths on this machine.
// Nothing in a line tells a path apart from anything else shaped like one — a
// control-API route reads exactly as an absolute path does — so a redactor that
// went by shape would cut "/ports/suggest" down to "suggest" and hand the
// reader a message naming something that is not a thing. What was handed in is
// known, finite, and known to be local; everything else is left whole.
func handed(opts runOptions) []string {
	return []string{opts.Out, opts.DB, opts.Queries}
}

// shortenPaths cuts each of the given paths down to the name of the file it
// points at, wherever it appears in the line.
//
// Which file was written is the answer the user asked for and says nothing
// about the machine. The directories above it say where they keep their things,
// and a line printed once is read for years.
func shortenPaths(text string, paths ...string) string {
	// Longest first. One handed path can sit inside another — a history kept
	// under the directory the export goes to — and taking the shorter one out
	// first leaves the longer one half rewritten.
	longestFirst := slices.Clone(paths)
	slices.SortFunc(longestFirst, func(a, b string) int { return len(b) - len(a) })

	for _, p := range longestFirst {
		base := filepath.Base(p)
		if p == "" || base == p {
			continue
		}
		text = strings.ReplaceAll(text, p, base)
	}
	return text
}

// scrub takes out of a line everything this run must not print: the values it
// was given in the environment, and where on this machine the files it was
// pointed at are kept.
func scrub(text string, opts runOptions) string {
	return hideSecrets(shortenPaths(text, handed(opts)...), secrets()...)
}

// scrubDB takes out of a line everything a command holding one database must
// not print: the values it was given in the environment, and where on this
// machine that database is kept.
//
// The path is taken out in both the shapes an error writes it. A quoted path
// doubles every separator, so on a machine that separates with backslashes the
// quoted form shares no substring with the one handed in and would otherwise go
// out whole.
func scrubDB(text, db string) string {
	quoted := strconv.Quote(db)
	return hideSecrets(shortenPaths(text, db, quoted[1:len(quoted)-1]), secrets()...)
}

// after returns what follows sep, and whether sep was there at all.
func after(s, sep string) (string, bool) {
	_, rest, ok := strings.Cut(s, sep)
	return rest, ok
}

// scrubbed rewrites an error so it can be printed.
//
// The chain goes with the secret. Nothing above this command tells these errors
// apart, and a wrapped error would carry the original text — the very thing
// being taken out — along inside it.
func scrubbed(err error) error {
	return errors.New(hideSecrets(err.Error(), secrets()...))
}
