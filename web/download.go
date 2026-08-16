// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/blanktrail/google-serp-parser/export"
	"github.com/blanktrail/google-serp-parser/store"
)

// exportTypes is what a browser is told a download is.
//
// A format this table has no name for is still handed over, as bytes: what may
// be written is settled by export and by nothing here, and a format added there
// must not come back from this route as a refusal.
var exportTypes = map[string]string{
	"csv":   "text/csv; charset=utf-8",
	"jsonl": "application/x-ndjson; charset=utf-8",
}

// nameLimit is how much of a job's name reaches the header. A job named with a
// pasted paragraph would otherwise put that paragraph in a header line, and
// every proxy between here and the reader has a limit on how long one may be.
const nameLimit = 60

// download writes a job's results to the reader as a file.
//
// The order is the whole of it: the format is settled, then the job is found,
// then the headers go out, and only then the first byte of the body. Every
// refusal there is has to happen before the response is committed, because once
// a byte of a file has gone out the only thing left to do with a mistake is
// stop writing, and a file that stops looks finished to whoever downloaded it.
//
// Nothing is gathered on the way. The export is as large as the job behind it,
// and the reader sees the first rows while the last are still being read.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	out, err := export.New(format, w)
	if err != nil {
		http.Error(w, "That is not a format this program writes.", http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(r.URL.Query().Get("job"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	job, err := s.store.Progress(r.Context(), id)
	if errors.Is(err, store.ErrNoJob) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	kind, named := exportTypes[format]
	if !named {
		kind = "application/octet-stream"
	}
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Disposition", `attachment; filename="`+attachmentName(job, format)+`"`)

	if err := s.stream(r.Context(), out, id); err != nil {
		// The header is out and part of the file with it, so there is nothing
		// left to tell the reader. The log is where this has to be visible.
		s.log.Error("an export stopped part way through", "job", id, "format", format, "error", err)
	}
}

// stream hands every row of a job to the file being written, and stops at the
// first row the file will not take.
//
// The refusal is passed back rather than swallowed, and that is what ends the
// walk: a reader who closed the tab leaves every write failing, and an export
// that reads on regardless spends the whole job on a socket nobody is holding.
func (s *Server) stream(ctx context.Context, out export.Writer, jobID int64) error {
	err := s.store.Rows(ctx, jobID, func(row store.Row) error {
		return out.Write(export.Row{
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
		return err
	}
	// Close is what writes the header of a file that found no rows, so an export
	// of a job that captured nothing is an empty table rather than an empty file
	// that reads as a failure.
	return out.Close()
}

// attachmentName is what the browser saves the export as.
//
// The name is built rather than taken. A job is named by a person, and the name
// lands in a header where a quote closes the name, a semicolon starts the next
// parameter, a backslash escapes what follows it and a line ending ends the
// header altogether. Keeping only what a file name is ordinarily made of settles
// all of them at once, including the ones not listed here.
//
// The date is the job's own, so two runs of the same list a week apart do not
// arrive as one file overwriting the other. A job whose stamp could not be read
// is dated 0001-01-01, which says plainly that the date is unknown rather than
// putting today's in its place.
func attachmentName(job store.JobSummary, format string) string {
	name := headerSafe(job.Name)
	if name == "" {
		name = "export"
	}
	return name + "-" + job.CreatedAt.Local().Format("2006-01-02") + "." + headerSafe(format)
}

// headerSafe keeps the letters, digits and marks a file name is made of, and
// stands a dash where anything else was.
//
// It is a list of what may pass rather than a list of what may not: the second
// kind of list is only ever as complete as whoever last thought about it, and
// the thing being guarded here is a header a person types half of.
func headerSafe(s string) string {
	var b strings.Builder
	dashed := false
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_':
			b.WriteRune(c)
			dashed = false
		case !dashed && b.Len() > 0:
			b.WriteByte('-')
			dashed = true
		}
		if b.Len() >= nameLimit {
			break
		}
	}
	return strings.Trim(b.String(), "-.")
}

// historyPage is one site and every place it has held.
type historyPage struct {
	page
	Host      string
	Positions []store.Position
}

// history draws where one site has stood, oldest run first.
//
// The positions are gathered rather than streamed, unlike an export: a page is
// read by a person, and what a person reads is bounded by that.
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	lang := rememberLang(w, r)
	host := strings.TrimSpace(r.URL.Query().Get("host"))

	var positions []store.Position
	if host != "" {
		err := s.store.History(r.Context(), host, func(p store.Position) error {
			positions = append(positions, p)
			return nil
		})
		if err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.render(w, r, "history.html", historyPage{
		page:      frame(r, lang, "history.title", historyAt),
		Host:      host,
		Positions: positions,
	})
}
