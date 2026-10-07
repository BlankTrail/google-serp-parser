// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/google"
)

// completionOrigin answers the completion address the way the gws-wiz-serp
// client is answered: for "<key> <letter>" it offers "<key> <letter>1" and
// "<key> shared", so every key has one completion all its questions repeat.
// refuse, when set, is asked for every request and answers it with a rate
// limit instead.
type completionOrigin struct {
	*httptest.Server
	mu     sync.Mutex
	asked  map[string]int
	refuse func(n int) bool
	hold   time.Duration
	// echo, when set, also hands every question back with a word after it,
	// as Google does for a letter typed on its own.
	echo    bool
	count   atomic.Int64
	at      atomic.Int64
	busiest atomic.Int64
}

func newCompletionOrigin(t *testing.T) *completionOrigin {
	t.Helper()
	o := &completionOrigin{asked: map[string]int{}}
	o.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/complete/search" {
			http.NotFound(w, r)
			return
		}
		n := int(o.count.Add(1))
		now := o.at.Add(1)
		defer o.at.Add(-1)
		for {
			b := o.busiest.Load()
			if now <= b || o.busiest.CompareAndSwap(b, now) {
				break
			}
		}
		if o.hold > 0 {
			time.Sleep(o.hold)
		}
		q := r.URL.Query()
		o.mu.Lock()
		o.asked[q.Get("pq")]++
		o.mu.Unlock()
		if o.refuse != nil && o.refuse(n) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		typed := strings.TrimSpace(q.Get("q"))
		key := q.Get("pq")
		back := ""
		if o.echo {
			back = "[\"" + typed + " tail\",0,[512]],"
		}
		_, _ = w.Write([]byte(")]}'\n[[" + back + "[\"" + typed + "\\u003cb\\u003e1\\u003c/b\\u003e\",0,[512]],[\"" + key +
			" shared\",0,[512]]],{}]"))
	}))
	t.Cleanup(o.Close)
	return o
}

func TestRunner_AsksEveryQuestionOfEveryKeyAndKeepsEachCompletionOnce(t *testing.T) {
	o := newCompletionOrigin(t)
	f := poolFacing(t, o.Listener.Addr().String(), 3)
	r := &Runner{Pool: f.Pool, Threads: 3}
	keys := []google.Query{usQuery("coffee"), usQuery("tea pot")}
	rep := r.Run(context.Background(), Job{Kind: Suggest, Queries: keys, Tries: 3})

	if rep.Done != 2 || rep.Failed != 0 {
		t.Fatalf("report says %d done and %d failed, want both keys done", rep.Done, rep.Failed)
	}
	alphabet := google.SuggestAlphabet("en")
	for i, q := range keys {
		want := len(google.SuggestVariants(q.Text, alphabet, false))
		o.mu.Lock()
		asked := o.asked[q.Text]
		o.mu.Unlock()
		if asked != want {
			t.Errorf("key %q: %d questions asked, want the generator's %d", q.Text, asked, want)
		}
		got := rep.Results[i].Pages
		if len(got) != 1 {
			t.Fatalf("key %q settled with %d pages, want one", q.Text, len(got))
		}
		shared := 0
		seen := map[string]bool{}
		for n, res := range got[0].Results {
			if seen[res.Title] {
				t.Errorf("key %q kept %q twice", q.Text, res.Title)
			}
			seen[res.Title] = true
			if res.Title == q.Text+" shared" {
				shared++
			}
			if res.Position != n+1 {
				t.Errorf("completion %d of %q is numbered %d", n+1, q.Text, res.Position)
			}
			if strings.Contains(res.Title, "<b>") {
				t.Errorf("completion %q kept its markup", res.Title)
			}
		}
		if shared != 1 {
			t.Errorf("key %q kept the completion every question repeats %d times, want once", q.Text, shared)
		}
	}
}

func TestRunner_TakesARefusedCompletionQuestionToAnotherAddress(t *testing.T) {
	// Every third request is a rate limit. A question refused once is asked
	// again through another address, and the key comes out whole.
	o := newCompletionOrigin(t)
	o.refuse = func(n int) bool { return n%3 == 0 }
	f := poolFacing(t, o.Listener.Addr().String(), 3)
	r := &Runner{Pool: f.Pool, Threads: 2}
	rep := r.Run(context.Background(), Job{Kind: Suggest, Queries: []google.Query{usQuery("coffee")}, Tries: 5, SuggestLimit: 12})

	if rep.Done != 1 {
		t.Fatalf("report says %d done, want the key done", rep.Done)
	}
	if o.count.Load() <= 12 {
		t.Fatalf("%d requests for twelve questions, want refusals among them", o.count.Load())
	}
	// Twelve questions, each answered "<what was typed>1": the first three
	// type the key itself and come to one, the nine with a letter to nine more,
	// and the one every question repeats makes eleven.
	if got := len(rep.Results[0].Pages[0].Results); got != 11 {
		t.Errorf("%d completions kept, want eleven: none lost to a refusal", got)
	}
}

func TestRunner_SpreadsOneKeysQuestionsOverTheThreads(t *testing.T) {
	// One key and four threads: the questions are the work, not the key, so
	// more than one is in flight at once.
	o := newCompletionOrigin(t)
	o.hold = 20 * time.Millisecond
	f := poolFacing(t, o.Listener.Addr().String(), 4)
	r := &Runner{Pool: f.Pool, Threads: 4}
	rep := r.Run(context.Background(), Job{Kind: Suggest, Queries: []google.Query{usQuery("coffee")}, SuggestLimit: 16})
	if rep.Done != 1 {
		t.Fatalf("report says %d done", rep.Done)
	}
	if got := o.busiest.Load(); got < 2 {
		t.Errorf("at most %d question in flight at once, want the threads sharing one key's questions", got)
	}
}

func TestSuggestion_FailsOnlyAKeyNoQuestionOfWhichWasAnswered(t *testing.T) {
	k := &suggestion{last: errFake}
	if _, err := k.settled("k", nil); err == nil {
		t.Error("a key with no answered question settled without an error")
	}
	k.answered = 1
	if pages, err := k.settled("k", nil); err != nil || len(pages) != 1 {
		t.Errorf("a key with one answered question settled %v, %v; want one page and no error", pages, err)
	}
}

var errFake = errorString("refused")

type errorString string

func (e errorString) Error() string { return string(e) }

func TestRunner_LeavesAKeyStoppedHalfwayUntried(t *testing.T) {
	// Stopped part way through a key's questions, the key is left as it was
	// found — untried, to be asked whole by the next run — rather than filed as
	// done with half its completions.
	o := newCompletionOrigin(t)
	o.hold = 30 * time.Millisecond
	f := poolFacing(t, o.Listener.Addr().String(), 2)
	r := &Runner{Pool: f.Pool, Threads: 2}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	rep := r.Run(ctx, Job{Kind: Suggest, Queries: []google.Query{usQuery("coffee")}})
	if o.count.Load() == 0 {
		t.Fatal("nothing was asked before the stop")
	}
	if got := rep.Results[0]; got.Attempted || got.Pages != nil {
		t.Errorf("the key stopped halfway came back attempted %v with %d pages, want untried and empty",
			got.Attempted, len(got.Pages))
	}
}

func TestAttempt_AsksACompletionQuestionAgainOnceThePortHasGivenUp(t *testing.T) {
	// The first three requests are rate limits: the port's own ladder asks
	// once more and gives up, and the question is taken to another address
	// rather than lost.
	o := newCompletionOrigin(t)
	o.refuse = func(n int) bool { return n <= 3 }
	f := poolFacing(t, o.Listener.Addr().String(), 3)
	a := &Attempt{Pool: f.Pool, Tries: 3}
	got, err := a.Complete(context.Background(), usQuery("coffee"),
		google.SuggestVariant{Key: "coffee", Text: "coffee a", Cursor: 1})
	if err != nil || len(got) != 2 {
		t.Fatalf("completions %q, %v; want the two of the answer that came through", got, err)
	}
	if n := o.count.Load(); n < 4 {
		t.Errorf("%d requests, want the question asked past three refusals", n)
	}
}

func TestRunner_ToldOfEveryAnsweredCompletionQuestion(t *testing.T) {
	// The screen counts a completions job's requests a minute off what it is
	// told comes back: one for every substitution answered, and none of them
	// carrying completions, which are counted once, when the key is written.
	o := newCompletionOrigin(t)
	f := poolFacing(t, o.Listener.Addr().String(), 2)
	r := &Runner{Pool: f.Pool, Threads: 2}
	var told, carried atomic.Int64
	rep := r.Run(context.Background(), Job{Kind: Suggest, Queries: []google.Query{usQuery("coffee")}, SuggestLimit: 20,
		Captured: func(p google.SERP) {
			told.Add(1)
			carried.Add(int64(len(p.Results)))
		}})
	if rep.Done != 1 {
		t.Fatalf("report says %d done", rep.Done)
	}
	if told.Load() != 20 || carried.Load() != 0 {
		t.Errorf("told of %d answers carrying %d completions, want 20 carrying none", told.Load(), carried.Load())
	}
}

func TestRunner_LeavesOutTheQuestionHandedBack(t *testing.T) {
	// Google hands a letter typed on its own straight back — "tea b pot tail"
	// for "tea b pot" — and that is the question, not a completion of the key.
	// A letter that is a word, "tea a pot tail", is kept, and so is everything
	// a question without a letter brings.
	o := newCompletionOrigin(t)
	o.echo = true
	f := poolFacing(t, o.Listener.Addr().String(), 3)
	r := &Runner{Pool: f.Pool, Threads: 3}
	rep := r.Run(context.Background(), Job{Kind: Suggest, Queries: []google.Query{usQuery("tea pot")}, Tries: 3, Multiword: true})
	if rep.Done != 1 || len(rep.Results[0].Pages) != 1 {
		t.Fatalf("report says %d done, want the key done with one page", rep.Done)
	}
	kept := map[string]bool{}
	for _, res := range rep.Results[0].Pages[0].Results {
		kept[res.Title] = true
	}
	for _, echo := range []string{"tea b pot tail", "tea pot b tail", "b tea pot tail"} {
		if kept[echo] {
			t.Errorf("%q was kept: the letter b typed on its own came back where it was typed", echo)
		}
	}
	for _, want := range []string{"tea a pot tail", "tea pot a tail", "a tea pot tail", "tea pot tail", "tea pot shared", "tea potb1"} {
		if !kept[want] {
			t.Errorf("%q was left out, want it kept", want)
		}
	}
}

func TestSuggestion_MarksACompletionWithNothingOfItsKey(t *testing.T) {
	// Marked, not left out: the completion about something else is still on
	// the page, and only it carries the mark.
	k := &suggestion{answered: 1, found: []string{"coffee maker app", "kafka on the shore"}}
	pages, err := k.settled("coffee maker", nil)
	if err != nil || len(pages) != 1 || len(pages[0].Results) != 2 {
		t.Fatalf("settled to %v, %v; want one page of both completions", pages, err)
	}
	for _, r := range pages[0].Results {
		if want := r.Title == "kafka on the shore"; r.Offtopic != want {
			t.Errorf("%q marked %v, want %v", r.Title, r.Offtopic, want)
		}
	}
}

func TestSuggestion_ScoresEachCompletionWhenAModelIsThere(t *testing.T) {
	k := &suggestion{answered: 1, found: []string{"coffee maker app", "kafka on the shore"}}
	asked := 0
	similar := func(_ string, cs []string) []float32 {
		asked++
		out := make([]float32, len(cs))
		for i, s := range cs {
			out[i] = 0.1
			if s == "coffee maker app" {
				out[i] = 0.8
			}
		}
		return out
	}
	pages, err := k.settled("coffee maker", similar)
	if asked != 1 {
		t.Errorf("the scorer was asked %d times for one key, want once with all its completions", asked)
	}
	if err != nil || len(pages) != 1 {
		t.Fatalf("settled to %v, %v", pages, err)
	}
	for _, r := range pages[0].Results {
		want := map[string]float32{"coffee maker app": 0.8, "kafka on the shore": 0.1}[r.Title]
		if !r.Scored || r.Similarity != want {
			t.Errorf("%q scored %v (%v), want %v", r.Title, r.Similarity, r.Scored, want)
		}
	}
	plain, _ := k.settled("coffee maker", nil)
	for _, r := range plain[0].Results {
		if r.Scored {
			t.Errorf("%q was scored with no model", r.Title)
		}
	}
}

func TestSuggestion_ACompletionScoredNoughtWasStillScored(t *testing.T) {
	// Nought is a measurement. Telling a scored suggestion from an unscored one
	// by the number would read every unrelated phrase as never measured.
	k := &suggestion{answered: 1, found: []string{"coffee maker app"}}
	pages, _ := k.settled("coffee maker", func(_ string, cs []string) []float32 { return make([]float32, len(cs)) })
	if r := pages[0].Results[0]; !r.Scored || r.Similarity != 0 {
		t.Errorf("a score of nought read back as %v (%v)", r.Similarity, r.Scored)
	}
}

func TestRunner_HandsTheJobsScorerToEveryKeyItSettles(t *testing.T) {
	// The scorer is read off the job in the worker that settles a key, a long
	// way from where the job is built: a job whose Similar never got there
	// would collect every suggestion unmeasured and nothing would say so.
	o := newCompletionOrigin(t)
	f := poolFacing(t, o.Listener.Addr().String(), 2)
	r := &Runner{Pool: f.Pool, Threads: 2}
	one := func(s string) float32 { return float32(len(s)) / 100 }
	similar := func(_ string, cs []string) []float32 {
		out := make([]float32, len(cs))
		for i, s := range cs {
			out[i] = one(s)
		}
		return out
	}
	rep := r.Run(context.Background(), Job{Kind: Suggest, Queries: []google.Query{usQuery("coffee")}, Tries: 3, Similar: similar})
	if rep.Done != 1 || len(rep.Results[0].Pages) != 1 {
		t.Fatalf("the key did not settle: %+v", rep)
	}
	for _, res := range rep.Results[0].Pages[0].Results {
		if !res.Scored || res.Similarity != one(res.Title) {
			t.Errorf("%q came back with %v (%v), want the job's score", res.Title, res.Similarity, res.Scored)
		}
	}
	plain := (&Runner{Pool: f.Pool, Threads: 2}).Run(context.Background(),
		Job{Kind: Suggest, Queries: []google.Query{usQuery("coffee")}, Tries: 3})
	for _, res := range plain.Results[0].Pages[0].Results {
		if res.Scored {
			t.Errorf("%q was scored on a job with no scorer", res.Title)
		}
	}
}

func TestSuggestion_AScorerThatAnswersForOtherCompletionsIsNotKept(t *testing.T) {
	// One score fewer than completions is a scorer that scored something else:
	// kept, every completion after the gap would carry its neighbour's number.
	k := &suggestion{answered: 1, found: []string{"coffee maker app", "coffee maker sale"}}
	pages, err := k.settled("coffee maker", func(string, []string) []float32 { return []float32{0.5} })
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range pages[0].Results {
		if r.Scored {
			t.Errorf("%q was kept as scored %v from a scorer that answered for one of two", r.Title, r.Similarity)
		}
	}
}
