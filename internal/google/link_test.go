// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClassifyLink_RecognisesAllThreeForms(t *testing.T) {
	cases := []struct {
		name, href, wantDest string
		wantForm             LinkForm
		wantOK               bool
	}{
		{"direct", "https://habr.com/ru/articles/704090/", "https://habr.com/ru/articles/704090/", LinkDirect, true},
		{"redirect", "/url?q=https%3A%2F%2Fhabr.com%2Fx&sa=U", "https://habr.com/x", LinkRedirect, true},
		{"encrypted", "/goto?url=CAESXAHuR6pN7OGc", "", LinkEncrypted, true},
		{"google's own property", "https://chromewebstore.google.com/detail/x", "", "", false},
		{"internal search link", "/search?q=next+page", "", "", false},
		{"anchor", "#", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest, form, ok := classifyLink(tc.href)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if form != tc.wantForm {
				t.Errorf("form=%q, want %q", form, tc.wantForm)
			}
			if dest != tc.wantDest {
				t.Errorf("dest=%q, want %q", dest, tc.wantDest)
			}
		})
	}
}

func TestClassifyLink_UsesTheURLParserNotPatternMatching(t *testing.T) {
	// The destination is a query parameter, so it must be read with the URL
	// parser: a percent-encoded ampersand inside it would defeat any
	// hand-rolled split and silently truncate the address.
	dest, form, ok := classifyLink("/url?q=https%3A%2F%2Fx.test%2Fa%3Fb%3D1%26c%3D2&sa=U")
	if !ok || form != LinkRedirect {
		t.Fatalf("ok=%v form=%q, want true redirect", ok, form)
	}
	if want := "https://x.test/a?b=1&c=2"; dest != want {
		t.Errorf("dest=%q, want %q", dest, want)
	}
}

func TestIsGoogleHost_MatchesOnlyGooglesOwnProperties(t *testing.T) {
	// The test is structural: "google" must be the label immediately left of
	// the public suffix. A substring test would drop a legitimate site such
	// as google.myshop.com as if Google owned it.
	cases := []struct {
		host string
		want bool
	}{
		{"google.com", true},
		{"www.google.com", true},
		{"chromewebstore.google.com", true},
		{"google.de", true},
		{"google.co.uk", true},
		{"google.com.br", true},
		{"google.myshop.com", false},
		{"notgoogle.com", false},
		{"mygoogle.net", false},
		{"habr.com", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			if got := isGoogleHost(tc.host); got != tc.want {
				t.Errorf("isGoogleHost(%q)=%v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestResolver_ReadsTheLocationHeaderWithoutFollowingIt(t *testing.T) {
	// Following the redirect would cost a second round trip and would reach
	// out to the destination, which never asked to be contacted. The address
	// is already in the first response's header.
	//
	// The redirect deliberately points back at this test server: that is what
	// makes a follow observable. Pointing it at an external address would
	// leave the flag below unreachable and the assertion meaningless.
	var followed bool
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/destination" {
			followed = true
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, srv.URL+"/destination", http.StatusFound)
	}))
	defer srv.Close()

	r := &Resolver{Client: srv.Client()}
	got, err := r.Resolve(context.Background(), srv.URL+"/goto?url=x")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := srv.URL + "/destination"; got != want {
		t.Errorf("Resolve=%q, want %q", got, want)
	}
	if followed {
		t.Error("the resolver followed the redirect; it must only read Location")
	}
}

func TestResolver_ReportsAResponseThatIsNotARedirect(t *testing.T) {
	// A 200 here means Google answered with a page instead of a redirect —
	// usually a challenge. Returning its URL as if it were the destination
	// would write google.com into the results as the ranking site.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := &Resolver{Client: srv.Client()}
	if _, err := r.Resolve(context.Background(), srv.URL+"/goto?url=x"); !errors.Is(err, ErrNotRedirect) {
		t.Fatalf("err=%v, want ErrNotRedirect", err)
	}
}

func TestResolver_HonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://x.test/", http.StatusFound)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &Resolver{Client: srv.Client()}
	if _, err := r.Resolve(ctx, srv.URL+"/goto?url=x"); err == nil {
		t.Error("Resolve ignored a cancelled context")
	}
}
