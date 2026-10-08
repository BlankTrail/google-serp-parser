package web

import (
	"strconv"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// The address on the page reads as a person writes it; the link stays as it came.
func TestReadableURL(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"cyrillic path", "https://llmneurotech.com/2026/10/03/%D0%BD%D0%B5%D0%B9%D1%80%D0%BE%D1%81%D0%B5%D1%82%D1%8C-%D1%81%D0%BE%D0%B7%D0%B4%D0%B0%D0%BB%D0%B0/",
			"https://llmneurotech.com/2026/10/03/нейросеть-создала/"},
		{"lowercase hex", "https://a.ru/%d0%bf%d1%80%d0%b8", "https://a.ru/при"},
		{"plain address", "https://deepai.org/music", "https://deepai.org/music"},
		{"ascii escapes stay", "https://a.ru/a%2Fb%3Fc%23d%25e%26f", "https://a.ru/a%2Fb%3Fc%23d%25e%26f"},
		{"space stays", "https://a.ru/%D0%B0%20%D0%B1", "https://a.ru/а%20б"},
		{"no-break space stays", "https://a.ru/%C2%A0x", "https://a.ru/%C2%A0x"},
		{"direction override stays", "https://a.ru/%E2%80%AEgpj.exe", "https://a.ru/%E2%80%AEgpj.exe"},
		{"zero width stays", "https://a.ru/a%E2%80%8Bb", "https://a.ru/a%E2%80%8Bb"},
		{"broken utf-8 stays", "https://a.ru/%D0%D0%B0", "https://a.ru/%D0а"},
		{"cut escape stays", "https://a.ru/%D0%B", "https://a.ru/%D0%B"},
		{"not an escape", "https://a.ru/100%", "https://a.ru/100%"},
	} {
		if got := readableURL(c.in); got != c.want {
			t.Errorf("%s: readableURL(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// 🔴 The results table and the History show a Cyrillic address as letters, and
// the link under it still goes to the address exactly as Google gave it.
func TestResultAddressesReadAsLettersAndLinkAsTheyCame(t *testing.T) {
	const escaped = "https://cyr.test/%D0%BF%D0%B5%D1%81%D0%BD%D1%8F/"
	s := testServer(t)
	ctx := t.Context()
	id, err := s.store.CreateJob(ctx, store.JobSpec{Name: "songs", Pages: 1}, []string{"песня"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.store.Record(ctx, id, store.QueryOutcome{Ordinal: 0,
		Pages: []google.SERP{{Origin: "https://www.google.com", Results: []google.Result{
			{Title: "Песня", URL: escaped, Host: "cyr.test"},
		}}}}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	job := get(t, s, "/job/"+strconv.FormatInt(id, 10)).Body.String()
	if !strings.Contains(job, `href="`+escaped+`"`) {
		t.Errorf("the job page links somewhere other than the address Google gave:\n%s", job)
	}
	if !strings.Contains(job, ">https://cyr.test/песня/</a>") {
		t.Errorf("the job page shows the address as escapes, not letters:\n%s", job)
	}
	history := get(t, s, "/history?host=cyr.test").Body.String()
	if !strings.Contains(history, "<td>https://cyr.test/песня/</td>") {
		t.Errorf("the History shows the address as escapes, not letters:\n%s", history)
	}
}
