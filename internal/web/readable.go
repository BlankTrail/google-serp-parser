package web

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// readableURL is an address as a person reads it: the percent-escaped letters
// of other alphabets shown as letters, the way a browser's address bar does.
// Only the text on the page changes; the link and everything stored or
// exported keep the address exactly as it came.
//
// An escape is opened only when it spells a whole UTF-8 character outside ASCII
// that prints and is not a space. ASCII escapes stay escaped, since %2F, %3F,
// %23 or %25 opened would change what the address says; so do invisible and
// direction-changing characters, which would let the shown text differ from
// the link.
func readableURL(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if raw, n := escapedRun(s[i:]); n > 0 {
			b.WriteString(openRun(raw, s[i:i+n]))
			i += n
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// escapedRun reads consecutive %XX escapes at the start of s: their bytes and
// how much of s they take.
func escapedRun(s string) ([]byte, int) {
	var raw []byte
	n := 0
	for n+2 < len(s) && s[n] == '%' && isHex(s[n+1]) && isHex(s[n+2]) {
		raw = append(raw, unhex(s[n+1])<<4|unhex(s[n+2]))
		n += 3
	}
	return raw, n
}

// openRun turns the decoded bytes back into text, character by character,
// keeping the escapes of anything that should not be shown as itself.
func openRun(raw []byte, escaped string) string {
	var b strings.Builder
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if r >= utf8.RuneSelf && r != utf8.RuneError && printsAsItself(r) {
			b.WriteRune(r)
		} else {
			if r == utf8.RuneError {
				size = 1
			}
			b.WriteString(escaped[i*3 : (i+size)*3])
		}
		i += size
	}
	return b.String()
}

// printsAsItself: letters, marks, digits, punctuation and symbols. Spaces
// other than the ASCII one (U+00A0 and the like), zero widths and direction
// overrides all fall outside it.
func printsAsItself(r rune) bool { return unicode.IsPrint(r) }

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}
