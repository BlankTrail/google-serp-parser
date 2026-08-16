// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/google"
	"github.com/blanktrail/google-serp-parser/run"
)

// What answers a search here has to be what the runner takes a query to, or
// this interface is asking something other than the program does.
var _ google.Searcher = (*run.Attempt)(nil)

// searchStub stands where the identities go.
//
// It answers however a test tells it to and counts how many searches are inside
// it at the same moment, which is the only way to ask whether the limiter
// limits anything. Its own counters are guarded because the whole point of it
// is being called from several goroutines at once.
type searchStub struct {
	answer func(ctx context.Context, q google.Query) (google.SERP, error)

	mu     sync.Mutex
	inside int
	most   int
	calls  int
	asked  []google.Query
}

func (f *searchStub) Search(ctx context.Context, q google.Query) (google.SERP, error) {
	f.enter(q)
	defer f.leave()
	return f.answer(ctx, q)
}

func (f *searchStub) enter(q google.Query) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.asked = append(f.asked, q)
	f.inside++
	if f.inside > f.most {
		f.most = f.inside
	}
}

func (f *searchStub) leave() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inside--
}

// count is how many searches were asked for and the most that were under way at
// once.
func (f *searchStub) count() (calls, most int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.most
}

// query is the nth query the stub was asked for.
func (f *searchStub) query(n int) google.Query {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.asked) {
		return google.Query{}
	}
	return f.asked[n]
}

// answering is a stub that hands the same page back at once.
func answering(serp google.SERP) *searchStub {
	return &searchStub{answer: func(context.Context, google.Query) (google.SERP, error) {
		return serp, nil
	}}
}

// failing is a stub that never manages to answer.
func failing(err error) *searchStub {
	return &searchStub{answer: func(context.Context, google.Query) (google.SERP, error) {
		return google.SERP{}, err
	}}
}

// held is a stub that stays inside the search until the returned channel is
// closed, and signals on entered as each search begins. It is how a test holds
// a place in the limiter without sleeping to guess that it has been taken.
func held() (stub *searchStub, entered chan struct{}, release chan struct{}) {
	entered = make(chan struct{}, 8)
	release = make(chan struct{})
	stub = &searchStub{answer: func(ctx context.Context, _ google.Query) (google.SERP, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return google.SERP{}, ctx.Err()
		}
		return twoResults(), nil
	}}
	return stub, entered, release
}

// twoResults is a page as the parser hands one over: one result whose exact
// address is known and one whose is not, because both arrive in practice.
func twoResults() google.SERP {
	return google.SERP{
		Query: "iphone 13",
		Results: []google.Result{
			{
				Position: 1, Title: "Apple", Host: "apple.test",
				URL: "https://apple.test/iphone", Snippet: "the phone",
			},
			{
				Position: 2, Title: "Shop", Host: "shop.test",
				DisplayPath: "shop.test › phones", Snippet: "for sale",
			},
		},
		Related: []string{"iphone 14"},
	}
}

// searchServer builds a server that searches the way a test says and a key that
// works.
func searchServer(t *testing.T, cfg SearchConfig) (*Server, string) {
	t.Helper()
	st := testStore(t)
	s, err := New(Config{Store: st, Logger: quiet(), Search: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, issue(t, st, "nightly")
}

// searchBody is a search as the wire carries it. The names are written out here
// so that renaming one fails a test rather than somebody's program.
type searchBody struct {
	Query   string  `json:"query"`
	Took    float64 `json:"took_seconds"`
	Results []struct {
		Position int    `json:"position"`
		Title    string `json:"title"`
		URL      string `json:"url"`
		Host     string `json:"host"`
		Snippet  string `json:"snippet"`
		Resolved bool   `json:"resolved"`
	} `json:"results"`
}

func TestSearch_AnswersTheResultsInTheReplyToTheRequestThatAskedForThem(t *testing.T) {
	// This is the whole reason the endpoint exists: somebody else's program
	// reads the results out of the answer it is already holding. An answer of
	// "started, ask again later" is the same as no endpoint at all.
	stub := answering(twoResults())
	s, secret := searchServer(t, SearchConfig{Searcher: stub})

	rec := call(t, s, secret, http.MethodGet, "/api/v1/search?q=iphone+13&gl=us&hl=en", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a search answered %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got searchBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("the answer is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if len(got.Results) != 2 {
		t.Fatalf("%d results came back, want 2", len(got.Results))
	}
	if got.Results[0].Position != 1 || got.Results[1].Position != 2 {
		t.Errorf("positions came back as %d and %d, want 1 and 2",
			got.Results[0].Position, got.Results[1].Position)
	}
	if got.Results[0].URL != "https://apple.test/iphone" || !got.Results[0].Resolved {
		t.Errorf("the first result came back as %+v, want the address it was parsed with",
			got.Results[0])
	}
	if got.Results[1].Resolved {
		t.Error("a result with no exact address came back saying it had one")
	}
	asked := stub.query(0)
	if asked.Text != "iphone 13" || asked.Country != "us" || asked.Language != "en" {
		t.Errorf("the search was made as %+v, want the query, country and language asked for", asked)
	}
}

func TestSearch_RefusesAtOnceWhenAsManySearchesAreUnderWayAsAreAllowed(t *testing.T) {
	// Holding the caller instead would spend the wait and then hand back a
	// place they have already timed out of on their side.
	// The deadline is named so that a server which holds the second caller
	// instead of refusing them fails here in seconds rather than sitting out the
	// minute it allows a search by default.
	stub, entered, release := held()
	s, secret := searchServer(t, SearchConfig{Searcher: stub, AtOnce: 1, Deadline: 5 * time.Second})

	first := make(chan int, 1)
	go func() {
		first <- call(t, s, secret, http.MethodGet, "/api/v1/search?q=one", "").Code
	}()
	<-entered

	began := time.Now()
	rec := call(t, s, secret, http.MethodGet, "/api/v1/search?q=two", "")
	waited := time.Since(began)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("a search over the limit answered %d, want 429 (body %q)", rec.Code, rec.Body.String())
	}
	if waited > 2*time.Second {
		t.Errorf("the refusal took %s: the caller was held rather than refused", waited)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if _, named := body["error"].(string); !named {
		t.Errorf("the refusal carries no error field: %v", body)
	}
	if calls, _ := stub.count(); calls != 1 {
		t.Errorf("%d searches were made, want 1: the refused one took an identity anyway", calls)
	}

	close(release)
	if code := <-first; code != http.StatusOK {
		t.Errorf("the search that was under way answered %d, want 200", code)
	}
}

func TestSearch_AllowsOneSearchPerIdentityWhenNobodySaysOtherwise(t *testing.T) {
	// The pool is what a search competes for. A limit set above the number of
	// identities is not a limit, and one set by hand is the operator's business.
	stub, entered, release := held()
	s, secret := searchServer(t, SearchConfig{Searcher: stub, Ports: 2, Deadline: 5 * time.Second})
	defer close(release)

	going := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			going <- call(t, s, secret, http.MethodGet, "/api/v1/search?q=one", "").Code
		}()
	}
	<-entered
	<-entered

	rec := call(t, s, secret, http.MethodGet, "/api/v1/search?q=three", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("the third search against two identities answered %d, want 429", rec.Code)
	}
}

func TestSearch_SaysTheSearchRanOutOfTimeRatherThanHoldingTheCallerForever(t *testing.T) {
	// A request that hangs is indistinguishable from a hung program to whoever
	// is waiting on it, and reaching an identity that answers can take minutes.
	stub := &searchStub{answer: func(ctx context.Context, _ google.Query) (google.SERP, error) {
		select {
		case <-ctx.Done():
			return google.SERP{}, ctx.Err()
		case <-time.After(5 * time.Second):
			// The deadline never arrived. Answering as if all were well is what
			// makes the check below fail rather than the test hang.
			return twoResults(), nil
		}
	}}
	s, secret := searchServer(t, SearchConfig{Searcher: stub, Deadline: 50 * time.Millisecond})

	began := time.Now()
	rec := call(t, s, secret, http.MethodGet, "/api/v1/search?q=one", "")
	took := time.Since(began)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("a search that ran out of time answered %d, want 504 (body %q)",
			rec.Code, rec.Body.String())
	}
	if took > 4*time.Second {
		t.Errorf("the answer took %s: the deadline the server was given did not apply", took)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v (body %q)", err, rec.Body.String())
	}
	said, _ := body["error"].(string)
	if !strings.Contains(said, "did not finish") {
		t.Errorf("the answer says %q, which does not tell a reader the search was unfinished rather than refused", said)
	}
}

func TestSearch_GoesOnServingAfterASearchFails(t *testing.T) {
	// A limiter that gives a place back only when the search worked loses one
	// per failure until it holds none, and nothing anywhere says so.
	stub := failing(errors.New("nothing came back"))
	s, secret := searchServer(t, SearchConfig{Searcher: stub, AtOnce: 1})

	for i := 0; i < 3; i++ {
		rec := call(t, s, secret, http.MethodGet, "/api/v1/search?q=one", "")
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("search %d was refused as too many at once after %d failures: a place was kept",
				i+1, i)
		}
		if rec.Code != http.StatusBadGateway {
			t.Errorf("a search that failed answered %d, want 502 (body %q)", rec.Code, rec.Body.String())
		}
	}
}

func TestSearch_GoesOnServingAfterASearchPanics(t *testing.T) {
	// A panic unwinds past the handler, and a place given back on the way out
	// of the function is given back then too. A place is lost otherwise, and
	// the loss is silent until the limiter holds none at all.
	stub := &searchStub{answer: func(context.Context, google.Query) (google.SERP, error) {
		panic("the search fell over")
	}}
	s, secret := searchServer(t, SearchConfig{Searcher: stub, AtOnce: 1})

	for i := 0; i < 3; i++ {
		reached := panicked(t, func() {
			call(t, s, secret, http.MethodGet, "/api/v1/search?q=one", "")
		})
		if !reached {
			t.Fatalf("search %d never reached the identities: the place taken by the panic was kept", i+1)
		}
	}
	if calls, _ := stub.count(); calls != 3 {
		t.Errorf("%d searches were made, want 3", calls)
	}
}

// panicked reports whether fn panicked, swallowing the panic so the test can
// carry on asking questions of the server it came out of.
func panicked(t *testing.T, fn func()) (did bool) {
	t.Helper()
	defer func() {
		did = recover() != nil
	}()
	fn()
	return false
}

func TestSearch_RefusesARequestThatNamesNothingToSearchFor(t *testing.T) {
	stub := answering(twoResults())
	s, secret := searchServer(t, SearchConfig{Searcher: stub})

	for _, target := range []string{"/api/v1/search", "/api/v1/search?q=", "/api/v1/search?q=%20%20"} {
		rec := call(t, s, secret, http.MethodGet, target, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400 (body %q)", target, rec.Code, rec.Body.String())
		}
	}
	if calls, _ := stub.count(); calls != 0 {
		t.Errorf("%d searches were made for a request that named no query, want 0", calls)
	}
}

func TestSearch_SaysSoWhenTheServerWasStartedWithNothingToSearchFrom(t *testing.T) {
	// A reader of a history on another machine has no identities behind it. It
	// should say what it cannot do rather than report a fault it does not have.
	s, secret := searchServer(t, SearchConfig{})
	rec := call(t, s, secret, http.MethodGet, "/api/v1/search?q=one", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a server with nothing to search from answered %d, want 503 (body %q)",
			rec.Code, rec.Body.String())
	}
}

func TestSearch_IsBehindTheKeyCheckLikeEverythingElse(t *testing.T) {
	stub := answering(twoResults())
	s, _ := searchServer(t, SearchConfig{Searcher: stub})

	rec := call(t, s, "", http.MethodGet, "/api/v1/search?q=one", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a search with no key answered %d, want 401", rec.Code)
	}
	if calls, _ := stub.count(); calls != 0 {
		t.Errorf("%d searches were made without a key, want 0", calls)
	}
}

func TestSearch_NeverHasMoreSearchesUnderWayThanItAllows(t *testing.T) {
	// The count is what protects a running job from being left with no identity
	// at all, so what matters is the most that were ever inside at once.
	const places, askers = 3, 24
	stub := &searchStub{answer: func(context.Context, google.Query) (google.SERP, error) {
		time.Sleep(2 * time.Millisecond)
		return twoResults(), nil
	}}
	s, secret := searchServer(t, SearchConfig{Searcher: stub, AtOnce: places})

	codes := make(chan int, askers)
	var wg sync.WaitGroup
	for i := 0; i < askers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- call(t, s, secret, http.MethodGet, "/api/v1/search?q=one", "").Code
		}()
	}
	wg.Wait()
	close(codes)

	for code := range codes {
		if code != http.StatusOK && code != http.StatusTooManyRequests {
			t.Fatalf("a search under load answered %d, want 200 or 429", code)
		}
	}
	if _, most := stub.count(); most > places {
		t.Errorf("%d searches were under way at once, want no more than %d", most, places)
	}

	// Every place has to be back afterwards, or the limiter shrinks with use.
	for i := 0; i < places; i++ {
		if rec := call(t, s, secret, http.MethodGet, "/api/v1/search?q=after", ""); rec.Code != http.StatusOK {
			t.Fatalf("after the load, search %d answered %d, want 200: places were not given back",
				i+1, rec.Code)
		}
	}
}

func TestSearchLimit_HoldsAsManyPlacesAsItWasBuiltWithAndNoMore(t *testing.T) {
	limit := NewSearchLimit(2)
	first, err := limit.Take()
	if err != nil {
		t.Fatalf("the first place: %v", err)
	}
	if _, err := limit.Take(); err != nil {
		t.Fatalf("the second place: %v", err)
	}
	if _, err := limit.Take(); !errors.Is(err, ErrTooBusy) {
		t.Errorf("a third place was handed out of two: %v", err)
	}
	first()
	if _, err := limit.Take(); err != nil {
		t.Errorf("a place given back was not free again: %v", err)
	}
}

func TestSearchLimit_GivingOnePlaceBackTwiceDoesNotFreeSomebodyElses(t *testing.T) {
	// The second call has to come while another search holds the place, which is
	// the only arrangement in which giving one back twice can do any harm — and
	// the harm is two searches holding what the limiter counts as one, which is
	// the same failure as having no limiter and quieter to find.
	limit := NewSearchLimit(1)
	mine, err := limit.Take()
	if err != nil {
		t.Fatalf("the only place: %v", err)
	}
	mine()

	if _, err := limit.Take(); err != nil {
		t.Fatalf("the place was not free after being given back: %v", err)
	}
	mine()

	if _, err := limit.Take(); !errors.Is(err, ErrTooBusy) {
		t.Errorf("giving a place back twice freed the one another search was holding: %v", err)
	}
}
