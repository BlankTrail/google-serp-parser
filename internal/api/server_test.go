// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/store"
)

// quiet is the logger a test hands the server. A test that provokes a refusal
// on purpose should not print the server's account of it beside the result.
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gserp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// testServer builds a server and hands back the history behind it, because
// almost every test here has to put a key in that history first.
func testServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := testStore(t)
	s, err := New(Config{Store: st, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, st
}

func TestServer_WillNotStartWithNoHistoryBehindIt(t *testing.T) {
	// A server that starts without one answers every request with a fault at
	// the first read, which reads to whoever deployed it as a broken database
	// rather than as a program that was started wrong.
	if _, err := New(Config{Logger: quiet()}); err == nil {
		t.Error("a server was built with nothing to read from")
	}
}

func TestServer_RefusesAnAddressItDoesNotServeInTheShapeEverythingElseIsRefusedIn(t *testing.T) {
	// A program reading this interface parses one shape. An address nobody
	// registered must not be the one place that answers in another.
	s, _ := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nothing-here", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("an address nobody serves gave %d, want 404", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if _, named := body["error"].(string); !named {
		t.Errorf("the refusal carries no error field: %v", body)
	}
}
