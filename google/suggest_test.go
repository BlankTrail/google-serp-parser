// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSuggestURL_CarriesTheAxesAndTheClient(t *testing.T) {
	raw, err := suggestURL(Query{Text: "iphone 13", Country: "de", Language: "en"})
	if err != nil {
		t.Fatalf("suggestURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Host != "www.google.de" {
		t.Errorf("host=%q, want www.google.de", u.Host)
	}
	if u.Path != "/complete/search" {
		t.Errorf("path=%q, want /complete/search", u.Path)
	}
	v := u.Query()
	if v.Get("q") != "iphone 13" {
		t.Errorf("q=%q", v.Get("q"))
	}
	// Measured: with no client parameter the endpoint answers HTTP 400.
	if v.Get("client") == "" {
		t.Error("client is empty; the endpoint refuses a request without it")
	}
	if v.Get("hl") != "en" || v.Get("gl") != "de" {
		t.Errorf("hl=%q gl=%q, want en and de", v.Get("hl"), v.Get("gl"))
	}
}

func TestSuggestURL_RejectsAnEmptyQuery(t *testing.T) {
	if _, err := suggestURL(Query{}); err == nil {
		t.Error("suggestURL accepted an empty query")
	}
}

func TestSuggester_ReadsTheSuggestionArray(t *testing.T) {
	// Measured shape: a two-element array, the echo of the query and the list.
	// The response is served as text/javascript but its body is plain JSON.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=UTF-8")
		_, _ = w.Write([]byte(`["iphone 13",["iphone 13 pro","iphone 13 mini","iphone 13 case"]]`))
	}))
	defer srv.Close()

	s := &Suggester{Client: srv.Client()}
	got, err := s.suggestFrom(context.Background(), srv.URL, Query{Text: "iphone 13"})
	if err != nil {
		t.Fatalf("suggestFrom: %v", err)
	}
	want := []string{"iphone 13 pro", "iphone 13 mini", "iphone 13 case"}
	if len(got) != len(want) {
		t.Fatalf("got %d suggestions %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("suggestion %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSuggester_AnEmptyListIsNotAnError(t *testing.T) {
	// A phrase nobody searches has no suggestions, and that is an answer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`["zzqqxx",[]]`))
	}))
	defer srv.Close()

	got, err := (&Suggester{Client: srv.Client()}).suggestFrom(context.Background(), srv.URL, Query{Text: "zzqqxx"})
	if err != nil {
		t.Fatalf("suggestFrom: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

func TestSuggester_RejectsABodyThatIsNotTheExpectedShape(t *testing.T) {
	// The endpoint answers HTML when it refuses a request. Reading that as an
	// empty suggestion list would report "no suggestions" for every query.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = w.Write([]byte(`<!DOCTYPE html><title>Error 400</title>`))
	}))
	defer srv.Close()

	if _, err := (&Suggester{Client: srv.Client()}).suggestFrom(context.Background(), srv.URL, Query{Text: "iphone 13"}); err == nil {
		t.Error("suggestFrom accepted an HTML body as a suggestion list")
	}
}

func TestSuggester_SendsTheAcceptLanguageTheQueryAsksFor(t *testing.T) {
	// The language axis reaches the completion endpoint the same two ways it
	// reaches a search: hl in the address and this header on the request. A
	// request carrying only hl asks for the language halfway.
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Accept-Language")
		_, _ = w.Write([]byte(`["тест",[]]`))
	}))
	defer srv.Close()

	q := Query{Text: "тест", Country: "ru", Language: "ru"}
	if _, err := (&Suggester{Client: srv.Client()}).suggestFrom(context.Background(), srv.URL, q); err != nil {
		t.Fatalf("suggestFrom: %v", err)
	}
	// Asserted whole, not by prefix: a header built from the wrong half of the
	// query still starts with the right two letters.
	if sent := <-got; sent != "ru-RU,ru;q=0.9" {
		t.Errorf("Accept-Language=%q, want ru-RU,ru;q=0.9", sent)
	}
}

func TestSuggester_ReportsANonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := (&Suggester{Client: srv.Client()}).suggestFrom(context.Background(), srv.URL, Query{Text: "iphone 13"})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("err=%v, want it to name the status", err)
	}
}
