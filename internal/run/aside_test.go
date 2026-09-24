// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/sessions"
)

// heldHidingOrigin is a search host that answers with encrypted links, as
// hidingOrigin does, and holds every lookup until the test lets them go — so a
// test can see what a run does while a query's addresses are still being read.
func heldHidingOrigin(t *testing.T, searches, lookups *atomic.Int64, gate <-chan struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/goto"):
			lookups.Add(1)
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Location", "https://example.com/page")
			w.WriteHeader(http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/search"):
			searches.Add(1)
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			_, _ = io.WriteString(w, `<!doctype html><html><body><div id="search"><div data-snc="x">`+
				`<a href="/goto?url=CAESXAHuR6pN7OGc" data-ved="2"><h3>Title</h3></a>`+
				`<cite>example.com</cite></div></div></body></html>`)
		default:
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			_, _ = io.WriteString(w, `<!doctype html><html><body><div id="main"></div></body></html>`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// until waits for a count to reach n, and says whether it did in time.
func until(n int64, count *atomic.Int64, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for count.Load() < n {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// walkingRunner is a one-thread run that keeps its own sessions, over a pool
// of two ports: one for the thread, one for a lookup.
func walkingRunner(t *testing.T, o *httptest.Server, sink Sink) *Runner {
	t.Helper()
	f := poolFacing(t, o.Listener.Addr().String(), 2, inSessions)
	return &Runner{Pool: f.Pool, Threads: 1, Sink: sink,
		Keeper: sessions.NewKeeper(sessions.NewMemory()), Want: searchDesktop}
}

func TestRunner_GoesOnAskingWhileAFinishedQuerysAddressesAreRead(t *testing.T) {
	// Every result whose address Google hid costs a request of its own, carried
	// from address to address until one answers, and the thread that finished
	// the query used to stand through all of it. Measured on the server on a
	// hundred threads over the wingate list: a sixth of the threads' time, and
	// seventy of the hundred at once when walks begun together ended together.
	// The query is finished and its lookups hold no session, so the thread goes
	// on to the next query while they are read.
	var searches, lookups atomic.Int64
	gate := make(chan struct{})
	sink := &recordingSink{}
	r := walkingRunner(t, heldHidingOrigin(t, &searches, &lookups, gate), sink)

	done := make(chan Report, 1)
	go func() {
		done <- r.Run(context.Background(), Job{Queries: usQueries(2), Pages: 1, Addresses: true})
	}()
	went := until(2, &searches, 5*time.Second)
	close(gate)
	rep := <-done

	if !went {
		t.Fatalf("the second query was not asked while the first one's address was being read: "+
			"%d searches, %d lookups", searches.Load(), lookups.Load())
	}
	if rep.Done != 2 {
		t.Fatalf("Done=%d, want both queries (failed %d, untried %d)", rep.Done, rep.Failed, rep.Untried)
	}
	for _, got := range sink.records() {
		if len(got.Pages) != 1 || len(got.Pages[0].Results) != 1 || got.Pages[0].Results[0].URL == "" {
			t.Errorf("%q was written down as %+v, want its one result with the address read", got.Query.Text, got.Pages)
		}
	}
}

func TestRunner_SettlesNoMoreQueriesAsideThanItHasThreads(t *testing.T) {
	// What is being settled is held in memory rather than written, and a history
	// falling behind the walks is what a stopped run loses. So one query a thread
	// is settled aside at a time, and a thread that finishes another while its
	// share is taken waits for room before it asks for anything else.
	var searches, lookups atomic.Int64
	gate := make(chan struct{})
	sink := &recordingSink{}
	r := walkingRunner(t, heldHidingOrigin(t, &searches, &lookups, gate), sink)

	done := make(chan Report, 1)
	go func() {
		done <- r.Run(context.Background(), Job{Queries: usQueries(3), Pages: 1, Addresses: true})
	}()
	// The first query is being settled and the second is finished: the thread
	// has nowhere to put the second, and the third must wait for it.
	if !until(2, &searches, 5*time.Second) {
		close(gate)
		<-done
		t.Fatalf("the second query was not asked at all: %d searches", searches.Load())
	}
	time.Sleep(300 * time.Millisecond)
	asked := searches.Load()
	close(gate)
	rep := <-done

	if asked != 2 {
		t.Errorf("%d queries were asked while one was being settled and another was waiting to be, want 2", asked)
	}
	if rep.Done != 3 {
		t.Errorf("Done=%d, want all three once the addresses could be read", rep.Done)
	}
}

func TestRunner_WritesDownTheQueryBeingSettledWhenTheRunIsStopped(t *testing.T) {
	// A stop does not cut short what is being settled. The query is finished and
	// its pages are in hand, and losing them to the press of a button is what the
	// settling after a stop is there to prevent; it gets the same span the walks
	// in hand get.
	var searches, lookups atomic.Int64
	gate := make(chan struct{})
	sink := &recordingSink{}
	r := walkingRunner(t, heldHidingOrigin(t, &searches, &lookups, gate), sink)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan Report, 1)
	go func() {
		done <- r.Run(ctx, Job{Queries: usQueries(1), Pages: 1, Addresses: true})
	}()
	if !until(1, &lookups, 5*time.Second) {
		close(gate)
		<-done
		t.Fatalf("the address was never asked for: %d searches, %d lookups", searches.Load(), lookups.Load())
	}
	stop()
	time.Sleep(100 * time.Millisecond)
	close(gate)
	<-done

	got := sink.records()
	if len(got) != 1 {
		t.Fatalf("%d queries were written down, want the one that was being settled when the run stopped", len(got))
	}
	if res := got[0].Pages[0].Results; len(res) != 1 || res[0].URL == "" {
		t.Errorf("the query was written down as %+v, want its result with the address read", res)
	}
}

func TestRunner_ReturnsOnlyOnceWhatItFinishedIsWrittenDown(t *testing.T) {
	// A run that returned with queries still being settled aside would hand its
	// caller a report of queries the history does not hold yet — and a server
	// taking the next job, or a program exiting, would take them with it.
	var searches, lookups atomic.Int64
	gate := make(chan struct{})
	sink := &recordingSink{}
	r := walkingRunner(t, heldHidingOrigin(t, &searches, &lookups, gate), sink)

	done := make(chan Report, 1)
	go func() {
		done <- r.Run(context.Background(), Job{Queries: usQueries(1), Pages: 1, Addresses: true})
	}()
	if !until(1, &lookups, 5*time.Second) {
		close(gate)
		<-done
		t.Fatalf("the address was never asked for: %d searches, %d lookups", searches.Load(), lookups.Load())
	}
	select {
	case <-done:
		close(gate)
		t.Fatal("the run returned while the query it had finished was still being settled")
	case <-time.After(200 * time.Millisecond):
	}
	close(gate)
	<-done
	if got := sink.records(); len(got) != 1 {
		t.Errorf("%d queries were written down by the time the run returned, want 1", len(got))
	}
}
