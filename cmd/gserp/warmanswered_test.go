// SPDX-License-Identifier: MIT

package main

import (
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
)

func TestWarmAnswered_OneRecordForAStandingSetAndANewOneForTheNext(t *testing.T) {
	// Every /search request is an Attempt of its own; the record of answered
	// sessions has to outlive them, or each request on a limited profile is
	// counted as a possible solve. A new set put its sessions on ports afresh,
	// as a new job does, and starts a new record.
	var w warmAnswered
	a, b := &blanktrail.Pool{}, &blanktrail.Pool{}
	first := w.of(a)
	if w.of(a) != first {
		t.Error("two requests on one standing set got two records")
	}
	if w.of(b) == first {
		t.Error("a new standing set kept the old set's record")
	}
}

func TestWarmSearch_EveryRequestOnOneSetSharesItsRecord(t *testing.T) {
	// What the /search interface hands each request: its own Attempt, the
	// set's record.
	s := warmSearch{warm: &warmSet{}, answered: &warmAnswered{}}
	pool := &blanktrail.Pool{}
	one, two := s.attempt(pool), s.attempt(pool)
	if one == two {
		t.Fatal("two requests were given one Attempt")
	}
	if one.Answered == nil || one.Answered != two.Answered {
		t.Error("two requests on one set do not share the set's record of answered sessions")
	}
}
