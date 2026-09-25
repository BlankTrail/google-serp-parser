// SPDX-License-Identifier: MIT

package export

// Ad is one paid placement, flat, for the same reason a Row is.
//
// It has a placement and no rank. An ad is not a result that happened to be
// paid for: it sits in a block of its own, above the results or below them or
// beside them, and the block it sat in is most of what somebody reading these
// wants to know.
type Ad struct {
	Ordinal   int    `json:"ordinal"`
	Query     string `json:"query"`
	Page      int    `json:"page"`
	Position  int    `json:"position"`
	Placement string `json:"placement"`
	Title     string `json:"title"`
	Host      string `json:"host"`
	URL       string `json:"url"`
	Snippet   string `json:"snippet"`
}

// Suggestion is one search a page offered beside its results.
type Suggestion struct {
	Ordinal  int    `json:"ordinal"`
	Query    string `json:"query"`
	Page     int    `json:"page"`
	Position int    `json:"position"`
	Text     string `json:"text"`
}
