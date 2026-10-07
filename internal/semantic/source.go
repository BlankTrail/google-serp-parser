// SPDX-License-Identifier: MIT

package semantic

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	b64, err := findCharsmap(doc.Normalizer)
	if err != nil {
		return Vocab{}, nil, err
	}
	charsmap, err := base64.StdEncoding.DecodeString(b64)
	return v, charsmap, err
}

// findCharsmap finds the Precompiled normalizer anywhere in the sequence.
func findCharsmap(raw json.RawMessage) (string, error) {
	var n struct {
		Type        string            `json:"type"`
		Charsmap    string            `json:"precompiled_charsmap"`
		Normalizers []json.RawMessage `json:"normalizers"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return "", err
	}
	if n.Type == "Precompiled" {
		return n.Charsmap, nil
	}
	for _, c := range n.Normalizers {
		if got, err := findCharsmap(c); err == nil && got != "" {
			return got, nil
		}
	}
	return "", errors.New("semantic: the tokenizer has no precompiled normalization table")
}
