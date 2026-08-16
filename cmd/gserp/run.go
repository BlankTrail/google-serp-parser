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
	"path/filepath"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/export"
	"github.com/blanktrail/google-serp-parser/google"
	"github.com/blanktrail/google-serp-parser/run"
	"github.com/blanktrail/google-serp-parser/store"
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
func runCommand(args []string, out io.Writer) error {
	var opts runOptions
	fs := runFlags(&opts)
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runJob(context.Background(), out, opts)
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

	cfg := blanktrail.PoolConfig{
		Threads:        opts.Threads,
		PortsPerThread: opts.Ports,
		Spec:           blanktrail.DefaultPortSpec(),
		DelayMin:       shortestPause,
		DelayMax:       longestPause,
		ReviveAfter:    time.Minute,
	}
	job := run.Job{
		Queries:  searchQueries(p.queries, p.spec),
		Ordinals: p.ordinals,
		Pages:    p.spec.Pages,
		SpecName: p.spec.SpecName,
	}
	// The estimate is printed on every run, not only on a dry one: the number a
	// user is about to spend is worth a line whether or not they asked for it.
	printEstimate(out, p.spec.Name, run.EstimateFor(job, cfg.Size(),
		blanktrail.DeriveCooldown(cfg.PortsPerThread, cfg.DelayMin, cfg.DelayMax)))
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

	pool, err := openPool(ctx, out, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()

	runner := &run.Runner{Pool: pool, Threads: opts.Threads, Sink: storeSink{st: st, jobID: p.id}}
	report := runner.Run(ctx, job)
	if report.Err != nil {
		return report.Err
	}
	printReport(out, report)

	return settle(ctx, out, st, p, opts)
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
		_, _ = fmt.Fprintf(out, "still to do: %d; gserp run -resume -name %q takes them up\n",
			len(left), p.spec.Name)
	}

	if opts.Out == "" {
		return nil
	}
	rows, err := exportJob(ctx, st, p.id, opts.Out, opts.Format)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%d rows written to %s\n", rows, opts.Out)
	return nil
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
	// The floor counts pauses and nothing else — no network time, no retries,
	// no walk that ends early — so it is printed as a floor and said to be one.
	// A measured run took many times it, and a number that gets believed as a
	// forecast is worse than no number at all.
	_, _ = fmt.Fprintf(out, "floor %v over %d ports, %v apart; the run takes longer than that\n",
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
// its own; a user and password inside any other address are recognised by their
// position, because those are not known ahead of time.
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
