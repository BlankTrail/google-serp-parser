// SPDX-License-Identifier: MIT

package semantic

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
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

func TestUnigram_ReturnsTheUnknownPieceForWhatTheVocabularyHasNoPieceFor(t *testing.T) {
	// A character no piece covers is the unknown piece, and it stays in what
	// comes back: model2vec does not remove it for a Unigram tokenizer. A run of
	// such characters is fused into one, as the HF tokenizers' Unigram does
	// (checked against the reference: three U+180E in a row give one [UNK]).
	u := NewUnigram(Vocab{
		Pieces: []string{"[PAD]", "[UNK]", "▁", "а", "б"},
		Scores: []float32{0, 0, -1, -1, -1},
		Unk:    1,
	})
	if got, want := u.Tokenize("▁а☕б"), []int32{2, 3, 1, 4}; !slices.Equal(got, want) {
		t.Errorf("Tokenize = %v, want %v", got, want)
	}
	if got, want := u.Tokenize("▁а☕☕☕б"), []int32{2, 3, 1, 4}; !slices.Equal(got, want) {
		t.Errorf("a run of unknown characters: %v, want one unknown piece %v", got, want)
	}
	if got, want := u.Tokenize("▁а☕б☕"), []int32{2, 3, 1, 4, 1}; !slices.Equal(got, want) {
		t.Errorf("unknown characters apart stay apart: %v, want %v", got, want)
	}
	if got, want := u.Tokenize("☕☕"), []int32{1}; !slices.Equal(got, want) {
		t.Errorf("only unknown characters: %v, want %v", got, want)
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

func TestUnigram_AnEqualTotalKeepsTheFirstPathFound(t *testing.T) {
	// ▁a + b and ▁ + ab both total -5 at the end of ▁ab. The reference's
	// lattice walks the pieces ending at a position in the order they were
	// inserted, which is by start position, and replaces a candidate only on a
	// strictly better score (HF tokenizers' Viterbi uses >), so the path through
	// the earlier-starting piece wins: ▁ then ab (ab starts at 1, b at 2).
	u := NewUnigram(Vocab{
		Pieces: []string{"[UNK]", "▁", "a", "b", "ab", "▁a"},
		Scores: []float32{0, -2, -9, -2, -3, -3},
		Unk:    0,
	})
	if got, want := u.Tokenize("▁ab"), []int32{1, 4}; !slices.Equal(got, want) {
		t.Errorf("Tokenize = %v, want %v", got, want)
	}
}

func TestUnigram_AnUnknownCharacterTyingAPieceDoesNotReplaceIt(t *testing.T) {
	// XY is one piece at 0; X (10) then an unknown Y (lowest 0, less the
	// penalty 10) also totals 0. The piece was found first and a tie keeps it,
	// so the text is [XY], not [X] with Y unknown.
	u := NewUnigram(Vocab{
		Pieces: []string{"[UNK]", "X", "XY"},
		Scores: []float32{0, 10, 0},
		Unk:    0,
	})
	if got, want := u.Tokenize("XY"), []int32{2}; !slices.Equal(got, want) {
		t.Errorf("Tokenize = %v, want %v", got, want)
	}
}

func writeTokenizer(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokenizer.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadTokenizerJSON(t *testing.T) {
	const norm = `{"type":"Precompiled","precompiled_charsmap":"AQID"}`
	const vocab = `[["[UNK]",0],["a",-1.5]]`
	doc := func(typ, vocab, unk, normalizer string) string {
		return `{"normalizer":` + normalizer + `,"model":{"type":"` + typ + `","unk_id":` + unk + `,"vocab":` + vocab + `}}`
	}
	good := doc("Unigram", vocab, "0", `{"type":"Sequence","normalizers":[{"type":"Strip"},`+norm+`]}`)
	v, raw, err := readTokenizerJSON(writeTokenizer(t, good))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(v.Pieces, []string{"[UNK]", "a"}) || !slices.Equal(v.Scores, []float32{0, -1.5}) || v.Unk != 0 {
		t.Errorf("vocabulary = %+v", v)
	}
	if !slices.Equal(raw, []byte{1, 2, 3}) {
		t.Errorf("charsmap = %v", raw)
	}

	bad := map[string]string{
		"not json":           `{`,
		"not Unigram":        doc("BPE", vocab, "0", norm),
		"malformed entry":    doc("Unigram", `[["[UNK]",0],["a","x"]]`, "0", norm),
		"no normalizer":      `{"model":{"type":"Unigram","unk_id":0,"vocab":` + vocab + `}}`,
		"no charsmap":        doc("Unigram", vocab, "0", `{"type":"Strip"}`),
		"empty charsmap":     doc("Unigram", vocab, "0", `{"type":"Precompiled","precompiled_charsmap":""}`),
		"broken nested":      doc("Unigram", vocab, "0", `{"type":"Sequence","normalizers":[7,`+norm+`]}`),
		"charsmap not b64":   doc("Unigram", vocab, "0", `{"type":"Precompiled","precompiled_charsmap":"!!"}`),
		"unk_id too big":     doc("Unigram", vocab, "2", norm),
		"unk_id is negative": doc("Unigram", vocab, "-1", norm),
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			v, raw, err := readTokenizerJSON(writeTokenizer(t, body))
			if err == nil {
				t.Fatal("no error")
			}
			if v.Pieces != nil || raw != nil {
				t.Errorf("an error came with a vocabulary %v or a table %v", v.Pieces, raw)
			}
		})
	}
	if _, _, err := readTokenizerJSON(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a missing file read without error")
	}
	if _, _, err := readTokenizerJSON(writeTokenizer(t, doc("Unigram", vocab, "0", `{"type":"Strip"}`))); err == nil || !strings.Contains(err.Error(), "no precompiled normalization table") {
		t.Errorf("no table gave %v", err)
	}
	if _, _, err := readTokenizerJSON(writeTokenizer(t, `{"model":{"type":"Unigram","unk_id":0,"vocab":`+vocab+`}}`)); err == nil || !strings.Contains(err.Error(), "no precompiled normalization table") {
		t.Errorf("no normalizer gave %v", err)
	}
}

func TestUnigram_CutsEveryPhraseAsTheReferenceTokenizerDoes(t *testing.T) {
	src := fullModelSource(t)
	v, raw, err := readTokenizerJSON(filepath.Join(src, "tokenizer.json"))
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
	wrong, seen := 0, 0
	for sc.Scan() {
		seen++
		phrase, ids, _ := strings.Cut(sc.Text(), "\t")
		var want []int32
		for _, s := range strings.Fields(ids) {
			n, err := strconv.Atoi(s)
			if err != nil {
				t.Fatalf("tokens.tsv line %d: %v", seen, err)
			}
			want = append(want, int32(n))
		}
		if got := u.Tokenize(Normalize(c, phrase)); !slices.Equal(got, want) {
			wrong++
			if wrong <= 10 {
				t.Errorf("%q: %v, want %v", phrase, got, want)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	// A reference file that read as empty must not look like a pass.
	lines, err := os.ReadFile("testdata/tokens.tsv")
	if err != nil {
		t.Fatal(err)
	}
	if want := bytes.Count(lines, []byte{'\n'}); seen == 0 || seen != want {
		t.Fatalf("checked %d phrases, tokens.tsv has %d lines", seen, want)
	}
	if wrong > 0 {
		t.Errorf("%d phrases cut differently from the reference", wrong)
	}
}
