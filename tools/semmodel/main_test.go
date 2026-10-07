// SPDX-License-Identifier: MIT

package main

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/semantic"
)

// snapshot writes a model2vec snapshot of three pieces of two into a new
// directory, the way the reference lays one out, and returns the directory. A
// table cut short makes a snapshot the conversion fails on as it reads it; a
// normalization table given here replaces the real one, which the conversion
// copies without parsing and only the read-back can find wrong.
func snapshot(t *testing.T, cut bool, charsmap []byte) string {
	t.Helper()
	dir := t.TempDir()
	if charsmap == nil {
		var err error
		if charsmap, err = os.ReadFile("../../internal/semantic/testdata/charsmap.bin"); err != nil {
			t.Fatal(err)
		}
	}
	tok := fmt.Sprintf(`{"normalizer":{"type":"Precompiled","precompiled_charsmap":%q},`+
		`"model":{"type":"Unigram","unk_id":1,"vocab":[["[PAD]",0],["[UNK]",0],["▁tea",-1]]}}`,
		base64.StdEncoding.EncodeToString(charsmap))
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
	var data []byte
	for _, x := range []float32{0, 0, 0.5, 0.5, 1, 0} {
		data = binary.LittleEndian.AppendUint32(data, math.Float32bits(x))
	}
	head := `{"embeddings":{"dtype":"F32","shape":[3,2],"data_offsets":[0,24]}}`
	file := append(binary.LittleEndian.AppendUint64(nil, uint64(len(head))), head...)
	file = append(file, data...)
	if cut {
		file = file[:len(file)-4]
	}
	if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), file, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestConvert_WritesAModelTheProgramReadsBack(t *testing.T) {
	out := filepath.Join(t.TempDir(), semantic.FileName)
	r, err := convert(snapshot(t, false, nil), out)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if r.pieces != 3 || r.snapshot != 3 {
		t.Errorf("read back %d pieces of the snapshot's %d, want 3 of 3", r.pieces, r.snapshot)
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() != r.size || len(r.sum) != 64 {
		t.Errorf("the file is %v (%v); reported %d bytes, sha256 %q", st, err, r.size, r.sum)
	}
	if _, err := semantic.Load(out); err != nil {
		t.Errorf("the converted model does not load: %v", err)
	}
	if _, err := os.Stat(out + ".part"); !os.IsNotExist(err) {
		t.Errorf("the temporary file is left behind: %v", err)
	}
}

func TestConvert_AFailureLeavesThePreviousModelAndNoTemporaryFile(t *testing.T) {
	// A conversion that fails must not leave a half-written model at the name
	// the release takes its hash from, nor remove the model already there.
	out := filepath.Join(t.TempDir(), semantic.FileName)
	if err := os.WriteFile(out, []byte("the previous model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := convert(snapshot(t, true, nil), out); err == nil {
		t.Fatal("a snapshot with its table cut short converted")
	}
	if got, err := os.ReadFile(out); err != nil || string(got) != "the previous model" {
		t.Errorf("the previous model is now %q (%v)", got, err)
	}
	if _, err := os.Stat(out + ".part"); !os.IsNotExist(err) {
		t.Errorf("the temporary file is left behind: %v", err)
	}
}

func TestConvert_AModelThatDoesNotReadBackIsNotPutInPlace(t *testing.T) {
	// The read-back is what stands between a file Write produced and the name
	// its hash is published under: a normalization table of three bytes is
	// copied into the file as it is, and only Load refuses it.
	out := filepath.Join(t.TempDir(), semantic.FileName)
	_, err := convert(snapshot(t, false, []byte{1, 2, 3}), out)
	if err == nil || !strings.Contains(err.Error(), "does not read back") {
		t.Errorf("a model the program refuses: got %v, want it refused on reading back", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a model the program refuses was put in place: %v", err)
	}
	if _, err := os.Stat(out + ".part"); !os.IsNotExist(err) {
		t.Errorf("the temporary file is left behind: %v", err)
	}
}

func TestConvert_AStaleTemporaryFileIsReplaced(t *testing.T) {
	// A conversion killed part way leaves its temporary file; the next one
	// writes over it rather than stopping.
	out := filepath.Join(t.TempDir(), semantic.FileName)
	if err := os.WriteFile(out+".part", []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := convert(snapshot(t, false, nil), out); err != nil {
		t.Fatalf("a stale temporary file stopped the conversion: %v", err)
	}
	if _, err := semantic.Load(out); err != nil {
		t.Errorf("the converted model does not load: %v", err)
	}
}
