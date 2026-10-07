// SPDX-License-Identifier: MIT

package semantic

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// tinyModel writes a model of a handful of pieces through Write and reads it
// back, so every test below goes through the file format a real model does.
func tinyModel(t *testing.T) *Model { return tinyModelWith(t, 4) }

// tinyModelWith is tinyModel with the median piece length the text is cut by.
func tinyModelWith(t *testing.T, medianLen int) *Model {
	t.Helper()
	raw, err := os.ReadFile("testdata/charsmap.bin")
	if err != nil {
		t.Fatal(err)
	}
	v := Vocab{
		Pieces: []string{"[PAD]", "[UNK]", "▁", "▁кофе", "▁машина", "▁чайник", "▁погода"},
		Scores: []float32{0, 0, -5, -2, -2, -2, -2},
		Unk:    1,
	}
	rows := [][]float32{
		{0, 0, 0}, {0, 0, 0}, {0.1, 0.1, 0.1},
		{1, 0.2, 0}, {0.9, 0.3, 0}, {0.8, 0.1, 0.1}, {0, 0, 1},
	}
	var buf bytes.Buffer
	if err := Write(&buf, v, raw, rows, medianLen); err != nil {
		t.Fatalf("Write: %v", err)
	}
	m, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return m
}

func TestModel_ScoresAPhraseAboutTheKeyAboveOneAboutSomethingElse(t *testing.T) {
	m := tinyModel(t)
	near, far := m.Score("кофе машина", "кофе чайник"), m.Score("кофе машина", "погода")
	if !(near > 0.9 && far < 0.2) {
		t.Errorf("near %.3f, far %.3f; want near above 0.9 and far below 0.2", near, far)
	}
}

func TestModel_VectorIsTheNormalizedSumOfThePieces(t *testing.T) {
	m := tinyModel(t)
	v := m.Vector("кофе машина")
	want := []float64{1.9, 0.5, 0}
	n := math.Sqrt(1.9*1.9 + 0.5*0.5)
	for i := range want {
		if d := math.Abs(float64(v[i]) - want[i]/n); d > 0.02 {
			t.Errorf("component %d is %.4f, want %.4f", i, v[i], want[i]/n)
		}
	}
	if z := m.Vector(""); z[0] != 0 || z[1] != 0 || z[2] != 0 {
		t.Errorf("an empty phrase is %v, want nought", z)
	}
	if got := m.Score("", "кофе"); got != 0 {
		t.Errorf("an empty key scores %v, want nought", got)
	}
}

func TestRead_RefusesAFileThatIsNotAModel(t *testing.T) {
	for name, raw := range map[string][]byte{
		"empty":           nil,
		"wrong signature": []byte("NOTAMODEL0000000"),
		"cut short":       append([]byte("GSSEM1\x00\x00"), binary.LittleEndian.AppendUint32(nil, 256)...),
	} {
		if _, err := Read(bytes.NewReader(raw)); err == nil {
			t.Errorf("%s: read as a model", name)
		}
	}
}

func TestModel_MatchesTheReferenceVectors(t *testing.T) {
	path := os.Getenv("GSERP_SEMANTIC_MODEL")
	if path == "" {
		t.Skip("GSERP_SEMANTIC_MODEL is not set: no converted model on this machine")
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := os.ReadFile("testdata/vectors.bin")
	if err != nil {
		t.Fatal(err)
	}
	count, dim := int(binary.LittleEndian.Uint32(ref)), int(binary.LittleEndian.Uint32(ref[4:]))
	phrases := readPhrases(t)
	worst, zero := float32(1), 0
	for i := 0; i < count; i++ {
		got := m.Vector(phrases[i])
		var dot, refNorm, gotNorm float32
		for j := 0; j < dim; j++ {
			want := math.Float32frombits(binary.LittleEndian.Uint32(ref[8+4*(i*dim+j):]))
			dot += got[j] * want
			refNorm += want * want
			gotNorm += got[j] * got[j]
		}
		if refNorm == 0 {
			// An empty or whitespace-only phrase has no pieces and the reference
			// gives the nought vector; agreeing means giving nought too, which a
			// cosine cannot say.
			zero++
			if gotNorm != 0 {
				t.Errorf("phrase %d has a nought reference vector but ours has norm %v", i, gotNorm)
			}
			continue
		}
		if dot < worst {
			worst = dot
		}
	}
	if zero == 0 {
		t.Error("no nought reference vector among the phrases: the empty phrases are missing from the reference")
	}
	t.Logf("%d phrases: worst cosine %.4f, %d nought reference vectors agreed", count, worst, zero)
	// int8 costs a little: the vectors agree to 0.99 or better, not exactly.
	if worst < 0.99 {
		t.Errorf("the worst phrase agrees with the reference to %.4f, want 0.99 or better", worst)
	}
}

func readPhrases(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("testdata/phrases.txt")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// tinyFile is the bytes of a three-piece model, for the tests that damage a file.
func tinyFile(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/charsmap.bin")
	if err != nil {
		t.Fatal(err)
	}
	v := Vocab{Pieces: []string{"[UNK]", "▁кофе", "▁чайник"}, Scores: []float32{0, -2, -2}, Unk: 0}
	rows := [][]float32{{0, 0, 0}, {1, 0.2, 0}, {0.8, 0.1, 0.1}}
	var buf bytes.Buffer
	if err := Write(&buf, v, raw, rows, 4); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Offsets of the header fields in a file: signature, dim, pieces, unk,
// medianLen, charsmapLen.
const (
	offDim    = 8
	offPieces = 12
	offUnk    = 16
	offCmLen  = 24
	headerEnd = 28
)

func patched(raw []byte, off int, v uint32) []byte {
	out := append([]byte(nil), raw...)
	binary.LittleEndian.PutUint32(out[off:], v)
	return out
}

func TestRead_RefusesAHeaderThatCannotBeAModel(t *testing.T) {
	raw := tinyFile(t)
	if _, err := Read(bytes.NewReader(raw)); err != nil {
		t.Fatalf("the undamaged file: %v", err)
	}
	for name, bad := range map[string][]byte{
		"another version of the format":   append([]byte("GSSEM2\x00\x00"), raw[8:]...),
		"a signature of another file":     append([]byte("GSSEM1\x00\x01"), raw[8:]...),
		"no dimension":                    patched(raw, offDim, 0),
		"no pieces":                       patched(raw, offPieces, 0),
		"an unknown id past the pieces":   patched(raw, offUnk, 3),
		"a charsmap longer than the file": patched(raw, offCmLen, uint32(len(raw))),
	} {
		if _, err := Read(bytes.NewReader(bad)); !errors.Is(err, ErrModel) {
			t.Errorf("%s: got %v, want ErrModel", name, err)
		}
	}
}

func TestRead_ABoundOnTheDimensionAndOnWhatItAllocates(t *testing.T) {
	raw, err := os.ReadFile("testdata/charsmap.bin")
	if err != nil {
		t.Fatal(err)
	}
	build := func(dim int) []byte {
		var buf bytes.Buffer
		v := Vocab{Pieces: []string{"[UNK]"}, Scores: []float32{0}}
		if err := Write(&buf, v, raw, [][]float32{make([]float32, dim)}, 4); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	if _, err := Read(bytes.NewReader(build(4096))); err != nil {
		t.Errorf("a dimension of 4096: %v", err)
	}
	if _, err := Read(bytes.NewReader(build(4097))); !errors.Is(err, ErrModel) {
		t.Errorf("a dimension of 4097: got %v, want ErrModel", err)
	}

	// A header that claims more than the limits is refused before anything is
	// allocated for it; the file behind it is only a few bytes.
	file := tinyFile(t)
	for name, bad := range map[string][]byte{
		"ten million and one pieces":                      patched(patched(file, offPieces, 10_000_001), offDim, 4),
		"a charsmap of 64 MB and a byte":                  patched(file, offCmLen, 64<<20+1),
		"a charsmap of exactly 64 MB, no bytes behind it": patched(file, offCmLen, 64<<20),
	} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := Read(bytes.NewReader(bad))
		runtime.ReadMemStats(&after)
		if !errors.Is(err, ErrModel) {
			t.Errorf("%s: got %v, want ErrModel", name, err)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
			t.Errorf("%s: reading it allocated %d MB before refusing", name, grew>>20)
		}
	}
}

func TestRead_RefusesAFileCutShortAnywhere(t *testing.T) {
	raw := tinyFile(t)
	cm := int(binary.LittleEndian.Uint32(raw[offCmLen:]))
	piecesAt := headerEnd + cm
	// Each pieces record is a uint16, the bytes of the piece and a float32.
	rowsAt := piecesAt
	for i := 0; i < 3; i++ {
		rowsAt += 2 + int(binary.LittleEndian.Uint16(raw[rowsAt:])) + 4
	}
	cuts := []int{headerEnd, headerEnd + 1, headerEnd + cm - 1, piecesAt, piecesAt + 1, piecesAt + 3,
		rowsAt - 1, rowsAt, rowsAt + 2, rowsAt + 4, rowsAt + 5, len(raw) - 1}
	for i := 9; i < headerEnd; i += 3 {
		cuts = append(cuts, i)
	}
	for _, n := range cuts {
		if _, err := Read(bytes.NewReader(raw[:n])); !errors.Is(err, ErrModel) {
			t.Errorf("cut to %d of %d bytes: got %v, want ErrModel", n, len(raw), err)
		}
	}
}

func TestRead_RefusesAPieceThatIsNotText(t *testing.T) {
	raw, err := os.ReadFile("testdata/charsmap.bin")
	if err != nil {
		t.Fatal(err)
	}
	v := Vocab{Pieces: []string{"[UNK]", "\xff\xfe"}, Scores: []float32{0, -1}}
	var buf bytes.Buffer
	if err := Write(&buf, v, raw, [][]float32{{0}, {1}}, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(&buf); !errors.Is(err, ErrModel) {
		t.Errorf("a piece of invalid UTF-8: got %v, want ErrModel", err)
	}
}

func TestRead_RefusesANormalizationTableThatIsNotOne(t *testing.T) {
	v := Vocab{Pieces: []string{"[UNK]"}, Scores: []float32{0}}
	var buf bytes.Buffer
	if err := Write(&buf, v, []byte{1, 2, 3}, [][]float32{{1}}, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(&buf); !errors.Is(err, ErrModel) {
		t.Errorf("a table of three bytes: got %v, want ErrModel", err)
	}
}

func TestLoad_ReadsAFileAndReportsAMissingOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, tinyFile(t), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Score("кофе", "кофе"); got < 0.999 {
		t.Errorf("a phrase scores %.4f against itself, want 1", got)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "absent.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing file: got %v, want not-exist", err)
	}
}

func TestWrite_RefusesWhatItCannotWriteAsAModel(t *testing.T) {
	raw, err := os.ReadFile("testdata/charsmap.bin")
	if err != nil {
		t.Fatal(err)
	}
	two := Vocab{Pieces: []string{"a", "b"}, Scores: []float32{0, 0}}
	for name, c := range map[string]struct {
		v    Vocab
		rows [][]float32
	}{
		"fewer rows than pieces":          {two, [][]float32{{1}}},
		"fewer scores than pieces":        {Vocab{Pieces: []string{"a", "b"}, Scores: []float32{0}}, [][]float32{{1}, {1}}},
		"no pieces":                       {Vocab{}, nil},
		"a ragged row":                    {two, [][]float32{{1, 2}, {1}}},
		"a piece too long for its length": {Vocab{Pieces: []string{strings.Repeat("a", 65536)}, Scores: []float32{0}}, [][]float32{{1}}},
	} {
		if err := Write(io.Discard, c.v, raw, c.rows, 4); !errors.Is(err, ErrModel) {
			t.Errorf("%s: got %v, want ErrModel", name, err)
		}
	}
}

// quantModel has a piece whose row quantizes with a fractional part near .9 and
// whose largest component is negative, and a known piece whose row is nought.
func quantModel(t *testing.T) *Model {
	t.Helper()
	raw, err := os.ReadFile("testdata/charsmap.bin")
	if err != nil {
		t.Fatal(err)
	}
	v := Vocab{Pieces: []string{"[UNK]", "▁кофе", "▁нуль"}, Scores: []float32{0, -2, -2}}
	x := float32(75.9 / 127)
	rows := [][]float32{{0, 0, 0}, {-1, -x, x}, {0, 0, 0}}
	var buf bytes.Buffer
	if err := Write(&buf, v, raw, rows, 4); err != nil {
		t.Fatal(err)
	}
	m, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestModel_QuantizesToTheNearestStepAndKeepsTheSign(t *testing.T) {
	m := quantModel(t)
	x := 75.9 / 127.0
	n := math.Sqrt(1 + 2*x*x)
	want := []float64{-1 / n, -x / n, x / n}
	got := m.Vector("кофе")
	for i := range want {
		if d := math.Abs(float64(got[i]) - want[i]); d > 0.0015 {
			t.Errorf("component %d is %.5f, want %.5f", i, got[i], want[i])
		}
	}
}

func TestModel_APieceOfNoughtGivesTheNoughtVector(t *testing.T) {
	m := quantModel(t)
	for _, phrase := range []string{"нуль", "ё"} {
		for i, c := range m.Vector(phrase) {
			if c != 0 {
				t.Errorf("%q: component %d is %v, want nought (not NaN)", phrase, i, c)
			}
		}
	}
	if got := m.Score("нуль", "кофе"); got != 0 {
		t.Errorf("a phrase of nought scores %v against a phrase, want 0", got)
	}
}

func TestModel_UnknownWordsCountAsTheUnknownPieceAndScoreIsSymmetric(t *testing.T) {
	m := tinyModel(t)
	// The unknown piece's row is nought in tinyModel, so an unknown word adds
	// nothing to the direction (a real table's row is not nought: see below).
	if got := m.Score("кофе", "кофе ёёё"); got < 0.99 {
		t.Errorf("an unknown word with a nought row changes the vector: score %.4f, want 0.99 or better", got)
	}
	if a, b := m.Score("кофе", "погода"), m.Score("погода", "кофе"); a != b {
		t.Errorf("score is not symmetric: %v and %v", a, b)
	}
}

func cosine(a, b []float32) float64 {
	var d float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
	}
	return d
}

func TestModel_CutsALongPhraseAsModel2vecDoes(t *testing.T) {
	m := tinyModel(t) // a median of 4: the text is cut at 512*4 = 2048 characters
	// The words of the tail lie past the 2048th character, so they are not read.
	past := strings.Repeat("машина ", 300) + strings.Repeat("погода ", 100)
	if c := cosine(m.Vector(past), m.Vector("машина")); c < 0.999 {
		t.Errorf("a phrase past the text limit: cosine %.4f with its head, want 0.999 or better", c)
	}
	// More than 512 characters, but under the limit: the tail is read.
	within := strings.Repeat("кофе ", 100) + strings.Repeat("погода ", 100)
	if c := cosine(m.Vector(within), m.Vector("кофе")); c > 0.8 {
		t.Errorf("a phrase under the text limit: cosine %.4f with its head, want the tail to count (under 0.8)", c)
	}
	// More than 512 pieces in under 2048 characters: only the first 512 are
	// read, all of them the lone mark, before the words that follow.
	manyPieces := strings.Repeat("а ", 600) + strings.Repeat("погода ", 100)
	if c := cosine(m.Vector(manyPieces), m.Vector("а")); c < 0.9999 {
		t.Errorf("a phrase of 600+ pieces: cosine %.4f with its first pieces, want 0.9999 or better", c)
	}
	// The cut falls on the 512th piece: it is read, the one after it is not.
	edge := strings.Repeat("а ", 511) + "кофе погода погода"
	if c := cosine(m.Vector(edge), m.Vector(strings.Repeat("а ", 511)+"кофе")); c < 0.99999 {
		t.Errorf("a cut at the 512th piece: cosine %.6f with the first 512 pieces, want 1", c)
	}
	// With no median in the file the text is not cut, only the pieces are.
	free := tinyModelWith(t, 0)
	if c := cosine(free.Vector(past), free.Vector("машина")); c > 0.99 {
		t.Errorf("a model of median 0: cosine %.4f, want the text to be left whole", c)
	}
	if v := free.Vector("кофе"); v[0] == 0 {
		t.Errorf("a model of median 0 reads nothing: %v", v)
	}
}

// pieceTables writes a model of the given pieces, their scores and the unknown
// id, with a row per piece taken from rows, and reads it back.
func pieceTables(t *testing.T, pieces []string, scores []float32, unk int32, rows [][]float32) *Model {
	t.Helper()
	raw, err := os.ReadFile("testdata/charsmap.bin")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, Vocab{Pieces: pieces, Scores: scores, Unk: unk}, raw, rows, 4); err != nil {
		t.Fatal(err)
	}
	m, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestModel_TheUnknownPieceIsTheOneTheFileNames(t *testing.T) {
	// The word is piece 0 and the unknown piece is 1: a file that lost its unk
	// id would drop the word as unknown and keep the marker for the rest.
	m := pieceTables(t, []string{"▁кофе", "[UNK]"}, []float32{-2, 0}, 1, [][]float32{{1, 0}, {0, 1}})
	if v := m.Vector("кофе"); v[0] < 0.99 {
		t.Errorf("a known word as piece 0 reads as %v, want its own row", v)
	}
	// The unknown piece's row is the second axis here, so an unknown character
	// reads as that row (model2vec averages it in), not as nought.
	if v := m.Vector("ё"); v[0] != 0 || v[1] < 0.99 {
		t.Errorf("an unknown character reads as %v, want the unknown piece's row", v)
	}
}

func TestModel_TheUnknownPieceIsSummedLikeAnyOtherPiece(t *testing.T) {
	m := pieceTables(t, []string{"▁кофе", "[UNK]"}, []float32{-2, 0}, 1, [][]float32{{1, 0}, {0, 1}})
	// "кофе ё" is the word and one unknown piece: the direction of (1,0)+(0,1).
	v := m.Vector("кофе ё")
	if math.Abs(float64(v[0])-math.Sqrt2/2) > 0.01 || math.Abs(float64(v[1])-math.Sqrt2/2) > 0.01 {
		t.Errorf("a word and an unknown piece read as %v, want both axes at 0.707", v)
	}
	// A run of unknown characters is one unknown piece, not three: with three
	// the unknown axis would outweigh the word.
	if w := m.Vector("кофе ёёё"); math.Abs(float64(w[0]-v[0])) > 0.01 {
		t.Errorf("a run of unknown characters reads as %v, want %v", w, v)
	}
}

func TestModel_TheScoresOfTheFileChooseTheCut(t *testing.T) {
	// "аб" is read as the one piece "аб" or as "а" and "б", whichever the scores
	// favour; the rows tell the two apart.
	pieces := []string{"[UNK]", "▁", "а", "б", "аб"}
	rows := [][]float32{{0, 0, 0}, {0, 0, 0}, {1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	whole := pieceTables(t, pieces, []float32{0, -1, -3, -3, -2}, 0, rows)
	split := pieceTables(t, pieces, []float32{0, -1, -1, -1, -5}, 0, rows)
	if v := whole.Vector("аб"); v[2] < 0.99 {
		t.Errorf("scores that favour the whole piece read as %v, want its row", v)
	}
	if v := split.Vector("аб"); v[2] > 0.01 || v[0] < 0.5 {
		t.Errorf("scores that favour the parts read as %v, want the rows of the parts", v)
	}
}

func TestRead_RefusesAHeaderWhosePiecesTimesDimensionIsTooMuch(t *testing.T) {
	file := tinyFile(t)
	// Each factor is within its own bound; the product is 40 billion values.
	hostile := patched(patched(file, offPieces, 10_000_000), offDim, 4096)
	start := time.Now()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := Read(bytes.NewReader(hostile))
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrModel) || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("got %v, want ErrModel for a table that is too large", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("refusing took %v", d)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Errorf("refusing allocated %d MB", grew>>20)
	}
	// Under the bound but with no data behind it: nothing is allocated for the
	// claim, the file runs out first.
	claim := patched(patched(file, offPieces, 2_000_000), offDim, 1000)
	runtime.ReadMemStats(&before)
	_, err = Read(bytes.NewReader(claim))
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrModel) {
		t.Errorf("got %v, want ErrModel", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Errorf("a short file claiming 2 GB allocated %d MB", grew>>20)
	}
}

func TestRead_RefusesBytesAfterTheLastRow(t *testing.T) {
	raw := tinyFile(t)
	if _, err := Read(bytes.NewReader(append(raw, 0))); !errors.Is(err, ErrModel) {
		t.Errorf("a file one byte long: got %v, want ErrModel", err)
	}
}

func TestRead_RefusesAScaleThatIsNotAScale(t *testing.T) {
	raw := tinyFile(t)
	cm := int(binary.LittleEndian.Uint32(raw[offCmLen:]))
	at := headerEnd + cm
	for i := 0; i < 3; i++ {
		at += 2 + int(binary.LittleEndian.Uint16(raw[at:])) + 4
	}
	// Row 1 follows row 0's scale (4 bytes) and 3 values.
	row1 := at + 4 + 3
	for name, v := range map[string]float32{
		"negative": -0.5, "NaN": float32(math.NaN()), "infinite": float32(math.Inf(1)), "negative infinity": float32(math.Inf(-1)),
	} {
		damaged := append([]byte(nil), raw...)
		binary.LittleEndian.PutUint32(damaged[row1:], math.Float32bits(v))
		if _, err := Read(bytes.NewReader(damaged)); !errors.Is(err, ErrModel) {
			t.Errorf("a %s scale: got %v, want ErrModel", name, err)
		}
	}
	good := append([]byte(nil), raw...)
	binary.LittleEndian.PutUint32(good[row1:], math.Float32bits(0.5))
	if _, err := Read(bytes.NewReader(good)); err != nil {
		t.Errorf("a scale of 0.5: %v", err)
	}
}

func TestWrite_RefusesARowWithNoNumberInIt(t *testing.T) {
	raw, err := os.ReadFile("testdata/charsmap.bin")
	if err != nil {
		t.Fatal(err)
	}
	v := Vocab{Pieces: []string{"a"}, Scores: []float32{0}}
	for name, x := range map[string]float32{"NaN": float32(math.NaN()), "Inf": float32(math.Inf(1)), "-Inf": float32(math.Inf(-1))} {
		if err := Write(io.Discard, v, raw, [][]float32{{1, x}}, 4); !errors.Is(err, ErrModel) {
			t.Errorf("%s: got %v, want ErrModel", name, err)
		}
	}
}

func TestRead_TheErrorIsErrModelAndSaysWhy(t *testing.T) {
	raw := tinyFile(t)
	_, err := Read(bytes.NewReader(raw[:len(raw)-1]))
	if !errors.Is(err, ErrModel) {
		t.Fatalf("got %v, want ErrModel", err)
	}
	if !strings.Contains(err.Error(), "EOF") {
		t.Errorf("%q does not say why the file was refused", err)
	}
}

func TestRead_ABoundOnTheNormalizationTable(t *testing.T) {
	// A well-formed table one byte past 64 MB: a trie of 8 bytes, then
	// replacements.
	big := make([]byte, 64<<20+1)
	binary.LittleEndian.PutUint32(big, 8)
	v := Vocab{Pieces: []string{"[UNK]"}, Scores: []float32{0}}
	var buf bytes.Buffer
	if err := Write(&buf, v, big, [][]float32{{1}}, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(&buf); !errors.Is(err, ErrModel) {
		t.Errorf("a table of 64 MB and a byte: got %v, want ErrModel", err)
	}
}

func TestRead_ReadsAModelAsBigAsTheRealOneIsInProportion(t *testing.T) {
	// Five thousand pieces of 256 is above a million values: the bound on the
	// product is a ceiling for hostile files, not a limit a real model meets.
	raw, err := os.ReadFile("testdata/charsmap.bin")
	if err != nil {
		t.Fatal(err)
	}
	const n = 5000
	v := Vocab{Pieces: make([]string, n), Scores: make([]float32, n)}
	rows := make([][]float32, n)
	for i := range rows {
		v.Pieces[i] = "p" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune(0x4e00+i))
		rows[i] = make([]float32, 256)
		rows[i][i%256] = 1
	}
	var buf bytes.Buffer
	if err := Write(&buf, v, raw, rows, 4); err != nil {
		t.Fatal(err)
	}
	m, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.rows) != n*256 || len(m.scale) != n {
		t.Errorf("read %d values and %d scales, want %d and %d", len(m.rows), len(m.scale), n*256, n)
	}
}

func TestRead_RefusesNoDimensionEvenWhenTheFileIsWhole(t *testing.T) {
	raw, err := os.ReadFile("testdata/charsmap.bin")
	if err != nil {
		t.Fatal(err)
	}
	v := Vocab{Pieces: []string{"[UNK]"}, Scores: []float32{0}}
	var buf bytes.Buffer
	if err := Write(&buf, v, raw, [][]float32{{}}, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(&buf); !errors.Is(err, ErrModel) {
		t.Errorf("a model of dimension 0: got %v, want ErrModel", err)
	}
}

// wideModelFile writes a model of pieces rows of dim, large enough that its
// table dwarfs the normalization table and the vocabulary, and returns its path
// and the size of the table (rows and scales).
func wideModelFile(t *testing.T, pieces, dim int) (string, uint64) {
	t.Helper()
	raw, err := os.ReadFile("testdata/charsmap.bin")
	if err != nil {
		t.Fatal(err)
	}
	v := Vocab{Unk: 0}
	rows := make([][]float32, pieces)
	for i := range rows {
		v.Pieces = append(v.Pieces, "▁p"+strconv.Itoa(i))
		v.Scores = append(v.Scores, -1)
		rows[i] = make([]float32, dim)
		rows[i][i%dim] = 1
	}
	var buf bytes.Buffer
	if err := Write(&buf, v, raw, rows, 4); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, uint64(pieces) * uint64(dim+4)
}

// allocatedBy is how many bytes f allocated, all told.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestLoad_AllocatesTheTableOnceAtItsSize(t *testing.T) {
	// The real model's 128 MB of rows once cost close to a gigabyte of
	// allocation to load: a slice doubling as rows were appended, and a fresh
	// buffer for every row and every scale read. Load knows the file's size and
	// must allocate the table once; a bound of one and a half times the table
	// leaves room for the normalization table and the vocabulary and none for a
	// second copy.
	path, table := wideModelFile(t, 3000, 4096)
	var m *Model
	var err error
	grew := allocatedBy(func() { m, err = Load(path) })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Logf("a table of %d bytes allocated %d to load", table, grew)
	if grew > table*3/2 {
		t.Errorf("loading a table of %d MB allocated %d MB, want under one and a half times it", table>>20, grew>>20)
	}
	if m.Pieces() != 3000 {
		t.Errorf("Load read %d pieces, want 3000", m.Pieces())
	}
	if got := m.Score("p7", "p7"); got < 0.999 {
		t.Errorf("a phrase scores %.4f against itself after Load, want 1", got)
	}
}

func TestLoad_RefusesAFileWhoseLengthDisagreesWithItsHeaderBeforeAllocating(t *testing.T) {
	file := tinyFile(t)
	dir := t.TempDir()
	write := func(name string, raw []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// A header within every bound claiming two gigabytes of rows, in a file of a
	// few hundred kilobytes: refused for its length, before a byte is allocated
	// for the claim.
	claim := write("claim.bin", patched(patched(file, offPieces, 2_000_000), offDim, 1000))
	var err error
	grew := allocatedBy(func() { _, err = Load(claim) })
	if !errors.Is(err, ErrModel) || !strings.Contains(err.Error(), "cannot hold") {
		t.Errorf("a header claiming 2 GB: got %v, want ErrModel for a file that cannot hold it", err)
	}
	// The one allocation allowed is the megabyte of the read buffer.
	if grew > 2<<20 {
		t.Errorf("refusing it allocated %d KB", grew>>10)
	}
	// One row's worth too many or too few bytes after the vocabulary: the rows
	// would not fill the rest exactly, and that is known before they are read.
	for name, raw := range map[string][]byte{
		"a row too long":  append(append([]byte(nil), file...), 0, 0, 0, 0, 0, 0, 0),
		"a row too short": file[:len(file)-7],
	} {
		if _, err := Load(write("rest.bin", raw)); !errors.Is(err, ErrModel) || !strings.Contains(err.Error(), "follow the vocabulary") {
			t.Errorf("%s: got %v, want ErrModel for rows that do not fill the file", name, err)
		}
	}
}

func TestModel_ScoreAllIsScoreWithTheKeyMadeOnce(t *testing.T) {
	m := tinyModel(t)
	key := "кофе машина"
	completions := []string{"кофе чайник", "погода", "", "кофе машина", "чайник погода"}
	got := m.ScoreAll(key, completions)
	if len(got) != len(completions) {
		t.Fatalf("ScoreAll gave %d scores for %d completions", len(got), len(completions))
	}
	for i, c := range completions {
		// To the bit: the export and the backfill compare these with what was
		// kept at collection, and a job scored both ways must read the same.
		if want := m.Score(key, c); got[i] != want {
			t.Errorf("ScoreAll(%q)[%d] = %v, Score gives %v", key, i, got[i], want)
		}
	}
	if len(m.ScoreAll(key, nil)) != 0 {
		t.Error("no completions scored to something")
	}
	// The key is made into a vector once: scoring ten completions costs the
	// key's vector once and each completion's once, and one slice for the
	// scores. Made per completion, it would cost ten of the key's.
	ten := make([]string, 10)
	for i := range ten {
		ten[i] = "кофе чайник"
	}
	keyCost := testing.AllocsPerRun(20, func() { _ = m.Vector(key) })
	oneCost := testing.AllocsPerRun(20, func() { _ = m.Vector("кофе чайник") })
	allCost := testing.AllocsPerRun(20, func() { _ = m.ScoreAll(key, ten) })
	if limit := keyCost + 10*oneCost + 1; allCost > limit {
		t.Errorf("ScoreAll of ten made %v allocations, want at most %v: the key's vector once", allCost, limit)
	}
}
