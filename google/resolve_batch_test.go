// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// redirector answers every /goto with a redirect to a destination derived from
// the request, so a batch can be checked result by result.
func redirector(t *testing.T, fail map[string]bool) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		id := r.URL.Query().Get("url")
		if fail[id] {
			w.WriteHeader(http.StatusOK) // not a redirect
			return
		}
		http.Redirect(w, r, "https://"+id+".test/page", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func unresolved(origin string, ids ...string) SERP {
	s := SERP{Origin: origin}
	for i, id := range ids {
		s.Results = append(s.Results, Result{
			Position: i + 1,
			Host:     id + ".test",
			Link:     "/goto?url=" + id,
			Form:     LinkEncrypted,
		})
	}
	return s
}

func TestResolveAll_FillsEveryUnresolvedResult(t *testing.T) {
	srv, hits := redirector(t, nil)
	serp := unresolved(srv.URL, "a", "b", "c")

	rep := (&Resolver{Client: srv.Client()}).ResolveAll(context.Background(), &serp, 2)
	if rep.Attempted != 3 || rep.Resolved != 3 || rep.Failed != 0 {
		t.Fatalf("report=%+v, want 3 attempted, 3 resolved, 0 failed", rep)
	}
	for i, r := range serp.Results {
		if !r.Resolved() {
			t.Errorf("result %d still unresolved", i+1)
		}
		if want := "https://" + r.Host + "/page"; r.URL != want {
			t.Errorf("result %d URL=%q, want %q", i+1, r.URL, want)
		}
	}
	if *hits != 3 {
		t.Errorf("made %d requests, want one per result", *hits)
	}
}

func TestResolveAll_SkipsResultsThatAlreadyCarryAnAddress(t *testing.T) {
	// Under the direct and redirect forms the address is already in the page,
	// and the result still carries the href it was read from. Asking again is a
	// wasted request per result, and at a run's volumes that is the difference
	// between a job that finishes and one that does not.
	srv, hits := redirector(t, nil)
	serp := SERP{Origin: srv.URL, Results: []Result{
		{Position: 1, Host: "a.test", URL: "https://a.test/known", Link: "/url?q=https%3A%2F%2Fa.test%2Fknown", Form: LinkRedirect},
		{Position: 2, Host: "b.test", Link: "/goto?url=b", Form: LinkEncrypted},
	}}

	rep := (&Resolver{Client: srv.Client()}).ResolveAll(context.Background(), &serp, 2)
	if rep.Attempted != 1 {
		t.Errorf("attempted %d, want 1 — the resolved one must be left alone", rep.Attempted)
	}
	if *hits != 1 {
		t.Errorf("made %d requests, want 1", *hits)
	}
	if serp.Results[0].URL != "https://a.test/known" {
		t.Errorf("the already-resolved result was overwritten: %q", serp.Results[0].URL)
	}
}

func TestResolveAll_LeavesAResultWithNoHrefAlone(t *testing.T) {
	// A result the page gave no href for has nothing to resolve. An empty link
	// joined to the origin asks the origin for itself, and whatever came back
	// would be written in as this result's address.
	srv, hits := redirector(t, nil)
	serp := SERP{Origin: srv.URL, Results: []Result{{Position: 1, Host: "a.test"}}}

	rep := (&Resolver{Client: srv.Client()}).ResolveAll(context.Background(), &serp, 2)
	if rep.Attempted != 0 || len(rep.Errs) != 0 {
		t.Errorf("report=%+v, want nothing attempted", rep)
	}
	if *hits != 0 {
		t.Errorf("made %d requests for a result with no link", *hits)
	}
	if serp.Results[0].Resolved() {
		t.Errorf("gave the result an address the page never carried: %q", serp.Results[0].URL)
	}
}

func TestResolveAll_OneFailureDoesNotSinkTheRest(t *testing.T) {
	// A link that will not resolve is a result with a host and no address —
	// still a usable result. Failing the whole page over one would throw away
	// the ranking the page was taken for.
	srv, _ := redirector(t, map[string]bool{"b": true})
	serp := unresolved(srv.URL, "a", "b", "c")

	rep := (&Resolver{Client: srv.Client()}).ResolveAll(context.Background(), &serp, 3)
	if rep.Resolved != 2 || rep.Failed != 1 {
		t.Fatalf("report=%+v, want 2 resolved and 1 failed", rep)
	}
	if len(rep.Errs) != 1 {
		t.Errorf("kept %d errors, want the one failure", len(rep.Errs))
	}
	if serp.Results[1].Resolved() {
		t.Error("the failing result was given an address anyway")
	}
	if !serp.Results[0].Resolved() || !serp.Results[2].Resolved() {
		t.Error("a neighbour of the failure was left unresolved")
	}
}

func TestResolveAll_JoinsARelativeLinkToThePagesOrigin(t *testing.T) {
	// Result.Link is origin-relative under the encrypted form, and the origin
	// is not always google.com — a capture with a country axis lands on that
	// country's domain. Hardcoding the host would resolve against the wrong one.
	srv, _ := redirector(t, nil)
	serp := unresolved(srv.URL, "a")
	rep := (&Resolver{Client: srv.Client()}).ResolveAll(context.Background(), &serp, 1)
	if rep.Resolved != 1 {
		t.Fatalf("report=%+v, want the relative link joined and resolved", rep)
	}
}

func TestResolveAll_ReportsAMissingOrigin(t *testing.T) {
	// Without an origin a relative link cannot be resolved at all, and
	// silently skipping would look like a page where nothing needed resolving.
	serp := unresolved("", "a")
	rep := (&Resolver{}).ResolveAll(context.Background(), &serp, 1)
	if rep.Failed != 1 || len(rep.Errs) != 1 {
		t.Fatalf("report=%+v, want the missing origin reported as a failure", rep)
	}
	if !strings.Contains(rep.Errs[0].Error(), "origin") {
		t.Errorf("error=%v, want it to name the missing origin", rep.Errs[0])
	}
}

func TestResolveAll_ReportsNothingForAnAbsentPage(t *testing.T) {
	// A caller resolving whatever a capture returned should not have to check
	// for a page first; a capture that produced none is an empty batch.
	rep := (&Resolver{}).ResolveAll(context.Background(), nil, 2)
	if rep.Attempted != 0 || rep.Resolved != 0 || rep.Failed != 0 || len(rep.Errs) != 0 {
		t.Errorf("report=%+v, want an empty one", rep)
	}
}

func TestResolveAll_HonoursContextCancellation(t *testing.T) {
	srv, _ := redirector(t, nil)
	serp := unresolved(srv.URL, "a", "b")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := (&Resolver{Client: srv.Client()}).ResolveAll(ctx, &serp, 2)
	if rep.Resolved != 0 {
		t.Errorf("resolved %d despite a cancelled context", rep.Resolved)
	}
}

func TestResolveAll_TreatsANonPositiveWorkerCountAsOne(t *testing.T) {
	srv, _ := redirector(t, nil)
	serp := unresolved(srv.URL, "a", "b")
	rep := (&Resolver{Client: srv.Client()}).ResolveAll(context.Background(), &serp, 0)
	if rep.Resolved != 2 {
		t.Errorf("report=%+v, want both resolved on a single worker", rep)
	}
}

func TestResolveAll_CountsACancelledBatchByWhatItTriedNotByWhatIsLeft(t *testing.T) {
	// Failed means tried and lost. Charging the untouched remainder to it tells
	// a caller that work was attempted and thrown away, when nobody ever got to
	// it, and leaves the count larger than the errors kept beside it.
	ctx, cancel := context.WithCancel(context.Background())
	held := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("url") == "b" {
			// Hold the second link open so the batch is cancelled with one
			// request in flight and the rest of the page not yet handed out.
			held <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		http.Redirect(w, r, "https://a.test/page", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	go func() {
		<-held
		cancel()
		close(release)
	}()

	serp := unresolved(srv.URL, "a", "b", "c", "d", "e", "f")
	rep := (&Resolver{Client: srv.Client()}).ResolveAll(ctx, &serp, 1)

	// One link finished, one died in flight, and four were never handed to a
	// worker. Only the second of those is a failure.
	if rep.Attempted != 6 || rep.Resolved != 1 || rep.Failed != 1 {
		t.Fatalf("report=%+v, want 6 attempted, 1 resolved and only the in-flight link failed", rep)
	}
	if left := rep.Attempted - rep.Resolved - rep.Failed; left != 4 {
		t.Errorf("%d links left untried, want the 4 the batch never reached", left)
	}
	if last := rep.Errs[len(rep.Errs)-1].Error(); !strings.Contains(last, "untried") {
		t.Errorf("last error=%q, want it to say the batch stopped with links untried", last)
	}
}
