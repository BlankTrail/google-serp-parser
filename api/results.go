// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/store"
)

// ndjsonType is what a stream of lines is called.
//
// It is deliberately not application/json. A reader told it has JSON hands the
// whole body to a parser, which fails on the second line; a reader told it has
// this reads a line, parses it, and starts on the next, which is the shape that
// makes a job of a million rows answerable at all.
const ndjsonType = "application/x-ndjson; charset=utf-8"

// rowJSON is one captured result as a line of the stream. The names are the
// names an export writes, so a row pulled from a file and a row pulled from
// this interface are the same row.
type rowJSON struct {
	Ordinal int    `json:"ordinal"`
	Query   string `json:"query"`
	Page    int    `json:"page"`
	Rank    int    `json:"rank"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Host    string `json:"host"`
	Snippet string `json:"snippet"`
}

// positionJSON is one place a site held on one run.
type positionJSON struct {
	JobID   int64     `json:"job_id"`
	JobName string    `json:"job_name"`
	TakenAt time.Time `json:"taken_at"`
	Query   string    `json:"query"`
	Rank    int       `json:"rank"`
	URL     string    `json:"url"`
}

// resultRoutes registers the two addresses that hand over what was captured.
// Both are behind the key check; either one left open hands the whole history
// to anybody who asks.
func (s *Server) resultRoutes() {
	s.mux.HandleFunc("GET /api/v1/jobs/{id}/results", s.authed(s.jobResults))
	s.mux.HandleFunc("GET /api/v1/history", s.authed(s.siteHistory))
}

// jobResults hands over every result a job captured, one object to a line.
//
// The job is found before a byte of the body goes out, and that order is the
// whole of it. A status is sent once and cannot be taken back, so a refusal
// discovered half way down leaves the caller holding a 200 and a body that
// stops — which reads as a job that captured nothing, and is believed.
//
// Nothing is gathered on the way. The answer is as large as the job behind it,
// and a caller reading it sees the first rows while the last are still being
// read out of the history.
func (s *Server) jobResults(w http.ResponseWriter, r *http.Request) {
	sum, ok := s.jobAsked(w, r)
	if !ok {
		return
	}
	lines := s.beginStream(w)
	err := s.store.Rows(r.Context(), sum.ID, func(row store.Row) error {
		return lines.Encode(rowJSON{
			Ordinal: row.Ordinal,
			Query:   row.Query,
			Page:    row.Page,
			Rank:    row.Rank,
			Title:   row.Title,
			URL:     row.URL,
			Host:    row.Host,
			Snippet: row.Snippet,
		})
	})
	if err != nil {
		// The status is out and part of the body with it, so there is nothing
		// left to tell the caller. The log is where this has to be visible.
		s.log.Error("a job's results stopped part way through", "job", sum.ID, "error", err)
	}
}

// siteHistory hands over every position a site has held, oldest run first.
//
// A site with no positions is answered with nothing, at 200. That a site has
// never ranked is an answer and not a missing thing: a rank tracker is asked
// about sites that do not rank yet as a matter of course, and a caller that
// treats a refusal as a fault would report one where nothing is wrong.
func (s *Server) siteHistory(w http.ResponseWriter, r *http.Request) {
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	if host == "" {
		// Answering nothing would be an answer the caller believes: they would
		// read it as a site that never ranked and never learn they asked wrongly.
		writeError(w, http.StatusBadRequest, "name the site to read the history of, as host=example.com")
		return
	}
	lines := s.beginStream(w)
	err := s.store.History(r.Context(), host, func(p store.Position) error {
		return lines.Encode(positionJSON{
			JobID:   p.JobID,
			JobName: p.JobName,
			TakenAt: p.TakenAt,
			Query:   p.Query,
			Rank:    p.Rank,
			URL:     p.URL,
		})
	})
	if err != nil {
		s.log.Error("a site's history stopped part way through", "host", host, "error", err)
	}
}

// beginStream commits the answer and hands back what the lines go through.
//
// Committing here is what makes every refusal above it final: after this call
// the status has been chosen, and everything that follows can only be more
// body. The encoder writes each object and its line ending in one call, so a
// line is either on the wire whole or not at all, and a transfer cut short is
// worth every line before the break.
//
// Nothing is flushed by hand. The server between here and the socket holds a
// few kilobytes and empties itself, which for an answer read out of a database
// as fast as it can be written costs a caller nothing and saves a job of a
// million rows a million system calls.
func (s *Server) beginStream(w http.ResponseWriter) *json.Encoder {
	w.Header().Set("Content-Type", ndjsonType)
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w)
}
