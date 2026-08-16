// SPDX-License-Identifier: MIT

// Package api serves the programmable interface: the same history and the same
// searches the browser pages show, in a shape another program can read.
//
// It sits beside the browser interface rather than on top of it. The pages know
// nothing of this package, and the command that starts the process mounts both
// against the same history, so a job set up by a program and a job set up by
// hand are the same job.
//
// The one thing this package takes from the browser interface is the queue jobs
// are set running through, because a second queue would not be a second way in:
// it would be a second job running on the same machine, and one job at a time is
// what the whole of the running was measured and built around.
//
// Everything this package refuses with is one shape: an object with a single
// error field. A program reading it parses that once and is done.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/blanktrail/google-serp-parser/store"
)

// Config is what the server needs to run.
type Config struct {
	// Store holds the keys, the jobs and their results. Required.
	Store *store.Store
	// Logger is where the server says what went wrong. It defaults to the
	// process logger, which stamps each line and can be pointed elsewhere.
	Logger *slog.Logger
	// Supervisor is the queue jobs are set running through, and it is the queue
	// the browser uses. A server built without one serves the history and
	// refuses to start anything, which is what a reader of a history on another
	// machine gets.
	Supervisor Supervisor
}

// Server is the programmable interface.
//
// Everything it holds is settled in New and only read afterwards, so the one
// server answers every request at once without anything of its own to guard.
// The history behind it is safe to use from several goroutines and serialises
// its own writes.
type Server struct {
	store *store.Store
	log   *slog.Logger
	sup   Supervisor
	mux   *http.ServeMux
}

// New builds the server.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("api: a server needs a store")
	}
	s := &Server{
		store: cfg.Store,
		log:   cfg.Logger,
		sup:   cfg.Supervisor,
		mux:   http.NewServeMux(),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.routes()
	return s, nil
}

// routes registers what this interface answers.
//
// The catch-all is registered here rather than left to the multiplexer's own
// answer, which is a line of plain text. A program that parses one shape for
// every refusal should not meet another one the first time it mistypes an
// address.
func (s *Server) routes() {
	s.jobRoutes()
	s.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})
}

// Handler is the server's routes, so a caller can mount them and a test can
// drive them without a socket.
func (s *Server) Handler() http.Handler { return s.mux }

// errorJSON is the single shape this interface refuses in.
type errorJSON struct {
	Error string `json:"error"`
}

// writeError refuses, saying what a caller can act on and nothing more.
//
// The sentence describes the answer, never the state of the machine behind it:
// which record was missing, which query failed and which file it sat in are for
// the log, where the operator reads them and a stranger does not.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// The header is already out, so a write that fails here leaves nothing to
	// tell the caller.
	_ = json.NewEncoder(w).Encode(errorJSON{Error: msg})
}
