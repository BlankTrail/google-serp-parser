// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
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

	"github.com/blanktrail/google-serp-parser/internal/settings"
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
	// The job is the one running from the moment it is taken to be asked for
	// again, before its pool is up, so it is waited for until the pool has gone
	// up and the mark is taken off; a mark that never goes is the failure.
	x.exits.Store(true)
	waitUntil(t, "the job's pool went up and it no longer waits for exits", func() bool {
		got, running := v.Running()
		_, waiting := v.WaitingForExits(id)
		return running && got == id && !waiting
	})
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
		LangEN: {says: []string{"no exits", "search jobs wait", "Proxies", "suggestion jobs go out directly"},
			never: []string{"own address"}},
		LangRU: {says: []string{"не указаны выходы", "поисковые задания ждут", "Прокси", "подсказок идут напрямую"},
			never: []string{"с адреса этой машины"}},
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

func TestSupervisor_ReadsAWaitingJobsPlanBackOnlyOnceItsPoolIsUp(t *testing.T) {
	// A job waiting for exits is asked for again every few seconds. Reading its
	// plan back each time loads every query it has left and says so in the log;
	// it is read once when the job is first taken, and again only when its pool
	// has gone up.
	const retry = 40 * time.Millisecond
	v, st, x, said, empty, _ := exitlessSupervisor(t, retry)
	id := enqueueOn(t, v, empty, "a", "b")
	waitUntil(t, "the job is waiting for exits", func() bool { _, ok := v.WaitingForExits(id); return ok })
	waitUntil(t, "the job was asked for again a few times", func() bool { asked, _ := x.asked(); return asked >= 5 })
	if n := strings.Count(said.String(), "was read back"); n != 1 {
		t.Errorf("the plan was read back %d times while the job waited, want once:\n%s", n, said.String())
	}

	x.exits.Store(true)
	waitUntilRunning(t, v, id)
	x.pools.let(t, 2)
	waitUntil(t, "the job is done", func() bool { return progress(t, st, id).Finished })
	if got := progress(t, st, id).Done; got != 2 {
		t.Errorf("the job recorded %d queries once its pool was up, want both", got)
	}
	// Once up, the pool is the one the job runs on: not raised a second time
	// on the way through the plan.
	if up := x.pools.up(); up != 1 {
		t.Errorf("%d pools were raised for the job, want one", up)
	}
}

func TestJobPage_OffersTheStopAllTheWhileAJobWaitsForExits(t *testing.T) {
	// The job is taken out of the queue for an instant every few seconds to be
	// asked for again; a stop that came and went with that would be one nobody
	// could press.
	v, st, _, _, empty, _ := exitlessSupervisor(t, 30*time.Millisecond)
	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := enqueueOn(t, v, empty, "a")
	waitUntil(t, "the job is waiting for exits", func() bool { _, ok := v.WaitingForExits(id); return ok })
	for i := 0; i < 20; i++ {
		if body := get(t, s, jobPath(id)).Body.String(); !strings.Contains(body, `action="/api/stop"`) {
			t.Fatalf("drawing %d of a job waiting for exits offers no stop", i)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStop_TakesAJobWaitingForExitsOutOfTheQueue(t *testing.T) {
	// The wait has no end of its own, so it must have a way out other than
	// giving the profile exits. A stop takes the job out as a stop leaves any
	// job: unstarted, unfinished, there to be carried on. The retry is an hour,
	// so the job is in the queue, not running, when the stop arrives.
	v, st, x, _, empty, _ := exitlessSupervisor(t, time.Hour)
	id := enqueueOn(t, v, empty, "a")
	waitUntil(t, "the job is waiting for exits", func() bool { _, ok := v.WaitingForExits(id); return ok })
	waitUntil(t, "the job is back in the queue", func() bool { return queued(v, id) })

	if err := v.Stop(id); err != nil {
		t.Fatalf("Stop of a job waiting for exits: %v", err)
	}
	if queued(v, id) || running(v, id) {
		t.Error("the stopped job is still queued or running")
	}
	if _, ok := v.WaitingForExits(id); ok {
		t.Error("a stopped job is still reported as waiting for exits")
	}
	asked, _ := x.asked()
	v.ExitsChanged()
	time.Sleep(150 * time.Millisecond)
	if again, _ := x.asked(); again != asked {
		t.Errorf("a stopped job was asked for %d more times", again-asked)
	}
	sum := progress(t, st, id)
	if sum.Finished || sum.Pending != 1 {
		t.Errorf("finished=%v pending=%d, want an unfinished job with its query left", sum.Finished, sum.Pending)
	}
}

func TestDelete_RemovesAJobWaitingForExits(t *testing.T) {
	// A job waiting for exits has had nothing done to it; refusing to delete it
	// "because it is running or waiting to" would leave it in the list for as
	// long as its profile is empty.
	v, st, _, _, empty, _ := exitlessSupervisor(t, time.Hour)
	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := enqueueOn(t, v, empty, "a")
	waitUntil(t, "the job is waiting for exits", func() bool { _, ok := v.WaitingForExits(id); return ok })
	if rec := postForm(t, s, "/api/delete", url.Values{"job": {strconv.FormatInt(id, 10)}}); rec.Code >= 400 {
		t.Fatalf("deleting a job waiting for exits answered %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := st.Progress(context.Background(), id); !errors.Is(err, store.ErrNoJob) {
		t.Errorf("the job is still in the history after the delete: %v", err)
	}
	if queued(v, id) {
		t.Error("the deleted job is still in the queue")
	}
}

func TestReconnect_AsksAtOnceForAJobWaitingForExits(t *testing.T) {
	// A connection saved in the browser may be what the job was missing; the
	// retry is an hour, so only the reconnect can be what asks again.
	v, _, _, _, empty, _ := exitlessSupervisor(t, time.Hour)
	id := enqueueOn(t, v, empty, "a")
	waitUntil(t, "the job is waiting for exits", func() bool { _, ok := v.WaitingForExits(id); return ok })
	var calls atomic.Int32
	if err := v.Reconnect(func(context.Context, Wanted) (Identities, error) {
		calls.Add(1)
		return Identities{}, fmt.Errorf("%w: proxy profile %q", ErrNoExits, "Empty")
	}); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	waitUntil(t, "the waiting job is asked for through the new connection", func() bool { return calls.Load() >= 1 })
}

func TestSaveSettings_SavesAndReconnectsWhileTheDefaultProfileNamesNoExits(t *testing.T) {
	// The identities kept warm cannot be opened on a default profile with no
	// exits, and that is no fault in the settings: refusing the save over it
	// left a new key or address unwritten and never handed to the queue.
	s, path := serverWithSettings(t, settings.Settings{
		ControlURL: "http://127.0.0.1:1", APIKey: "keep-me",
	})
	st := testStore(t)
	v := newSupervisor(st, nil)
	t.Cleanup(func() { _ = v.Close() })
	s.sup = v
	opener := &stubConnect{}
	s.connect = opener.open
	var asked atomic.Int32
	s.standing = func(context.Context, settings.Settings) error {
		asked.Add(1)
		return fmt.Errorf("%w: proxy profile %q", ErrNoExits, "Default")
	}

	postForm(t, s, settingsAt, url.Values{
		"control_url": {"http://127.0.0.1:2"}, "api_key": {""}, "hot_ports": {"4"},
	})
	if asked.Load() != 1 {
		t.Fatalf("the warm identities were asked for %d times, want once", asked.Load())
	}
	if got := loaded(t, path); got.ControlURL != "http://127.0.0.1:2" || got.HotPorts != 4 {
		t.Errorf("the file holds %s with %d warm, want the address and the 4 just saved",
			got.ControlURL, got.HotPorts)
	}
	enqueueSized(t, v, "after the save", 2, 1, "a")
	waitUntil(t, "the job to have asked for a pool through the saved connection", func() bool {
		built, asked := opener.asked()
		return asked && built.ControlURL == "http://127.0.0.1:2"
	})
}
