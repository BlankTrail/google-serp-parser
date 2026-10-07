// SPDX-License-Identifier: MIT

package semantic

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"unicode/utf8"
)

// readTokenizerJSON reads the reference tokenizer.json: the Unigram pieces and
// their scores, the unknown piece, and the precompiled normalization table.
func readTokenizerJSON(path string) (Vocab, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Vocab{}, nil, err
	}
	var doc struct {
		Normalizer json.RawMessage `json:"normalizer"`
		Model      struct {
			Type  string   `json:"type"`
			UnkID int32    `json:"unk_id"`
			Vocab [][2]any `json:"vocab"`
		} `json:"model"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Vocab{}, nil, fmt.Errorf("semantic: reading %s: %w", path, err)
	}
	if doc.Model.Type != "Unigram" {
		return Vocab{}, nil, fmt.Errorf("semantic: %s is a %q tokenizer, want Unigram", path, doc.Model.Type)
	}
	v := Vocab{Unk: doc.Model.UnkID}
	for _, p := range doc.Model.Vocab {
		piece, ok1 := p[0].(string)
		score, ok2 := p[1].(float64)
		if !ok1 || !ok2 {
			return Vocab{}, nil, errors.New("semantic: a vocabulary entry is not a piece and a score")
		}
		v.Pieces = append(v.Pieces, piece)
		v.Scores = append(v.Scores, float32(score))
	}
	if doc.Model.UnkID < 0 || int(doc.Model.UnkID) >= len(v.Pieces) {
		// An unknown id outside the vocabulary would make the tokenizer drop the
		// wrong piece (or none) and index past the scores.
		return Vocab{}, nil, fmt.Errorf("semantic: %s: unk_id %d is outside the %d pieces", path, doc.Model.UnkID, len(v.Pieces))
	}
	b64, err := findCharsmap(doc.Normalizer)
	if err != nil {
		return Vocab{}, nil, err
	}
	charsmap, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return Vocab{}, nil, fmt.Errorf("semantic: %s: the normalization table is not base64: %w", path, err)
	}
	return v, charsmap, nil
}

// errNoCharsmap is "this branch of the normalizer has no table", as against a
// branch that is broken: only the first is a reason to look further.
var errNoCharsmap = errors.New("semantic: the tokenizer has no precompiled normalization table")

// findCharsmap finds the Precompiled normalizer anywhere in the sequence.
func findCharsmap(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", errNoCharsmap
	}
	var n struct {
		Type        string            `json:"type"`
		Charsmap    string            `json:"precompiled_charsmap"`
		Normalizers []json.RawMessage `json:"normalizers"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return "", fmt.Errorf("semantic: reading the normalizer: %w", err)
	}
	if n.Type == "Precompiled" {
		if n.Charsmap == "" {
			return "", errors.New("semantic: the precompiled normalization table is empty")
		}
		return n.Charsmap, nil
	}
	for _, c := range n.Normalizers {
		got, err := findCharsmap(c)
		if errors.Is(err, errNoCharsmap) {
			continue
		}
		return got, err
	}
	return "", errNoCharsmap
}

// ReadSource reads a model2vec snapshot, tokenizer.json and model.safetensors,
// and returns what Write needs: the vocabulary, the normalization table, one
// float32 row per piece, and the median piece length in characters that
// model2vec cuts a long text by.
//
// The table is half a gigabyte for the real model and the machines this runs on
// can be short of memory, so the file is neither read whole nor copied: its
// data is streamed in small chunks straight into one backing array, and the
// rows are slices of that array rather than hundreds of thousands of separate
// allocations. What the header claims is checked against the file's real size
// before anything is allocated, so a damaged header cannot ask for more memory
// than the file holds.
func ReadSource(dir string) (Vocab, []byte, [][]float32, int, error) {
	v, charsmap, err := readTokenizerJSON(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		return Vocab{}, nil, nil, 0, err
	}
	rows, err := readEmbeddings(filepath.Join(dir, "model.safetensors"), len(v.Pieces))
	if err != nil {
		return Vocab{}, nil, nil, 0, err
	}
	// np.median of the piece lengths: the middle one, or the mean of the two
	// middle ones when there is an even number, cut to a whole number.
	lens := make([]int, len(v.Pieces))
	for i, p := range v.Pieces {
		lens[i] = utf8.RuneCountInString(p)
	}
	slices.Sort(lens)
	median := lens[len(lens)/2]
	if len(lens)%2 == 0 {
		median = (lens[len(lens)/2-1] + lens[len(lens)/2]) / 2
	}
	return v, charsmap, rows, median, nil
}

// readEmbeddings reads the float32 "embeddings" tensor of a safetensors file as
// one row per piece, and refuses a table of any other shape or type.
func readEmbeddings(path string, pieces int) ([][]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only: a failed close loses nothing
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := uint64(st.Size())
	var lenBuf [8]byte
	if _, err := io.ReadFull(f, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("%w: %s has no header: %v", ErrModel, path, err)
	}
	headLen := binary.LittleEndian.Uint64(lenBuf[:])
	if headLen > size-8 {
		return nil, fmt.Errorf("%w: %s: the header is longer than the file", ErrModel, path)
	}
	headRaw := make([]byte, headLen)
	if _, err := io.ReadFull(f, headRaw); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrModel, path, err)
	}
	var head map[string]json.RawMessage
	if err := json.Unmarshal(headRaw, &head); err != nil {
		return nil, fmt.Errorf("%w: %s: the header is not JSON: %v", ErrModel, path, err)
	}
	var t struct {
		DType   string   `json:"dtype"`
		Shape   []int    `json:"shape"`
		Offsets [2]int64 `json:"data_offsets"`
	}
	if err := json.Unmarshal(head["embeddings"], &t); err != nil || t.DType != "F32" || len(t.Shape) != 2 {
		return nil, fmt.Errorf("%w: %s has no float32 embeddings table", ErrModel, path)
	}
	if t.Shape[0] != pieces {
		return nil, fmt.Errorf("%w: %d rows for %d pieces", ErrModel, t.Shape[0], pieces)
	}
	n, dim := t.Shape[0], t.Shape[1]
	if dim <= 0 || n <= 0 {
		return nil, fmt.Errorf("%w: a table of %d by %d", ErrModel, n, dim)
	}
	// The data must be exactly the table, and lie inside the file; both are
	// checked before the backing array is sized from the shape. Offsets that run
	// backwards give a negative span, which as an unsigned number is never the
	// size of a table, so the size check below refuses them too.
	body := size - 8 - headLen
	if t.Offsets[0] < 0 || uint64(t.Offsets[1]) > body {
		return nil, fmt.Errorf("%w: the embeddings lie outside the file", ErrModel)
	}
	if uint64(t.Offsets[1]-t.Offsets[0]) != 4*uint64(n)*uint64(dim) || uint64(n)*uint64(dim) > 1<<31 {
		return nil, fmt.Errorf("%w: the embeddings are not %d by %d float32 values", ErrModel, n, dim)
	}
	if _, err := f.Seek(8+int64(headLen)+t.Offsets[0], io.SeekStart); err != nil {
		return nil, err
	}
	all := make([]float32, n*dim)
	chunk := make([]byte, 4<<20)
	for done := 0; done < len(all); {
		want := min(len(chunk), 4*(len(all)-done))
		if _, err := io.ReadFull(f, chunk[:want]); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrModel, path, err)
		}
		for i := 0; i < want; i += 4 {
			all[done] = math.Float32frombits(binary.LittleEndian.Uint32(chunk[i:]))
			done++
		}
	}
	rows := make([][]float32, n)
	for i := range rows {
		rows[i] = all[i*dim : (i+1)*dim : (i+1)*dim]
	}
	return rows, nil
}
