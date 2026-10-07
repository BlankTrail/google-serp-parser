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
	if doc.Model.UnkID < 0 || int(doc.Model.UnkID) >= len(v.Pieces) {
		// An unknown id outside the vocabulary would make the tokenizer drop the
		// wrong piece (or none) and index past the scores.
		return Vocab{}, nil, fmt.Errorf("semantic: %s: unk_id %d is outside the %d pieces", path, doc.Model.UnkID, len(v.Pieces))
	}
	b64, err := findCharsmap(doc.Normalizer)
	if err != nil {
		return Vocab{}, nil, err
	}
	charsmap, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return Vocab{}, nil, fmt.Errorf("semantic: %s: the normalization table is not base64: %w", path, err)
	}
	return v, charsmap, nil
}

// errNoCharsmap is "this branch of the normalizer has no table", as against a
// branch that is broken: only the first is a reason to look further.
var errNoCharsmap = errors.New("semantic: the tokenizer has no precompiled normalization table")

// findCharsmap finds the Precompiled normalizer anywhere in the sequence.
func findCharsmap(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", errNoCharsmap
	}
	var n struct {
		Type        string            `json:"type"`
		Charsmap    string            `json:"precompiled_charsmap"`
		Normalizers []json.RawMessage `json:"normalizers"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return "", fmt.Errorf("semantic: reading the normalizer: %w", err)
	}
	if n.Type == "Precompiled" {
		if n.Charsmap == "" {
			return "", errors.New("semantic: the precompiled normalization table is empty")
		}
		return n.Charsmap, nil
	}
	for _, c := range n.Normalizers {
		got, err := findCharsmap(c)
		if errors.Is(err, errNoCharsmap) {
			continue
		}
		return got, err
	}
	return "", errNoCharsmap
}
