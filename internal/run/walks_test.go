// SPDX-License-Identifier: MIT

package run

import (
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/google"
)

func TestWalks_KeepAQueryWithTheSessionCarryingIt(t *testing.T) {
	// A query deeper than one page belongs to the session that opened it: the
	// address of its next page was issued to that session. The register is how
	// whichever thread is handed that session afterwards learns what it owes.
	w := newWalks()
	began := time.Now()
	one := w.begin(3, google.Query{Text: "x"}, began)
	if one.at != 3 || one.page != 0 || one.next != "" {
		t.Errorf("a walk starts as %+v, want the query's place and nothing taken yet", one)
	}
	if got, took := w.give(7, one); !took || got != one {
		t.Errorf("giving a free session the walk answered %+v (%v), want the walk it was given", got, took)
	}
	if w.open() != 1 {
		t.Errorf("%d walks are open, want the one just started", w.open())
	}
	got, ok := w.of(7)
	if !ok || got != one {
		t.Errorf("the session's walk reads %+v (%v), want the one it started", got, ok)
	}
	if _, ok := w.of(8); ok {
		t.Error("a session that owes nothing was given a walk")
	}
	if ids := w.carriers(); len(ids) != 1 || ids[0] != 7 {
		t.Errorf("the sessions carrying a query read %v, want the one that is", ids)
	}

	// A page taken carries the walk on to the address that page offered, and
	// the tries spent reaching Google start again.
	one.tries = 2
	w.carry(7, "https://www.google.ru/search?start=10&ei=E")
	got, _ = w.of(7)
	if got.page != 1 || got.next != "https://www.google.ru/search?start=10&ei=E" || got.tries != 0 {
		t.Errorf("after a page the walk reads %+v, want one page taken, the next address kept and no tries spent", got)
	}

	ended, ok := w.end(7)
	if !ok || ended.at != 3 || ended.page != 1 {
		t.Errorf("ending the walk gave %+v (%v), want the query and what it took", ended, ok)
	}
	if w.open() != 0 {
		t.Errorf("%d walks are open after the only one ended", w.open())
	}
	if _, ok := w.end(7); ok {
		t.Error("a walk already ended was handed out a second time")
	}
}

func TestWalks_RefuseASecondQueryToASessionCarryingOne(t *testing.T) {
	// One session carries one query. The pages it is taking are the pages of
	// one search Google showed it, and a second query put on top of the first
	// would take its place in the register: the first would never be taken
	// further, never settled, and never reported — a query lost out of the job
	// with its results half collected.
	w := newWalks()
	first := w.begin(1, google.Query{Text: "one"}, time.Now())
	w.give(5, first)
	w.carry(5, "https://www.google.ru/search?start=10")

	second := w.begin(2, google.Query{Text: "two"}, time.Now())
	got, took := w.give(5, second)
	if took {
		t.Error("a session already carrying a query was given another")
	}
	if got != first {
		t.Errorf("the session is carrying %+v, want the query it started with", got)
	}
	if w.open() != 2 {
		t.Errorf("%d walks are open, want both — the one carried and the one still looking for a session", w.open())
	}

	// And the one that was refused is not lost: it waits for a session of its
	// own.
	w.waitFor(second)
	if one, ok := w.resume(); !ok || one != second {
		t.Errorf("the walk waiting for a session reads %+v (%v), want the one refused", one, ok)
	}
}

func TestWalks_CountAQueryBetweenSessionsAsOpen(t *testing.T) {
	// A thread stops when the queue is drained and nothing is open, so what is
	// open has to count a query the instant it begins — including the moment it
	// is in a thread's hands, taken from those waiting and not yet given to a
	// session. Counted only where it sits, the last thread would go home while
	// another was still carrying one across.
	w := newWalks()
	one := w.begin(0, google.Query{Text: "x"}, time.Now())
	if w.open() != 1 {
		t.Errorf("%d walks are open with one just begun and no session yet", w.open())
	}
	w.give(4, one)
	w.park(4)
	if w.open() != 1 {
		t.Errorf("%d walks are open with one waiting for a session", w.open())
	}
	got, _ := w.resume()
	if w.open() != 1 {
		t.Errorf("%d walks are open with one in a thread's hands between sessions", w.open())
	}
	w.give(9, got)
	if _, ok := w.end(9); !ok || w.open() != 0 {
		t.Errorf("%d walks are open after the only one was settled", w.open())
	}
}

func TestWalks_AreUsedByEveryThreadAtOnce(t *testing.T) {
	// The keeper hands a session to whichever thread asks, so any of them may
	// be the one that continues a walk, and the register is shared by all of
	// them. This works it from eight at once; the race detector is what would
	// judge the sharing, and it needs a C compiler this machine has not got, so
	// it is run where one is.
	w := newWalks()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := int64(n)
			w.give(id, w.begin(n, google.Query{Text: "x"}, time.Now()))
			w.carry(id, "https://www.google.ru/search?start=10")
			if _, ok := w.of(id); !ok {
				t.Errorf("session %d lost its walk", id)
			}
			w.carriers()
			w.end(id)
		}(i)
	}
	wg.Wait()
	if w.open() != 0 {
		t.Errorf("%d walks are open after every thread ended its own", w.open())
	}
}
