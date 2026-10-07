// SPDX-License-Identifier: MIT

package semantic

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// writeTinyModel puts the tiny model of the tests into a file at path.
func writeTinyModel(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/charsmap.bin")
	if err != nil {
		t.Fatal(err)
	}
	v := Vocab{
		Pieces: []string{"[PAD]", "[UNK]", "▁", "▁кофе"},
		Scores: []float32{0, 0, -5, -2},
		Unk:    1,
	}
	var buf bytes.Buffer
	if err := Write(&buf, v, raw, [][]float32{{0, 0}, {0, 0}, {0.1, 0.1}, {1, 0}}, 4); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHolder_LooksAgainForAFileThatWasNotThereAndReadsOnceOneThatIs(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	h := NewHolder(path)
	if h.Path() != path {
		t.Errorf("Path = %q, want %q", h.Path(), path)
	}
	if h.Present() {
		t.Error("Present with no file")
	}
	if _, err := h.Get(); !os.IsNotExist(err) {
		t.Fatalf("Get with no file: %v, want not-exist", err)
	}
	// Downloaded in the meantime: not remembering the absence is what lets the
	// program use it without a restart.
	writeTinyModel(t, path)
	if !h.Present() {
		t.Error("not Present with a file")
	}
	m, err := h.Get()
	if err != nil || m == nil {
		t.Fatalf("Get after the download: %v, %v", m, err)
	}
	if again, _ := h.Get(); again != m {
		t.Error("a second Get read the file again instead of keeping the model")
	}
}

func TestHolder_RemembersABrokenFileUntilReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("not a model"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHolder(path)
	if _, err := h.Get(); err == nil {
		t.Fatal("a broken file was read")
	}
	// Mended behind the holder's back: a file that cannot be read is not read
	// again on every call, so the mending is not seen yet.
	writeTinyModel(t, path)
	if _, err := h.Get(); err == nil {
		t.Error("the broken file was forgotten without a Reset")
	}
	h.Reset()
	if m, err := h.Get(); err != nil || m == nil {
		t.Errorf("Get after Reset: %v, %v", m, err)
	}
}

func TestHolder_ResetMakesTheNextGetReadTheFileAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	writeTinyModel(t, path)
	h := NewHolder(path)
	first, err := h.Get()
	if err != nil {
		t.Fatal(err)
	}
	h.Reset()
	second, err := h.Get()
	if err != nil || second == first {
		t.Errorf("after Reset got %p again (err %v), want a fresh read", second, err)
	}
}
