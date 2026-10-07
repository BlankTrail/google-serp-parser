// SPDX-License-Identifier: MIT

package semantic

import (
	"bufio"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestUnigram_TakesThePathOfTheHighestTotalScore(t *testing.T) {
	// ▁кофе as one piece scores -2; as ▁ко + фе it scores -3 + -3. The best
	// total wins, not the longest first piece.
	u := NewUnigram(Vocab{
		Pieces: []string{"[PAD]", "[UNK]", "▁", "▁ко", "фе", "▁кофе", "▁кофем", "ашина", "машина", "▁машина"},
		Scores: []float32{0, 0, -5, -3, -3, -2, -1, -9, -4, -3},
		Unk:    1,
	})
	got := u.Tokenize("▁кофе▁машина")
	if want := []int32{5, 9}; !slices.Equal(got, want) {
		t.Errorf("Tokenize = %v, want %v", got, want)
	}
}

func TestUnigram_ALaterPathWithABetterTotalReplacesAnEarlierOne(t *testing.T) {
	// ▁ab is found first, from the start of the text, at -9; ▁ then ab reaches
	// the same end later, at -2. The later path must replace the earlier one
	// when its total is better, not only when nothing reached the end yet.
	u := NewUnigram(Vocab{
		Pieces: []string{"[UNK]", "▁", "▁ab", "ab"},
		Scores: []float32{0, -1, -9, -1},
		Unk:    0,
	})
	if got, want := u.Tokenize("▁ab"), []int32{1, 3}; !slices.Equal(got, want) {
		t.Errorf("Tokenize = %v, want %v", got, want)
	}
}

func TestUnigram_LeavesOutWhatTheVocabularyHasNoPieceFor(t *testing.T) {
	// A character no piece covers is the unknown piece, and the unknown piece
	// is dropped: model2vec averages nothing for it.
	u := NewUnigram(Vocab{
		Pieces: []string{"[PAD]", "[UNK]", "▁", "а", "б"},
		Scores: []float32{0, 0, -1, -1, -1},
		Unk:    1,
	})
	if got, want := u.Tokenize("▁а☕б"), []int32{2, 3, 4}; !slices.Equal(got, want) {
		t.Errorf("Tokenize = %v, want %v", got, want)
	}
	if got := u.Tokenize(""); len(got) != 0 {
		t.Errorf("an empty text tokenized to %v", got)
	}
}

// fullModelSource is the snapshot of the reference model, when this machine has
// one; the tests that need the whole vocabulary are skipped without it.
func fullModelSource(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("GSERP_SEMANTIC_SRC")
	if dir == "" {
		t.Skip("GSERP_SEMANTIC_SRC is not set: no reference model on this machine")
	}
	return dir
}

func TestUnigram_CutsEveryPhraseAsTheReferenceTokenizerDoes(t *testing.T) {
	src := fullModelSource(t)
	v, raw, err := readTokenizerJSON(src + "/tokenizer.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseCharsmap(raw)
	if err != nil {
		t.Fatal(err)
	}
	u := NewUnigram(v)
	f, err := os.Open("testdata/tokens.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	wrong := 0
	for sc.Scan() {
		phrase, ids, _ := strings.Cut(sc.Text(), "\t")
		var want []int32
		for _, s := range strings.Fields(ids) {
			n, _ := strconv.Atoi(s)
			want = append(want, int32(n))
		}
		if got := u.Tokenize(Normalize(c, phrase)); !slices.Equal(got, want) {
			wrong++
			if wrong <= 10 {
				t.Errorf("%q: %v, want %v", phrase, got, want)
			}
		}
	}
	if wrong > 0 {
		t.Errorf("%d phrases cut differently from the reference", wrong)
	}
}
