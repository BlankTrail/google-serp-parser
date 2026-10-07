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

func TestEchoes_DropsTheQuestionHandedBackAndKeepsWhatAnswersIt(t *testing.T) {
	// The completions of the operator's own report, for "картина дрим арт"
	// with Multiword: the letter р put between дрим and арт comes back where
	// it was typed, and that is the question, not a completion of the key.
	ru := SuggestAlphabet("ru")
	variant := func(t *testing.T, key, text string, multiword bool) SuggestVariant {
		t.Helper()
		for _, v := range SuggestVariants(key, ru, multiword) {
			if v.Text == text {
				return v
			}
		}
		t.Fatalf("no question %q is asked for %q", text, key)
		return SuggestVariant{}
	}
	between := variant(t, "картина дрим арт", "картина дрим р арт", true)
	after := variant(t, "картина дрим арт", "картина дрим арт р", false)
	before := variant(t, "картина дрим арт", "р картина дрим арт", false)
	afterWord := variant(t, "картина дрим арт", "картина дрим арт в", false)
	beforeWord := variant(t, "картина дрим арт", "в картина дрим арт", false)
	betweenWord := variant(t, "картина дрим арт", "картина в дрим арт", true)
	joined := variant(t, "картина дрим арт", "картина дрим арть", false)
	firstPlace := variant(t, "картина дрим арт", "картина р дрим арт", true)

	cases := []struct {
		name       string
		v          SuggestVariant
		completion string
		echo       bool
	}{
		{"the letter left between the words", between, "картина дрим р арт это", true},
		{"the letter left before a longer word", between, "картина дрим р артемьева", true},
		{"the question itself", between, "картина дрим р арт", true},
		{"as Google writes it, in capitals", between, "Картина Дрим Р Арт портреты", true},
		{"an answer that did not keep the letter", between, "дрим арт до после", false},
		{"the letter in its place after other words", between, "купить дрим р арт", false},
		{"the letter grown into a word", between, "картина дрим рисунок арт", false},
		{"a letter one place further on", firstPlace, "картина дрим р арт", false},
		{"the letter left after the key", after, "картина дрим арт р", true},
		{"the letter left after the key, with more", after, "картина дрим арт р это", true},
		{"the letter after the key finished", after, "картина дрим арт ростов", false},
		{"the letter left before the key", before, "р картина дрим арт", true},
		{"the letter before the key finished", before, "рисунок картина дрим арт", false},
		{"a word of one letter after the key", afterWord, "картина дрим арт в москве", false},
		{"a word of one letter before the key", beforeWord, "в картина дрим арт", false},
		{"a word of one letter between the words", betweenWord, "картина в дрим арт", false},
		{"a letter joined to the key", joined, "картина дрим арть", false},
		{"a question with no letter", SuggestVariant{Key: "k", Text: "k "}, "k", false},
	}
	for _, c := range cases {
		if got := Echoes(c.v, "ru", c.completion); got != c.echo {
			t.Errorf("%s: Echoes(%q, %q) = %v, want %v", c.name, c.v.Text, c.completion, got, c.echo)
		}
	}
}

func TestEchoes_KnowsTheWordsOfOneLetterOfTheJobsLanguage(t *testing.T) {
	// The words of one letter are the language's, so the same letter is a word
	// in one job and the question handed back in another; a digit is a model
	// or a year, and kept in every language; a language with no list of its
	// own has English's, as it has English's alphabet.
	q := func(key, letter string) SuggestVariant {
		for _, v := range SuggestVariants(key, []string{letter}, false) {
			if v.Text == key+" "+letter {
				return v
			}
		}
		t.Fatalf("no question %q is asked", key+" "+letter)
		return SuggestVariant{}
	}
	cases := []struct {
		lang, key, letter string
		echo              bool
	}{
		{"en", "coffee", "a", false},
		{"en", "coffee", "i", false},
		{"en", "coffee", "b", true},
		{"ru", "кофе", "в", false},
		{"ru", "кофе", "я", true},
		{"ru", "coffee", "a", true},
		{"es", "cafe", "y", false},
		{"de", "kaffee", "a", true},
		{"", "coffee", "a", false},
		{"", "coffee", "q", true},
		{"zz", "coffee", "i", false},
		{"en", "iphone", "5", false},
		{"ru", "айфон", "5", false},
	}
	for _, c := range cases {
		v := q(c.key, c.letter)
		if got := Echoes(v, c.lang, v.Text+" case"); got != c.echo {
			t.Errorf("language %q, %q left standing alone: Echoes = %v, want %v", c.lang, c.letter, got, c.echo)
		}
	}
}

func TestRelated_TellsACompletionAboutTheKeyFromOneAboutSomethingElse(t *testing.T) {
	cases := []struct {
		name, key, completion string
		related               bool
	}{
		{"the key finished", "coffee maker", "coffee maker reviews", true},
		{"one word of the key", "coffee maker", "best coffee", true},
		{"another form of a word", "купить кофемашину", "кофемашины в москве", true},
		{"a stem two thirds of the shorter word", "делает видео", "делающий", true},
		{"a misspelling put right", "expresso", "espresso machine", true},
		{"a word typed in the other script", "kofemashina", "кофемашина купить", true},
		{"a Russian word typed in Latin letters", "нейросеть", "neyroset online", true},
		{"two words joined", "coffee maker", "coffeemaker deals", true},
		{"one word split", "coffeemaker", "coffee maker deals", true},
		{"an underscore between words", "coffee_maker", "coffee maker deals", true},
		{"a word spelled out of a long brand", "bestcoffeegrinder", "grinder for espresso", true},
		{"a word spelled out of a short one", "tvshow", "show times", true},
		{"three letters are not spelled out", "bestcoffeegrinder", "fee schedule", false},
		{"two letters apart in a long word", "photoshop", "fotoshop online", true},
		{"a letter apart in a short word", "gogle", "google translate", true},
		{"one word split into short ones", "aiart", "ai art generator", true},
		{"an underscore between short words", "ai_art", "art prints", true},
		{"two short words joined", "pl ai", "plai beach", true},
		{"a short word matched whole", "ai art", "ai music", true},
		{"a key of words that say nothing", "how to", "anything at all", true},
		{"something else entirely", "coffee maker", "kafka on the shore", false},
		{"a short word is not a prefix", "ai art", "air fryer", false},
		{"only a word that says nothing", "как сварить кофе", "как похудеть быстро", false},
		{"letters in common inside another word", "pl ai", "jpl airport", false},
		{"two letters apart in a short word", "maker", "mixer grinder", false},
	}
	for _, c := range cases {
		if got := Related(c.key, c.completion); got != c.related {
			t.Errorf("%s: Related(%q, %q) = %v, want %v", c.name, c.key, c.completion, got, c.related)
		}
	}
}
