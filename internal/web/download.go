// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/blanktrail/google-serp-parser/internal/export"
	"github.com/blanktrail/google-serp-parser/internal/store"
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
	if !export.Writes(format) {
		http.Error(w, "That is not a format this program writes.", http.StatusBadRequest)
		return
	}
	// What a text file separates its columns with, refused here rather than half
	// way down the file: a separator this program cannot write has to be said
	// before a byte has gone out, because a file that stops looks finished to
	// whoever downloaded it. Every other format ignores it.
	sep, err := export.SeparatorOf(r.URL.Query().Get(separatorField))
	if err != nil {
		http.Error(w, "That cannot separate columns.", http.StatusBadRequest)
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

	// Which part of the job is being asked for. The results are what a download
	// with nothing said means, because that is what a job is for; the other two
	// are offered only by a job that kept them. A part the job never captured is
	// refused rather than handed over empty — and it is refused here, before a
	// byte of the answer has gone out, like every other refusal on this route.
	part, ok := partOf(job, r.URL.Query().Get(partField))
	if !ok {
		http.NotFound(w, r)
		return
	}
	layout := export.DefaultLayout(format, fieldsOf(job, part))
	layout.Sep = sep
	ask := exportAsk{job: job, part: part, layout: layout}

	kind, named := exportTypes[format]
	if !named {
		kind = "application/octet-stream"
	}
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Disposition", `attachment; filename="`+attachmentName(job, format)+`"`)

	if err := s.writePart(r.Context(), w, ask, false); err != nil {
		// The header is out and part of the file with it, so there is nothing
		// left to tell the reader. The log is where this has to be visible.
		s.log.Error("an export stopped part way through", "job", job.ID, "format", format, "error", err)
	}
}

// exportAsk is one file asked for: of which job, which part of it, and how it
// is written.
type exportAsk struct {
	job    store.JobSummary
	part   string
	layout export.Layout
}

// partsOf is what a job can be exported as, the first being what a download
// naming no part gets.
//
// What a job was asked settles what its file answers. An index job is run to
// learn which addresses are held and which are not, and its results hold only
// the first half: walking them would leave every address Google does not hold
// out of the file altogether, indistinguishable from one that was never in the
// list — so its one part is its verdicts. A parse job has its results and
// whatever else of the page it kept.
func partsOf(job store.JobSummary) []string {
	if job.Kind == store.KindIndex {
		return []string{partVerdicts}
	}
	parts := []string{partResults}
	if job.Kind == store.KindParse && job.Fields.Keeps(store.FieldAds) {
		parts = append(parts, partAds)
	}
	if job.Kind == store.KindParse && job.Fields.Keeps(store.FieldRelated) {
		parts = append(parts, partRelated)
	}
	return parts
}

// partOf reads the part a download named. Nothing named is the first part, and
// the results of an index job are its verdicts — which is what every link to
// such a job has always meant.
func partOf(job store.JobSummary, asked string) (string, bool) {
	parts := partsOf(job)
	if asked == "" || (asked == partResults && job.Kind == store.KindIndex) {
		return parts[0], true
	}
	return asked, slices.Contains(parts, asked)
}

// fieldsOf is every column the part of this job can be written with, in the
// order a file carries them when nobody chose. The results carry only what the
// job kept: a column it never kept would stand in the file empty, which reads as
// a result that had none of that rather than as one nobody asked to keep — and
// on a job of ten million results it is also several hundred megabytes of
// separators.
func fieldsOf(job store.JobSummary, part string) []string {
	switch part {
	case partAds:
		return export.Ads.Names()
	case partRelated:
		return export.Suggestions.Names()
	case partVerdicts:
		return export.Verdicts.Names()
	}
	return columnsOf(job.Fields)
}

// writePart writes one part of a job as the layout says. A preview stops at the
// first records a screen shows.
func (s *Server) writePart(ctx context.Context, w io.Writer, ask exportAsk, preview bool) error {
	id := ask.job.ID
	switch ask.part {
	case partAds:
		return feed(w, export.Ads, ask.layout, preview, func(fn func(export.Ad) error) error {
			return s.store.Ads(ctx, id, func(a store.Ad) error {
				return fn(export.Ad{
					Ordinal: a.Ordinal, Query: a.Query, Page: a.Page,
					Position: a.Position, Placement: a.Placement,
					Title: a.Title, Host: a.Host, URL: a.URL, Snippet: a.Snippet,
				})
			})
		})
	case partRelated:
		return feed(w, export.Suggestions, ask.layout, preview, func(fn func(export.Suggestion) error) error {
			return s.store.Suggestions(ctx, id, func(g store.Suggestion) error {
				return fn(export.Suggestion{
					Ordinal: g.Ordinal, Query: g.Query, Page: g.Page,
					Position: g.Position, Text: g.Text,
				})
			})
		})
	case partVerdicts:
		// What it leaves out is as deliberate as what it writes: an address still
		// waiting, or one whose request was refused, carries no verdict, and
		// store.Verdicts hands over neither. Writing those as not held would
		// report a check that never happened.
		return feed(w, export.Verdicts, ask.layout, preview, func(fn func(export.Verdict) error) error {
			return s.store.Verdicts(ctx, id, func(v store.Verdict) error {
				return fn(export.Verdict{Ordinal: v.Ordinal, Target: v.Target, Held: v.Held})
			})
		})
	}
	return feed(w, export.Results, ask.layout, preview, func(fn func(export.Row) error) error {
		return s.walkRows(ctx, id, fn)
	})
}

// feed walks one part of a job into a file written as the layout says.
//
// The refusal of a write is passed back rather than swallowed, and that is what
// ends the walk: a reader who closed the tab leaves every write failing, and an
// export that reads on regardless spends the whole job on a socket nobody is
// holding.
func feed[T any](w io.Writer, c export.Catalog[T], l export.Layout, preview bool,
	walk func(func(T) error) error) error {
	out, err := export.NewTable(w, c, l)
	if err != nil {
		return err
	}
	take := out.Write
	if preview {
		take = firstOf(out)
	}
	if err := walk(take); err != nil && !errors.Is(err, errPreviewFull) {
		return err
	}
	// Close is what writes the header of a file that found no records, so an
	// export of a job that captured nothing is an empty table rather than an
	// empty file that reads as a failure.
	return out.Close()
}

// walkRows hands every result of a job to fn, in the order the job had.
func (s *Server) walkRows(ctx context.Context, jobID int64, fn func(export.Row) error) error {
	return s.store.Rows(ctx, jobID, func(row store.Row) error {
		return fn(export.Row{
			Ordinal:     row.Ordinal,
			Query:       row.Query,
			Page:        row.Page,
			Rank:        row.Rank,
			Title:       row.Title,
			URL:         row.URL,
			Host:        row.Host,
			Snippet:     row.Snippet,
			Link:        row.Link,
			DisplayPath: row.DisplayPath,
		})
	})
}

// streamTo is the walk of the results into a file the caller built. It is
// separate so a test can hand one that refuses a row part way, which is the only
// way to ask what an export does when the reader has walked away.
func (s *Server) streamTo(ctx context.Context, out export.Writer, jobID int64) error {
	if err := s.walkRows(ctx, jobID, out.Write); err != nil {
		return err
	}
	return out.Close()
}

// errPreviewFull ends the walk of a preview once it has what it shows.
var errPreviewFull = errors.New("web: the preview has what it shows")

// previewRecords is how many records a preview shows.
const previewRecords = 10

// previewScan is how many records a preview reads looking for them. With
// repeats dropped, a job whose every result repeats the first would otherwise
// be read to its end to show one line. A variable so a test can make the scan
// short.
var previewScan = 10000

// firstOf is a table's Write that stops the walk once the preview is full.
func firstOf[T any](out *export.Table[T]) func(T) error {
	read := 0
	return func(rec T) error {
		read++
		if err := out.Write(rec); err != nil {
			return err
		}
		if out.Written() >= previewRecords || read >= previewScan {
			return errPreviewFull
		}
		return nil
	}
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
	lang := s.rememberLang(w, r)
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
		page:      s.frame(r, lang, "history.title", historyAt),
		Host:      host,
		Positions: positions,
	})
}

// columnsOf is the columns a job's file carries: what says which result this is,
// then whatever the job was asked to keep.
func columnsOf(fields store.Fields) []string {
	cols := []string{export.ColOrdinal, export.ColQuery, export.ColPage, export.ColRank}
	for _, part := range fields.Kept() {
		switch part {
		case store.FieldTitle:
			cols = append(cols, export.ColTitle)
		case store.FieldURL:
			cols = append(cols, export.ColURL)
		case store.FieldLink:
			cols = append(cols, export.ColLink)
		case store.FieldHost:
			cols = append(cols, export.ColHost)
		case store.FieldSnippet:
			cols = append(cols, export.ColSnippet)
		case store.FieldPath:
			cols = append(cols, export.ColPath)
		}
	}
	return cols
}

// The parts of a job a download can ask for. Nothing said is the results, which
// is what a job is for.
const (
	partField   = "part"
	partResults = "results"
	partAds     = "ads"
	partRelated = "related"
	// partVerdicts is an index job's only part. Its links have always named no
	// part, or the results, and they go on meaning this.
	partVerdicts = "verdicts"
)

// separatorField is what a text file separates its columns with. Nothing said
// is a tab, which is what a column of addresses is pasted into a spreadsheet
// with, and every other format ignores it.
const separatorField = "sep"
