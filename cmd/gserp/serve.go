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
	"net/http"
	"os"

	"github.com/blanktrail/google-serp-parser/api"
	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/run"
	"github.com/blanktrail/google-serp-parser/store"
	"github.com/blanktrail/google-serp-parser/web"
)

// defaultServeAddr is where the interface listens when the caller names no
// address. It is loopback: the history is one machine's own, and a default that
// answered the network would put it on every one this machine is attached to.
const defaultServeAddr = "127.0.0.1:8080"

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
	fs.IntVar(&opts.Threads, "threads", 2, "queries taken at once")
	fs.IntVar(&opts.Ports, "ports", 3, "ports per thread")
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
	sup, pool := opts.jobs(ctx, out, st)
	if sup != nil {
		defer func() { _ = sup.Close() }()
	}

	log := opts.logger(os.Stderr)
	pages, err := web.New(web.Config{Store: st, Logger: log, Supervisor: sup})
	if err != nil {
		_ = ln.Close()
		return err
	}
	programs, err := opts.programmable(st, log, sup, pool)
	if err != nil {
		_ = ln.Close()
		return err
	}
	_, _ = fmt.Fprintf(out, "gserp is listening on http://%s — open that in a browser\n", ln.Addr())
	return pages.ServeHandler(ctx, ln, mount(pages.Handler(), programs.Handler()))
}

// programmable builds the interface a program reads, on the history, the queue
// and the log the pages already have.
//
// Sharing those is the whole of it. One history is why a job set up by a program
// is the job the browser lists, and one queue is why the machine runs one job at
// a time however that job was asked for. Two of either would be two products in
// one process, agreeing on nothing but the port.
func (o serveOptions) programmable(st *store.Store, log *slog.Logger,
	sup *web.Supervisor, pool *blanktrail.Pool) (*api.Server, error) {
	cfg := api.Config{Store: st, Logger: log}
	// A supervisor that was never built is left out rather than passed on. The
	// interface asks whether it has a queue at all, and a pointer that is nil put
	// into an interface answers that question yes: it would take a job and fail
	// on it instead of saying plainly that it can run nothing.
	if sup != nil {
		cfg.Supervisor = sup
	}
	// A search answered inside the request goes to the identities directly rather
	// than through the queue, so it is handed the pool the jobs run on and not the
	// queue in front of it. It competes for ports with a running job, which is why
	// it is capped at the number of ports there are.
	if pool != nil {
		cfg.Search = api.SearchConfig{
			Searcher: &run.Attempt{Pool: pool},
			Ports:    o.Threads * o.Ports,
		}
	}
	return api.New(cfg)
}

// browserPolls are the addresses the job page's own script calls. They have sat
// under /api/ since before anything else did, and they are named here so that
// mounting the programmable interface under that prefix does not take the job
// page's buttons away.
var browserPolls = []string{"/api/progress", "/api/stop", "/api/resume"}

// mount puts the two interfaces on one address: the pages at the root, and the
// programmable interface under the two prefixes it is documented at.
//
// An address under /api/ that neither of them serves is still the programmable
// interface's to refuse. A program that mistyped one is answered in the single
// shape it parses every refusal in, rather than with a page written for somebody
// reading a screen.
//
// A pattern naming a whole path beats a pattern naming a prefix, whichever was
// registered first, so the three above stay with the pages.
func mount(pages, programs http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", pages)
	mux.Handle("/api/", programs)
	mux.Handle("/search", programs)
	for _, at := range browserPolls {
		mux.Handle(at, pages)
	}
	return mux
}

// jobs opens the ports the interface will run jobs on, or says why it will only
// be able to show what is already there.
//
// The pool comes back beside the queue because a search answered inside a
// request does not go through that queue: it takes a port of its own from the
// same set, and it can only do that if it is handed them. The supervisor closes
// the pool when it closes, so both ways in end together and neither is left
// searching on identities that have been given up.
//
// Neither a missing key nor ports that would not open is a reason to refuse to
// start. Half of what this interface does is read a history, that half needs
// nothing opened, and a machine with no key is a machine somebody is reading a
// history on. Refusing there would take away the part that still works.
//
// The key is read from the environment and is not a flag, for the reason it is
// not one anywhere else in this command: a key on a command line is a key in the
// shell history and in the process list of everyone on the machine.
func (o serveOptions) jobs(ctx context.Context, out io.Writer, st *store.Store) (*web.Supervisor, *blanktrail.Pool) {
	if os.Getenv(envAPIKey) == "" {
		_, _ = fmt.Fprintf(out, "%s is not set: this interface will show the history and cannot run a job\n",
			envAPIKey)
		return nil, nil
	}
	pool, err := openPool(ctx, out, poolConfig(o.Threads, o.Ports))
	if err != nil {
		_, _ = fmt.Fprintf(out,
			"no ports could be opened, so this interface will show the history and cannot run a job: %s\n",
			o.clean(err.Error()))
		_, _ = fmt.Fprintln(out, "gserp doctor prints the whole report")
		return nil, nil
	}
	return web.NewSupervisor(st, pool, o.Threads), pool
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
func (o serveOptions) clean(text string) string {
	return scrubDB(text, o.DB)
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
