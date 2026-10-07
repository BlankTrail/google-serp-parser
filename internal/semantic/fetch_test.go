// SPDX-License-Identifier: MIT

package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFetch_KeepsAFileThatHashesRightAndNothingElse(t *testing.T) {
	body := []byte("a model, as far as this test is concerned")
	sum := sha256.Sum256(body)
	good := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body)
		case "/short":
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body[:10])
		case "/gone":
			// A server that says no and sends the very bytes asked for anyway:
			// only the status can tell this from the real thing.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("the old one"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ url, sum string }{
		{srv.URL + "/ok", strings.Repeat("0", 64)}, // wrong hash
		{srv.URL + "/short", good},                 // cut short
		{srv.URL + "/missing", good},               // 404
		{srv.URL + "/gone", good},                  // 404 with the right body
	} {
		if err := Fetch(context.Background(), srv.Client(), c.url, c.sum, path, nil); err == nil {
			t.Errorf("%s with %s was kept", c.url, c.sum[:8])
		}
		if got, _ := os.ReadFile(path); string(got) != "the old one" {
			t.Errorf("%s left the old file as %q", c.url, got)
		}
		if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
			t.Errorf("%s left its part behind", c.url)
		}
	}
	var last int64
	if err := Fetch(context.Background(), srv.Client(), srv.URL+"/ok", good, path, func(done, _ int64) { last = done }); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(body) || last != int64(len(body)) {
		t.Errorf("kept %q with progress %d, want the body and %d", got, last, len(body))
	}
	if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
		t.Errorf("a good download left its part behind")
	}
}

func TestFetch_SaysWhyAWrongHashWasRefused(t *testing.T) {
	// The settings page shows one sentence for every failure and the log carries
	// the cause, so the cause has to be one that can be told apart.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("something else"))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), FileName)
	err := Fetch(context.Background(), srv.Client(), srv.URL, strings.Repeat("0", 64), path, nil)
	if err != ErrChecksum {
		t.Errorf("got %v, want ErrChecksum", err)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Errorf("a refused download put a file at %s", path)
	}
}
