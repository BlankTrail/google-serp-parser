// SPDX-License-Identifier: MIT

// Package web serves the browser interface: a list of what has been run, and
// what each job managed.
//
// Everything it serves lives inside the binary. There is no asset directory to
// deploy alongside it and no build step beyond go build.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"time"

	"github.com/blanktrail/google-serp-parser/store"
)

//go:embed assets
var assets embed.FS

// layoutFile is the frame every page is drawn in. It is named here because it
// is parsed alongside each page rather than being a page of its own.
const layoutFile = "layout.html"

// shutdownGrace bounds the wind-down. It is a deadline of its own rather than
// the caller's, because the answer going out when the caller pressed stop is a
// page a browser has already begun drawing, and ending it at that instant hands
// the reader half of one.
const shutdownGrace = 15 * time.Second

// readHeaderGrace is how long a client has to finish sending its request line
// and headers. Without it a connection that says nothing holds its slot for as
// long as the process runs.
const readHeaderGrace = 10 * time.Second

// Config is what the server needs to run.
type Config struct {
	// Store holds the jobs. Required.
	Store *store.Store
	// Logger is where the server says what went wrong. It defaults to the
	// process logger, which stamps each line and can be pointed elsewhere;
	// a server whose only account of itself is unstamped text on standard
	// output is a server nobody can operate.
	Logger *slog.Logger
	// Supervisor runs the jobs. A server built without one shows the history
	// and refuses to start anything, which is what a reader of a history on
	// another machine gets.
	Supervisor *Supervisor
}

// Server is the browser interface.
type Server struct {
	store *store.Store
	log   *slog.Logger
	sup   *Supervisor
	pages map[string]*template.Template
	mux   *http.ServeMux
}

// New builds the server and parses its pages once.
//
// The pages are parsed here rather than per request: a broken template is a
// mistake in this repository, not in the user's data, and it should stop the
// program at startup rather than render half a page an hour later.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("web: a server needs a store")
	}
	if err := checkCatalogue(catalogue); err != nil {
		return nil, err
	}
	pages, err := parsePages()
	if err != nil {
		return nil, err
	}
	s := &Server{
		store: cfg.Store,
		log:   cfg.Logger,
		sup:   cfg.Supervisor,
		pages: pages,
		mux:   http.NewServeMux(),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.routes()
	return s, nil
}

// parsePages parses each page together with the layout, into a set of its own.
//
// One set per page, because a set holds one template per name: two pages both
// filling in the layout's body would collide there, and the last one parsed
// would answer for both.
func parsePages() (map[string]*template.Template, error) {
	names, err := fs.Glob(assets, "assets/*.html")
	if err != nil {
		return nil, fmt.Errorf("web: looking for the pages: %w", err)
	}
	pages := make(map[string]*template.Template)
	for _, name := range names {
		base := path.Base(name)
		if base == layoutFile {
			continue
		}
		// The layout comes first so the page's own body replaces the empty one
		// the layout declares.
		tpl, err := template.New(layoutFile).ParseFS(assets, "assets/"+layoutFile, name)
		if err != nil {
			return nil, fmt.Errorf("web: parsing %s: %w", base, err)
		}
		pages[base] = tpl
	}
	if len(pages) == 0 {
		return nil, errors.New("web: no pages were built into this binary")
	}
	return pages, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("GET /new", s.newJob)
	s.mux.HandleFunc("POST /new", s.createJob)
	s.mux.HandleFunc("GET /job/{id}", s.job)
	s.mux.HandleFunc("GET /history", s.history)
	s.mux.HandleFunc("GET /export", s.download)
	// What the job page polls, and what its two buttons send. Both buttons are
	// registered for post alone, so a browser prefetching a link, or anything
	// else that walks one, is answered with a refusal rather than with somebody
	// else's job ending.
	s.mux.HandleFunc("GET /api/progress", s.apiProgress)
	s.mux.HandleFunc("POST /api/stop", s.apiStop)
	s.mux.HandleFunc("POST /api/resume", s.apiResume)
	// One path element, so a name can never walk out of the directory it is
	// looked up in.
	s.mux.HandleFunc("GET /assets/{file}", s.asset)
}

// Handler is the server's routes, so a test can drive them without a socket.
func (s *Server) Handler() http.Handler { return s.mux }

// ServeHandler runs the given handler on this server's socket and wind-down,
// until the context is cancelled.
//
// It exists because the process serves more than these pages: the programmable
// interface answers on the same address, and whoever mounts the two writes the
// router that tells them apart. What happens to a page half sent when somebody
// presses stop is decided here and nowhere else — written a second time in the
// command, it would be two answers to one question.
//
// It takes a listener rather than an address so a caller can hand it a port the
// system chose. A test that had to name a port in advance would race whatever
// else on the machine wanted it.
//
// The goroutine below shares two things with this one: the http.Server, whose
// Shutdown is documented to be called while Serve runs, and the idle channel,
// which is closed on one side and received on the other. It returns only after
// that receive, so the caller is never told the server has stopped while a page
// is still going out.
func (s *Server) ServeHandler(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderGrace,
	}
	idle := make(chan struct{})
	go func() {
		defer close(idle)
		<-ctx.Done()
		stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(stop); err != nil {
			s.log.Error("the wind-down did not finish in time", "error", err)
		}
	}()

	err := srv.Serve(ln)
	<-idle
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Serve runs these pages and nothing else, which is what a test of them wants
// and what a process serving only them would ask for.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	return s.ServeHandler(ctx, ln, s.mux)
}

// page is what every template is handed, whatever else the page carries. The
// layout can then count on the language and the switcher being there.
type page struct {
	// Lang is the language the page is written in.
	Lang Lang
	// Title is the key of the page's name, not the name itself: the layout
	// translates it like every other phrase.
	Title string
	// Langs is the switcher, offering this same address in each language.
	Langs []langLink
}

// T is how a template asks for a phrase. Templates name a key and never a
// language, so the same markup serves every reader.
func (p page) T(key string) string { return p.Lang.T(key) }

// frame builds the part of a page that does not depend on what is on it.
func frame(r *http.Request, lang Lang, title string) page {
	return page{Lang: lang, Title: title, Langs: switcher(r, lang)}
}

// indexPage is the job list.
type indexPage struct {
	page
	Jobs []store.JobSummary
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	lang := rememberLang(w, r)
	jobs, err := s.store.Jobs(r.Context(), 0)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "index.html", indexPage{page: frame(r, lang, "jobs.title"), Jobs: jobs})
}

// render writes a page, and says so plainly when it cannot.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	tpl := s.pages[name]
	if tpl == nil {
		s.fail(w, r, fmt.Errorf("web: no page named %s", name))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, layoutFile, data); err != nil {
		// The header is already out and part of the page with it, so nothing can
		// be done for the reader but stop writing. The log is where this has to
		// be visible.
		s.log.Error("a page could not be finished", "page", name, "path", r.URL.Path, "error", err)
	}
}

// fail tells the reader a sentence and the log the reason.
//
// The reason stays behind: a page quoting the query or the file behind it hands
// whoever is reading facts about the machine, and helps them not at all.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	http.Error(w, "The server could not read its own records. The reason is in its log.",
		http.StatusInternalServerError)
	s.log.Error("a page could not be built", "path", r.URL.Path, "error", err)
}

// contentTypes is what a browser is told about each kind of file this program
// ships, and the only extensions it will hand out at all.
//
// The types are named rather than looked up because the lookup asks the
// operating system: on a machine where something else has claimed .css, the
// stylesheet arrives as plain text and every page loads unstyled.
var contentTypes = map[string]string{
	".css": "text/css; charset=utf-8",
	".js":  "text/javascript; charset=utf-8",
	".svg": "image/svg+xml",
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	kind, served := contentTypes[path.Ext(name)]
	if !served {
		http.NotFound(w, r)
		return
	}
	body, err := fs.ReadFile(assets, "assets/static/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", kind)
	_, _ = w.Write(body)
}
