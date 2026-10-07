// SPDX-License-Identifier: MIT

// Package semantic tells how close in meaning a search suggestion is to the
// key it was collected for, with a static embedding model: every piece of
// text the model knows has a vector, a phrase is the average of its pieces'
// vectors, and closeness is the cosine between two phrases. It is pure Go and
// needs nothing installed; the model itself is a file of its own, downloaded
// once.
package semantic

import (
	"encoding/binary"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrCharsmap is a normalization table that cannot be read.
var ErrCharsmap = errors.New("semantic: the normalization table cannot be read")

// Charsmap is sentencepiece's precompiled normalization table: a double-array
// trie over the bytes of what is replaced, and the replacements one after
// another, each ended by a NUL. It is the table the model's tokenizer was
// built with ("nmt_nfkc"), carried over as it is so a phrase is normalized
// exactly as the model saw its training text, not merely close to it.
type Charsmap struct {
	trie       []uint32
	normalized []byte
}

// ParseCharsmap reads the table: four bytes of trie size, the trie, and the
// replacements.
func ParseCharsmap(raw []byte) (*Charsmap, error) {
	if len(raw) < 4 {
		return nil, ErrCharsmap
	}
	size := int(binary.LittleEndian.Uint32(raw))
	if size%4 != 0 || size == 0 || 4+size > len(raw) {
		return nil, ErrCharsmap
	}
	trie := make([]uint32, size/4)
	for i := range trie {
		trie[i] = binary.LittleEndian.Uint32(raw[4+4*i:])
	}
	return &Charsmap{trie: trie, normalized: raw[4+size:]}, nil
}

// The darts-clone unit layout sentencepiece writes its trie in.
func hasLeaf(u uint32) bool  { return (u>>8)&1 == 1 }
func value(u uint32) uint32  { return u & (1<<31 - 1) }
func label(u uint32) uint32  { return u & (1<<31 | 0xFF) }
func offset(u uint32) uint32 { return (u >> 10) << ((u & (1 << 9)) >> 6) }

// replacement is what the table puts in place of chunk, if anything: the
// first match of the common-prefix search, as the reference implementation
// takes it.
func (c *Charsmap) replacement(chunk string) (string, bool) {
	pos := offset(c.trie[0])
	for i := 0; i < len(chunk); i++ {
		b := uint32(chunk[i])
		if b == 0 {
			break
		}
		pos ^= b
		if int(pos) >= len(c.trie) {
			return "", false
		}
		u := c.trie[pos]
		if label(u) != b {
			return "", false
		}
		pos ^= offset(u)
		if hasLeaf(u) {
			if int(pos) >= len(c.trie) {
				return "", false
			}
			at := int(value(c.trie[pos]))
			if at >= len(c.normalized) {
				return "", false
			}
			end := at
			for end < len(c.normalized) && c.normalized[end] != 0 {
				end++
			}
			return string(c.normalized[at:end]), true
		}
	}
	return "", false
}

// Normalize applies the table. The reference walks grapheme clusters: a
// cluster shorter than six bytes is looked up whole, and otherwise — or when
// the whole is not in the table — rune by rune. A cluster here is a rune and
// the combining marks and joiners after it, which is what a grapheme is for
// everything a search box is typed with; the parity test holds it to the
// reference.
func (c *Charsmap) Normalize(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		n := clusterLen(s)
		cluster := s[:n]
		s = s[n:]
		if len(cluster) < 6 {
			if r, ok := c.replacement(cluster); ok {
				b.WriteString(r)
				continue
			}
		}
		for len(cluster) > 0 {
			_, size := utf8.DecodeRuneInString(cluster)
			part := cluster[:size]
			cluster = cluster[size:]
			if r, ok := c.replacement(part); ok {
				b.WriteString(r)
			} else {
				b.WriteString(part)
			}
		}
	}
	return b.String()
}

// clusterLen is how many bytes the first cluster of s takes: one rune and the
// marks, variation selectors and zero-width joiners (with what they join) that
// follow it.
func clusterLen(s string) int {
	_, n := utf8.DecodeRuneInString(s)
	for n < len(s) {
		r, size := utf8.DecodeRuneInString(s[n:])
		switch {
		case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), unicode.Is(unicode.Mc, r),
			r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF:
			n += size
		case r == 0x200D:
			n += size
			if n < len(s) {
				_, next := utf8.DecodeRuneInString(s[n:])
				n += next
			}
		default:
			return n
		}
	}
	return n
}
