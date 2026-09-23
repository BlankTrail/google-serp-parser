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
	one := w.start(7, 3, google.Query{Text: "x"}, began)
	if one.at != 3 || one.page != 0 || one.next != "" {
		t.Errorf("a walk starts as %+v, want the query's place and nothing taken yet", one)
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
			w.start(id, n, google.Query{Text: "x"}, time.Now())
			w.carry(id, "https://www.google.ru/search?start=10")
			if _, ok := w.of(id); !ok {
				t.Errorf("session %d lost its walk", id)
			}
			w.end(id)
		}(i)
	}
	wg.Wait()
	if w.open() != 0 {
		t.Errorf("%d walks are open after every thread ended its own", w.open())
	}
}
