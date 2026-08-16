// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// screenServer answers the egress-test endpoint, failing every address whose
// URL contains one of the listed fragments.
func screenServer(t *testing.T, bad map[string]string) (*Client, *int64) {
	t.Helper()
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		var body struct {
			Upstream string `json:"upstream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		res := map[string]CheckResult{"http": {OK: true}}
		for fragment, detail := range bad {
			if strings.Contains(body.Upstream, fragment) {
				res = map[string]CheckResult{"http": {OK: false, Detail: detail}}
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL, "test-key")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, &calls
}

func TestScreen_SeparatesTheWorkingAddressesFromTheRest(t *testing.T) {
	c, calls := screenServer(t, map[string]string{"10.0.0.2": "connect timeout"})
	ups := []Upstream{
		{Scheme: "socks5", Host: "10.0.0.1", Port: "1080"},
		{Scheme: "socks5", Host: "10.0.0.2", Port: "1080"},
		{Scheme: "socks5", Host: "10.0.0.3", Port: "1080"},
	}

	rep := Screen(context.Background(), ScreenConfig{Client: c, Workers: 2}, ups)

	if len(rep.Good) != 2 {
		t.Errorf("Good=%d, want 2", len(rep.Good))
	}
	if len(rep.Bad) != 1 {
		t.Fatalf("Bad=%d, want 1", len(rep.Bad))
	}
	if rep.Bad[0].Detail != "connect timeout" {
		t.Errorf("Bad[0].Detail=%q, want the reason the check gave", rep.Bad[0].Detail)
	}
	if got := atomic.LoadInt64(calls); got != 3 {
		t.Errorf("%d checks made, want one per address", got)
	}
}

func TestScreen_KeepsGoingAfterAnAddressFails(t *testing.T) {
	// A bought list is expected to hold dead addresses. Stopping at the first
	// would make the feature useless on exactly the input it exists for.
	c, _ := screenServer(t, map[string]string{"10.0.0.": "dead"})
	ups := make([]Upstream, 20)
	for i := range ups {
		ups[i] = Upstream{Scheme: "socks5", Host: fmt.Sprintf("10.0.0.%d", i+1), Port: "1080"}
	}

	rep := Screen(context.Background(), ScreenConfig{Client: c, Workers: 4}, ups)

	if len(rep.Bad) != 20 {
		t.Errorf("Bad=%d, want all 20 checked and reported", len(rep.Bad))
	}
	if len(rep.Good) != 0 {
		t.Errorf("Good=%d, want none", len(rep.Good))
	}
}

func TestScreen_KeepsTheInputOrderSoTwoRunsBuildTheSamePool(t *testing.T) {
	// The pool's whole layout is a pure function of its configuration, which is
	// what makes two runs comparable. A screen that returned its results in
	// completion order would put a different set of addresses in front of it
	// every time and take that property away.
	c, _ := screenServer(t, nil)
	ups := make([]Upstream, 12)
	for i := range ups {
		ups[i] = Upstream{Scheme: "socks5", Host: fmt.Sprintf("10.0.0.%d", i+1), Port: "1080"}
	}

	rep := Screen(context.Background(), ScreenConfig{Client: c, Workers: 6}, ups)

	if len(rep.Good) != len(ups) {
		t.Fatalf("Good=%d, want %d", len(rep.Good), len(ups))
	}
	for i := range ups {
		if rep.Good[i] != ups[i] {
			t.Fatalf("Good[%d]=%v, want %v - the input order was not kept", i, rep.Good[i], ups[i])
		}
	}
}

func TestScreen_ReportsWhatItFinishedWhenCancelled(t *testing.T) {
	// Cancelling before anything was checked leaves nothing to report. An address
	// the pass never reached is not a bad address, and listing it as one would
	// throw away a working proxy on no evidence.
	c, _ := screenServer(t, nil)
	ups := make([]Upstream, 50)
	for i := range ups {
		ups[i] = Upstream{Scheme: "socks5", Host: "10.0.0.1", Port: "1080"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Whether a worker is already waiting when the pass starts handing out work is
	// down to the scheduler, and a pass that only usually stops is not one a user
	// can stop. Repeating settles it: no attempt may check anything.
	for attempt := 0; attempt < 2000; attempt++ {
		rep := Screen(ctx, ScreenConfig{Client: c, Workers: 2}, ups)
		if n := len(rep.Good) + len(rep.Bad); n != 0 {
			t.Fatalf("attempt %d reported %d verdicts, want none - nothing was checked "+
				"before the cancellation, and an address that was never reached is "+
				"neither good nor bad", attempt, n)
		}
	}
}

func TestScreen_ReportsEveryAddressItReachedBeforeTheCancellation(t *testing.T) {
	// A user who stops a screen of thousands part way keeps what it already
	// established rather than losing the whole pass.
	reached := make(chan struct{}, 1)
	var served int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt64(&served, 1) == 1 {
			reached <- struct{}{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]CheckResult{"http": {OK: true}})
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, "test-key")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ups := make([]Upstream, 50)
	for i := range ups {
		ups[i] = Upstream{Scheme: "socks5", Host: fmt.Sprintf("10.0.0.%d", i+1), Port: "1080"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-reached
		cancel()
	}()
	defer cancel()

	rep := Screen(ctx, ScreenConfig{Client: c, Workers: 1}, ups)

	n := len(rep.Good) + len(rep.Bad)
	if n == 0 {
		t.Error("no verdicts at all, want the addresses the pass got through")
	}
	if n > len(ups) {
		t.Errorf("reported %d verdicts for %d addresses", n, len(ups))
	}
	// Which addresses the pass got through before the cancellation is a race, but
	// whichever they are, they keep the order they were given in.
	at := map[Upstream]int{}
	for i, u := range ups {
		at[u] = i
	}
	prev := -1
	for _, u := range rep.Good {
		if at[u] <= prev {
			t.Fatalf("%v came back out of order after index %d", u, prev)
		}
		prev = at[u]
	}
}

func TestScreen_ReportsProgressOncePerAddress(t *testing.T) {
	c, _ := screenServer(t, nil)
	ups := []Upstream{
		{Scheme: "socks5", Host: "10.0.0.1", Port: "1080"},
		{Scheme: "socks5", Host: "10.0.0.2", Port: "1080"},
	}
	var seen int64
	rep := Screen(context.Background(), ScreenConfig{
		Client:   c,
		Workers:  2,
		Progress: func(_, _ int) { atomic.AddInt64(&seen, 1) },
	}, ups)

	if len(rep.Good) != 2 {
		t.Fatalf("Good=%d, want 2", len(rep.Good))
	}
	if got := atomic.LoadInt64(&seen); got != 2 {
		t.Errorf("progress called %d times, want one per address", got)
	}
}

func TestScreen_TreatsANonPositiveWorkerCountAsOne(t *testing.T) {
	c, _ := screenServer(t, nil)
	rep := Screen(context.Background(), ScreenConfig{Client: c, Workers: 0},
		[]Upstream{{Scheme: "socks5", Host: "10.0.0.1", Port: "1080"}})
	if len(rep.Good) != 1 {
		t.Errorf("Good=%d, want 1 - a zero worker count must not deadlock", len(rep.Good))
	}
}

func TestScreen_RejectsAnAddressWhoseChecksWereAllSkipped(t *testing.T) {
	// Silence is not approval. A verdict built from no evidence is how a dead
	// address reaches a port.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]CheckResult{"http": {Skipped: true}})
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, "test-key")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	rep := Screen(context.Background(), ScreenConfig{Client: c, Workers: 1},
		[]Upstream{{Scheme: "socks5", Host: "10.0.0.1", Port: "1080"}})

	if len(rep.Good) != 0 {
		t.Errorf("Good=%d, want none - nothing was actually checked", len(rep.Good))
	}
	if len(rep.Bad) != 1 {
		t.Fatalf("Bad=%d, want 1", len(rep.Bad))
	}
	if rep.Bad[0].Detail != "no check ran" {
		t.Errorf("Bad[0].Detail=%q, want the reason to be that nothing ran - a skipped "+
			"check is not a failed one, and reporting it as one hides that the "+
			"address is still unknown", rep.Bad[0].Detail)
	}
}
