// SPDX-License-Identifier: MIT

package semantic

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// snapshotSpec is a model2vec snapshot in miniature: what the tests vary is
// the pieces, and how the safetensors header describes the table.
type snapshotSpec struct {
	pieces  []string
	dtype   string
	shape   []int
	offsets [2]int
	rows    [][]float32 // the table's data; nil means zeros for shape
	trim    int         // bytes cut off the end of the file
	pad     int         // bytes between the header and the table, as other tensors would take
}

// writeSnapshot writes tokenizer.json and model.safetensors the way the
// reference does and returns their directory.
func writeSnapshot(t *testing.T, s snapshotSpec) string {
	t.Helper()
	dir := t.TempDir()
	charsmap, err := os.ReadFile("testdata/charsmap.bin")
	if err != nil {
		t.Fatal(err)
	}
	var vocab []string
	for i, p := range s.pieces {
		pj, _ := json.Marshal(p)
		vocab = append(vocab, fmt.Sprintf("[%s,%d]", pj, -i))
	}
	tok := fmt.Sprintf(`{"normalizer":{"type":"Sequence","normalizers":[{"type":"Precompiled","precompiled_charsmap":%q}]},`+
		`"model":{"type":"Unigram","unk_id":1,"vocab":[%s]}}`, base64.StdEncoding.EncodeToString(charsmap), strings.Join(vocab, ","))
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
	var data []byte
	for _, r := range s.rows {
		for _, x := range r {
			data = binary.LittleEndian.AppendUint32(data, math.Float32bits(x))
		}
	}
	head := fmt.Sprintf(`{"__metadata__":{"format":"pt"},"embeddings":{"dtype":%q,"shape":%s,"data_offsets":[%d,%d]}}`,
		s.dtype, mustJSON(s.shape), s.offsets[0], s.offsets[1])
	file := binary.LittleEndian.AppendUint64(nil, uint64(len(head)))
	file = append(append(file, head...), make([]byte, s.pad)...)
	file = append(file, data...)
	file = file[:len(file)-s.trim]
	if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), file, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

var tinyPieces = []string{"[PAD]", "[UNK]", "▁", "▁кофе", "▁машина", "▁чайник", "▁погода"}

var tinyRows = [][]float32{
	{0, 0, 0}, {0, 0, 0}, {0.1, 0.1, 0.1},
	{1, 0.2, 0}, {0.9, 0.3, 0}, {0.8, 0.1, 0.1}, {0, 0, 1},
}

func tinySpec() snapshotSpec {
	return snapshotSpec{pieces: tinyPieces, dtype: "F32", shape: []int{7, 3}, offsets: [2]int{0, 84}, rows: tinyRows}
}

// zeroRows is n rows of dim zeros, for a snapshot whose values do not matter.
func zeroRows(n, dim int) [][]float32 {
	rows := make([][]float32, n)
	for i := range rows {
		rows[i] = make([]float32, dim)
	}
	return rows
}

func TestReadSource_ConvertsASnapshotIntoTheSameModel(t *testing.T) {
	v, charsmap, rows, median, err := ReadSource(writeSnapshot(t, tinySpec()))
	if err != nil {
		t.Fatalf("ReadSource: %v", err)
	}
	if len(rows) != 7 || len(rows[3]) != 3 || rows[3][0] != 1 || rows[6][2] != 1 {
		t.Fatalf("rows read wrongly: %v", rows)
	}
	if median != 5 {
		t.Errorf("median %d, want 5 (lengths 1,5,5,5,7,7,7)", median)
	}
	var buf bytes.Buffer
	if err := Write(&buf, v, charsmap, rows, median); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := tinyModel(t)
	for _, p := range [][2]string{{"кофе машина", "кофе чайник"}, {"кофе машина", "погода"}} {
		if g, w := got.Score(p[0], p[1]), want.Score(p[0], p[1]); math.Abs(float64(g-w)) > 1e-6 {
			t.Errorf("Score(%q, %q) = %v, tinyModel gives %v", p[0], p[1], g, w)
		}
	}
}

func TestReadSource_FindsTheTableWhereTheHeaderSaysItIs(t *testing.T) {
	// Another tensor may come first; the table starts at its own offset, which
	// is counted from the end of the header.
	s := tinySpec()
	s.pad, s.offsets = 12, [2]int{12, 96}
	_, _, rows, _, err := ReadSource(writeSnapshot(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if rows[3][0] != 1 || rows[6][2] != 1 {
		t.Errorf("the table was read from the wrong place: %v", rows)
	}
}

func TestReadSource_TheRowsDoNotShareTheirTails(t *testing.T) {
	// Rows are slices of one backing array; a row that could be appended to would
	// overwrite its neighbour.
	_, _, rows, _, err := ReadSource(writeSnapshot(t, tinySpec()))
	if err != nil {
		t.Fatal(err)
	}
	_ = append(rows[3], 9)
	if rows[4][0] != 0.9 {
		t.Errorf("appending to a row changed the next one: %v", rows[4])
	}
}

func TestReadSource_TheMedianIsTheMeanOfTheMiddleTwoCutToAnInteger(t *testing.T) {
	// Lengths 1, 2, 5, 6: np.median is 3.5 and the integer part is 3.
	s := snapshotSpec{
		pieces: []string{"a", "bb", "ccccc", "dddddd"}, dtype: "F32",
		shape: []int{4, 1}, offsets: [2]int{0, 16}, rows: zeroRows(4, 1),
	}
	_, _, _, median, err := ReadSource(writeSnapshot(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if median != 3 {
		t.Errorf("median %d, want 3", median)
	}
	// Characters, not bytes: three Cyrillic letters are length 3, and an odd
	// count takes the middle one.
	s = snapshotSpec{
		pieces: []string{"abcd", "жжж", "z"}, dtype: "F32",
		shape: []int{3, 1}, offsets: [2]int{0, 12}, rows: zeroRows(3, 1),
	}
	if _, _, _, median, err = ReadSource(writeSnapshot(t, s)); err != nil || median != 3 {
		t.Errorf("median %d, err %v; want 3 (lengths 1, 3, 4)", median, err)
	}
}

func TestReadSource_RefusesATableThatIsNotTheVocabulary(t *testing.T) {
	s := tinySpec()
	s.shape, s.offsets, s.rows = []int{6, 3}, [2]int{0, 72}, tinyRows[:6]
	_, _, _, _, err := ReadSource(writeSnapshot(t, s))
	if !errors.Is(err, ErrModel) || !strings.Contains(err.Error(), "6 rows for 7 pieces") {
		t.Errorf("a table of 6 rows for 7 pieces: %v", err)
	}
}

func TestReadSource_RefusesATableThatIsNotFloat32OrNotTwoDimensional(t *testing.T) {
	s := tinySpec()
	s.dtype = "F16"
	if _, _, _, _, err := ReadSource(writeSnapshot(t, s)); !errors.Is(err, ErrModel) {
		t.Errorf("F16: %v", err)
	}
	s = tinySpec()
	s.shape = []int{7}
	if _, _, _, _, err := ReadSource(writeSnapshot(t, s)); !errors.Is(err, ErrModel) {
		t.Errorf("one dimension: %v", err)
	}
}

func TestReadSource_RefusesDataThatIsNotTheTableItDescribes(t *testing.T) {
	cases := map[string]func(*snapshotSpec){
		"offsets span less than the shape": func(s *snapshotSpec) { s.offsets = [2]int{0, 80} },
		"offsets span more than the shape": func(s *snapshotSpec) { s.offsets = [2]int{0, 88} },
		"offsets reach past the file":      func(s *snapshotSpec) { s.offsets = [2]int{4, 88} },
		"offsets run backwards":            func(s *snapshotSpec) { s.offsets = [2]int{84, 0} },
		"negative offset":                  func(s *snapshotSpec) { s.offsets = [2]int{-4, 80} },
		"the file is cut inside the table": func(s *snapshotSpec) { s.trim = 8; s.offsets = [2]int{0, 84} },
		"no columns":                       func(s *snapshotSpec) { s.shape = []int{7, 0}; s.offsets = [2]int{0, 0} },
		"shape too big for the file":       func(s *snapshotSpec) { s.shape = []int{7, 1 << 40} },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			s := tinySpec()
			mut(&s)
			if _, _, _, _, err := ReadSource(writeSnapshot(t, s)); !errors.Is(err, ErrModel) {
				t.Errorf("want ErrModel, got %v", err)
			}
		})
	}
}

func TestReadSource_AllocatesNothingForATableThatIsNotInTheFile(t *testing.T) {
	// The shape and the offsets agree with each other and with the pieces, so
	// only the size of the file can say the table is not there; the backing array
	// must not be sized from the header before that is checked.
	s := tinySpec()
	s.shape, s.offsets = []int{7, 1 << 22}, [2]int{0, 4 * 7 << 22}
	dir := writeSnapshot(t, s)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _, _, _, err := ReadSource(dir)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrModel) {
		t.Errorf("want ErrModel, got %v", err)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 32<<20 {
		t.Errorf("a table 117 MB long that is not in a 200-byte file cost %d MB of allocation", grown>>20)
	}
}

func TestReadSource_RefusesAHeaderThatIsNotOne(t *testing.T) {
	dir := writeSnapshot(t, tinySpec())
	path := filepath.Join(dir, "model.safetensors")
	for name, raw := range map[string][]byte{
		"shorter than the length": {1, 2, 3},
		"longer than the file":    binary.LittleEndian.AppendUint64(nil, 1<<40),
		"not JSON":                append(binary.LittleEndian.AppendUint64(nil, 3), "abc"...),
		"cut off in the file":     append(binary.LittleEndian.AppendUint64(nil, 20), "{}"...),
	} {
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, _, _, err := ReadSource(dir); !errors.Is(err, ErrModel) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestReadSource_ReportsAMissingSnapshotFile(t *testing.T) {
	dir := writeSnapshot(t, tinySpec())
	if err := os.Remove(filepath.Join(dir, "model.safetensors")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := ReadSource(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no safetensors: %v", err)
	}
	if _, _, _, _, err := ReadSource(t.TempDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no tokenizer: %v", err)
	}
}

func TestReadSource_ReadsATableLargerThanItsChunk(t *testing.T) {
	// The table is streamed in 4 MiB chunks; one a little over a chunk long must
	// come out whole and in order, with the last short chunk included.
	const n, dim = 3000, 400 // 4.8 MB: one full chunk and a short one
	pieces := make([]string, n)
	rows := zeroRows(n, dim)
	for i := range pieces {
		pieces[i] = fmt.Sprintf("p%d", i)
		rows[i][0], rows[i][dim-1] = float32(i), float32(-i)
	}
	pieces[1] = "[UNK]"
	s := snapshotSpec{pieces: pieces, dtype: "F32", shape: []int{n, dim}, offsets: [2]int{0, 4 * n * dim}, rows: rows}
	_, _, got, _, err := ReadSource(writeSnapshot(t, s))
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 2, 1310, 1311, 1312, 2999} {
		if got[i][0] != float32(i) || got[i][dim-1] != float32(-i) {
			t.Errorf("row %d came out as %v ... %v", i, got[i][0], got[i][dim-1])
		}
	}
}
