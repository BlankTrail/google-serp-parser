// SPDX-License-Identifier: MIT

package semantic

import (
	"strings"
	"unicode"
)

// metaspace is the mark the tokenizer writes in place of a space, and in front
// of the whole text.
const metaspace = "▁"

// spaced is the punctuation the reference surrounds with spaces, so a comma or
// a slash is a piece of its own and never glued to the word beside it.
const spaced = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

// Normalize writes a phrase the way the model's tokenizer reads it, in the
// reference's order: the table; runs of two or more spaces to one; the
// punctuation above with a space on each side; every run of white space to
// one space; the ends trimmed; then every space to ▁ and one ▁ in front.
// Case is left as it is: the model has a vector for Coffee and another for
// coffee.
func Normalize(c *Charsmap, s string) string {
	s = c.Normalize(s)
	s = collapse(s, func(r rune) bool { return r == ' ' })
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(spaced, r) {
			b.WriteByte(' ')
			b.WriteRune(r)
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	s = collapse(b.String(), unicode.IsSpace)
	s = strings.TrimSpace(s)
	if s == "" {
		// The reference's Metaspace has nothing to put a mark in front of when
		// the text is empty (or only white space): it writes no piece at all, not
		// a lone ▁. The reference files say so, and a phrase with no words must
		// come out with no pieces here too.
		return ""
	}
	return metaspace + strings.ReplaceAll(s, " ", metaspace)
}

// collapse writes every run of runes that match as a single space.
func collapse(s string, match func(rune) bool) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		if match(r) {
			if !in {
				b.WriteByte(' ')
			}
			in = true
			continue
		}
		in = false
		b.WriteRune(r)
	}
	return b.String()
}
