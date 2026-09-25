// SPDX-License-Identifier: MIT

package web

import (
	"bytes"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/blanktrail/google-serp-parser/internal/export"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// What the export tab's form carries besides what a download reads: the order
// of every field on the screen, chosen or not, and the one moved.
const (
	orderField = "order"
	moveField  = "move"
)

// exportsPage is the export tab: which job, which part of it, how its file is
// written, and how that file begins.
type exportsPage struct {
	page
	Jobs    []store.JobSummary
	Job     *store.JobSummary
	Parts   []exportPart
	Fields  []exportField
	Order   string
	Formats []string
	Format  string
	Sep     string
	CRLF    bool
	Header  bool
	Unique  bool
	BOM     bool
	// Explicit says the screen was drawn from a choice rather than opened
	// cold. The script restores the last choice only onto a screen opened cold,
	// so a screen it drew itself is never drawn again.
	Explicit bool
	Preview  string
	// Note is what the preview area says instead of a preview.
	Note string
}

// exportPart is one part of a job the tab offers.
type exportPart struct {
	Name, Label string
	Current     bool
}

// exportField is one field of the part, as the list on the tab draws it.
type exportField struct {
	Name, Label string
	Chosen      bool
	// Up and Down are the addresses that move the field, and empty where it
	// cannot move that way.
	Up, Down string
}

// fieldLabel is the phrase naming a column. The parts a job can be asked to
// keep are named as the new-job form names them.
func fieldLabel(name string) string {
	switch name {
	case export.ColTitle, export.ColURL, export.ColLink, export.ColHost, export.ColSnippet:
		return "field." + name
	case export.ColPath:
		return "field.path"
	}
	return "export.field." + name
}

// partLabel is the phrase naming a part of a job.
func partLabel(part string) string {
	switch part {
	case partAds:
		return "field.ads"
	case partRelated:
		return "field.related"
	case partVerdicts:
		return "exports.part.verdicts"
	}
	return "job.results"
}

// orderOf is every field in the order the screen shows them: the order asked
// for, less what the part does not have, then whatever it left out in the
// part's own order — and the one field moved, moved.
func orderOf(allowed []string, asked, move string) []string {
	var order []string
	for name := range strings.SplitSeq(asked, ",") {
		name = strings.TrimSpace(name)
		if slices.Contains(allowed, name) && !slices.Contains(order, name) {
			order = append(order, name)
		}
	}
	for _, name := range allowed {
		if !slices.Contains(order, name) {
			order = append(order, name)
		}
	}
	name, way, _ := strings.Cut(move, ":")
	at := slices.Index(order, name)
	switch {
	case at > 0 && way == "up":
		order[at-1], order[at] = order[at], order[at-1]
	case at >= 0 && at < len(order)-1 && way == "down":
		order[at], order[at+1] = order[at+1], order[at]
	}
	return order
}

// newestWithResults is the job a cold tab opens on: the newest that collected
// anything, or the newest of all when none has. The tab is most often opened
// for the job that has just finished, and a job that collected nothing has
// nothing to lay out.
func newestWithResults(jobs []store.JobSummary) store.JobSummary {
	for _, j := range jobs {
		if j.Done > 0 {
			return j
		}
	}
	return jobs[0]
}

// yes is how a choice of two travels in the tab's addresses.
func yes(on bool) string {
	if on {
		return "1"
	}
	return "0"
}

// exports draws the export tab.
//
// All of it is drawn here, from the address, so the tab works with no script at
// all and can be bookmarked with a choice in it. The script only does the same
// things in place.
func (s *Server) exports(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	view := exportsPage{page: s.frame(r, lang, "exports.title", exportsAt), Formats: export.Formats()}
	jobs, err := s.store.Jobs(r.Context(), 0)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	view.Jobs = jobs
	q := r.URL.Query()

	var job store.JobSummary
	if id, err := strconv.ParseInt(q.Get("job"), 10, 64); err == nil {
		if job, err = s.store.Progress(r.Context(), id); err != nil {
			http.NotFound(w, r)
			return
		}
		// A job older than the list still has to be the one shown as chosen.
		if !slices.ContainsFunc(jobs, func(j store.JobSummary) bool { return j.ID == job.ID }) {
			view.Jobs = append([]store.JobSummary{job}, jobs...)
		}
	} else if len(jobs) > 0 {
		job = newestWithResults(jobs)
	} else {
		s.render(w, r, "exports.html", view)
		return
	}
	view.Job = &job

	part, ok := partOf(job, q.Get(partField))
	if !ok {
		part = partsOf(job)[0]
	}
	for _, p := range partsOf(job) {
		view.Parts = append(view.Parts, exportPart{Name: p, Label: partLabel(p), Current: p == part})
	}

	allowed := fieldsOf(job, part)
	order := orderOf(allowed, q.Get(orderField), q.Get(moveField))
	// A screen opened cold chooses every field, which is the file this program
	// always wrote; one drawn from a choice chooses what the choice says, even
	// nothing.
	view.Explicit = q.Has("format")
	chosen := map[string]bool{}
	if view.Explicit || q.Has(colsField) {
		for _, name := range colsOf(q) {
			chosen[name] = true
		}
	} else {
		for _, name := range allowed {
			chosen[name] = true
		}
	}
	var fields []string
	for _, name := range order {
		if chosen[name] {
			fields = append(fields, name)
		}
	}

	view.Format = q.Get("format")
	if !export.Writes(view.Format) {
		view.Format = "csv"
	}
	view.Sep = q.Get(separatorField)
	sep, sepErr := export.SeparatorOf(view.Sep)
	view.CRLF = q.Get(eolField) == "crlf"
	view.Header = q.Get(headerField) != "0"
	view.Unique = q.Get(uniqueField) == "1"
	view.BOM = q.Get(bomField) == "1"
	view.Order = strings.Join(order, ",")

	// What every move link carries: the whole choice as it stands, so a move
	// made without a script loses nothing already chosen.
	state := url.Values{}
	state.Set("job", strconv.FormatInt(job.ID, 10))
	state.Set(partField, part)
	state.Set("format", view.Format)
	state.Set(separatorField, view.Sep)
	state.Set(eolField, map[bool]string{true: "crlf", false: "lf"}[view.CRLF])
	state.Set(headerField, yes(view.Header))
	state.Set(uniqueField, yes(view.Unique))
	state.Set(bomField, yes(view.BOM))
	state[colsField] = fields
	state.Set(orderField, view.Order)
	moveTo := func(name, way string) string {
		m := url.Values{}
		for k, v := range state {
			m[k] = v
		}
		m.Set(moveField, name+":"+way)
		return exportsAt + "?" + m.Encode()
	}
	for i, name := range order {
		f := exportField{Name: name, Label: fieldLabel(name), Chosen: chosen[name]}
		if i > 0 {
			f.Up = moveTo(name, "up")
		}
		if i < len(order)-1 {
			f.Down = moveTo(name, "down")
		}
		view.Fields = append(view.Fields, f)
	}

	switch {
	case sepErr != nil:
		view.Note = "exports.refused.sep"
	case len(fields) == 0:
		view.Note = "exports.refused.fields"
	default:
		eol := export.LF
		if view.CRLF {
			eol = export.CRLF
		}
		layout := export.Layout{Format: view.Format, Fields: fields, Header: view.Header,
			EOL: eol, Sep: sep, Unique: view.Unique}
		var buf bytes.Buffer
		if err := s.writePart(r.Context(), &buf, exportAsk{job: job, part: part, layout: layout}, true); err != nil {
			s.fail(w, r, err)
			return
		}
		view.Preview = buf.String()
		if view.Preview == "" {
			view.Note = "exports.preview.empty"
		}
	}
	s.render(w, r, "exports.html", view)
}
