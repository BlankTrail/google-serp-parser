// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/settings"
	"github.com/blanktrail/google-serp-parser/store"
	"github.com/blanktrail/google-serp-parser/web"
)

// defaultServeAddr is where the interface listens when the caller names no
// address. It is loopback: the history is one machine's own, and a default that
// answered the network would put it on every one this machine is attached to.
const defaultServeAddr = "127.0.0.1:8080"

// The pool a job runs on when neither the caller nor this machine's own
// settings say otherwise. They are the run command's defaults, so a job set up
// in the browser costs what the same job costs from the command line.
const (
	defaultThreads = 2
	defaultPorts   = 3
)

// settingsName is what the file the interface saves its settings in is called.
// It sits beside the history rather than inside it, so a history that is
// copied, handed over or backed up does not carry the connection with it.
const settingsName = "gserp-settings.json"

// translationsName is the directory a language is added in, beside the program
// rather than beside the history: it is part of what this copy of the program
// says, and a history carried to another machine should not bring somebody's
// half-finished translation along with it.
const translationsName = "gserp-translations"

// serveOptions is everything the command was asked to do.
type serveOptions struct {
	Addr    string
	DB      string
	Threads int
	Ports   int
}

// serveFlags declares the flags. It is separate from the parsing so the help
// text can be checked against the flags themselves rather than against a list
// somebody has to remember to keep up.
//
// The two numbers are the run command's own defaults, so a job set up in the
// browser costs what the same job costs from the command line.
func serveFlags(opts *serveOptions) *flag.FlagSet {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&opts.Addr, "addr", defaultServeAddr, "address to listen on")
	fs.StringVar(&opts.DB, "db", "gserp.db", "history database to open")
	fs.IntVar(&opts.Threads, "threads", defaultThreads, "queries taken at once")
	fs.IntVar(&opts.Ports, "ports", defaultPorts, "ports per thread")
	return fs
}

// serveCommand serves the browser interface until it is stopped.
func serveCommand(ctx context.Context, args []string, out io.Writer) error {
	var opts serveOptions
	fs := serveFlags(&opts)
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The interruption is heard here rather than in main, as it is for a run:
	// a test ends the context this one is built on and gets the same path a
	// user gets, without raising a signal at the process running the tests.
	ctx, stop := interruptible(ctx, out)
	defer stop()
	return serveInterface(ctx, out, opts)
}

// serveInterface opens the history, takes the address and serves until the
// context ends.
func serveInterface(ctx context.Context, out io.Writer, opts serveOptions) error {
	st, err := store.Open(opts.DB)
	if err != nil {
		return opts.scrubbed(err)
	}
	// Closed only after Serve has returned, which it does not do while a page is
	// still going out — so no request is ever left reading a history that has
	// been shut underneath it.
	defer func() { _ = st.Close() }()

	// The address is taken here rather than inside the server so that the line
	// printed below names the port the system actually gave. A caller who asked
	// for port zero has no other way to learn it. It is also taken before the
	// ports below are opened, which take a while: a browser opened alongside
	// this command waits on a socket that is already there rather than being
	// refused and shown an error page.
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return opts.scrubbed(err)
	}

	// Read before the server is built, because the languages it finds are the
	// ones the switcher offers on the first page that goes out. Where this
	// program is, is something only the system can say; without that there is
	// no directory to look in, and both built-in languages are there already.
	if exe, err := os.Executable(); err == nil {
		opts.translate(out, filepath.Join(filepath.Dir(exe), translationsName))
	}

	// Closed before the history, because a job it ends is written down as it
	// lets go of it. Close waits for that.
	sup := opts.jobs(ctx, out, st)
	defer func() { _ = sup.Close() }()

	srv, err := web.New(web.Config{
		Store:        st,
		Logger:       opts.logger(os.Stderr),
		Supervisor:   sup,
		SettingsPath: opts.settingsPath(),
		Connect:      opts.connect,
	})
	if err != nil {
		_ = ln.Close()
		return err
	}
	_, _ = fmt.Fprintf(out, "gserp is listening on http://%s — open that in a browser\n", ln.Addr())
	return srv.Serve(ctx, ln)
}

// jobs opens the ports the interface will run jobs on, or says why it has none
// yet.
//
// There is always a supervisor. Neither a missing key nor ports that would not
// open is a reason to refuse to start: half of what this interface does is read
// a history and that half needs nothing opened. What such a server has is a
// supervisor with nothing to run on, which is what lets a connection set up in
// the browser be taken into use without the process being started again.
//
// The key is read from the environment and is not a flag, for the reason it is
// not one anywhere else in this command: a key on a command line is a key in the
// shell history and in the process list of everyone on the machine. It is also
// read from the settings, because a connection set up in a browser that had to
// be set up again after every restart is one nobody would trust.
func (o serveOptions) jobs(ctx context.Context, out io.Writer, st *store.Store) *web.Supervisor {
	saved, configured := o.saved(out)
	threads, ports := o.runOn(saved, configured)

	var pool *blanktrail.Pool
	var err error
	switch {
	case os.Getenv(envAPIKey) != "":
		// What this process was started with wins. Somebody who put a key in the
		// environment for this run meant it for this run.
		pool, err = openPool(ctx, out, poolConfig(threads, ports))
	case saved.APIKey != "":
		pool, err = o.dial(ctx, saved, threads, ports)
	default:
		_, _ = fmt.Fprintf(out,
			"%s is not set and no connection has been saved: this interface shows the history and runs nothing until the connection is set up in it\n",
			envAPIKey)
		return web.NewSupervisorWithoutAPool(st)
	}
	if err != nil {
		_, _ = fmt.Fprintf(out,
			"no ports could be opened, so this interface shows the history and runs nothing until the connection is set up in it: %s\n",
			o.clean(err.Error()))
		_, _ = fmt.Fprintln(out, "gserp doctor prints the whole report")
		return web.NewSupervisorWithoutAPool(st)
	}
	return web.NewSupervisor(st, pool, threads)
}

// connect opens the ports a connection just saved in the browser describes.
//
// It is what the interface is handed to build a pool with, and it is written
// here rather than there because how many ports a run opens, how long they rest
// and what they are opened as is this command's to decide — the same decision
// the estimate on the form is worked out from.
//
// The numbers are the ones just saved rather than the ones this process was
// started with: somebody who has this moment typed a port count into the
// settings has said which they want, and a flag from last week that quietly won
// would make the box on the screen a box that does nothing.
func (o serveOptions) connect(ctx context.Context, saved settings.Settings) (*blanktrail.Pool, error) {
	return o.dial(ctx, saved, saved.Threads, saved.Ports)
}

// dial opens a pool against the connection described, after the check that says
// whether it would get through at all.
//
// The check is the one gserp doctor runs, for the reason it is run before a run
// from the command line: the most common way a job is dead on arrival is one a
// single request would have shown.
func (o serveOptions) dial(ctx context.Context, saved settings.Settings, threads, ports int) (*blanktrail.Pool, error) {
	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		return nil, o.scrubbed(err)
	}
	if threads < 1 {
		threads = o.Threads
	}
	if ports < 1 {
		ports = o.Ports
	}
	cfg := poolConfig(threads, ports)
	if saved.Cooldown > 0 {
		cfg.Cooldown = saved.Cooldown
	}

	report := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: checkedDomains,
		Ports:   cfg.Size(),
	})
	if !report.OK() {
		return nil, errors.New("this connection would not get through; the settings page says what the check found")
	}
	cfg.Client = client
	cfg.CA = report.CA

	if saved.Proxy.Kind != "" {
		// The list is loaded here and reloaded on its own interval afterwards, so a
		// list that changes during a run is a list this pool follows.
		rotor, err := blanktrail.NewRotor(ctx, listFrom(saved.Proxy))
		if err != nil {
			return nil, o.scrubbed(err)
		}
		cfg.Channels = []blanktrail.Channel{blanktrail.NewListChannel("list", rotor)}
	}

	pool, err := blanktrail.NewPool(ctx, cfg)
	if err != nil {
		return nil, o.scrubbed(err)
	}
	return pool, nil
}

// listScheme is how an address is reached when its line does not say. Lists are
// mostly sold without one, and this is what the rest of this command has always
// assumed of them.
const listScheme = "socks5"

// listFrom is where the addresses come from, as the settings describe it.
//
// It is one function rather than a few lines at the place a pool is opened
// because every field of it is a box somebody filled in: a place that quietly
// read its own interval, or its own idea of file or address, would leave a box
// on the settings page that changes nothing and says nothing about it.
func listFrom(p settings.ProxySource) blanktrail.Source {
	return blanktrail.Source{
		Kind:          p.Kind,
		Location:      p.Location,
		Refresh:       p.Refresh,
		DefaultScheme: listScheme,
	}
}

// saved is what was set in the browser, and whether anything was.
//
// The file does not exist until somebody opens the settings, and a machine
// nobody has configured has to run on what it was started with: answering there
// with the settings package's own defaults would open a pool of a size nobody
// on this machine ever asked for.
//
// A file that is there and cannot be read is said out loud rather than passed
// over. It is the file somebody's connection is kept in, and a program that
// quietly ran without it would look like one that had lost their settings.
func (o serveOptions) saved(out io.Writer) (settings.Settings, bool) {
	path := o.settingsPath()
	if _, err := os.Stat(path); err != nil {
		return settings.Settings{}, false
	}
	s, err := settings.Load(path)
	if err != nil {
		_, _ = fmt.Fprintf(out,
			"the saved settings could not be read, so this interface runs on what it was started with: %s\n",
			o.clean(err.Error()))
		return settings.Settings{}, false
	}
	return s, true
}

// settingsPath is the file the connection is kept in: beside the history rather
// than inside it, so a history that is copied, handed over or backed up does not
// carry the connection with it.
func (o serveOptions) settingsPath() string {
	return filepath.Join(filepath.Dir(o.DB), settingsName)
}

// translate reads the languages kept beside this program and says what came of
// it.
//
// Every line here is a report about somebody else's files, so none of them
// stops the command: a translation that could not be read leaves the interface
// speaking what it was built to speak, which is what it does on the machines
// that have no such directory at all — nearly all of them.
//
// A language short of phrases is named out loud with the count. It is the one
// thing about a translation that is invisible from inside the browser: the page
// renders, in English, and the reader who asked for the other language has no
// way of telling a missing phrase from a decision.
func (o serveOptions) translate(out io.Writer, dir string) {
	added, incomplete, err := web.LoadTranslations(dir)
	if err != nil {
		_, _ = fmt.Fprintf(out, "a translation beside this program could not be read, and the rest are in use: %s\n",
			o.clean(err.Error()))
	}
	for _, lang := range added {
		_, _ = fmt.Fprintf(out, "this interface also answers in %s, from a file beside the program\n", lang)
	}
	for _, lang := range slices.Sorted(maps.Keys(incomplete)) {
		_, _ = fmt.Fprintf(out, "the %s translation is short of %d phrases, which are shown in English: %s\n",
			lang, len(incomplete[lang]), strings.Join(incomplete[lang], " "))
	}
}

// runOn is how many queries this interface takes at once and how many ports
// each of them gets.
//
// A number the caller moved off its default is one they meant for this run, and
// it stands. A number they left alone is one they left to whatever this machine
// is set up with, and what it is set up with is the file the settings are saved
// in — which is what makes a change made in a browser outlive the restart.
func (o serveOptions) runOn(saved settings.Settings, configured bool) (threads, ports int) {
	threads, ports = o.Threads, o.Ports
	if !configured {
		return threads, ports
	}
	if threads == defaultThreads && saved.Threads > 0 {
		threads = saved.Threads
	}
	if ports == defaultPorts && saved.Ports > 0 {
		ports = saved.Ports
	}
	return threads, ports
}

// logger is where the server says what went wrong.
//
// It goes to standard error, stamped and levelled, rather than onto standard
// output among the lines written for a person to read: the two are read by
// different things, and only one of them can be redirected away.
func (o serveOptions) logger(w io.Writer) *slog.Logger {
	return slog.New(&scrubbing{Handler: slog.NewTextHandler(w, nil), clean: o.clean})
}

// clean takes out of a line everything this command must not print: the values
// it was given in the environment, and where on this machine the history is
// kept.
//
// The path is taken out in both the shapes an error writes it. A quoted path
// doubles every separator, so on a machine that separates with backslashes the
// quoted form shares no substring with the one handed in and would otherwise go
// out whole.
func (o serveOptions) clean(text string) string {
	quoted := strconv.Quote(o.DB)
	return hideSecrets(shortenPaths(text, o.DB, quoted[1:len(quoted)-1]), secrets()...)
}

// scrubbed rewrites an error so it can be printed.
//
// The chain goes with the path. Nothing above this command tells these errors
// apart, and a wrapped error would carry the original text — the very thing
// being taken out — along inside it.
func (o serveOptions) scrubbed(err error) error {
	return errors.New(o.clean(err.Error()))
}

// scrubbing is a log handler that puts every line through the filter the rest
// of the command prints through. The server behind it is handed a history and a
// logger and knows neither what is secret nor where anything on this machine
// lives, so the filtering belongs on this side of it.
type scrubbing struct {
	slog.Handler
	clean func(string) string
}

func (h *scrubbing) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, h.clean(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.cleanAttr(a))
		return true
	})
	return h.Handler.Handle(ctx, out)
}

func (h *scrubbing) WithAttrs(attrs []slog.Attr) slog.Handler {
	cleaned := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		cleaned[i] = h.cleanAttr(a)
	}
	return &scrubbing{Handler: h.Handler.WithAttrs(cleaned), clean: h.clean}
}

func (h *scrubbing) WithGroup(name string) slog.Handler {
	return &scrubbing{Handler: h.Handler.WithGroup(name), clean: h.clean}
}

// cleanAttr rewrites the values a line can carry a path or a key inside. A
// number, a time or a boolean cannot hold one, and rewriting those would turn
// them into text for nothing.
func (h *scrubbing) cleanAttr(a slog.Attr) slog.Attr {
	switch v := a.Value.Resolve(); v.Kind() {
	case slog.KindString, slog.KindAny:
		return slog.String(a.Key, h.clean(v.String()))
	case slog.KindGroup:
		inner := make([]any, 0, len(v.Group()))
		for _, g := range v.Group() {
			inner = append(inner, h.cleanAttr(g))
		}
		return slog.Group(a.Key, inner...)
	default:
		return a
	}
}
