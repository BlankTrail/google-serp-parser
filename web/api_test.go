// SPDX-License-Identifier: MIT

package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// askProgress reads the answer the page's own script polls for.
func askProgress(t *testing.T, s *Server, id int64) progressJSON {
	t.Helper()
	rec := get(t, s, "/api/progress?job="+strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("progress gave %d, want 200", rec.Code)
	}
	var got progressJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	return got
}

// press sends what a button on the job page sends: a form, by post, naming the
// job it is aimed at.
func press(t *testing.T, s *Server, at string, id int64) *http.Response {
	t.Helper()
	return postForm(t, s, at, url.Values{"job": {strconv.FormatInt(id, 10)}}).Result()
}

func TestProgress_SaysTheSameNumbersThePageShows(t *testing.T) {
	// Two sources for one truth is how a page ends up disagreeing with itself
	// two seconds after it loads: the numbers the reader was given are replaced
	// by numbers counted somewhere else, and neither of them is wrong enough to
	// notice.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 9, 4, 2)

	got := askProgress(t, s, id)
	sum, err := s.store.Progress(t.Context(), id)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	// Four numbers that differ from each other and none of them zero: on a job
	// of nothing every count is 0, and a page showing the wrong number would
	// still be showing the right one.
	if sum.Total != 9 || sum.Done != 4 || sum.Failed != 2 || sum.Pending != 3 {
		t.Fatalf("the fixture reads back as %d/%d/%d/%d, and nothing below is worth asking of it",
			sum.Total, sum.Done, sum.Failed, sum.Pending)
	}
	if got.ID != id {
		t.Errorf("the answer is about job %d, and %d was asked for", got.ID, id)
	}
	if got.Total != sum.Total || got.Done != sum.Done ||
		got.Failed != sum.Failed || got.Pending != sum.Pending {
		t.Errorf("the api says %+v and the store says %d/%d/%d/%d",
			got, sum.Total, sum.Done, sum.Failed, sum.Pending)
	}

	body := get(t, s, jobPath(id)).Body.String()
	for cell, want := range map[string]int{
		"count-total": got.Total, "count-done": got.Done,
		"count-failed": got.Failed, "count-pending": got.Pending,
	} {
		if shown := shown(t, body, cell); shown != strconv.Itoa(want) {
			t.Errorf("the page shows %s = %q and the api says %d", cell, shown, want)
		}
	}
}

func TestProgress_IsAnsweredAsJSON(t *testing.T) {
	// A browser handed a page's own data as text guesses what it is, and a
	// proxy in the way guesses differently.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 2, 1, 0)

	rec := get(t, s, "/api/progress?job="+strconv.FormatInt(id, 10))
	if kind := rec.Header().Get("Content-Type"); !strings.HasPrefix(kind, "application/json") {
		t.Errorf("Content-Type=%q, want json", kind)
	}
}

func TestProgress_RefusesAJobThatIsNotThere(t *testing.T) {
	s := testServer(t)
	for _, at := range []string{"/api/progress?job=4242", "/api/progress?job=nonsense", "/api/progress"} {
		if code := get(t, s, at).Code; code != http.StatusNotFound {
			t.Errorf("GET %s gave %d, want 404", at, code)
		}
	}
}

func TestProgress_SaysNothingMoreIsComingOnceNobodyIsRunningTheJob(t *testing.T) {
	// This is what a page stops asking on. A job in flight has more to say; a
	// job stopped half way has nothing more to say until somebody presses
	// something, and a page that goes on asking knocks every two seconds until
	// morning for an answer that never changes.
	s, v, eng := heldServer(t)
	id := enqueue(t, v, "nightly", "a", "b")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})

	live := askProgress(t, s, id)
	if !live.Running || !live.Watch || live.Finished {
		t.Errorf("a running job answers %+v, want it running and worth watching", live)
	}

	eng.let(t, 1)
	waitUntil(t, "one query is recorded", func() bool { return progress(t, s.store, id).Done == 1 })
	if err := v.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitUntil(t, "the job has stopped", func() bool { _, ok := v.Running(); return !ok })

	idle := askProgress(t, s, id)
	if idle.Running || idle.Watch || idle.Finished {
		t.Errorf("an abandoned job answers %+v, want nothing running and nothing more to wait for", idle)
	}
	if idle.Done != 1 || idle.Pending != 1 {
		t.Errorf("an abandoned job answers %+v, want the one query it took and the one it did not", idle)
	}

	// Taken up again, the job is either running or waiting its turn, and either
	// way the page has something to wait for. The answer is read straight away
	// rather than waited for, because a resume that queued nothing would then be
	// reported as a wrong answer instead of as a test that timed out.
	if err := v.Resume(id); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if again := askProgress(t, s, id); !again.Watch {
		t.Errorf("a job taken up again answers %+v, want a page told to keep watching", again)
	}

	eng.let(t, 1)
	waitUntil(t, "the job is done", func() bool { return progress(t, s.store, id).Finished })
	over := askProgress(t, s, id)
	if !over.Finished || over.Watch || over.Running {
		t.Errorf("a finished job answers %+v, want it finished and nothing left to watch", over)
	}
	if over.Done != 2 || over.Pending != 0 {
		t.Errorf("a finished job answers %+v, want both queries done and none left", over)
	}
}

func TestStop_RefusesAGetSoNothingCanStopAJobByFollowingALink(t *testing.T) {
	// A browser's link prefetcher, a scanner, or anything else that walks links
	// must not be able to end somebody's run by reading a page.
	//
	// The job named is one that is actually running. Asking about a job that is
	// not there would be refused before anything could act on it, and the test
	// would pass on a route that stops whatever it is pointed at.
	s, v, _ := heldServer(t)
	id := enqueue(t, v, "nightly", "a", "b")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})

	for _, at := range []string{"/api/stop?job=", "/api/resume?job="} {
		at += strconv.FormatInt(id, 10)
		if code := get(t, s, at).Code; code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s gave %d, want 405", at, code)
		}
	}
}

func TestStop_EndsTheRunAndLeavesWhatWasNotReachedToBeTakenUp(t *testing.T) {
	// The button ends one job and throws nothing away. What was reached stays
	// recorded and what was not stays waiting, which is the whole of what a
	// resume takes up.
	s, v, eng := heldServer(t)
	id := enqueue(t, v, "nightly", "a", "b", "c")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})
	eng.let(t, 1)
	waitUntil(t, "one query is recorded", func() bool { return progress(t, s.store, id).Done == 1 })

	res := press(t, s, "/api/stop", id)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("stopping gave %d, want the reader sent back to the job", res.StatusCode)
	}
	// The reader pressed a button on a page and has no script to read an answer
	// with, so the answer is the page they were on, saying what is true now.
	if at := res.Header.Get("Location"); at != jobPath(id) {
		t.Errorf("Location=%q, want the job's own page", at)
	}

	waitUntil(t, "the job has stopped", func() bool { _, ok := v.Running(); return !ok })
	if sum := progress(t, s.store, id); sum.Done != 1 || sum.Pending != 2 || sum.Finished {
		t.Errorf("the stopped job is %d done, %d pending, finished=%v, want 1/2/false",
			sum.Done, sum.Pending, sum.Finished)
	}
}

func TestResume_TakesUpAJobThatWasLeftPartWay(t *testing.T) {
	// A stop that could not be undone is a job thrown away by whoever wanted a
	// pause.
	s, v, eng := heldServer(t)
	id := enqueue(t, v, "nightly", "a", "b")
	waitUntil(t, "the job is running", func() bool {
		got, ok := v.Running()
		return ok && got == id
	})
	if res := press(t, s, "/api/stop", id); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("stopping gave %d", res.StatusCode)
	}
	waitUntil(t, "the job has stopped", func() bool { _, ok := v.Running(); return !ok })

	res := press(t, s, "/api/resume", id)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("resuming gave %d, want the reader sent back to the job", res.StatusCode)
	}
	eng.let(t, 2)
	waitUntil(t, "the job is done", func() bool { return progress(t, s.store, id).Finished })
	if sum := progress(t, s.store, id); sum.Done != 2 || sum.Pending != 0 {
		t.Errorf("the resumed job is %d done and %d pending, want both queries taken",
			sum.Done, sum.Pending)
	}
}

func TestStop_AnswersNotFoundForAJobThatIsNotThere(t *testing.T) {
	// A stop aimed at a job nobody has is not a stop that worked.
	s := testServerWithSupervisor(t)
	for _, at := range []string{"/api/stop", "/api/resume"} {
		if res := press(t, s, at, 4242); res.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s for a missing job gave %d, want 404", at, res.StatusCode)
		}
	}
}

func TestStop_SaysSoOnAServerThatWasNeverGivenAnythingToRunWith(t *testing.T) {
	// A server reading a history on another machine cannot stop anything, and a
	// button that quietly does nothing is one somebody presses until they
	// believe the job cannot be stopped at all.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 2, 1, 0)

	res := press(t, s, "/api/stop", id)
	if res.StatusCode < 400 {
		t.Errorf("stopping on a server with nothing to run jobs gave %d", res.StatusCode)
	}
	body := make([]byte, 512)
	n, _ := res.Body.Read(body)
	if !strings.Contains(string(body[:n]), LangEN.T("form.norunner")) {
		t.Errorf("the reader was not told why nothing happened: %q", body[:n])
	}
}
