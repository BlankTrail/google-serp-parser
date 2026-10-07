// SPDX-License-Identifier: MIT

package semantic

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"unicode/utf8"
)

// FileName is the model's file, kept beside the history.
const FileName = "gserp-semantic-v1.bin"

// ErrModel is a file that is not a model this program can read.
var ErrModel = errors.New("semantic: the model file cannot be read")

const signature = "GSSEM1\x00\x00"

// maxTokens is where model2vec cuts a phrase, and the text is cut first to
// maxTokens pieces' worth of characters at the median piece length.
const maxTokens = 512

// Model is a static embedding model: a vector for every piece of the
// vocabulary, as int8 with a scale per row.
type Model struct {
	charsmap  *Charsmap
	unigram   *Unigram
	dim       int
	medianLen int
	scale     []float32
	rows      []int8
}

// Write writes a model. Each row is quantized to int8 against its own largest
// component, which keeps a 500-thousand-piece table at a quarter of its size
// for an agreement with the full vectors of 0.99 or better.
func Write(w io.Writer, v Vocab, charsmap []byte, rows [][]float32, medianLen int) error {
	if len(rows) != len(v.Pieces) || len(v.Scores) != len(v.Pieces) || len(rows) == 0 {
		return fmt.Errorf("%w: %d pieces, %d scores, %d rows", ErrModel, len(v.Pieces), len(v.Scores), len(rows))
	}
	for _, p := range v.Pieces {
		// The length is written as a uint16: a longer piece would be written
		// short and the file would then read as garbage from there on.
		if len(p) > math.MaxUint16 {
			return fmt.Errorf("%w: a piece of %d bytes", ErrModel, len(p))
		}
	}
	dim := len(rows[0])
	bw := bufio.NewWriter(w)
	put := func(x any) { _ = binary.Write(bw, binary.LittleEndian, x) }
	_, _ = bw.WriteString(signature)
	put(uint32(dim))
	put(uint32(len(v.Pieces)))
	put(uint32(v.Unk))
	put(uint32(medianLen))
	put(uint32(len(charsmap)))
	_, _ = bw.Write(charsmap)
	for i, p := range v.Pieces {
		put(uint16(len(p)))
		_, _ = bw.WriteString(p)
		put(v.Scores[i])
	}
	q := make([]int8, dim)
	for _, row := range rows {
		if len(row) != dim {
			return fmt.Errorf("%w: a row of %d, want %d", ErrModel, len(row), dim)
		}
		var top float32
		for _, x := range row {
			top = max(top, float32(math.Abs(float64(x))))
		}
		scale := top / 127
		for j, x := range row {
			if scale > 0 {
				q[j] = int8(math.Round(float64(x / scale)))
			} else {
				q[j] = 0
			}
		}
		put(scale)
		put(q)
	}
	return bw.Flush()
}

// Load reads a model from a file.
func Load(path string) (*Model, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return Read(f)
}

// Read reads a model.
func Read(r io.Reader) (*Model, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	head := make([]byte, len(signature))
	if _, err := io.ReadFull(br, head); err != nil || string(head) != signature {
		return nil, ErrModel
	}
	var dim, pieces, unk, median, cmLen uint32
	for _, x := range []*uint32{&dim, &pieces, &unk, &median, &cmLen} {
		if err := binary.Read(br, binary.LittleEndian, x); err != nil {
			return nil, ErrModel
		}
	}
	if dim == 0 || dim > 4096 || pieces == 0 || pieces > 10_000_000 || unk >= pieces || cmLen > 64<<20 {
		return nil, ErrModel
	}
	cm := make([]byte, cmLen)
	if _, err := io.ReadFull(br, cm); err != nil {
		return nil, ErrModel
	}
	charsmap, err := ParseCharsmap(cm)
	if err != nil {
		return nil, ErrModel
	}
	v := Vocab{Pieces: make([]string, pieces), Scores: make([]float32, pieces), Unk: int32(unk)}
	for i := range v.Pieces {
		var n uint16
		if err := binary.Read(br, binary.LittleEndian, &n); err != nil {
			return nil, ErrModel
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(br, b); err != nil || !utf8.Valid(b) {
			return nil, ErrModel
		}
		v.Pieces[i] = string(b)
		if err := binary.Read(br, binary.LittleEndian, &v.Scores[i]); err != nil {
			return nil, ErrModel
		}
	}
	m := &Model{charsmap: charsmap, unigram: NewUnigram(v), dim: int(dim), medianLen: int(median),
		scale: make([]float32, pieces), rows: make([]int8, int(pieces)*int(dim))}
	for i := 0; i < int(pieces); i++ {
		if err := binary.Read(br, binary.LittleEndian, &m.scale[i]); err != nil {
			return nil, ErrModel
		}
		if err := binary.Read(br, binary.LittleEndian, m.rows[i*int(dim):(i+1)*int(dim)]); err != nil {
			return nil, ErrModel
		}
	}
	return m, nil
}

// Vector is a phrase's vector: the mean of its pieces' vectors, normalized, as
// model2vec makes it. A phrase with no known piece is the nought vector.
func (m *Model) Vector(text string) []float32 {
	if m.medianLen > 0 {
		if limit := maxTokens * m.medianLen; utf8.RuneCountInString(text) > limit {
			text = string([]rune(text)[:limit])
		}
	}
	ids := m.unigram.Tokenize(Normalize(m.charsmap, text))
	if len(ids) > maxTokens {
		ids = ids[:maxTokens]
	}
	out := make([]float32, m.dim)
	if len(ids) == 0 {
		return out
	}
	for _, id := range ids {
		row, s := m.rows[int(id)*m.dim:(int(id)+1)*m.dim], m.scale[id]
		for j, q := range row {
			out[j] += float32(q) * s
		}
	}
	// model2vec divides the sum by the number of pieces and then normalizes; the
	// division changes nothing the normalization does not undo, so it is left
	// out and the sum is normalized as it is.
	var norm float64
	for j := range out {
		norm += float64(out[j]) * float64(out[j])
	}
	if norm == 0 {
		return out
	}
	inv := float32(1 / math.Sqrt(norm))
	for j := range out {
		out[j] *= inv
	}
	return out
}

// Score is how close a completion is to its key: the cosine of their vectors,
// from -1 to 1 and in practice from nought to one. A phrase with no known piece
// scores nought against anything.
func (m *Model) Score(key, completion string) float32 {
	a, b := m.Vector(key), m.Vector(completion)
	var dot float32
	for j := range a {
		dot += a[j] * b[j]
	}
	return dot
}
