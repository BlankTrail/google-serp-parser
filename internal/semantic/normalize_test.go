// SPDX-License-Identifier: MIT

package semantic

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func testCharsmap(t *testing.T) *Charsmap {
	t.Helper()
	raw, err := os.ReadFile("testdata/charsmap.bin")
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseCharsmap(raw)
	if err != nil {
		t.Fatalf("ParseCharsmap: %v", err)
	}
	return c
}

func TestNormalize_WritesEveryPhraseAsTheReferenceTokenizerDoes(t *testing.T) {
	// The references were written by the tokenizer the model was trained with;
	// a phrase normalized any other way is cut into other pieces and lands on
	// other vectors.
	c := testCharsmap(t)
	f, err := os.Open("testdata/normalized.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n, wrong := 0, 0
	for sc.Scan() {
		phrase, want, _ := strings.Cut(sc.Text(), "\t")
		n++
		if got := Normalize(c, phrase); got != want {
			wrong++
			if wrong <= 10 {
				t.Errorf("Normalize(%q) = %q, want %q", phrase, got, want)
			}
		}
	}
	if n != 2000 {
		t.Fatalf("%d references read, want 2000", n)
	}
	if wrong > 0 {
		t.Errorf("%d of %d phrases normalized differently from the reference", wrong, n)
	}
}

func TestParseCharsmap_RefusesWhatIsNotOne(t *testing.T) {
	for _, raw := range [][]byte{nil, {1, 0}, {255, 255, 255, 255, 0, 0, 0, 0},
		{0, 0, 0, 0, 9, 9}, {5, 0, 0, 0, 1, 2, 3, 4, 5}, {8, 0, 0, 0, 1, 2, 3, 4}} {
		if _, err := ParseCharsmap(raw); err == nil {
			t.Errorf("ParseCharsmap(%v) accepted it", raw)
		}
	}
}
