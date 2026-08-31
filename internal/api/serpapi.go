// SPDX-License-Identifier: MIT

package api

import (
	"crypto/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/google"
)

// serpAPIEngine is the only engine this address answers for.
//
// A caller who leaves it out gets it anyway, because the service this shape
// copies takes it as the default and somebody else's program is entitled to
// have left it unset.
const serpAPIEngine = "google"

// serpAPIEngines is every engine this address answers for, and a refusal says
// so rather than only saying no.
//
// Images, news, shopping, video, maps, trends and translation are not here and
// are not coming in the next few days. Nothing in this program reads any of
// those pages, so a caller who asked for images and was handed the ordinary web
// results would file web results as images, keep them, and find out a week
// later. Naming what is supported is what turns that from a discovery into a
// refusal the caller can act on at once: the list is short, it is the truth,
// and it costs one sentence.
var serpAPIEngines = []string{serpAPIEngine}

// serpAPIStatus is what a finished search is called. The word is theirs, spelled
// their way: a program branching on it compares against this exact string.
const serpAPIStatus = "Success"

// serpAPITimeLayout is how the two timestamps are written. It is the layout the
// shape being copied uses, and a program parsing one of those timestamps has
// that layout compiled into it.
const serpAPITimeLayout = "2006-01-02 15:04:05 MST"

// crumbSeparator is what a displayed address puts between the host and the path
// Google drew beside it.
const crumbSeparator = " › "

// serpAPIResponse is one search in the shape another service already answers in.
//
// Every name here is theirs. That is the whole of this file's purpose: a program
// written against that service should read this answer after changing the
// address it calls and nothing else, so a name changed here for being prettier
// costs somebody a rewrite and buys nothing.
type serpAPIResponse struct {
	SearchMetadata   serpAPIMetadata   `json:"search_metadata"`
	SearchParameters serpAPIParameters `json:"search_parameters"`
	OrganicResults   []organicResult   `json:"organic_results"`
	RelatedSearches  []relatedSearch   `json:"related_searches,omitempty"`

	// AdsOmitted is how many paid placements were on the captured page and are
	// not reported below, and it is not one of their names.
	//
	// This address reports no advertising at all. Google draws three separate
	// products — the block above the results, the block below them, and the
	// product listings beside them — and this parser reads a title and nothing
	// else from the product listings. Rendering them into the paid shapes that
	// service defines would mean handing over records whose price, seller and
	// address fields are empty because nobody looked, which a caller reads as a
	// page that carried none of those things.
	//
	// So none are handed over, and this counts what was dropped rather than
	// leaving the caller to believe the page carried no advertising. A field
	// they never defined is a field their programs ignore, so saying so costs
	// the compatibility nothing.
	AdsOmitted int `json:"ads_omitted,omitempty"`
}

// serpAPIMetadata is what the answer says about itself.
type serpAPIMetadata struct {
	// ID names this one answer. Nothing is filed under it: that service keeps
	// every search and hands back the identifier to fetch it again, and this
	// program keeps a search answered in the request nowhere at all. It is worth
	// having anyway, because it is what a caller's log line is keyed by when
	// they come back asking about one answer among a day of them.
	ID     string `json:"id"`
	Status string `json:"status"`
	// CreatedAt is when the search was asked for and ProcessedAt when it was
	// answered, so the pair brackets the same span TotalTimeTaken measures.
	CreatedAt      string  `json:"created_at"`
	ProcessedAt    string  `json:"processed_at"`
	TotalTimeTaken float64 `json:"total_time_taken"`
}

// serpAPIParameters repeats the search back as it was understood.
//
// It repeats what was asked and not what this server prefers, because that is
// what a caller checks when the results are not what they expected: a query
// read differently from how it was sent shows up here and nowhere else.
type serpAPIParameters struct {
	Engine       string `json:"engine"`
	Q            string `json:"q"`
	GoogleDomain string `json:"google_domain"`
	GL           string `json:"gl,omitempty"`
	HL           string `json:"hl,omitempty"`
	Device       string `json:"device"`
}

// organicResult is one unpaid result.
type organicResult struct {
	// Position is the result's place on the page it was read from, counted from
	// one.
	//
	// Their number is the position within one page of results and ours is the
	// rank across a whole walk, and the two differ as soon as a walk goes past
	// the first page. They do not differ here: this address captures one page,
	// the parser numbers a page's results from one, and no result is dropped on
	// the way out, so the place on the page is the rank and the sequence has no
	// gaps. The cross-page rank of a job lives in the stored rows and is not
	// this number.
	Position int    `json:"position"`
	Title    string `json:"title"`
	// Link is an address that reaches the result, and it is never empty for a
	// result the parser read.
	//
	// Google gives an address three ways and picks between them itself; two
	// captures of one query minutes apart have differed. Two of the three carry
	// the destination in the page. The third carries only ciphertext, and the
	// exact destination behind it costs one request each to learn.
	//
	// So the exact destination is handed over when the page carried it, and
	// Google's own redirector for that result when it did not. The redirector is
	// what a browser follows when somebody clicks the result, so it goes where
	// the result goes; it is simply Google's address rather than the site's, and
	// a caller reading the host out of it reads Google. That is worth saying in
	// the documentation and it is better than the alternatives: an empty string
	// is the field their programs rely on handed over blank, dropping the result
	// loses a page's ranking without telling anybody, and resolving each one
	// inside the request would multiply the wait on an address that already has
	// a deadline it must answer inside.
	Link          string `json:"link"`
	DisplayedLink string `json:"displayed_link"`
	Snippet       string `json:"snippet,omitempty"`
}

// relatedSearch is one of the searches Google offered beside the results. The
// address is not read off the page — it is the search itself, rendered the same
// way this program renders the one it was asked for.
type relatedSearch struct {
	Query string `json:"query"`
	Link  string `json:"link,omitempty"`
}

// serpAPIRoutes registers the address somebody else's program already calls.
//
// It sits at the root rather than under this interface's own prefix because the
// point is that only the host changes: a program pointed at a different server
// keeps the path it was written with.
func (s *Server) serpAPIRoutes() {
	s.mux.HandleFunc("GET /search", s.authed(s.serpAPISearch))
}

// serpAPISearch answers one search in the shape another service answers in.
//
// It runs the search through the same limiter, the same deadline and the same
// refusals as this interface's own search address. Two doors into one machine
// have to behave alike, and a second copy of the counting would be a second
// limit that lets twice as many searches at the identities as either one
// allows.
func (s *Server) serpAPISearch(w http.ResponseWriter, r *http.Request) {
	q, refusal := serpAPIQuery(r.URL.Query())
	if refusal != "" {
		// Nothing is asked of the identities. A request this server will not
		// answer must not spend a place in the limiter or a lease on the way to
		// being refused.
		writeError(w, http.StatusBadRequest, refusal)
		return
	}

	serp, took, err := s.searchNow(r.Context(), q)
	if err != nil {
		s.refuseSearch(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, serpAPIOf(q, serp, took))
}

// serpAPIQuery reads the search out of the request, or says why it will not be
// answered.
//
// Every refusal here is a request this server would otherwise answer with
// results that are not the ones asked for. A caller told nothing would take
// desktop results for mobile ones, or one country's results for another's, and
// believe them.
func serpAPIQuery(asked url.Values) (google.Query, string) {
	if engine := strings.TrimSpace(asked.Get("engine")); engine != "" &&
		!strings.EqualFold(engine, serpAPIEngine) {
		return google.Query{}, "engine=" + engine +
			" is not supported by this server. Supported engines: " + strings.Join(serpAPIEngines, ", ") +
			". Answering an engine this server does not read would hand back one kind of page under another kind's name"
	}

	text := strings.TrimSpace(asked.Get("q"))
	if text == "" {
		return google.Query{}, "the request names no query: ask with q=<what to search for>"
	}

	// The layout is recorded on a query and acts on nothing this program sends,
	// so a mobile capture asked for here would come back as the desktop page
	// with the word "mobile" written beside it.
	if device := strings.TrimSpace(asked.Get("device")); device != "" &&
		!strings.EqualFold(device, string(google.DeviceDesktop)) {
		return google.Query{}, "this server captures the desktop layout only: device=" + device +
			" would be answered with the desktop page under another name"
	}

	q := google.Query{
		Text:     text,
		Country:  strings.TrimSpace(asked.Get("gl")),
		Language: strings.TrimSpace(asked.Get("hl")),
		Device:   google.DeviceDesktop,
	}

	// The domain follows from the country here, and their interface lets a
	// caller name it outright. A caller naming one this query does not go to has
	// asked for a country's results and would be handed another country's.
	domain, _ := googleDomainOf(q)
	if wanted := strings.TrimSpace(asked.Get("google_domain")); wanted != "" &&
		!sameGoogleDomain(wanted, domain) {
		return google.Query{}, "this search goes to " + domain + " and not to " + wanted +
			": name the country as gl=<country> and the domain follows from it"
	}
	return q, ""
}

// sameGoogleDomain compares two spellings of a Google domain. The prefix is
// dropped because the domain is written both ways and neither spelling picks a
// different set of results.
func sameGoogleDomain(a, b string) bool {
	return strings.EqualFold(strings.TrimPrefix(strings.ToLower(a), "www."), b)
}

// googleDomainOf reports which Google the query goes to, and where addresses
// found on that page have to be resolved against.
//
// Both come from the address the query renders itself as, rather than from a
// second table of countries and domains kept here. One table that decides where
// the request goes and another that says where it went would agree until
// somebody edited one of them.
func googleDomainOf(q google.Query) (domain, origin string) {
	raw, err := q.URL()
	if err != nil {
		return "", ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}
	return strings.TrimPrefix(u.Host, "www."), u.Scheme + "://" + u.Host
}

// serpAPIOf renders one captured page in the shape another service answers in.
func serpAPIOf(q google.Query, serp google.SERP, took time.Duration) serpAPIResponse {
	done := time.Now().UTC()
	domain, asked := googleDomainOf(q)

	// The page's own origin is where its addresses belong, and it is what the
	// response actually landed on rather than what was requested. A page parsed
	// from a body alone carries none, and the domain the query was rendered
	// against is then the best account there is of where its links point.
	origin := serp.Origin
	if origin == "" {
		origin = asked
	}

	out := serpAPIResponse{
		SearchMetadata: serpAPIMetadata{
			ID:     rand.Text(),
			Status: serpAPIStatus,
			// The search took as long as it took, so subtracting says when it
			// began without a second clock reading that could disagree.
			CreatedAt:      done.Add(-took).Format(serpAPITimeLayout),
			ProcessedAt:    done.Format(serpAPITimeLayout),
			TotalTimeTaken: took.Seconds(),
		},
		SearchParameters: serpAPIParameters{
			Engine:       serpAPIEngine,
			Q:            q.Text,
			GoogleDomain: domain,
			GL:           q.Country,
			HL:           q.Language,
			Device:       string(google.DeviceDesktop),
		},
		// A search that found nothing is an empty list and never a missing one:
		// a caller walking the results should not have to tell the two apart.
		OrganicResults: make([]organicResult, 0, len(serp.Results)),
		AdsOmitted:     len(serp.Ads),
	}

	for _, res := range serp.Results {
		out.OrganicResults = append(out.OrganicResults, organicResult{
			Position:      res.Position,
			Title:         res.Title,
			Link:          organicLink(res, origin),
			DisplayedLink: displayedLink(res),
			Snippet:       res.Snippet,
		})
	}
	for _, related := range serp.Related {
		out.RelatedSearches = append(out.RelatedSearches, relatedSearch{
			Query: related,
			Link:  searchLink(q, related),
		})
	}
	return out
}

// organicLink is the address handed over for one result.
//
// The exact destination when the page carried it, and Google's redirector for
// that result when it did not — see Link. The redirector is origin-relative
// under the form that hides the destination, which is an address nothing can
// fetch, so it is joined to where the page came from before it goes out.
func organicLink(res google.Result, origin string) string {
	if res.Resolved() {
		return res.URL
	}
	return absoluteAgainst(origin, res.Link)
}

// absoluteAgainst joins a page-relative address to where the page came from,
// and leaves anything it cannot join alone: a relative address handed over is
// worth more to a caller who knows the origin than an empty string is to
// anybody.
func absoluteAgainst(origin, href string) string {
	if href == "" || origin == "" {
		return href
	}
	base, err := url.Parse(origin)
	if err != nil {
		return href
	}
	ref, err := url.Parse(href)
	if err != nil {
		return href
	}
	return base.ResolveReference(ref).String()
}

// displayedLink rebuilds the address Google drew under the title.
//
// It is drawn for the eye and read by nothing, which is why it can be rebuilt
// from the host and the breadcrumb the page carried rather than from the
// destination. The scheme comes from the destination where there is one and is
// otherwise the one every result page has been observed to use.
func displayedLink(res google.Result) string {
	if res.Host == "" {
		return ""
	}
	scheme := "https"
	if res.Resolved() {
		if u, err := url.Parse(res.URL); err == nil && u.Scheme != "" {
			scheme = u.Scheme
		}
	}
	shown := scheme + "://" + res.Host
	if res.DisplayPath != "" {
		shown += crumbSeparator + res.DisplayPath
	}
	return shown
}

// searchLink renders one of the offered searches as the address that runs it,
// keeping the country and language of the search it was offered beside.
func searchLink(q google.Query, text string) string {
	next := google.Query{Text: text, Country: q.Country, Language: q.Language}
	raw, err := next.URL()
	if err != nil {
		return ""
	}
	return raw
}
