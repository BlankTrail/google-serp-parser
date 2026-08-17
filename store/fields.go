// SPDX-License-Identifier: MIT

package store

import "strings"

// The parts of a result a job can be asked to keep.
//
// These are what the parser reads and nothing more. A choice offering something
// it never looks at would be a switch that does nothing, which is worse than an
// absent one: somebody would turn it on and believe the page had been asked for
// that.
const (
	FieldTitle   = "title"
	FieldURL     = "url"
	FieldLink    = "link"
	FieldHost    = "host"
	FieldSnippet = "snippet"
	FieldPath    = "path"
)

// EveryField is what a job keeps when it says nothing, in the order a file
// writes them.
func EveryField() []string {
	return []string{FieldTitle, FieldURL, FieldLink, FieldHost, FieldSnippet, FieldPath}
}

// Fields is what a job keeps of each result, written down as the names
// themselves.
//
// Empty means all of them. Every job written before this existed kept
// everything, and a reader of those rows has to understand them the way they
// were meant; it is also what somebody who has not touched the choice means.
//
// Names rather than a bitmask: the column is read by eye when something is
// wrong with a job, and adding a part later must not change what is already
// written down. The cost is a string compare per result, against a list of six.
type Fields string

// Keeps reports whether this job keeps that part of a result.
//
// The place a result stood is not among them and is always kept. It is one or
// two bytes, it is what orders the rows within a page, and a file of addresses
// with no order is a bag rather than a result page.
func (f Fields) Keeps(name string) bool {
	if f == "" {
		return true
	}
	for part := range strings.SplitSeq(string(f), ",") {
		if part == name {
			return true
		}
	}
	return false
}

// Kept is the parts this job keeps, in the order a file writes them.
func (f Fields) Kept() []string {
	var kept []string
	for _, name := range EveryField() {
		if f.Keeps(name) {
			kept = append(kept, name)
		}
	}
	return kept
}

// FieldsOf builds the choice from the names given, keeping the order EveryField
// declares and passing over anything that is not a part of a result.
//
// A choice naming everything is written down as everything rather than as the
// empty string: the two mean the same to a reader and the second is what a job
// that never chose carries, and telling those apart is what a page offering the
// choice again has to do.
func FieldsOf(names []string) Fields {
	want := map[string]bool{}
	for _, name := range names {
		want[strings.TrimSpace(name)] = true
	}
	var kept []string
	for _, name := range EveryField() {
		if want[name] {
			kept = append(kept, name)
		}
	}
	return Fields(strings.Join(kept, ","))
}
