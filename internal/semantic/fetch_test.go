// SPDX-License-Identifier: MIT

package semantic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
		if err := Fetch(context.Background(), srv.Client(), c.url, c.sum, int64(len(body)), path, nil); err == nil {
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
	if err := Fetch(context.Background(), srv.Client(), srv.URL+"/ok", good, int64(len(body)), path, func(done, _ int64) { last = done }); err != nil {
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
	err := Fetch(context.Background(), srv.Client(), srv.URL, strings.Repeat("0", 64), 100, path, nil)
	if err != ErrChecksum {
		t.Errorf("got %v, want ErrChecksum", err)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Errorf("a refused download put a file at %s", path)
	}
}

func TestFetch_StopsReadingOneBytePastTheModelsSize(t *testing.T) {
	// A server that sends more than the model is refused at the first byte too
	// many, not after the disk has taken whatever it chose to send: the hash
	// would refuse it too, but only at the end.
	body := []byte("a model, as far as this test is concerned")
	sum := sha256.Sum256(body)
	good := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
		// Then a megabyte more, a chunk at a time, until the reader hangs up.
		chunk := bytes.Repeat([]byte("x"), 1024)
		for i := 0; i < 1024; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), FileName)
	var got int64
	err := Fetch(context.Background(), srv.Client(), srv.URL, good, int64(len(body)), path, func(done, _ int64) { got = done })
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("a body larger than the model: got %v, want ErrTooLarge", err)
	}
	if got != int64(len(body))+1 {
		t.Errorf("read %d bytes, want the model's %d and one more", got, len(body))
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Errorf("a download larger than the model was kept")
	}
	if _, serr := os.Stat(path + ".part"); !os.IsNotExist(serr) {
		t.Errorf("a download larger than the model left its part behind")
	}
	// Exactly the size is the model.
	exact := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer exact.Close()
	if err := Fetch(context.Background(), exact.Client(), exact.URL, good, int64(len(body)), path, nil); err != nil {
		t.Errorf("a body of exactly the model's size: %v", err)
	}
}

func TestFetch_TriesTheRenameAgainWhileTheOldFileIsHeld(t *testing.T) {
	body := []byte("a model, as far as this test is concerned")
	sum := sha256.Sum256(body)
	good := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	held := errors.New("the file is held by another process")
	failing := func(times int) *int {
		calls := new(int)
		rename = func(from, to string) error {
			*calls++
			if *calls <= times {
				return held
			}
			return os.Rename(from, to)
		}
		return calls
	}
	renamePause = time.Millisecond
	t.Cleanup(func() { rename, renamePause = os.Rename, 200*time.Millisecond })

	path := filepath.Join(t.TempDir(), FileName)
	// Held for a moment: the third try finds it free, and the model is in place.
	calls := failing(2)
	if err := Fetch(context.Background(), srv.Client(), srv.URL, good, int64(len(body)), path, nil); err != nil {
		t.Fatalf("a rename that succeeds on its third try: %v", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, body) || *calls != 3 {
		t.Errorf("kept %q after %d tries, want the body after 3", got, *calls)
	}
	// Held for good: given up after renameTries, and the part is not left.
	if err := os.WriteFile(path, []byte("the old one"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls = failing(1000)
	if err := Fetch(context.Background(), srv.Client(), srv.URL, good, int64(len(body)), path, nil); !errors.Is(err, held) {
		t.Errorf("a rename that never succeeds: got %v, want its error", err)
	}
	if *calls != renameTries {
		t.Errorf("tried %d times, want %d", *calls, renameTries)
	}
	if got, _ := os.ReadFile(path); string(got) != "the old one" {
		t.Errorf("the old file is now %q", got)
	}
	if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
		t.Errorf("a rename given up on left its part behind")
	}
}
