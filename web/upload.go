// SPDX-License-Identifier: MIT

package web

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/blanktrail/google-serp-parser/store"
)

// uploadAt is where a list arrives as a file, and listField is the box it
// arrives in.
//
// It is an address of its own rather than a second button on the form beside
// it. The form is read whole by the server before the handler sees a field of
// it, which is the only way two buttons can be told apart, and a form read
// whole is a file of a million lines held in memory or spooled to disk before a
// line of it is looked at. This one is read as it arrives and never held.
const (
	uploadAt  = "/new/upload"
	listField = "list"
)

// lineCap is the longest line this reads. The default is 64 KB, and a line
// longer than that is not rare in somebody else's list: a phrase with a comment
// after it, an address with a query string, a file whose lines were joined by
// the program that wrote it. A cap there still has to be, because without one a
// file of a million bytes and no newline in it is a million bytes held.
//
// lineStart is what the reader begins with, and it grows to lineCap for the
// lines that need it. Beginning at the cap would hold a megabyte for a file of
// short phrases.
const (
	lineStart = 64 << 10
	lineCap   = 1 << 20
)

// boxCap is the most a text box of this form may carry. The boxes hold a name
// and four short settings, and a part sending more than this is not one of
// them.
const boxCap = 4 << 10

// uploadList takes a list of any size as a file and writes it straight into a
// job.
//
// Nothing here holds the list. The parts are walked as they arrive, each line
// is handed to the plan as it is read, and the plan writes in batches — so what
// this costs is bounded by a batch rather than by the file.
//
// Measured through this handler: a hundred lines peaked at 907 KB of heap, and
// 102 000 lines at 3 649 KB. A thousandfold file for four times the memory —
// bounded, not constant, and the difference is worth stating rather than
// rounding away.
//
// The order is the whole of it: the boxes stand before the file in the markup,
// a browser sends the parts in the order they stand, and so everything needed
// to write the job down is known before the first line of the list arrives.
func (s *Server) uploadList(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)
	parts, err := r.MultipartReader()
	if err != nil {
		s.showNew(w, r, lang, jobForm{}, []string{"form.upload.unreadable"}, nil)
		return
	}

	var form jobForm
	for {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.showNew(w, r, lang, form, []string{"form.upload.broke"}, nil)
			return
		}
		if part.FormName() != listField {
			form = form.carrying(part.FormName(), readBox(part))
			_ = part.Close()
			continue
		}
		s.takeUpload(w, r, lang, form, part)
		return
	}
	// Every part went by and none of them was the list.
	s.showNew(w, r, lang, form, []string{"form.upload.none"}, nil)
}

// takeUpload writes one job from the file part of an upload, and answers the
// reader.
func (s *Server) takeUpload(w http.ResponseWriter, r *http.Request, lang Lang,
	form jobForm, list *multipart.Part) {
	defer func() { _ = list.Close() }()

	if complaints := s.uploadFaults(form); len(complaints) > 0 {
		// The rest of the file is read and thrown away rather than left unread. A
		// handler that answers and stops reading leaves the browser sending into a
		// connection with nobody at the other end, and what the reader is shown
		// then is their browser's own network error rather than the sentence
		// naming the box they left empty.
		_, _ = io.Copy(io.Discard, list)
		s.showNew(w, r, lang, form, complaints, nil)
		return
	}

	id, took, err := s.streamInto(r, form, list)
	if err != nil {
		// The job stays where it is, without the mark that makes it work. What
		// arrived is kept and nothing will run it, and the reader is told both.
		s.log.Error("a list stopped arriving part way", "job", id, "lines", took, "error", err)
		s.showNew(w, r, lang, form, []string{complaintFor(err)}, nil)
		return
	}
	// The job is written and complete, so it is queued. A refusal here is a
	// server winding down or a job already in the queue, and neither is worth
	// keeping the reader from the page that says what is true now.
	if s.sup != nil {
		if err := s.sup.Start(id); err != nil {
			s.log.Error("an uploaded job could not be queued", "job", id, "error", err)
		}
	}
	// A page rendered into the answer to a form is a page the browser's reload
	// button sends again, and the second send would upload the file twice.
	http.Redirect(w, r, jobPath(id), http.StatusSeeOther)
}

// streamInto reads the file into a plan and hands back the job, how many lines
// it took, and what stopped it.
//
// The plan is abandoned on the way out of every path. After a list that was
// finished that is nothing at all, and after one that was not it is what leaves
// a job that can be seen and cannot be run.
func (s *Server) streamInto(r *http.Request, form jobForm, list io.Reader) (int64, int, error) {
	plan, err := s.store.OpenPlan(r.Context(), form.spec())
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if err := plan.Abandon(); err != nil {
			s.log.Error("an upload could not be given up", "job", plan.JobID(), "error", err)
		}
	}()

	lines := bufio.NewScanner(list)
	lines.Buffer(make([]byte, 0, lineStart), lineCap)
	for lines.Scan() {
		text, ok := queryOf(lines.Text())
		if !ok {
			continue
		}
		if err := plan.Add(r.Context(), text); err != nil {
			return plan.JobID(), plan.Count(), err
		}
	}
	if err := lines.Err(); err != nil {
		return plan.JobID(), plan.Count(), fmt.Errorf("web: reading an uploaded list: %w", err)
	}
	if err := plan.Ready(r.Context()); err != nil {
		return plan.JobID(), plan.Count(), err
	}
	return plan.JobID(), plan.Count(), nil
}

// complaintFor names what to tell the reader about an upload that did not
// finish.
//
// A list holding no query is the same fault as an empty box and is said the
// same way, because it is the same mistake seen from a different door.
func complaintFor(err error) string {
	switch {
	case errors.Is(err, store.ErrNoQueries):
		return "form.queries.required"
	case errors.Is(err, bufio.ErrTooLong):
		return "form.upload.longline"
	}
	return "form.upload.broke"
}

// uploadFaults is everything wrong with an upload that can be known before the
// file is read.
//
// The connection is among them, and it is asked here rather than after the
// file, because writing a million lines into a job that has nothing to run it
// is minutes spent to arrive at a sentence that could have been said first.
func (s *Server) uploadFaults(form jobForm) []string {
	complaints := form.faults()
	switch {
	case s.sup == nil:
		complaints = append(complaints, "form.norunner")
	case !s.sup.canRun():
		complaints = append(complaints, "form.notsetup")
	}
	return complaints
}

// carrying is the form with one box filled in.
//
// The boxes are named exactly as they are on the form beside this one, so a job
// set up here and a job set up there are set up by the same words.
func (f jobForm) carrying(box, value string) jobForm {
	value = strings.TrimSpace(value)
	switch box {
	case "name":
		f.Name = value
	case "kind":
		f.Kind = value
	case "target":
		f.Target = value
	case "unique":
		f.Unique = value
	case "country":
		f.Country = value
	case "language":
		f.Language = value
	case "spec":
		f.SpecName = value
	case "pages":
		// A number that will not parse is left at nought, so the complaint the
		// reader gets names the depth rather than the file.
		f.Pages, _ = strconv.Atoi(value)
	}
	return f
}

// readBox reads one text box of the upload, and no more of it than a box holds.
func readBox(part io.Reader) string {
	text, _ := io.ReadAll(io.LimitReader(part, boxCap))
	return string(text)
}
