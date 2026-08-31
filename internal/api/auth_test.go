// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/store"
)

// guarded is a handler behind the key check, and a flag saying whether it ran.
// A refusal that answers 401 and runs the handler anyway has already done the
// thing the key was standing in front of.
func guarded(s *Server) (http.HandlerFunc, *bool) {
	ran := new(bool)
	return s.authed(func(w http.ResponseWriter, _ *http.Request) {
		*ran = true
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"reached":true}`)
	}), ran
}

// ask drives the guarded handler once. The header is left off entirely when it
// is empty, because a request that sends no Authorization at all and one that
// sends an empty one are different requests.
func ask(h http.HandlerFunc, target, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// issue puts a key in the history and hands back the secret.
func issue(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	secret, _, err := st.CreateKey(context.Background(), name)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	return secret
}

// issueAndRevoke puts a key in the history and stops it, so a test has a secret
// that was genuinely issued and no longer works.
func issueAndRevoke(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	secret, key, err := st.CreateKey(context.Background(), name)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := st.RevokeKey(context.Background(), key.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	return secret
}

// nearMiss is a secret of the right shape differing from a real one in a single
// character. A check that weighs the length, or the first few characters, or
// merely that something key-shaped arrived, lets it through.
func nearMiss(secret string) string {
	head, last := secret[:len(secret)-1], secret[len(secret)-1]
	if last == '0' {
		return head + "1"
	}
	return head + "0"
}

func TestKeyCheck_LetsThroughAKeyCarriedInTheAuthorizationHeader(t *testing.T) {
	s, st := testServer(t)
	h, ran := guarded(s)

	rec := ask(h, "/api/v1/jobs", "Bearer "+issue(t, st, "nightly"))

	if rec.Code != http.StatusOK {
		t.Errorf("a key that works gave %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !*ran {
		t.Error("the request was answered without ever reaching the handler behind the check")
	}
}

func TestKeyCheck_LetsThroughAKeyCarriedInTheQueryParameterOtherClientsAlreadySend(t *testing.T) {
	// This is the whole point of the milestone: a program written against
	// another service starts working when its address is changed and nothing
	// else is.
	s, st := testServer(t)
	h, ran := guarded(s)

	rec := ask(h, "/api/v1/jobs?api_key="+issue(t, st, "nightly"), "")

	if rec.Code != http.StatusOK {
		t.Errorf("a key in the query gave %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !*ran {
		t.Error("the request was answered without ever reaching the handler behind the check")
	}
}

func TestKeyCheck_RefusesARequestCarryingNoKeyAtAll(t *testing.T) {
	// The most ordinary request a public address receives.
	s, st := testServer(t)
	issue(t, st, "nightly")
	h, ran := guarded(s)

	rec := ask(h, "/api/v1/jobs", "")

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a request with no key gave %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}
	if *ran {
		t.Error("a request with no key reached the handler behind the check")
	}
}

func TestKeyCheck_RefusesAKeyThatWasRevokedWhileAnotherKeyStillWorks(t *testing.T) {
	// Both keys sit in the same history, so a check that looks up whether any
	// key exists, rather than whether this one does, fails here.
	s, st := testServer(t)
	live := issue(t, st, "nightly")
	stopped := issueAndRevoke(t, st, "the machine that was decommissioned")
	h, ran := guarded(s)

	rec := ask(h, "/api/v1/jobs", "Bearer "+stopped)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a revoked key gave %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}
	if *ran {
		t.Error("a revoked key reached the handler behind the check")
	}

	if rec := ask(h, "/api/v1/jobs", "Bearer "+live); rec.Code != http.StatusOK {
		t.Errorf("the key that was left alone gave %d, want 200", rec.Code)
	}
}

func TestKeyCheck_RefusesAWellFormedKeyThisProgramNeverIssued(t *testing.T) {
	s, st := testServer(t)
	live := issue(t, st, "nightly")
	h, ran := guarded(s)

	rec := ask(h, "/api/v1/jobs", "Bearer "+nearMiss(live))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a key off by one character gave %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}
	if *ran {
		t.Error("a key that was never issued reached the handler behind the check")
	}

	if rec := ask(h, "/api/v1/jobs?api_key="+nearMiss(live), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("a key off by one character in the query gave %d, want 401", rec.Code)
	}
}

func TestKeyCheck_ReadsTheBearerWordWhateverCaseItArrivesIn(t *testing.T) {
	// The word is case-insensitive by the standard that defines it, and clients
	// spell it every way there is.
	s, st := testServer(t)
	secret := issue(t, st, "nightly")
	h, _ := guarded(s)

	for _, word := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		if rec := ask(h, "/api/v1/jobs", word+" "+secret); rec.Code != http.StatusOK {
			t.Errorf("%q gave %d, want 200 (body %q)", word, rec.Code, rec.Body.String())
		}
	}
}

func TestKeyCheck_RefusesAnAuthorizationHeaderThatIsNotAWordAndAKey(t *testing.T) {
	s, st := testServer(t)
	secret := issue(t, st, "nightly")
	h, ran := guarded(s)

	headers := map[string]string{
		"the word run into the key": "Bearer" + secret,
		"the word and nothing else": "Bearer",
		"the word and an empty key": "Bearer ",
		"the key with no word":      secret,
		"another scheme":            "Basic " + secret,
	}
	for what, header := range headers {
		*ran = false
		rec := ask(h, "/api/v1/jobs", header)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s gave %d, want 401 (body %q)", what, rec.Code, rec.Body.String())
		}
		if *ran {
			t.Errorf("%s reached the handler behind the check", what)
		}
	}
}

func TestKeyCheck_RefusesInJSONThatNamesNoReason(t *testing.T) {
	// Whether a key was never issued, was revoked, or was never sent is worth
	// knowing only to somebody working through keys one at a time. All three
	// answer with the same bytes.
	s, st := testServer(t)
	live := issue(t, st, "nightly")
	h, _ := guarded(s)

	answers := map[string]*httptest.ResponseRecorder{
		"no key":       ask(h, "/api/v1/jobs", ""),
		"never issued": ask(h, "/api/v1/jobs", "Bearer "+nearMiss(live)),
		"revoked":      ask(h, "/api/v1/jobs", "Bearer "+issueAndRevoke(t, st, "gone")),
	}

	var first *httptest.ResponseRecorder
	var firstWhat string
	for what, rec := range answers {
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("the refusal for %s is not JSON: %v (body %q)", what, err, rec.Body.String())
		}
		said, named := body["error"].(string)
		if !named || said == "" {
			t.Fatalf("the refusal for %s carries no error field: %v", what, body)
		}
		if len(body) != 1 {
			t.Errorf("the refusal for %s carries more than the error: %v", what, body)
		}
		for _, giveaway := range []string{"revok", "expire", "unknown", "not found", "no such", "missing", "prefix"} {
			if strings.Contains(strings.ToLower(said), giveaway) {
				t.Errorf("the refusal for %s says %q, which separates it from the others", what, said)
			}
		}
		if kind := rec.Header().Get("Content-Type"); !strings.Contains(kind, "application/json") {
			t.Errorf("the refusal for %s is served as %q", what, kind)
		}
		if first == nil {
			first, firstWhat = rec, what
			continue
		}
		if rec.Code != first.Code || !bytes.Equal(rec.Body.Bytes(), first.Body.Bytes()) {
			t.Errorf("%s answers %d %q and %s answers %d %q; a reader can tell them apart",
				what, rec.Code, rec.Body.String(), firstWhat, first.Code, first.Body.String())
		}
	}
}
