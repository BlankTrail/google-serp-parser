// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// The keys the reference lists were generated from, in the order they were
// given to run4linkgen.php. The last has two spaces between its words, which
// the generator split on as written.
var linkgenKeys = []string{"coffee maker", "купить кофе машину", "kaffee  maschine"}

func TestSuggestVariants_AsksWhatTheLinkGeneratorAsked(t *testing.T) {
	// testdata/linkgen_*.txt is what run4linkgen.php printed for linkgenKeys,
	// run through PHP's own command line with nothing changed but the proxy
	// list it fetched and never used for generating. Every line, in order.
	for _, c := range []struct {
		file      string
		lang      string
		multiword bool
	}{
		{"linkgen_en0.txt", "", false},
		{"linkgen_en1.txt", "", true},
		{"linkgen_ru1.txt", "ru", true},
		{"linkgen_de1.txt", "de", true},
		{"linkgen_es0.txt", "es", false},
	} {
		t.Run(c.file, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/" + c.file)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
			if c.lang == "es" {
				// The one deliberate difference: the generator sent the line
				// end after the last letter of its Spanish file as part of
				// that letter. Here the letter is ch.
				for i := range want {
					want[i] = strings.NewReplacer("%0A", "", "%0D", "").Replace(want[i])
				}
			}
			var got []string
			for _, key := range linkgenKeys {
				for _, v := range SuggestVariants(key, SuggestAlphabet(c.lang), c.multiword) {
					u, err := url.Parse(SuggestURL(Query{}, v))
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, u.RawQuery)
				}
			}
			if len(got) != len(want) {
				t.Fatalf("%d links, want the generator's %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("link %d is\n  %s\nwant\n  %s", i+1, got[i], want[i])
				}
			}
		})
	}
}

func TestSuggestAlphabet_FallsBackToLatinAndDigitsAndKnowsTheGeneratorsPortugueseName(t *testing.T) {
	if got := strings.Join(SuggestAlphabet("xx"), ""); got != "abcdefghijklmnopqrstuvwxyz0123456789" {
		t.Errorf("an unknown language's alphabet is %q, want a to z and nought to nine", got)
	}
	if got, want := SuggestAlphabet("pg"), SuggestAlphabet("pt-BR"); strings.Join(got, "|") != strings.Join(want, "|") || len(got) == 0 {
		t.Errorf("pg gave %v and pt-BR %v, want the generator's Portuguese for both", got, want)
	}
	for _, a := range SuggestAlphabet("ar") {
		if a != strings.TrimSpace(a) || a == "" {
			t.Errorf("Arabic letter %q kept the spaces around it", a)
		}
	}
}

func TestSuggestURL_AsksOnTheCountrysHostInItsLanguage(t *testing.T) {
	got := SuggestURL(Query{Country: "de", Language: "de"}, SuggestVariant{Key: "kaffee", Text: "kaffee m", Cursor: 1})
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "www.google.de" || u.Path != "/complete/search" {
		t.Errorf("asked %s%s, want www.google.de/complete/search", u.Host, u.Path)
	}
	if !strings.HasSuffix(u.RawQuery, "&dpr=1.5&hl=de&gl=de") {
		t.Errorf("the link ends %q, want the generator's parameters and then hl and gl", u.RawQuery)
	}
}

// completionBody is an answer in the shape the gws-wiz-serp client gets, made
// up here rather than captured: the typed part plain, the rest in <b>, an
// entity, and the XSSI guard in front.
const completionBody = ")]}'\n" +
	`[[["coffee m<b>achine<\/b>",0,[512]],["coffee m<b>aker<\/b>",0,[512]],` +
	`["coffee &amp; m<b>ore<\/b>",0,[512]]],{"q":"x"}]`

func TestParseSuggestions_ReadsTheTextASearcherWouldSee(t *testing.T) {
	got, err := ParseSuggestions([]byte(completionBody))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"coffee machine", "coffee maker", "coffee & more"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("read %q, want %q", got, want)
	}
	if _, err := ParseSuggestions([]byte("<html>sorry</html>")); !errors.Is(err, ErrNotSuggestions) {
		t.Errorf("a page that is not the list read as %v, want ErrNotSuggestions", err)
	}
}

func TestSuggester_Complete_TellsAWallFromAnAnswer(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		class  Class
	}{
		{"rate limited", http.StatusTooManyRequests, "", ClassWall},
		{"refused", http.StatusForbidden, "", ClassBanned},
		{"a challenge page in place of the list", http.StatusOK, "<html>unusual traffic</html>", ClassWall},
		{"something else", http.StatusBadGateway, "", ClassHTTP},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			s := &Suggester{Client: redirectedTo(srv)}
			_, err := s.Complete(context.Background(), Query{}, SuggestVariant{Key: "k", Text: "k a", Cursor: 1})
			if class, ok := ClassOf(err); !ok || class != c.class {
				t.Errorf("classed %q (%v), want %q", class, err, c.class)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("client") != "gws-wiz-serp" || r.Header.Get("Accept-Language") != "de-DE,de;q=0.9" {
			http.Error(w, "wrong question", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(completionBody))
	}))
	defer srv.Close()
	got, err := (&Suggester{Client: redirectedTo(srv)}).Complete(context.Background(),
		Query{Country: "de", Language: "de"}, SuggestVariant{Key: "coffee", Text: "coffee m", Cursor: 1})
	if err != nil || len(got) != 3 {
		t.Errorf("completions %q, %v; want three and no error", got, err)
	}
}

// redirectedTo is a client that sends every request to a test server whatever
// host it names, so the real Google address can be built and still answered
// here.
func redirectedTo(srv *httptest.Server) *http.Client {
	target, _ := url.Parse(srv.URL)
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(r)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
