// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/store"
)

// exportsOf is the export tab drawn for the query given.
func exportsOf(t *testing.T, s *Server, query string) string {
	t.Helper()
	rec := get(t, s, exportsAt+query)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s%s came back %d", exportsAt, query, rec.Code)
	}
	return rec.Body.String()
}

func TestExports_IsATabOfItsOwnBesideTheJobs(t *testing.T) {
	// The builder is a tab rather than a card on a job's page: the page is drawn
	// again every few seconds while the job runs, and a choice of columns half
	// made would be drawn away with it.
	at := slices.IndexFunc(tabs, func(x tab) bool { return x.At == exportsAt })
	jobs := slices.IndexFunc(tabs, func(x tab) bool { return x.At == jobsAt })
	if at < 0 || at != jobs+1 {
		t.Fatalf("the export tab stands at %d and the jobs at %d, want it right after them", at, jobs)
	}
	body := exportsOf(t, testServer(t), "")
	if !strings.Contains(body, `href="`+exportsAt+`"`) {
		t.Error("the header offers no way to the export tab")
	}
}

func TestExports_SaysSoWhenNoJobHasAnythingToExport(t *testing.T) {
	body := exportsOf(t, testServer(t), "")
	if strings.Contains(body, `id="export-builder"`) {
		t.Error("a builder is offered with no job to build from")
	}
}

func TestExports_TakesTheNewestJobThatCollectedSomethingWhenNoneIsNamed(t *testing.T) {
	// The tab opened cold is most often opened for the job that has just
	// finished, and a job that collected nothing has nothing to lay out.
	s := testServer(t)
	older := seedJob(t, s, "older with results", 1, 1, 0)
	seedJob(t, s, "newest without", 1, 0, 0)
	body := exportsOf(t, s, "")
	if !strings.Contains(body, `<option value="`+strconv.FormatInt(older, 10)+`" selected`) {
		t.Errorf("the job chosen is not the newest one with results:\n%s", body)
	}
}

func TestExports_OffersOnlyThePartsAndFieldsTheJobKept(t *testing.T) {
	// A field the job never kept would be a column of blanks, and a part it
	// never kept a file of nothing.
	s := testServer(t)
	id := withAside(t, s, store.FieldURL)
	body := exportsOf(t, s, "?job="+strconv.FormatInt(id, 10))
	for _, kept := range []string{"ordinal", "query", "page", "rank", "url"} {
		if !strings.Contains(body, `data-field="`+kept+`"`) {
			t.Errorf("the field %q the job keeps is not offered", kept)
		}
	}
	for _, not := range []string{"title", "snippet", "host"} {
		if strings.Contains(body, `data-field="`+not+`"`) {
			t.Errorf("the field %q the job never kept is offered", not)
		}
	}
	for _, part := range []string{`value="ads"`, `value="related"`} {
		if strings.Contains(body, `name="part" `+part) {
			t.Errorf("a part the job never kept is offered: %s", part)
		}
	}
}

func TestExports_MovesAFieldWhereItWasAskedTo(t *testing.T) {
	got := orderOf([]string{"a", "b", "c"}, "a,b,c", "c:up")
	if !slices.Equal(got, []string{"a", "c", "b"}) {
		t.Errorf("c moved up gives %v", got)
	}
	got = orderOf([]string{"a", "b", "c"}, "a,b,c", "a:down")
	if !slices.Equal(got, []string{"b", "a", "c"}) {
		t.Errorf("a moved down gives %v", got)
	}
	got = orderOf([]string{"a", "b", "c"}, "c,a", "")
	if !slices.Equal(got, []string{"c", "a", "b"}) {
		t.Errorf("an order naming two of three gives %v, want the third after them", got)
	}
	got = orderOf([]string{"a", "b"}, "a,zzz,b", "a:up")
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("an unknown name and a move past the top give %v", got)
	}
}

func TestExports_ShowsHowTheFileWillBegin(t *testing.T) {
	s := testServer(t)
	id := seedJob(t, s, "nightly", 1, 1, 0)
	body := exportsOf(t, s, "?job="+strconv.FormatInt(id, 10)+"&format=txt&cols=url&header=0")
	if !strings.Contains(body, "https://first.test/1\nhttps://second.test/2\n") {
		t.Errorf("the preview is not the list of addresses:\n%s", body)
	}
}

func TestExports_DownloadsWithTheOnePressThatActs(t *testing.T) {
	s := testServer(t)
	id := seedJob(t, s, "nightly", 1, 1, 0)
	body := exportsOf(t, s, "?job="+strconv.FormatInt(id, 10))
	if !strings.Contains(body, `class="primary" formaction="/export"`) {
		t.Errorf("the press that acts does not download:\n%s", body)
	}
}

func TestJob_OffersTheWayToTheExportTabForThisJob(t *testing.T) {
	s := testServer(t)
	id := seedJob(t, s, "nightly", 1, 1, 0)
	body := get(t, s, jobPath(id)).Body.String()
	if !strings.Contains(body, `href="`+exportsAt+`?job=`+strconv.FormatInt(id, 10)+`"`) {
		t.Errorf("the job page offers no way to its export:\n%s", body)
	}
}

func TestExports_MovesAFieldWithoutLosingWhatWasChosen(t *testing.T) {
	// Without a script a move is a link, and a link that carried only the move
	// would draw the screen back to what nobody chose.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 1, 1, 0)
	body := exportsOf(t, s, "?job="+strconv.FormatInt(id, 10)+"&format=txt&cols=url&header=0&unique=1&eol=crlf")
	_, rest, ok := strings.Cut(body, `data-field="url"`)
	if !ok {
		t.Fatalf("the url field is not drawn:\n%s", body)
	}
	link, _, _ := strings.Cut(rest, "</li>")
	for _, kept := range []string{"format=txt", "header=0", "unique=1", "eol=crlf", "cols=url", "move=url%3Aup"} {
		if !strings.Contains(link, kept) {
			t.Errorf("the move up of url does not carry %q: %s", kept, link)
		}
	}
}

func TestExports_SaysWhenItWasDrawnFromAChoice(t *testing.T) {
	// The script gives a tab opened cold the choice made last time, and only
	// such a tab: one drawn from a choice is left as it is, which is also what
	// keeps the script from drawing again the screen it has just drawn.
	s := testServer(t)
	id := strconv.FormatInt(seedJob(t, s, "nightly", 1, 1, 0), 10)
	if cold := exportsOf(t, s, "?job="+id); strings.Contains(cold, `data-explicit`) {
		t.Error("a tab opened cold says it was drawn from a choice")
	}
	if chosen := exportsOf(t, s, "?job="+id+"&format=csv"); !strings.Contains(chosen, `data-explicit="1"`) {
		t.Error("a tab drawn from a choice does not say so")
	}
}
