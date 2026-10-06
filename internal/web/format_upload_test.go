// SPDX-License-Identifier: MIT

package web

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/store"
)

// postFields sends a form the way the page does, its boxes in the order given
// — the same name more than once, as the page's formats are — and the list
// last.
func postFields(t *testing.T, s *Server, fields [][2]string, list string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, f := range fields {
		if err := form.WriteField(f[0], f[1]); err != nil {
			t.Fatalf("writing %s: %v", f[0], err)
		}
	}
	part, err := form.CreateFormFile(listField, "queries.txt")
	if err != nil {
		t.Fatalf("opening the file part: %v", err)
	}
	_, _ = io.WriteString(part, list)
	if err := form.Close(); err != nil {
		t.Fatalf("closing the form: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, uploadAt, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestUpload_MakesEveryQueryOfEveryFormatAndKeepsEachOnce(t *testing.T) {
	// Three formats on two lines: the query as it is, site:, and a run of
	// numbers. The two lines meet in what the formats make of them — "x 1" is
	// the second line and the first line's first number — and it is kept once.
	s := testServerHolding(t)
	rec := postFields(t, s, [][2]string{{"name", "f"}, {"pages", "1"},
		{"format", "{query}"}, {"format", "site:{query}"}, {"format", "{query} {num:1:2}"}}, "x\nx 1\n")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("uploading gave %d: %s", rec.Code, rec.Body)
	}
	left, err := s.store.Pending(t.Context(), theOneJob(t, s).ID)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	var got []string
	for _, q := range left {
		got = append(got, q.Text)
	}
	want := []string{"x", "site:x", "x 1", "x 2", "site:x 1", "x 1 1", "x 1 2"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("queued %q, want %q", got, want)
	}
}

func TestUpload_RefusesAFormatWithNoPlaceForTheQuery(t *testing.T) {
	s := testServerHolding(t)
	rec := postFields(t, s, [][2]string{{"name", "f"}, {"pages", "1"}, {"format", "site:example.com"}}, "x\n")
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a format with no {query} was taken")
	}
	if want := LangEN.T("form.format.noquery"); !strings.Contains(rec.Body.String(), want) {
		t.Errorf("the form does not say %q", want)
	}
	rec = postFields(t, s, [][2]string{{"name", "f"}, {"pages", "1"}, {"format", "{query} {num:9:1}"}}, "x\n")
	if want := LangEN.T("form.format.macro"); !strings.Contains(rec.Body.String(), want) {
		t.Errorf("a bad macro: the form does not say %q", want)
	}
}

func TestUpload_LeavesTheListOfAnotherKindAsItIsWhateverTheFormatSays(t *testing.T) {
	// The formats are a parse job's: an index job sent with one takes its
	// addresses as they are.
	s := testServerHolding(t)
	rec := postFields(t, s, [][2]string{{"name", "f"}, {"pages", "1"}, {"kind", store.KindIndex},
		{"format", "site:{query}"}}, "example.com/a\n")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("uploading gave %d: %s", rec.Code, rec.Body)
	}
	left, err := s.store.Pending(t.Context(), theOneJob(t, s).ID)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(left) != 1 || left[0].Text != "example.com/a" {
		t.Errorf("queued %v, want the address as it was", left)
	}
}

func TestUpload_CarriesASuggestionsJobsSettingsThroughThePageItself(t *testing.T) {
	// The page sends its form here, and a suggestions job's Multiword, its cap
	// and a choice to keep every repeat all have to arrive: read only on the
	// other door, they were lost on the one the page uses.
	s := testServerHolding(t)
	rec := postFields(t, s, [][2]string{{"name", "c"}, {"kind", store.KindSuggest}, {"unique", ""},
		{"multiword", "on"}, {"suggestlimit", "40"}}, "coffee maker\n")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("uploading gave %d: %s", rec.Code, rec.Body)
	}
	job := theOneJob(t, s)
	sum, err := s.store.Progress(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if !sum.Multiword || sum.SuggestLimit != 40 || sum.UniqueBy != store.UniqueOff {
		t.Errorf("filed with multiword %v, cap %d, filter %q; want true, 40 and keep everything",
			sum.Multiword, sum.SuggestLimit, sum.UniqueBy)
	}
}

func TestNewJob_OffersOneFormatOfTheQueryAloneWithAPlus(t *testing.T) {
	body := get(t, testServer(t), "/new?lang=en").Body.String()
	if !strings.Contains(body, `name="format" value="{query}"`) || strings.Count(body, `name="format"`) != 1 {
		t.Errorf("the form does not start on one format of the query alone")
	}
	if !strings.Contains(body, `class="format-add"`) || !strings.Contains(body, LangEN.T("form.format.why.abc")) {
		t.Errorf("the form has no + or no word on the macros")
	}
}

func TestCreateJob_MakesEveryQueryOfEveryFormatOnTheOtherDoorToo(t *testing.T) {
	// A job written down from the plain form, not the upload, is expanded by
	// the same formats: two doors, one list.
	s := testServerWithSupervisor(t)
	rec := postForm(t, s, "/new?do=start", url.Values{"name": {"f"}, "queries": {"x"}, "pages": {"1"},
		"format": {"{query}", `"{query}"`}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("starting gave %d: %s", rec.Code, rec.Body)
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	left, err := s.store.Pending(t.Context(), jobs[0].ID)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(left) != 2 || left[0].Text != "x" || left[1].Text != `"x"` {
		t.Errorf("queued %v, want x and the exact phrase", left)
	}
}
