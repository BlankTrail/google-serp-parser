// SPDX-License-Identifier: MIT

package google

import (
	"net/url"
	"strings"
	"testing"
)

func parseQ(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestQuery_CountryPicksBothTheDomainAndTheGlParameter(t *testing.T) {
	// Country is two settings that must agree. google.de with gl=us returns a
	// mixture neither setting asked for, and the mistake is invisible in the
	// results.
	raw, err := Query{Text: "kaufen", Country: "de", Language: "de"}.URL()
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	u := parseQ(t, raw)
	if u.Host != "www.google.de" {
		t.Errorf("host=%q, want www.google.de", u.Host)
	}
	if got := u.Query().Get("gl"); got != "de" {
		t.Errorf("gl=%q, want de", got)
	}
}

func TestQuery_LanguageIsIndependentOfCountry(t *testing.T) {
	// "German results in English" is an ordinary request, not an exotic one.
	raw, err := Query{Text: "buy", Country: "de", Language: "en"}.URL()
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	u := parseQ(t, raw)
	if u.Host != "www.google.de" {
		t.Errorf("host=%q, want www.google.de", u.Host)
	}
	if got := u.Query().Get("hl"); got != "en" {
		t.Errorf("hl=%q, want en", got)
	}
}

func TestQuery_AcceptLanguageMatchesTheLanguageAxis(t *testing.T) {
	// BlankTrail never spoofs Accept-Language — it is strict client
	// passthrough — so this header is the parser's own responsibility and
	// must agree with hl, or the page language and the browser language
	// disagree on every request.
	got := Query{Text: "x", Language: "ru"}.AcceptLanguage()
	if !strings.HasPrefix(got, "ru") {
		t.Errorf("AcceptLanguage()=%q, want it to lead with ru", got)
	}
	if !strings.Contains(got, "q=0.9") {
		t.Errorf("AcceptLanguage()=%q, want a weighted list as a browser sends", got)
	}
}

func TestQuery_PaginationStepsByTen(t *testing.T) {
	// num=100 was withdrawn by Google; depth is pages of ten and the cost of
	// a deep run follows from that.
	first, _ := Query{Text: "x", Page: 1}.URL()
	if got := parseQ(t, first).Query().Get("start"); got != "" {
		t.Errorf("page 1 carries start=%q, want it absent", got)
	}
	third, _ := Query{Text: "x", Page: 3}.URL()
	if got := parseQ(t, third).Query().Get("start"); got != "20" {
		t.Errorf("page 3 start=%q, want 20", got)
	}
}

func TestQuery_DefaultsAreUsable(t *testing.T) {
	raw, err := Query{Text: "hello world"}.URL()
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	u := parseQ(t, raw)
	if u.Host != "www.google.com" {
		t.Errorf("host=%q, want www.google.com by default", u.Host)
	}
	if got := u.Query().Get("q"); got != "hello world" {
		t.Errorf("q=%q, want the query text", got)
	}
}

func TestQuery_RejectsAnEmptyText(t *testing.T) {
	if _, err := (Query{}).URL(); err == nil {
		t.Error("URL() accepted an empty query")
	}
}

func TestQuery_UnknownCountryFallsBackToTheComDomain(t *testing.T) {
	// A country with no ccTLD entry must still produce a working request with
	// gl set, rather than an error that stops a whole run over one row.
	raw, err := Query{Text: "x", Country: "zz"}.URL()
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	u := parseQ(t, raw)
	if u.Host != "www.google.com" {
		t.Errorf("host=%q, want the .com fallback", u.Host)
	}
	if got := u.Query().Get("gl"); got != "zz" {
		t.Errorf("gl=%q, want it passed through anyway", got)
	}
}
