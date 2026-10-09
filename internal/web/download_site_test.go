// SPDX-License-Identifier: MIT

package web

import (
	"bytes"
	"encoding/csv"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// seedSiteJob is a job whose one search found the site, its subdomains, a
// result Google drew with no host of its own, and sites that only look like it.
func seedSiteJob(t *testing.T, s *Server) int64 {
	t.Helper()
	ctx := t.Context()
	id, err := s.store.CreateJob(ctx, store.JobSpec{Name: "site pages", Pages: 1}, []string{"site:kling.test a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	unhosted := resultAt("kling.test", 6)
	unhosted.Host = ""
	unhosted.Title = "unhosted at 6"
	err = s.store.Record(ctx, id, store.QueryOutcome{
		Ordinal: 0,
		Pages: []google.SERP{{Origin: "https://www.google.com", Results: []google.Result{
			resultAt("kling.test", 1), resultAt("other.test", 2), resultAt("www.kling.test", 3),
			resultAt("notkling.test", 4), resultAt("app.kling.test", 5), unhosted,
		}}},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	return id
}

// titlesOf is the title column of a CSV file with a header.
func titlesOf(t *testing.T, body []byte) []string {
	t.Helper()
	records, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("what came back does not read as CSV: %v\n%s", err, body)
	}
	var out []string
	for _, r := range records[1:] {
		out = append(out, r[0])
	}
	return out
}

var siteTitles = []string{"kling.test at 1", "www.kling.test at 3", "app.kling.test at 5", "unhosted at 6"}

func TestDownload_KeepsOnlyTheSiteAskedForWithItsSubdomains(t *testing.T) {
	// A search limited to a site still answers with other sites, and a list of
	// the site's own titles is what a position check is fed: a stranger's title
	// in it is a phrase checked for nothing.
	s := testServer(t)
	id := seedSiteJob(t, s)

	rec := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=csv&cols=title&site=kling.test")
	if rec.Code != http.StatusOK {
		t.Fatalf("export gave %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := titlesOf(t, rec.Body.Bytes()); !slices.Equal(got, siteTitles) {
		t.Errorf("the file carries %q, want %q", got, siteTitles)
	}
}

func TestDownload_ReadsTheSiteHoweverItWasTypedIn(t *testing.T) {
	// What is pasted into the box is as often an address as a name.
	s := testServer(t)
	id := seedSiteJob(t, s)

	for _, typed := range []string{"https://www.Kling.test/some/page", " KLING.TEST. ", "kling.test/"} {
		rec := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=csv&cols=title&site="+url.QueryEscape(typed))
		if got := titlesOf(t, rec.Body.Bytes()); !slices.Equal(got, siteTitles) {
			t.Errorf("site %q: the file carries %q, want %q", typed, got, siteTitles)
		}
	}
}

func TestDownload_KeepsEverythingWhenNoSiteIsNamed(t *testing.T) {
	s := testServer(t)
	id := seedSiteJob(t, s)

	rec := get(t, s, "/export?job="+strconv.FormatInt(id, 10)+"&format=csv&cols=title&site=")
	if got := titlesOf(t, rec.Body.Bytes()); len(got) != 6 {
		t.Errorf("the file carries %d rows, want all 6: %q", len(got), got)
	}
}

func TestExports_ShowsTheSiteBoxAndPreviewsOnlyTheSite(t *testing.T) {
	s := testServer(t)
	id := seedSiteJob(t, s)

	rec := get(t, s, "/exports?job="+strconv.FormatInt(id, 10)+"&format=txt&header=0&cols=title&site=kling.test")
	body := rec.Body.String()
	if !strings.Contains(body, `name="site"`) || !strings.Contains(body, `value="kling.test"`) {
		t.Fatalf("the tab draws no site box holding the site:\n%s", body)
	}
	if strings.Contains(body, "other.test at 2") || strings.Contains(body, "notkling.test at 4") {
		t.Errorf("the preview shows another site's rows")
	}
	if !strings.Contains(body, "app.kling.test at 5") {
		t.Errorf("the preview leaves out the site's own subdomain")
	}

	pre := get(t, s, "/export/preview?job="+strconv.FormatInt(id, 10)+"&format=txt&header=0&cols=title&site=kling.test")
	if got := strings.Fields(strings.ReplaceAll(pre.Body.String(), " at ", "@")); !slices.Equal(got,
		[]string{"kling.test@1", "www.kling.test@3", "app.kling.test@5", "unhosted@6"}) {
		t.Errorf("the preview route shows %q", got)
	}
}
