// SPDX-License-Identifier: MIT

package export

// Verdict is what one index check settled about one address.
//
// It is a shape of its own rather than a Row with a column added. A row stands
// for something that was found and carries a title, a rank and a snippet; a
// verdict stands for an address that was asked about, and for half of them
// there is nothing to put in any of those columns. Bending one into the other
// would either put a column on every search export that has no business there,
// or fill a verdict's columns with blanks that read as missing data rather than
// as an answer.
type Verdict struct {
	// Ordinal is the address's place in the list the job was given, so a reader
	// can lay each answer against the line of their own file it came from.
	Ordinal int    `json:"ordinal"`
	Target  string `json:"target"`
	Held    bool   `json:"held"`
}
