// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/google"
)

// serpAPIPage is a captured page as the parser hands one over.
//
// Everything awkward about a real one is in it on purpose. The first result
// carries both the destination and Google's redirector, so handing over the
// wrong one of the two is visible. The second carries only a page-relative
// redirector and no destination at all, and it sits in the middle so that
// dropping it leaves both a short list and a gap in the numbering. The page's
// own record of the query differs from what any test asks for, so an answer
// that repeats the page's record instead of the request is visible too. Two of
// the three kinds of paid placement are on it.
func serpAPIPage() google.SERP {
	return google.SERP{
		Query:  "whatever the page said it ran",
		Origin: "https://www.google.test",
		Results: []google.Result{
			{
				Position: 1, Title: "Apple", Host: "apple.test", DisplayPath: "iphone",
				Snippet: "the phone",
				URL:     "https://apple.test/iphone",
				Link:    "https://www.google.test/url?q=https%3A%2F%2Fapple.test%2Fiphone",
				Form:    google.LinkRedirect,
			},
			{
				Position: 2, Title: "Shop", Host: "shop.test", DisplayPath: "phones",
				Snippet: "for sale",
				Link:    "/goto?url=Ciphertext",
				Form:    google.LinkEncrypted,
			},
			{
				Position: 3, Title: "Review", Host: "review.test",
				Snippet: "we tried it",
				URL:     "https://review.test/iphone-13",
				Link:    "https://review.test/iphone-13",
				Form:    google.LinkDirect,
			},
		},
		Ads: []google.Ad{
			{
				Position: 1, Placement: google.PlacementTop, Title: "Buy one",
				Host: "ads.test", URL: "https://ads.test/buy", Snippet: "in stock",
			},
			{Position: 1, Placement: google.PlacementProduct, Title: "iPhone 13 128GB"},
		},
		Related: []string{"iphone 14"},
	}
}

// serpAPIAsk drives the address and hands the answer back as a plain tree of
// names, which is the only way to ask what somebody else's program will find:
// a check against a struct in this package passes just as well when the tag on
// a field has been renamed underneath it.
func serpAPIAsk(t *testing.T, s *Server, secret, target string) (int, map[string]any) {
	t.Helper()
	rec := call(t, s, secret, http.MethodGet, target, "")
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer to %s is not JSON: %v (body %q)", target, err, rec.Body.String())
	}
	return rec.Code, body
}

func objectAt(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	got, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%q is not an object in %v", key, m)
	}
	return got
}

func listAt(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	got, ok := m[key].([]any)
	if !ok {
		t.Fatalf("%q is not a list in %v", key, m)
	}
	return got
}

func nthOf(t *testing.T, list []any, n int) map[string]any {
	t.Helper()
	got, ok := list[n].(map[string]any)
	if !ok {
		t.Fatalf("entry %d is not an object: %v", n, list[n])
	}
	return got
}

func textAt(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	got, ok := m[key].(string)
	if !ok {
		t.Fatalf("%q is not a string in %v", key, m)
	}
	return got
}

func numberAt(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	got, ok := m[key].(float64)
	if !ok {
		t.Fatalf("%q is not a number in %v", key, m)
	}
	return got
}

func TestSerpAPI_AnswersUnderTheNamesSomebodyElsesProgramAlreadyReads(t *testing.T) {
	// This is the whole of the endpoint. A program written against the service
	// this shape copies must find what it looks for after changing one address,
	// so every name is checked as a name and not as a field of a struct here.
	s, secret := searchServer(t, SearchConfig{Searcher: answering(serpAPIPage())})

	code, body := serpAPIAsk(t, s, secret, "/search?q=iphone+13&gl=us&hl=en")
	if code != http.StatusOK {
		t.Fatalf("a search answered %d, want 200 (body %v)", code, body)
	}

	meta := objectAt(t, body, "search_metadata")
	for _, name := range []string{"id", "status", "created_at", "processed_at"} {
		if _, named := meta[name].(string); !named {
			t.Errorf("search_metadata carries no %q: %v", name, meta)
		}
	}
	if _, named := meta["total_time_taken"].(float64); !named {
		t.Errorf("search_metadata carries no total_time_taken: %v", meta)
	}
	if said := textAt(t, meta, "status"); said != "Success" {
		t.Errorf("the search is reported as %q, want %q", said, "Success")
	}

	params := objectAt(t, body, "search_parameters")
	for _, name := range []string{"engine", "q", "google_domain", "gl", "hl", "device"} {
		if _, named := params[name].(string); !named {
			t.Errorf("search_parameters carries no %q: %v", name, params)
		}
	}

	results := listAt(t, body, "organic_results")
	if len(results) != 3 {
		t.Fatalf("%d results came back, want 3: %v", len(results), results)
	}
	first := nthOf(t, results, 0)
	for _, name := range []string{"title", "link", "displayed_link", "snippet"} {
		if _, named := first[name].(string); !named {
			t.Errorf("a result carries no %q: %v", name, first)
		}
	}
	if _, named := first["position"].(float64); !named {
		t.Errorf("a result carries no position: %v", first)
	}

	// The interface's own shape must not have leaked out of this address under
	// its own names, which is what a rename in either direction looks like.
	for _, ours := range []string{"results", "took_seconds", "query"} {
		if _, there := body[ours]; there {
			t.Errorf("the answer carries %q, which is this program's name and not theirs: %v", ours, body)
		}
	}
}

func TestSerpAPI_NumbersEveryResultFromOneInPageOrderWithNoGaps(t *testing.T) {
	// A caller reads the ranking off these numbers. Starting from zero shifts
	// every rank by one, and a gap says a place on the page belongs to a result
	// nobody was shown.
	s, secret := searchServer(t, SearchConfig{Searcher: answering(serpAPIPage())})

	_, body := serpAPIAsk(t, s, secret, "/search?q=iphone+13")
	results := listAt(t, body, "organic_results")
	if len(results) != 3 {
		t.Fatalf("%d results came back, want 3", len(results))
	}
	for i := range results {
		if got := numberAt(t, nthOf(t, results, i), "position"); got != float64(i+1) {
			t.Errorf("result %d is at position %v, want %d", i, got, i+1)
		}
	}
}

func TestSerpAPI_HandsOverGooglesRedirectorForAResultThePageCarriedNoAddressFor(t *testing.T) {
	// The three ways Google expresses a link are Google's choice and change
	// between two captures of one query. Under the third the page holds no
	// destination at all, and this is what was decided for it: the redirector,
	// which is where a browser goes when somebody clicks the result. An empty
	// string would be the field their programs rely on handed over blank, and
	// dropping the result would lose a place on the page without saying so.
	s, secret := searchServer(t, SearchConfig{Searcher: answering(serpAPIPage())})

	_, body := serpAPIAsk(t, s, secret, "/search?q=iphone+13")
	results := listAt(t, body, "organic_results")
	if len(results) != 3 {
		t.Fatalf("%d results came back, want 3: a result with no address of its own was dropped", len(results))
	}

	// The first result carries both a destination and a redirector, so handing
	// over the redirector where the destination was known shows up here.
	if got := textAt(t, nthOf(t, results, 0), "link"); got != "https://apple.test/iphone" {
		t.Errorf("a result whose address the page carried came back as %q, want the destination itself", got)
	}

	middle := nthOf(t, results, 1)
	const wantLink = "https://www.google.test/goto?url=Ciphertext"
	if got := textAt(t, middle, "link"); got != wantLink {
		t.Errorf("a result with no address of its own came back with link %q, want %q", got, wantLink)
	}
	if got := textAt(t, middle, "title"); got != "Shop" {
		t.Errorf("the middle result is %q, want the one the page had no address for", got)
	}
	if got := textAt(t, middle, "displayed_link"); got != "https://shop.test › phones" {
		t.Errorf("a result with no address of its own shows %q, want the address Google drew", got)
	}
}

func TestSerpAPI_ResolvesAPageRelativeRedirectorAgainstTheDomainTheSearchWentTo(t *testing.T) {
	// A page parsed from a body alone records no origin, and the redirector is
	// page-relative. Handed over as it stands it is "/goto?url=…", which nothing
	// can fetch. The domain the query was rendered against is where it belongs.
	page := serpAPIPage()
	page.Origin = ""
	s, secret := searchServer(t, SearchConfig{Searcher: answering(page)})

	_, body := serpAPIAsk(t, s, secret, "/search?q=iphone+13&gl=uk")
	results := listAt(t, body, "organic_results")
	const wantLink = "https://www.google.co.uk/goto?url=Ciphertext"
	if got := textAt(t, nthOf(t, results, 1), "link"); got != wantLink {
		t.Errorf("a page that said nothing about where it came from gave link %q, want %q", got, wantLink)
	}
}

func TestSerpAPI_RepeatsBackTheSearchThatWasAskedForAndNotTheOneThePageRecorded(t *testing.T) {
	// This block is what a caller checks when the results are not what they
	// expected, so it has to say how the request was understood. The captured
	// page carries a query of its own, and repeating that one would tell the
	// caller their request arrived intact whatever had happened to it.
	stub := answering(serpAPIPage())
	s, secret := searchServer(t, SearchConfig{Searcher: stub})

	_, body := serpAPIAsk(t, s, secret, "/search?q=iphone+13&gl=uk&hl=en")
	params := objectAt(t, body, "search_parameters")
	for _, want := range []struct{ name, value string }{
		{"engine", "google"},
		{"q", "iphone 13"},
		{"google_domain", "google.co.uk"},
		{"gl", "uk"},
		{"hl", "en"},
		{"device", "desktop"},
	} {
		if got := textAt(t, params, want.name); got != want.value {
			t.Errorf("%s came back as %q, want %q", want.name, got, want.value)
		}
	}
	if asked := stub.query(0); asked.Text != "iphone 13" || asked.Country != "uk" || asked.Language != "en" {
		t.Errorf("the search was made as %+v, want what the request named", asked)
	}
}

func TestSerpAPI_NamesTheDomainTheCountryActuallySendsTheSearchTo(t *testing.T) {
	// A domain reported from anywhere but the address the query renders itself
	// as is a second table of countries, and two tables agree until one is
	// edited.
	s, secret := searchServer(t, SearchConfig{Searcher: answering(serpAPIPage())})

	for country, want := range map[string]string{"": "google.com", "de": "google.de", "br": "google.com.br"} {
		target := "/search?q=one"
		if country != "" {
			target += "&gl=" + country
		}
		_, body := serpAPIAsk(t, s, secret, target)
		if got := textAt(t, objectAt(t, body, "search_parameters"), "google_domain"); got != want {
			t.Errorf("gl=%q went to %q, want %q", country, got, want)
		}
	}
}

func TestSerpAPI_RefusesAnEngineItDoesNotSearchBeforeSpendingAnIdentityOnIt(t *testing.T) {
	// Answering Google's results under another engine's name is a wrong answer
	// the caller cannot tell from a right one. Leaving the engine out is not
	// wrong: the service this stands in for takes google as its default, so
	// somebody's program is entitled never to have set it.
	stub := answering(serpAPIPage())
	s, secret := searchServer(t, SearchConfig{Searcher: stub})

	code, body := serpAPIAsk(t, s, secret, "/search?q=one&engine=bing")
	if code != http.StatusBadRequest {
		t.Errorf("engine=bing answered %d, want 400 (body %v)", code, body)
	}
	if _, named := body["error"].(string); !named {
		t.Errorf("the refusal carries no error field: %v", body)
	}
	if calls, _ := stub.count(); calls != 0 {
		t.Errorf("%d searches were made for an engine this server does not search, want 0", calls)
	}

	for _, target := range []string{"/search?q=one", "/search?q=one&engine=google", "/search?q=one&engine=GOOGLE"} {
		if code, body := serpAPIAsk(t, s, secret, target); code != http.StatusOK {
			t.Errorf("%s answered %d, want 200 (body %v)", target, code, body)
		}
	}
}

func TestSerpAPI_RefusesALayoutItDoesNotCapture(t *testing.T) {
	// The layout travels with a query and changes nothing this program sends, so
	// a mobile capture asked for here would be the desktop page with the word
	// mobile written beside it — and believed.
	stub := answering(serpAPIPage())
	s, secret := searchServer(t, SearchConfig{Searcher: stub})

	code, body := serpAPIAsk(t, s, secret, "/search?q=one&device=mobile")
	if code != http.StatusBadRequest {
		t.Errorf("device=mobile answered %d, want 400 (body %v)", code, body)
	}
	if calls, _ := stub.count(); calls != 0 {
		t.Errorf("%d searches were made for a layout this server cannot capture, want 0", calls)
	}
	if code, body := serpAPIAsk(t, s, secret, "/search?q=one&device=desktop"); code != http.StatusOK {
		t.Errorf("device=desktop answered %d, want 200 (body %v)", code, body)
	}
}

func TestSerpAPI_RefusesADomainTheSearchWouldNotHaveGoneTo(t *testing.T) {
	// The domain follows from the country here. Accepting one that contradicts
	// the country would hand a caller one country's results under another
	// country's name.
	stub := answering(serpAPIPage())
	s, secret := searchServer(t, SearchConfig{Searcher: stub})

	code, body := serpAPIAsk(t, s, secret, "/search?q=one&gl=de&google_domain=google.co.uk")
	if code != http.StatusBadRequest {
		t.Errorf("a domain the search does not go to answered %d, want 400 (body %v)", code, body)
	}
	if calls, _ := stub.count(); calls != 0 {
		t.Errorf("%d searches were made for a domain this search does not go to, want 0", calls)
	}
	for _, target := range []string{
		"/search?q=one&gl=de&google_domain=google.de",
		"/search?q=one&gl=de&google_domain=www.google.de",
		"/search?q=one&google_domain=google.com",
	} {
		if code, body := serpAPIAsk(t, s, secret, target); code != http.StatusOK {
			t.Errorf("%s answered %d, want 200 (body %v)", target, code, body)
		}
	}
}

func TestSerpAPI_SaysHowManyPaidPlacementsItDidNotReportRatherThanHalfOfThem(t *testing.T) {
	// Google draws three separate paid products and this parser reads a title
	// and nothing else from one of them. Rendering that one into their paid
	// shape would hand over records whose price, seller and address are empty
	// because nobody looked. None are handed over, and the count is what stops a
	// caller reading the silence as a page that carried no advertising.
	s, secret := searchServer(t, SearchConfig{Searcher: answering(serpAPIPage())})

	_, body := serpAPIAsk(t, s, secret, "/search?q=iphone+13")
	if got := numberAt(t, body, "ads_omitted"); got != 2 {
		t.Errorf("the answer says %v paid placements were dropped, want 2", got)
	}
	for _, half := range []string{"ads", "shopping_results", "inline_shopping_results"} {
		if _, there := body[half]; there {
			t.Errorf("the answer carries %q: paid placements are reported or they are not, never half", half)
		}
	}
}

func TestSerpAPI_SaysNothingAboutPaidPlacementsOnAPageThatCarriedNone(t *testing.T) {
	// A count on every answer would be noise, and a count of zero on a page that
	// had ads would be the lie the count exists to prevent — so it appears
	// exactly when something was dropped.
	page := serpAPIPage()
	page.Ads = nil
	s, secret := searchServer(t, SearchConfig{Searcher: answering(page)})

	_, body := serpAPIAsk(t, s, secret, "/search?q=iphone+13")
	if _, there := body["ads_omitted"]; there {
		t.Errorf("a page with no paid placements reported %v dropped", body["ads_omitted"])
	}
}

func TestSerpAPI_OffersTheRelatedSearchesAsSearchesThatCanBeRun(t *testing.T) {
	s, secret := searchServer(t, SearchConfig{Searcher: answering(serpAPIPage())})

	_, body := serpAPIAsk(t, s, secret, "/search?q=iphone+13&gl=uk")
	related := listAt(t, body, "related_searches")
	if len(related) != 1 {
		t.Fatalf("%d related searches came back, want 1", len(related))
	}
	first := nthOf(t, related, 0)
	if got := textAt(t, first, "query"); got != "iphone 14" {
		t.Errorf("the related search is %q, want %q", got, "iphone 14")
	}
	link := textAt(t, first, "link")
	if !strings.HasPrefix(link, "https://www.google.co.uk/search?") || !strings.Contains(link, "q=iphone+14") {
		t.Errorf("the related search links to %q, want the same search on the domain this one went to", link)
	}
}

func TestSerpAPI_AnswersAPageWithNoResultsWithAnEmptyListRatherThanNothingAtAll(t *testing.T) {
	// A caller walking the results should not have to tell an empty list from a
	// missing one before they can loop over it.
	s, secret := searchServer(t, SearchConfig{Searcher: answering(google.SERP{})})

	code, body := serpAPIAsk(t, s, secret, "/search?q=nothing+matches+this")
	if code != http.StatusOK {
		t.Fatalf("a search that found nothing answered %d, want 200 (body %v)", code, body)
	}
	if got := listAt(t, body, "organic_results"); len(got) != 0 {
		t.Errorf("a page with no results gave %v", got)
	}
}

func TestSerpAPI_ReportsTheTimeTheSearchItselfTook(t *testing.T) {
	// A caller deciding whether to keep asking this way or to set a job going
	// has nothing else to compare, and a nought reported here reads as an answer
	// that cost nothing.
	stub := &searchStub{answer: func(context.Context, google.Query) (google.SERP, error) {
		time.Sleep(30 * time.Millisecond)
		return serpAPIPage(), nil
	}}
	s, secret := searchServer(t, SearchConfig{Searcher: stub})

	_, body := serpAPIAsk(t, s, secret, "/search?q=iphone+13")
	meta := objectAt(t, body, "search_metadata")
	if took := numberAt(t, meta, "total_time_taken"); took < 0.03 {
		t.Errorf("a search that took at least 30ms is reported as %v seconds", took)
	}
	began, err := time.Parse(serpAPITimeLayout, textAt(t, meta, "created_at"))
	if err != nil {
		t.Fatalf("created_at is not written the way their clients parse it: %v", err)
	}
	done, err := time.Parse(serpAPITimeLayout, textAt(t, meta, "processed_at"))
	if err != nil {
		t.Fatalf("processed_at is not written the way their clients parse it: %v", err)
	}
	if done.Before(began) {
		t.Errorf("the search was processed at %s and created at %s", done, began)
	}
}

func TestSerpAPI_NamesEveryAnswerSeparately(t *testing.T) {
	// The identifier is what a caller's log line is keyed by. One name shared by
	// every answer names nothing.
	s, secret := searchServer(t, SearchConfig{Searcher: answering(serpAPIPage())})

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		_, body := serpAPIAsk(t, s, secret, "/search?q=iphone+13")
		id := textAt(t, objectAt(t, body, "search_metadata"), "id")
		if id == "" {
			t.Fatal("an answer carries an empty identifier")
		}
		if seen[id] {
			t.Fatalf("two answers were both named %q", id)
		}
		seen[id] = true
	}
}

func TestSerpAPI_TakesTheKeyInTheQueryStringTheWayTheServiceItStandsInForDoes(t *testing.T) {
	// Their clients put the key in the address, and this endpoint exists so that
	// such a client works after changing the address and nothing else. A key it
	// would only take in a header would break exactly the promise it is here for.
	stub := answering(serpAPIPage())
	s, secret := searchServer(t, SearchConfig{Searcher: stub})

	code, body := serpAPIAsk(t, s, "", "/search?q=iphone+13&api_key="+secret)
	if code != http.StatusOK {
		t.Fatalf("a search carrying its key in the address answered %d, want 200 (body %v)", code, body)
	}
	if code, _ := serpAPIAsk(t, s, "", "/search?q=iphone+13"); code != http.StatusUnauthorized {
		t.Errorf("a search with no key at all answered %d, want 401", code)
	}
	if code, _ := serpAPIAsk(t, s, "", "/search?q=iphone+13&api_key=not-a-key"); code != http.StatusUnauthorized {
		t.Errorf("a search carrying a key nobody issued answered %d, want 401", code)
	}
	if calls, _ := stub.count(); calls != 1 {
		t.Errorf("%d searches were made, want 1: a request with no working key reached the identities", calls)
	}
}

func TestSerpAPI_RefusesARequestThatNamesNothingToSearchFor(t *testing.T) {
	stub := answering(serpAPIPage())
	s, secret := searchServer(t, SearchConfig{Searcher: stub})

	for _, target := range []string{"/search", "/search?q=", "/search?q=%20%20"} {
		if code, body := serpAPIAsk(t, s, secret, target); code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400 (body %v)", target, code, body)
		}
	}
	if calls, _ := stub.count(); calls != 0 {
		t.Errorf("%d searches were made for a request that named no query, want 0", calls)
	}
}

func TestSerpAPI_CountsAgainstTheSameLimitAsTheInterfacesOwnSearchAddress(t *testing.T) {
	// Two doors into one machine have to share the counting. A second limiter
	// behind this address would be a second allowance, and the pair would let
	// twice as many searches at the identities as either one permits — which is
	// the whole of what the limit was put there to prevent.
	stub, entered, release := held()
	s, secret := searchServer(t, SearchConfig{Searcher: stub, AtOnce: 1, Deadline: 5 * time.Second})

	first := make(chan int, 1)
	go func() {
		first <- call(t, s, secret, http.MethodGet, "/api/v1/search?q=one", "").Code
	}()
	<-entered

	began := time.Now()
	code, body := serpAPIAsk(t, s, secret, "/search?q=two")
	waited := time.Since(began)

	if code != http.StatusTooManyRequests {
		t.Errorf("a search over the shared limit answered %d, want 429 (body %v)", code, body)
	}
	if waited > 2*time.Second {
		t.Errorf("the refusal took %s: the caller was held rather than refused", waited)
	}
	if calls, _ := stub.count(); calls != 1 {
		t.Errorf("%d searches were made, want 1: the refused one took an identity anyway", calls)
	}

	close(release)
	if code := <-first; code != http.StatusOK {
		t.Errorf("the search that was under way answered %d, want 200", code)
	}
}

func TestSerpAPI_GivesBackItsPlaceInTheLimitLikeTheInterfacesOwnSearchAddress(t *testing.T) {
	// A place kept by this address and not the other one would starve the pair
	// after a handful of requests, and nothing would say which door lost it.
	stub := failing(errors.New("nothing came back"))
	s, secret := searchServer(t, SearchConfig{Searcher: stub, AtOnce: 1})

	for i := 0; i < 3; i++ {
		if code, _ := serpAPIAsk(t, s, secret, "/search?q=one"); code == http.StatusTooManyRequests {
			t.Fatalf("search %d was refused as too many at once after %d failures: a place was kept", i+1, i)
		}
	}
	if code, body := serpAPIAsk(t, s, secret, "/search?q=one"); code == http.StatusTooManyRequests {
		t.Errorf("the limit filled up with searches that had finished (body %v)", body)
	}
}

func TestSerpAPI_SaysTheSearchRanOutOfTimeRatherThanHoldingTheCallerForever(t *testing.T) {
	// The same deadline as the interface's own search address, refused the same
	// way. A caller told a different code by one door than by the other has two
	// servers to write against.
	stub := &searchStub{answer: func(ctx context.Context, _ google.Query) (google.SERP, error) {
		select {
		case <-ctx.Done():
			return google.SERP{}, ctx.Err()
		case <-time.After(5 * time.Second):
			return serpAPIPage(), nil
		}
	}}
	s, secret := searchServer(t, SearchConfig{Searcher: stub, Deadline: 50 * time.Millisecond})

	began := time.Now()
	code, body := serpAPIAsk(t, s, secret, "/search?q=one")
	took := time.Since(began)

	if code != http.StatusGatewayTimeout {
		t.Fatalf("a search that ran out of time answered %d, want 504 (body %v)", code, body)
	}
	if took > 4*time.Second {
		t.Errorf("the answer took %s: the deadline the server was given did not apply", took)
	}
	if said, _ := body["error"].(string); !strings.Contains(said, "did not finish") {
		t.Errorf("the answer says %q, which does not tell a reader the search was unfinished rather than refused", said)
	}
}

func TestSerpAPI_SaysSoWhenTheServerWasStartedWithNothingToSearchFrom(t *testing.T) {
	s, secret := searchServer(t, SearchConfig{})
	if code, body := serpAPIAsk(t, s, secret, "/search?q=one"); code != http.StatusServiceUnavailable {
		t.Errorf("a server with nothing to search from answered %d, want 503 (body %v)", code, body)
	}
}
