// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/export"
	"github.com/blanktrail/google-serp-parser/google"
	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
	"github.com/blanktrail/google-serp-parser/run"
	"github.com/blanktrail/google-serp-parser/store"
)

// syncBuffer is written by the goroutine that watches for the interruption
// while the test reads it, which a plain buffer does not survive.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor waits for something another goroutine will do, and says what was
// waited for when it never happens.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if ok() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s never happened", what)
}

// invented builds an absolute path in the shape this platform writes them,
// naming nothing that exists on this machine — so a failure printing one says
// nothing about where anybody keeps their things. The volume comes from a real
// path because on Windows a rooted path without one is not absolute.
func invented(elems ...string) string {
	return filepath.VolumeName(os.TempDir()) + string(filepath.Separator) + filepath.Join(elems...)
}

// listOf writes a query list and returns its path.
func listOf(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "q.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the list: %v", err)
	}
	return path
}

func TestRunCommand_EstimatesWithoutSendingAnything(t *testing.T) {
	// The first thing anyone does with ten thousand queries is ask what it
	// will cost. Answering that must not need a working proxy or a network.
	dir := t.TempDir()
	list := listOf(t, dir, "iphone 13\ngolang generics\n\n# a comment\n")

	var out bytes.Buffer
	err := runCommand(context.Background(), []string{
		"-queries", list, "-db", filepath.Join(dir, "h.db"),
		"-pages", "3", "-ports", "4", "-dry-run",
	}, &out)
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	got := out.String()
	for _, want := range []string{"2 queries", "3 pages", "requests"} {
		if !strings.Contains(got, want) {
			t.Errorf("the estimate does not mention %q:\n%s", want, got)
		}
	}
}

// estimated returns what a dry run says a job of twenty queries costs at the
// given thread and port counts.
func estimated(t *testing.T, threads, ports string) string {
	t.Helper()
	dir := t.TempDir()
	list := listOf(t, dir, strings.Repeat("golang generics\n", 20))

	var out bytes.Buffer
	err := runCommand(context.Background(), []string{
		"-queries", list, "-db", filepath.Join(dir, "h.db"),
		"-pages", "2", "-threads", threads, "-ports", ports, "-dry-run",
	}, &out)
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	return out.String()
}

// expectedIn reads back the time a printed estimate expects the job to take.
func expectedIn(t *testing.T, printed string) time.Duration {
	t.Helper()
	for _, line := range strings.Split(printed, "\n") {
		rest, ok := strings.CutPrefix(line, "expected ≈ ")
		if !ok {
			continue
		}
		d, err := time.ParseDuration(strings.Fields(rest)[0])
		if err != nil {
			t.Fatalf("the time in %q does not read back as one: %v", line, err)
		}
		return d
	}
	t.Fatalf("the estimate never says how long the job is expected to take:\n%s", printed)
	return 0
}

func TestRunCommand_QuotesTheTimeOverTheThreadsItWasGivenAndNotOverThePorts(t *testing.T) {
	// Threads are the lanes; ports are only what a lane can run on. Eight ports
	// worked by one thread take eight times as long as eight ports worked by
	// eight, so quoting the port count as the thread count halves the number on
	// a run of four threads over eight ports — which is the shape the live runs
	// were measured at.
	slow := expectedIn(t, estimated(t, "1", "8"))
	fast := expectedIn(t, estimated(t, "8", "1"))

	if slow <= fast {
		t.Errorf("one thread over eight ports is quoted at %v and eight threads at %v; "+
			"the threads are not reaching the estimate", slow, fast)
	}
	if slow < 4*fast {
		t.Errorf("one thread is quoted at %v against eight threads at %v, "+
			"which is not the eightfold the lanes make it", slow, fast)
	}
}

func TestRunCommand_SaysWhichOfTheTimesItQuotesIsABoundAndWhereTheOtherCameFrom(t *testing.T) {
	// The two lengths answer different questions and are printed as such: one is
	// a bound no run of this shape has come in under, the other is what to plan
	// around and rests on costs measured on one list on one day — so a number
	// printed bare gets believed and should not be.
	got := estimated(t, "4", "2")

	if !strings.Contains(got, "not sooner than") {
		t.Errorf("the estimate no longer states the bound below which no run has gone:\n%s", got)
	}
	var expected string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "expected ≈ ") {
			expected = line
		}
	}
	if expected == "" {
		t.Fatalf("the estimate does not quote a time to plan around:\n%s", got)
	}
	if !strings.Contains(expected, "measured") {
		t.Errorf("the expected time is quoted bare, with nothing about where it came from:\n%s", expected)
	}
}

func TestRunCommand_PacesTheEstimateAsThePoolItWillOpenWouldPaceItself(t *testing.T) {
	// The estimate is printed before the ports are opened — and on a dry run
	// they never are — so the gap between two requests on one port has to be
	// arrived at here. Arrived at differently from the pool, it quotes a job
	// nobody is going to run.
	fake := fakebt.New(t)
	client, err := blanktrail.NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	cfg := poolConfig(2, 3)
	cfg.Client = client
	cfg.Insecure = true // the fake serves plain HTTP
	cfg.Channels = []blanktrail.Channel{blanktrail.NewDirectChannel("direct")}

	pool, err := blanktrail.NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	if got := settledCooldown(cfg); got != pool.Cooldown() {
		t.Errorf("the estimate paces the job at %v and the pool paces it at %v", got, pool.Cooldown())
	}
}

func TestRunCommand_LeavesNoHistoryBehindWhenItOnlyEstimates(t *testing.T) {
	// An estimate is a question, and a question should not create the file the
	// answer would have been written into.
	dir := t.TempDir()
	db := filepath.Join(dir, "h.db")

	var out bytes.Buffer
	if err := runCommand(context.Background(), []string{"-queries", listOf(t, dir, "a\n"), "-db", db, "-dry-run"}, &out); err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a dry run created %s", filepath.Base(db))
	}
}

func TestRunCommand_SkipsBlankLinesAndComments(t *testing.T) {
	dir := t.TempDir()
	list := listOf(t, dir, "  \n# note\na\n\nb\n")

	var out bytes.Buffer
	if err := runCommand(context.Background(), []string{"-queries", list, "-db", filepath.Join(dir, "h.db"), "-dry-run"}, &out); err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if !strings.Contains(out.String(), "2 queries") {
		t.Errorf("the list was read as something other than two queries:\n%s", out.String())
	}
}

func TestRunCommand_SaysWhichFormatsItKnows(t *testing.T) {
	dir := t.TempDir()
	list := listOf(t, dir, "a\n")

	var out bytes.Buffer
	err := runCommand(context.Background(), []string{
		"-queries", list, "-db", filepath.Join(dir, "h.db"),
		"-out", filepath.Join(dir, "o.txt"), "-format", "xlsx", "-dry-run",
	}, &out)
	if err == nil {
		t.Fatal("an unknown format was accepted")
	}
	if !strings.Contains(err.Error(), "csv") {
		t.Errorf("the refusal %q does not say what is available", err)
	}
}

func TestRunCommand_RefusesAMisspeltFormatBeforeItOpensAnything(t *testing.T) {
	// Learning about a typo in a format name an hour into a job helps nobody,
	// and neither does a job that runs to the end and then cannot be written
	// out. This is the check that runs before the work, so it is the one that
	// has to come first — the history is not even opened.
	dir := t.TempDir()
	db := filepath.Join(dir, "h.db")

	var out bytes.Buffer
	err := runCommand(context.Background(), []string{
		"-queries", listOf(t, dir, "a\n"), "-db", db,
		"-out", filepath.Join(dir, "o.txt"), "-format", "tsv",
	}, &out)
	if err == nil {
		t.Fatal("an unknown format was accepted")
	}
	if !strings.Contains(err.Error(), "tsv") {
		t.Errorf("the refusal %q does not name the format that was asked for", err)
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Error("the history was opened before the format was checked")
	}
}

func TestRunCommand_RefusesAnEmptyQueryList(t *testing.T) {
	dir := t.TempDir()
	list := listOf(t, dir, "\n# only a comment\n")

	var out bytes.Buffer
	if err := runCommand(context.Background(), []string{"-queries", list, "-db", filepath.Join(dir, "h.db"), "-dry-run"}, &out); err == nil {
		t.Error("a list with no queries was accepted")
	}
}

// startedJob writes a job of three queries and records the first one, leaving
// the two a resume has to find.
func startedJob(t *testing.T, db, name string) {
	t.Helper()
	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	id, err := s.CreateJob(context.Background(),
		store.JobSpec{Name: name, Pages: 3, Country: "de", Language: "de"},
		[]string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.Record(context.Background(), id, store.QueryOutcome{Ordinal: 0}); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func TestRunCommand_ResumesWhatIsLeftAtTheDepthTheJobWasCreatedWith(t *testing.T) {
	// A job picked up part way holds only what is left, and it has to run as
	// the job it is. Estimating today's flags over the whole list would quote a
	// run nobody asked for, twice.
	dir := t.TempDir()
	db := filepath.Join(dir, "h.db")
	startedJob(t, db, "nightly")

	var out bytes.Buffer
	err := runCommand(context.Background(), []string{"-db", db, "-name", "nightly", "-resume", "-pages", "1", "-dry-run"}, &out)
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "2 queries") {
		t.Errorf("the estimate covers something other than the two queries left:\n%s", got)
	}
	if !strings.Contains(got, "3 pages") {
		t.Errorf("the resumed job was re-planned at the depth of today's flags:\n%s", got)
	}
}

func TestRunCommand_SaysSoWhenThereIsNoJobOfThatNameToTakeUp(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "h.db")
	startedJob(t, db, "nightly")

	var out bytes.Buffer
	err := runCommand(context.Background(), []string{"-db", db, "-name", "weekly", "-resume", "-dry-run"}, &out)
	if err == nil {
		t.Fatal("a resume of a name that was never run was accepted")
	}
	if !strings.Contains(err.Error(), "weekly") {
		t.Errorf("the refusal %q does not name the job that was asked for", err)
	}
}

func TestRunCommand_SaysSoWhenAResumedJobHasNothingLeft(t *testing.T) {
	// Every query is recorded and the job was never stamped done. Running it
	// again would open ports to send nothing.
	dir := t.TempDir()
	db := filepath.Join(dir, "h.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id, err := s.CreateJob(context.Background(), store.JobSpec{Name: "nightly", Pages: 1}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.Record(context.Background(), id, store.QueryOutcome{Ordinal: 0}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var out bytes.Buffer
	if err := runCommand(context.Background(), []string{"-db", db, "-name", "nightly", "-resume", "-dry-run"}, &out); err == nil {
		t.Error("a job with nothing left was taken up again")
	}
}

func TestReadQueries_TakesTheFirstLineOfAListSavedByAWindowsEditor(t *testing.T) {
	// Notepad puts a byte-order mark in front of the file. Carried into the
	// first query it changes the search without changing how it looks.
	dir := t.TempDir()
	list := listOf(t, dir, "\ufeffiphone 13\n  spaced  \n")

	got, err := readQueries(list)
	if err != nil {
		t.Fatalf("readQueries: %v", err)
	}
	want := []string{"iphone 13", "spaced"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("readQueries returned %q, want %q", got, want)
	}
}

// storeSink is handed to a runner, so it has to be one.
var _ run.Sink = storeSink{}

// caughtOutcome keeps what the bridge handed the history.
type caughtOutcome struct {
	jobID int64
	out   store.QueryOutcome
}

type catchingRecorder struct{ caught []caughtOutcome }

func (c *catchingRecorder) Record(_ context.Context, jobID int64, out store.QueryOutcome) error {
	c.caught = append(c.caught, caughtOutcome{jobID, out})
	return nil
}

func TestStoreSink_CarriesEveryPartOfAFinishedQueryToTheHistory(t *testing.T) {
	// The bridge is the one place that knows both shapes, so it is the one
	// place a field can be dropped without anything else noticing.
	c := &catchingRecorder{}
	refused := errors.New("nothing answered this query")

	err := storeSink{st: c, jobID: 42}.Record(context.Background(), run.QueryResult{
		Query:   google.Query{Text: "iphone 13"},
		Ordinal: 7,
		Pages: []google.SERP{
			{Origin: "https://www.google.com", Results: []google.Result{{Host: "a.test"}}},
			{Origin: "https://www.google.com"},
		},
		Err: refused,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(c.caught) != 1 {
		t.Fatalf("the history was handed %d outcomes, want one", len(c.caught))
	}
	got := c.caught[0]
	if got.jobID != 42 {
		t.Errorf("the outcome was filed against job %d, want 42", got.jobID)
	}
	if got.out.Ordinal != 7 {
		t.Errorf("Ordinal=%d, want the query's own 7", got.out.Ordinal)
	}
	if len(got.out.Pages) != 2 || len(got.out.Pages[0].Results) != 1 {
		t.Errorf("Pages=%+v, want both pages as they were captured", got.out.Pages)
	}
	if !errors.Is(got.out.Err, refused) {
		t.Errorf("Err=%v, want the reason the query produced nothing", got.out.Err)
	}
}

func TestStoreSink_FilesAResultAgainstTheJobItBelongsTo(t *testing.T) {
	// The bridge is written against an interface, and an interface the real
	// history does not answer to would compile and record nothing.
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	id, err := s.CreateJob(context.Background(), store.JobSpec{Name: "j", Pages: 1}, []string{"a", "b"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	sink := storeSink{st: s, jobID: id}
	err = sink.Record(context.Background(), run.QueryResult{
		Ordinal: 1,
		Pages: []google.SERP{{Results: []google.Result{
			{Title: "One", URL: "https://b.test/1", Host: "b.test"},
		}}},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	pending, err := s.Pending(context.Background(), id)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 || pending[0].Ordinal != 0 {
		t.Errorf("pending is %+v, want only the query that was not recorded", pending)
	}
	var hosts []string
	if err := s.Rows(context.Background(), id, func(r store.Row) error {
		hosts = append(hosts, r.Host)
		return nil
	}); err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if len(hosts) != 1 || hosts[0] != "b.test" {
		t.Errorf("the history holds %q, want the one result that was recorded", hosts)
	}
}

func TestExportRow_CarriesEveryColumnOfAStoredRow(t *testing.T) {
	// Two flat structures with the same fields are exactly where a copy loses
	// one silently: the export still writes, with a column of blanks.
	got := exportRow(store.Row{
		Ordinal: 3, Query: "iphone 13", Page: 2, Rank: 14,
		Title: "One", URL: "https://a.test/1", Host: "a.test", Snippet: "a snippet",
	})
	want := export.Row{
		Ordinal: 3, Query: "iphone 13", Page: 2, Rank: 14,
		Title: "One", URL: "https://a.test/1", Host: "a.test", Snippet: "a snippet",
	}
	if got != want {
		t.Errorf("exportRow returned %+v, want %+v", got, want)
	}
}

func TestHideSecrets_KeepsAFailedAddressOutOfWhatIsPrinted(t *testing.T) {
	// A transport failure quotes the address it failed on, and an address this
	// program is handed can carry a key inside it. Neither belongs in a log.
	line := "loading the list from https://example.test/list?key=s3cret failed; " +
		"socks5://user:pass@203.0.113.7:1080 refused the connection"
	got := hideSecrets(line, "https://example.test/list?key=s3cret")

	for _, kept := range []string{"s3cret", "example.test", "user:pass"} {
		if strings.Contains(got, kept) {
			t.Errorf("%q survived into %q", kept, got)
		}
	}
	if !strings.Contains(got, "refused the connection") {
		t.Errorf("the reason was hidden along with the address: %q", got)
	}
}

func TestScrub_KeepsWhereOnThisMachineAHandedFileIsKeptOutOfWhatIsPrinted(t *testing.T) {
	// On the user's own terminal an absolute path is merely their own. In a log,
	// a bug report or a transcript it is a description of their machine, and it
	// outlives the terminal it was printed on. The file's name is what they
	// asked about; the directories above it are not.
	dir := t.TempDir()
	opts := runOptions{Out: filepath.Join(dir, "results.csv"), DB: filepath.Join(dir, "history.db")}
	line := fmt.Sprintf("wrote %s beside %s, and results.csv is relative", opts.Out, opts.DB)

	got := scrub(line, opts)

	if strings.Contains(got, dir) {
		t.Errorf("the directory survived into %q", got)
	}
	for _, kept := range []string{"results.csv", "history.db", "is relative"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q was taken out along with the directory: %q", kept, got)
		}
	}
}

func TestScrub_LeavesAlonePathShapedTextTheCommandWasNeverHanded(t *testing.T) {
	// Guessing at what a path is cannot work: nothing in a line tells a route
	// apart from a file. The first of these is a real message from the control
	// API, and a redactor going by shape cut it down to "blanktrail: suggest
	// returned no port" — naming something that is not a thing. The second is a
	// path in every sense, written the way this platform writes one, and still
	// none of the command's business; it is here because the first is only
	// path-shaped on the platforms where the separator is a slash, and a guard
	// that sleeps on the machine the tests run on is not a guard.
	opts := runOptions{Out: invented("mine", "results.csv")}

	for _, line := range []string{
		"blanktrail: /ports/suggest returned no port",
		"failed on " + invented("somebody-elses", "results.csv"),
	} {
		if got := scrub(line, opts); got != line {
			t.Errorf("a line the command was never handed came back as %q, want %q", got, line)
		}
	}
}

func TestScrub_CutsEachHandedPathToItsOwnNameWhateverOrderTheyCameIn(t *testing.T) {
	// One handed path can sit inside another — a history kept under the
	// directory the export goes to. Taking the shorter one out first leaves the
	// longer one half rewritten, and half a path is still a path.
	dir := t.TempDir()
	opts := runOptions{Out: dir, DB: filepath.Join(dir, "history.db")}

	got := scrub("wrote "+opts.DB, opts)

	if want := "wrote history.db"; got != want {
		t.Errorf("scrub returned %q, want %q", got, want)
	}
}

func TestSettle_NamesTheExportWithoutSayingWhereOnThisMachineItSits(t *testing.T) {
	s, id, dir := halfDoneJob(t)
	path := filepath.Join(dir, "out.csv")

	var out bytes.Buffer
	err := settle(context.Background(), &out, s, plan{id: id, spec: store.JobSpec{Name: "j"}},
		runOptions{Out: path, Format: "csv"})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}

	got := out.String()
	if strings.Contains(got, dir) {
		t.Errorf("the run printed where on this machine it wrote:\n%s", got)
	}
	if !strings.Contains(got, "out.csv") {
		t.Errorf("the run does not say which file it wrote:\n%s", got)
	}
}

func TestSettle_KeepsThePathOutOfTheReasonAnExportCouldNotBeWritten(t *testing.T) {
	// The failure quotes the file it could not open, which is the same path by
	// another route, and this one is handed on to whatever prints the error.
	s, id, dir := halfDoneJob(t)
	missing := filepath.Join(dir, "no-such-directory", "out.csv")

	var out bytes.Buffer
	err := settle(context.Background(), &out, s, plan{id: id, spec: store.JobSpec{Name: "j"}},
		runOptions{Out: missing, Format: "csv"})
	if err == nil {
		t.Fatal("an export into a directory that does not exist was reported as written")
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("the refusal says where on this machine the file would have gone: %v", err)
	}
	if !strings.Contains(err.Error(), "out.csv") {
		t.Errorf("the refusal does not say which file could not be written: %v", err)
	}
}

func TestSettle_LeavesTheResumeCommandExactlyAsItHasToBeTyped(t *testing.T) {
	// The hint exists to be copied. A name cut short on its way out — because
	// the user filed the job under something shaped like a path — leaves a line
	// that reads as a command and takes up nothing.
	s, id, dir := halfDoneJob(t)
	name := filepath.Join(dir, "nightly")

	var out bytes.Buffer
	if err := settle(context.Background(), &out, s, plan{id: id, spec: store.JobSpec{Name: name}}, runOptions{}); err != nil {
		t.Fatalf("settle: %v", err)
	}

	want := fmt.Sprintf("-name %q", name)
	if !strings.Contains(out.String(), want) {
		t.Errorf("the resume command does not carry the name the job is filed under:\n%s", out.String())
	}
}

func TestUsage_ListsEveryFlagTheRunCommandTakes(t *testing.T) {
	// Help that has drifted from the flags is the same defect as documentation
	// that is wrong: it is read instead of the code, and it is believed.
	var opts runOptions
	fs := runFlags(&opts)

	if !strings.Contains(usageText, "gserp run") {
		t.Error("the help does not list the run command")
	}
	fs.VisitAll(func(f *flag.Flag) {
		if !strings.Contains(usageText, "--"+f.Name) {
			t.Errorf("the help does not mention --%s", f.Name)
		}
	})
}

// recordedJob writes a job of two queries and records the first with two
// results, leaving one query pending.
func recordedJob(t *testing.T, s *store.Store) int64 {
	t.Helper()
	id, err := s.CreateJob(context.Background(), store.JobSpec{Name: "j", Pages: 1}, []string{"a", "b"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	err = s.Record(context.Background(), id, store.QueryOutcome{
		Ordinal: 0,
		Pages: []google.SERP{{Results: []google.Result{
			{Title: "One", URL: "https://a.test/1", Host: "a.test", Snippet: "first, with a comma"},
			{Title: "Two", URL: "https://a.test/2", Host: "a.test"},
		}}},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	return id
}

func TestSettle_WritesEveryStoredResultIntoTheExport(t *testing.T) {
	// The export is the whole point of the run for whoever asked for it, and a
	// file short of a row is not something anyone spots by eye.
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	id := recordedJob(t, s)
	path := filepath.Join(dir, "out.csv")

	var out bytes.Buffer
	err = settle(context.Background(), &out, s, plan{id: id, spec: store.JobSpec{Name: "j"}},
		runOptions{Out: path, Format: "csv"})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening the export: %v", err)
	}
	defer func() { _ = f.Close() }()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("the export does not read back as CSV: %v", err)
	}
	if len(records) != 3 {
		t.Errorf("the export holds %d lines, want a header and two results", len(records))
	}
	if !strings.Contains(out.String(), "2 rows") {
		t.Errorf("the run does not say how much it wrote:\n%s", out.String())
	}
}

func TestSettle_LeavesAJobWithWorkLeftInItThereToBeTakenUp(t *testing.T) {
	// Stamping a job done while queries are still pending closes the door on
	// them: a resume would find nothing and the results would never be taken.
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	id := recordedJob(t, s)

	var out bytes.Buffer
	err = settle(context.Background(), &out, s, plan{id: id, spec: store.JobSpec{Name: "j"}}, runOptions{})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}

	if _, err := s.LastUnfinished(context.Background(), "j"); err != nil {
		t.Errorf("the job with a query left in it is no longer there to take up: %v", err)
	}
	if !strings.Contains(out.String(), "-resume") {
		t.Errorf("nothing says how to take up what is left:\n%s", out.String())
	}
}

func TestSettle_StampsAJobThatHasNothingLeftAsDone(t *testing.T) {
	// A finished job that is never stamped is one every later resume of that
	// name picks up again, to run nothing.
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	id := recordedJob(t, s)
	if err := s.Record(context.Background(), id, store.QueryOutcome{Ordinal: 1}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var out bytes.Buffer
	if err := settle(context.Background(), &out, s, plan{id: id, spec: store.JobSpec{Name: "j"}}, runOptions{}); err != nil {
		t.Fatalf("settle: %v", err)
	}

	if _, err := s.LastUnfinished(context.Background(), "j"); !errors.Is(err, store.ErrNoUnfinishedJob) {
		t.Errorf("LastUnfinished returned %v, want the job to be stamped done", err)
	}
}

func TestInterrupt_TellsTheUserTheJobIsStoppingAndThatWhatIsRecordedIsKept(t *testing.T) {
	// A long job over a network is stopped with Ctrl+C, and a user who has just
	// pressed it wants to know two things at once: that the program heard them,
	// and that the hour it has already spent is not being thrown away.
	parent, interrupt := context.WithCancel(context.Background())
	var out syncBuffer
	ctx, give := interruptible(parent, &out)
	defer give()

	interrupt()
	<-ctx.Done()
	waitFor(t, "the notice that the job is stopping", func() bool { return out.String() != "" })

	got := out.String()
	for _, want := range []string{"stopping", "kept"} {
		if !strings.Contains(got, want) {
			t.Errorf("the interrupted run does not say %q:\n%s", want, got)
		}
	}
}

func TestInterrupt_SaysNothingWhenTheRunSimplyFinished(t *testing.T) {
	// A run that reached the end was not stopped, and telling every finished job
	// that it is stopping makes the line worthless on the one occasion it means
	// something.
	var out syncBuffer
	_, give := interruptible(context.Background(), &out)
	give()

	if got := out.String(); got != "" {
		t.Errorf("a run that finished on its own printed %q", got)
	}
}

func TestSecondInterrupt_EndsTheProcessWithoutWaitingForTheShutdown(t *testing.T) {
	// A graceful shutdown that cannot itself be interrupted is a hang, and the
	// user pressing Ctrl+C a second time has stopped asking politely.
	again := make(chan os.Signal, 1)
	killed := make(chan struct{})
	go insist(again, nil, func() { close(killed) })

	again <- os.Interrupt
	select {
	case <-killed:
	case <-time.After(5 * time.Second):
		t.Error("a second interrupt did not end the process")
	}
}

func TestSecondInterrupt_DoesNotEndAProcessThatHasAlreadyStoppedOnItsOwn(t *testing.T) {
	// The watch is given up when the run winds up. Killing then would turn every
	// ordinary exit into one, losing whatever the shutdown was still doing.
	stopped := make(chan struct{})
	close(stopped)
	insist(make(chan os.Signal), stopped, func() {
		t.Error("the process was killed after the run had already stopped")
	})
}

// halfDoneJob opens a history holding a job of two queries with the first one
// recorded, which is the shape an interrupted run leaves behind.
func halfDoneJob(t *testing.T) (*store.Store, int64, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, recordedJob(t, s), dir
}

func TestSettling_GivesAStoppedRunALiveContextWithADeadlineOfItsOwn(t *testing.T) {
	// The reads that produce the report cannot run on the context the
	// interruption ended. They need one that is alive, and one that ends by
	// itself, or a run the user stopped hangs in its own shutdown instead.
	stopped, stop := context.WithCancel(context.Background())
	stop()

	ctx, done := settling(stopped)
	defer done()

	if err := ctx.Err(); err != nil {
		t.Errorf("the winding up of a stopped run has nothing to read with: %v", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Error("nothing bounds the winding up, so a stopped run need never stop")
	}
}

func TestSettling_LeavesARunThatWasNotStoppedOnTheContextItRanOn(t *testing.T) {
	// A run nobody interrupted is still interruptible while it writes its
	// export, and detaching it here would take that away.
	live, stop := context.WithCancel(context.Background())
	defer stop()

	ctx, done := settling(live)
	defer done()
	stop()

	if ctx.Err() == nil {
		t.Error("the winding up carried on after the run it belongs to was ended")
	}
}

func TestFinish_SaysHowToTakeUpAJobTheUserStoppedThoughItsContextIsDead(t *testing.T) {
	// Measured on two live runs: the settling read ran on the context the
	// interruption had just ended, so the run died on "reading pending queries:
	// context canceled" and the line written for exactly this moment — how many
	// are left and the command that takes them up — could not print on the one
	// occasion it exists for.
	s, id, dir := halfDoneJob(t)
	path := filepath.Join(dir, "out.csv")

	stopped, stop := context.WithCancel(context.Background())
	stop()

	var out bytes.Buffer
	err := finish(stopped, &out, s, plan{id: id, spec: store.JobSpec{Name: "j"}},
		runOptions{Out: path, Format: "csv"}, stopped.Err())

	if !errors.Is(err, errStopped) {
		t.Errorf("a run the user stopped returned %v, want an outcome that says it was stopped", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("the cancellation was reported to the user as a fault: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "still to do: 1") {
		t.Errorf("the stopped run does not say how much is left:\n%s", got)
	}
	if !strings.Contains(got, "-resume") {
		t.Errorf("the stopped run does not say what takes up the rest:\n%s", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the stopped run wrote no export of what it did reach: %v", err)
	}
	if _, err := s.LastUnfinished(context.Background(), "j"); err != nil {
		t.Errorf("the stopped job is no longer there to take up: %v", err)
	}
}

func TestFinish_KeepsTheReasonARunThatNeverGotGoingFailed(t *testing.T) {
	// A run that could not open its ports has nothing to settle: no results to
	// write out, and a job the next resume finds by itself. An empty export and
	// a hint about resuming would bury the reason it failed.
	s, id, dir := halfDoneJob(t)
	path := filepath.Join(dir, "out.csv")
	refused := errors.New("this run would not get through")

	var out bytes.Buffer
	err := finish(context.Background(), &out, s, plan{id: id, spec: store.JobSpec{Name: "j"}},
		runOptions{Out: path, Format: "csv"}, refused)

	if !errors.Is(err, refused) {
		t.Errorf("finish returned %v, want the reason the run never got going", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("a run that never got going wrote an export anyway")
	}
}

func TestExitStatus_TellsAJobTheUserStoppedApartFromOneThatFailed(t *testing.T) {
	// Whatever started the run reads the status and nothing else. A stopped job
	// that exits nought reads as one that finished, and one that exits like a
	// fault sends somebody looking for a fault.
	if got := exitCode(errors.Join(errStopped, nil)); got != interruptExit {
		t.Errorf("a stopped job exits %d, want %d", got, interruptExit)
	}
	if got := exitCode(errors.New("the history could not be opened")); got != 1 {
		t.Errorf("a failed job exits %d, want 1", got)
	}
}

func TestResumedPlan_KeepsTheNumberEachQueryHadInTheOriginalList(t *testing.T) {
	// A job taken up part way holds only what is left of it. Numbering those
	// from zero would file every result against the wrong query: the first one
	// still to do would be recorded as the one that is already done.
	dir := t.TempDir()
	db := filepath.Join(dir, "h.db")
	startedJob(t, db, "nightly")

	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	p, err := resumedPlan(context.Background(), s, "nightly")
	if err != nil {
		t.Fatalf("resumedPlan: %v", err)
	}
	if !slices.Equal(p.queries, []string{"b", "c"}) {
		t.Errorf("the plan holds %q, want the two queries that were never run", p.queries)
	}
	if !slices.Equal(p.ordinals, []int{1, 2}) {
		t.Errorf("the plan numbers them %v, want the 1 and 2 they had in the list", p.ordinals)
	}
}
