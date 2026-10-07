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
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				return fmt.Errorf("%w: a row holds %v", ErrModel, x)
			}
			top = max(top, float32(math.Abs(float64(x))))
		}
		scale := top / 127
		for j, x := range row {
			// A zero row would divide 0 by 0 and convert NaN to int8, which Go
			// leaves to the implementation. It is 0 on amd64 and arm64 today,
			// so no test here can tell the guard from its absence; it stays so
			// that the zero row does not depend on that.
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

// maxPieces, maxDim and maxCells bound what a header may claim. The real model
// is about 500 thousand pieces of 256, some 128 million cells; the bound on the
// product is what keeps a hostile header from asking for tens of gigabytes,
// which Go does not report as an error but dies of.
const (
	maxPieces = 10_000_000
	maxDim    = 4096
	maxCells  = 1 << 31
)

// bad is an unreadable file, with what went wrong behind it.
func bad(err error) error { return fmt.Errorf("%w: %v", ErrModel, err) }

// Read reads a model. Nothing is allocated from a size in the header before the
// bytes it promises have arrived: the tables grow as they are read, so a short
// file with a large header costs what it is, not what it claims.
func Read(r io.Reader) (*Model, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	head := make([]byte, len(signature))
	if _, err := io.ReadFull(br, head); err != nil {
		return nil, bad(err)
	}
	if string(head) != signature {
		return nil, bad(errors.New("the signature is not this format's"))
	}
	var dim, pieces, unk, median, cmLen uint32
	for _, x := range []*uint32{&dim, &pieces, &unk, &median, &cmLen} {
		if err := binary.Read(br, binary.LittleEndian, x); err != nil {
			return nil, bad(err)
		}
	}
	if dim == 0 || dim > maxDim || pieces == 0 || pieces > maxPieces || unk >= pieces || cmLen > 64<<20 {
		return nil, bad(fmt.Errorf("a header of %d pieces of %d, unknown %d, table of %d bytes", pieces, dim, unk, cmLen))
	}
	if uint64(pieces)*uint64(dim) > maxCells {
		return nil, bad(fmt.Errorf("%d pieces of %d is larger than the %d values a model may hold", pieces, dim, uint64(maxCells)))
	}
	cm, err := io.ReadAll(io.LimitReader(br, int64(cmLen)))
	if err != nil {
		return nil, bad(err)
	}
	charsmap, err := ParseCharsmap(cm)
	if err != nil {
		return nil, bad(err)
	}
	v := Vocab{Unk: int32(unk)}
	for i := 0; i < int(pieces); i++ {
		var n uint16
		if err := binary.Read(br, binary.LittleEndian, &n); err != nil {
			return nil, bad(err)
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(br, b); err != nil {
			return nil, bad(err)
		}
		if !utf8.Valid(b) {
			return nil, bad(errors.New("a piece is not text"))
		}
		var score float32
		if err := binary.Read(br, binary.LittleEndian, &score); err != nil {
			return nil, bad(err)
		}
		v.Pieces = append(v.Pieces, string(b))
		v.Scores = append(v.Scores, score)
	}
	m := &Model{charsmap: charsmap, unigram: NewUnigram(v), dim: int(dim), medianLen: int(median)}
	row := make([]int8, dim)
	for i := 0; i < int(pieces); i++ {
		var scale float32
		if err := binary.Read(br, binary.LittleEndian, &scale); err != nil {
			return nil, bad(err)
		}
		if !(scale >= 0) || math.IsInf(float64(scale), 0) {
			// A negative scale flips a row and NaN or infinity poisons every
			// phrase that reaches it; Write never writes either.
			return nil, bad(fmt.Errorf("the scale of row %d is %v", i, scale))
		}
		if err := binary.Read(br, binary.LittleEndian, row); err != nil {
			return nil, bad(err)
		}
		m.scale = append(m.scale, scale)
		m.rows = append(m.rows, row...)
	}
	if _, err := br.ReadByte(); err != io.EOF {
		return nil, bad(errors.New("bytes follow the last row"))
	}
	return m, nil
}

// Vector is a phrase's vector: the direction of the sum of its pieces' vectors,
// which is the normalized mean model2vec makes (the mean is the sum over a
// positive number, and normalizing undoes it). The unknown piece counts like any
// other: model2vec keeps it for this tokenizer and its row is not nought in the
// real table. Only a phrase that normalizes to nothing has no pieces and is the
// nought vector.
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
// from -1 to 1 and in practice from nought to one. A phrase that normalizes to
// nothing has no pieces and scores nought against anything.
func (m *Model) Score(key, completion string) float32 {
	a, b := m.Vector(key), m.Vector(completion)
	var dot float32
	for j := range a {
		dot += a[j] * b[j]
	}
	return dot
}
