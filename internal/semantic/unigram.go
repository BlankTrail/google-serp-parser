// SPDX-License-Identifier: MIT

package semantic

import (
	"math"
	"unicode/utf8"
)

// Vocab is the tokenizer's pieces, their log-probability scores, and which of
// them is the unknown piece.
type Vocab struct {
	Pieces []string
	Scores []float32
	Unk    int32
}

// unkPenalty is how far below the worst piece the unknown piece scores, as the
// reference sets it, so a character is called unknown only where no piece
// covers it.
const unkPenalty = 10

// Unigram cuts normalized text into the pieces of the highest total score, the
// way sentencepiece's Unigram model does.
type Unigram struct {
	ids      map[string]int32
	scores   []float32
	unk      int32
	unkScore float32
	longest  int // the longest piece, in bytes
}

// NewUnigram indexes a vocabulary.
func NewUnigram(v Vocab) *Unigram {
	u := &Unigram{ids: make(map[string]int32, len(v.Pieces)), scores: v.Scores, unk: v.Unk}
	lowest := float32(math.MaxFloat32)
	for i, p := range v.Pieces {
		if int32(i) == v.Unk {
			continue
		}
		u.ids[p] = int32(i)
		lowest = min(lowest, v.Scores[i])
		u.longest = max(u.longest, len(p))
	}
	u.unkScore = lowest - unkPenalty
	return u
}

// Tokenize is the best path through the text (Viterbi): for every position the
// best total score of the pieces that reach it, and the piece that got there.
// Unknown pieces are left out of what comes back, as model2vec leaves them out
// of the average.
func (u *Unigram) Tokenize(text string) []int32 {
	n := len(text)
	if n == 0 {
		return nil
	}
	best := make([]float32, n+1)
	from := make([]int, n+1)
	piece := make([]int32, n+1)
	reached := make([]bool, n+1)
	reached[0] = true
	for at := 0; at < n; at++ {
		if !reached[at] {
			continue
		}
		_, first := utf8.DecodeRuneInString(text[at:])
		single := false
		for end := at + first; end <= n && end-at <= u.longest; {
			if id, ok := u.ids[text[at:end]]; ok {
				if end-at == first {
					single = true
				}
				if s := best[at] + u.scores[id]; !reached[end] || s > best[end] {
					best[end], from[end], piece[end], reached[end] = s, at, id, true
				}
			}
			if end == n {
				break
			}
			_, size := utf8.DecodeRuneInString(text[end:])
			end += size
		}
		if !single {
			end := at + first
			if s := best[at] + u.unkScore; !reached[end] || s > best[end] {
				best[end], from[end], piece[end], reached[end] = s, at, u.unk, true
			}
		}
	}
	var out []int32
	for end := n; end > 0; end = from[end] {
		if piece[end] != u.unk {
			out = append(out, piece[end])
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
