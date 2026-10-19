// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/store"
)

// exitless stands where the pools go for a machine with two profiles: "Empty",
// which names no exits until a test gives it some, and "Listed", which has
// them. It counts every time a pool was asked for on each.
type exitless struct {
	pools  *raisedPools
	exits  atomic.Bool
	mu     sync.Mutex
	askedE int
	askedL int
	// block, when set, holds every asking until it is closed, so a test can
	// land something in the instant a pool is being asked for.
	block chan struct{}
}

func (x *exitless) raise(ctx context.Context, want Wanted) (engine, error) {
	x.mu.Lock()
	if want.Profile.Name == "Empty" {
		x.askedE++
	} else {
		x.askedL++
	}
	block := x.block
	x.mu.Unlock()
	if block != nil {
		<-block
	}
	if want.Profile.Empty() && !x.exits.Load() {
		// What cmd/gserp answers: the sentinel, and the profile by its name.
		return nil, fmt.Errorf("%w: proxy profile %q", ErrNoExits, want.Profile.Name)
	}
	return x.pools.raise(ctx, want)
}

func (x *exitless) asked() (empty, listed int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.askedE, x.askedL
}

// exitlessSupervisor is a supervisor on exitless, asking again for a waiting
// job every retry, with both profiles in its history and what it logs kept.
func exitlessSupervisor(t *testing.T, retry time.Duration) (*Supervisor, *store.Store, *exitless, *recorded, int64, int64) {
	t.Helper()
	st := testStore(t)
	ctx := context.Background()
	// The first profile written is the default.
	empty, err := st.CreateProfile(ctx, store.Profile{Name: "Empty"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	listed, err := st.CreateProfile(ctx, store.Profile{Name: "Listed", Kind: "file", Location: "list.txt"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	x := &exitless{pools: &raisedPools{hold: make(chan struct{})}}
	v := start(st, source{raise: x.raise}, 2, 1)
	said := &recorded{}
	v.mu.Lock()
	v.retry = retry
	v.log = slog.New(slog.NewTextHandler(said, nil))
	v.mu.Unlock()
	t.Cleanup(func() { _ = v.Close() })
	return v, st, x, said, empty, listed
}

func enqueueOn(t *testing.T, v *Supervisor, profile int64, queries ...string) int64 {
	t.Helper()
	id, err := v.Enqueue(store.JobSpec{Name: "on a profile", Pages: 1, Country: "us", Language: "en",
		ProfileID: profile}, queries)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return id
}

func TestSupervisor_KeepsAJobWhoseProfileNamesNoExitsInTheQueueAndAsksAgainCalmly(t *testing.T) {
	// Ports opened with nowhere to go out through cannot carry a session, and on
	// 2026-10-19 twenty threads on such ports sent the service thirty thousand
	// calls in two minutes. So the pool is never raised for such a profile. What
	// that must not turn into is a job that is lost — failed, stamped, dropped
	// from the queue — or a queue that asks for its pool again as fast as it
	// can: the job waits where it was, says why, and is asked for again every
	// so often, so a list that is filled in is taken up without anybody
	// restarting anything.
	const retry = 100 * time.Millisecond
	v, st, x, said, empty, _ := exitlessSupervisor(t, retry)

	id := enqueueOn(t, v, empty, "a", "b")
	waitUntil(t, "the job is waiting for exits", func() bool { _, ok := v.WaitingForExits(id); return ok })

	// Watched for a while: asked again about once a retry, and no more.
	watched := 8 * retry
	time.Sleep(watched)
	if asked, _ := x.asked(); asked > int(watched/retry)+3 {
		t.Errorf("the pool of a profile with no exits was asked for %d times in %v, want about one every %v",
			asked, watched, retry)
	}
	if asked, _ := x.asked(); asked < 2 {
		t.Errorf("the pool was asked for %d times in %v, want it asked again every %v", asked, watched, retry)
	}
	if !queued(v, id) {
		t.Errorf("the job is not in the queue (%v): a profile with no exits lost it its place", v.Queued())
	}
	if name, _ := v.WaitingForExits(id); name != "Empty" {
		t.Errorf("the job waits on profile %q, want Empty", name)
	}
	sum := progress(t, st, id)
	if sum.Finished || sum.Done != 0 || sum.Failed != 0 || sum.Pending != 2 {
		t.Errorf("finished=%v done=%d failed=%d pending=%d, want an untouched job with both queries pending",
			sum.Finished, sum.Done, sum.Failed, sum.Pending)
	}
	// Said once, by the profile's name, however many times it was asked.
	if n := strings.Count(said.String(), "names no exits"); n != 1 {
		t.Errorf("the log says the profile has no exits %d times, want once:\n%s", n, said.String())
	}
	if !strings.Contains(said.String(), "profile=Empty") {
		t.Errorf("the log does not name the profile:\n%s", said.String())
	}

	// The list is filled in where it lives. Nobody presses anything, and the
	// next asking raises it.
	x.exits.Store(true)
	waitUntilRunning(t, v, id)
	if _, ok := v.WaitingForExits(id); ok {
		t.Error("a job whose pool went up is still reported as waiting for exits")
	}
	x.pools.let(t, 2)
	waitUntil(t, "the job is done", func() bool { return progress(t, st, id).Finished })
}

func TestSupervisor_TakesTheJobBehindOneThatWaitsForExits(t *testing.T) {
	// One profile with nothing in it is no reason for a job on another to wait.
	v, st, x, _, empty, listed := exitlessSupervisor(t, time.Hour)

	waiting := enqueueOn(t, v, empty, "a")
	waitUntil(t, "the first job is waiting for exits", func() bool { _, ok := v.WaitingForExits(waiting); return ok })
	behind := enqueueOn(t, v, listed, "b")
	waitUntilRunning(t, v, behind)
	x.pools.let(t, 1)
	waitUntil(t, "the job behind is done", func() bool { return progress(t, st, behind).Finished })

	if q := v.Queued(); len(q) != 1 || q[0] != waiting {
		t.Errorf("the queue is %v, want the waiting job %d alone still in it", q, waiting)
	}
}

func TestSupervisor_AsksAtOnceForAJobWaitingForExitsWhenAProfileIsSaved(t *testing.T) {
	// The retry is an hour here, so only the saved profile can be what starts it.
	v, _, x, _, empty, _ := exitlessSupervisor(t, time.Hour)

	id := enqueueOn(t, v, empty, "a")
	waitUntil(t, "the job is waiting for exits", func() bool { _, ok := v.WaitingForExits(id); return ok })
	x.exits.Store(true)
	v.ExitsChanged()
	waitUntilRunning(t, v, id)
	x.pools.let(t, 1)
}

func TestSupervisor_DoesNotPutBackAJobStoppedWhileItWaitedForExits(t *testing.T) {
	// A job is taken out of the queue for the instant its pool is asked for, and
	// a stop that lands then is a stop: it is not put back to wait again.
	v, _, x, _, empty, _ := exitlessSupervisor(t, 50*time.Millisecond)
	id := enqueueOn(t, v, empty, "a")
	waitUntil(t, "the job is waiting for exits", func() bool { _, ok := v.WaitingForExits(id); return ok })
	block := make(chan struct{})
	x.mu.Lock()
	x.block = block
	x.mu.Unlock()
	waitUntilRunning(t, v, id)
	if err := v.Stop(id); err != nil {
		t.Fatalf("Stop while the pool was being asked for: %v", err)
	}
	close(block)
	waitUntil(t, "the job has left the queue", func() bool { return !queued(v, id) && !running(v, id) })
	time.Sleep(200 * time.Millisecond)
	if queued(v, id) {
		t.Error("a job stopped while it waited for exits was put back in the queue")
	}
	if _, ok := v.WaitingForExits(id); ok {
		t.Error("a stopped job is still reported as waiting for exits")
	}
}

func TestJobPage_SaysAJobIsWaitingForExitsInBothLanguages(t *testing.T) {
	// "Waiting its turn" would send the reader to wait for something that is
	// not coming. The page says what is missing and where it is put right.
	v, st, _, _, empty, _ := exitlessSupervisor(t, time.Hour)
	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := enqueueOn(t, v, empty, "a")
	waitUntil(t, "the job is waiting for exits", func() bool { _, ok := v.WaitingForExits(id); return ok })

	if got := s.progress(progress(t, st, id)); !got.NoExits || !got.Watch {
		t.Errorf("the job's progress reads noExits=%v watch=%v, want both: it is waiting and will start",
			got.NoExits, got.Watch)
	}
	body := get(t, s, jobPath(id)).Body.String()
	if got, want := html.UnescapeString(shown(t, body, "state")), LangEN.T("job.state.noexits"); got != want {
		t.Errorf("the job's state reads %q, want %q", got, want)
	}
	for _, lang := range []Lang{LangEN, LangRU} {
		said := lang.T("job.state.noexits")
		if said == "" || said == "job.state.noexits" || said == lang.T("job.state.waiting") {
			t.Errorf("%s has nothing of its own to say for a job waiting for exits: %q", lang, said)
		}
	}
}

func TestNoticeProfileEmpty_SaysJobsWaitForExitsRatherThanGoOutFromThisMachine(t *testing.T) {
	// It used to say a job would go out from this machine's own address. That
	// stopped being true when jobs came to run on sessions, and it is now the
	// opposite of what happens: nothing is opened, and the job waits.
	cases := map[Lang]struct{ says, never []string }{
		LangEN: {says: []string{"no exits", "wait", "Proxies"}, never: []string{"own address"}},
		LangRU: {says: []string{"не указаны выходы", "ждут", "Прокси"}, never: []string{"с адреса этой машины"}},
	}
	for lang, c := range cases {
		said := lang.T("notice.profile.empty")
		for _, want := range c.says {
			if !strings.Contains(said, want) {
				t.Errorf("%s: %q does not say %q", lang, said, want)
			}
		}
		for _, wrong := range c.never {
			if strings.Contains(said, wrong) {
				t.Errorf("%s: %q still says %q", lang, said, wrong)
			}
		}
	}
}

func TestProxiesAndReshape_AskAtOnceForAJobWaitingForExits(t *testing.T) {
	// Every press that can give a waiting job somewhere to go out through tells
	// the supervisor so: the retry is an hour here, so the press is the only
	// thing that can start the job within the test.
	cases := []struct {
		name  string
		named bool // the job names its profile rather than running on the default
		press func(t *testing.T, s *Server, job, empty, listed int64)
	}{
		{"the profile is given a list", true, func(t *testing.T, s *Server, _, empty, _ int64) {
			postForm(t, s, proxiesAt, url.Values{
				"profile": {strconv.FormatInt(empty, 10)}, "profile_name": {"Empty"},
				"source": {"file"}, "source_at": {"list.txt"},
			})
		}},
		{"another profile is made the default", false, func(t *testing.T, s *Server, _, _, listed int64) {
			postForm(t, s, proxiesAt+"/default", url.Values{profileField: {strconv.FormatInt(listed, 10)}})
		}},
		{"the empty profile is removed", true, func(t *testing.T, s *Server, _, empty, _ int64) {
			postForm(t, s, proxiesAt+"/delete", url.Values{profileField: {strconv.FormatInt(empty, 10)}})
		}},
		{"the job is pointed at another profile", true, func(t *testing.T, s *Server, job, _, listed int64) {
			postForm(t, s, "/api/reshape", url.Values{"job": {strconv.FormatInt(job, 10)},
				profileField: {strconv.FormatInt(listed, 10)}})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, st, x, _, empty, listed := exitlessSupervisor(t, time.Hour)
			s, err := New(Config{Store: st, Supervisor: v, Logger: quiet()})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			on := int64(0)
			if c.named {
				on = empty
			}
			id := enqueueOn(t, v, on, "a")
			waitUntil(t, "the job is waiting for exits", func() bool { _, ok := v.WaitingForExits(id); return ok })
			c.press(t, s, id, empty, listed)
			waitUntilRunning(t, v, id)
			x.pools.let(t, 1)
		})
	}
}

func queued(v *Supervisor, id int64) bool {
	for _, q := range v.Queued() {
		if q == id {
			return true
		}
	}
	return false
}

func running(v *Supervisor, id int64) bool {
	got, ok := v.Running()
	return ok && got == id
}
