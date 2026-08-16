// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"

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

	// Closed before the history, because a job it ends is written down as it
	// lets go of it. Close waits for that.
	sup := opts.jobs(ctx, out, st)
	if sup != nil {
		defer func() { _ = sup.Close() }()
	}

	srv, err := web.New(web.Config{Store: st, Logger: opts.logger(os.Stderr), Supervisor: sup})
	if err != nil {
		_ = ln.Close()
		return err
	}
	_, _ = fmt.Fprintf(out, "gserp is listening on http://%s — open that in a browser\n", ln.Addr())
	return srv.Serve(ctx, ln)
}

// jobs opens the ports the interface will run jobs on, or says why it will only
// be able to show what is already there.
//
// Neither a missing key nor ports that would not open is a reason to refuse to
// start. Half of what this interface does is read a history, that half needs
// nothing opened, and a machine with no key is a machine somebody is reading a
// history on. Refusing there would take away the part that still works.
//
// The key is read from the environment and is not a flag, for the reason it is
// not one anywhere else in this command: a key on a command line is a key in the
// shell history and in the process list of everyone on the machine.
func (o serveOptions) jobs(ctx context.Context, out io.Writer, st *store.Store) *web.Supervisor {
	if os.Getenv(envAPIKey) == "" {
		_, _ = fmt.Fprintf(out, "%s is not set: this interface will show the history and cannot run a job\n",
			envAPIKey)
		return nil
	}
	threads, ports := o.runOn(o.saved(out))
	pool, err := openPool(ctx, out, poolConfig(threads, ports))
	if err != nil {
		_, _ = fmt.Fprintf(out,
			"no ports could be opened, so this interface will show the history and cannot run a job: %s\n",
			o.clean(err.Error()))
		_, _ = fmt.Fprintln(out, "gserp doctor prints the whole report")
		return nil
	}
	return web.NewSupervisor(st, pool, threads)
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
	path := filepath.Join(filepath.Dir(o.DB), settingsName)
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
