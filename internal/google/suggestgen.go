// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A key's completions are not read with one request. Google offers ten at a
// time for what has been typed so far, so the key is typed again and again with
// one more character around it — before it, after it, joined to it, and with
// Multiword between its words — and each of those is one request. What comes
// back from all of them, without repeats, is the key's completions.
//
// The variants are generated exactly as the operator's own link generator
// (run4linkgen.php) generates them, so a job here asks Google the same
// questions that generator's lists did and its results can be laid beside the
// old ones: the same seven patterns in the same order, the same alphabets, the
// same cursor positions, and nothing removed as a repeat that the generator
// kept.

// suggestPatterns are the generator's patterns, in its order. [KEY] is the key
// and [A] one letter of the alphabet.
var suggestPatterns = []string{
	"[KEY]",
	"[KEY] ",
	" [KEY]",
	"[KEY] [A]",
	"[KEY][A]",
	"[A] [KEY]",
	"[A][KEY]",
}

// SuggestVariant is one question asked about a key: the text as typed, the
// key it was typed for, and where the cursor stands in it.
type SuggestVariant struct {
	// Key is the key as the job holds it; it is sent as pq, the query the
	// typing started from.
	Key string
	// Text is what has been typed, sent as q.
	Text string
	// Cursor is the cp parameter. The generator sends 1 for every pattern and,
	// for Multiword, the place just after the inserted letter.
	Cursor int
	// Letter is the letter put in as a word of its own — after the key, before
	// it, or between two of its words — and Echo the words typed up to and
	// including it, lower-cased. Both are empty for a question with no letter,
	// and for one whose letter is joined to a word: Google finishes that word,
	// where a letter standing alone is as often handed straight back.
	Letter string
	Echo   []string
}

// Echoes says whether a completion is the question handed back rather than
// an answer to it: the letter put in still stands alone where it was typed,
// "кофе машина р дома это" for "кофе машина р дома". Measured on 897 thousand
// completions of 7773 Russian keys with Multiword, 458 thousand kept the
// letter where it was typed, and 426 thousand of those a letter that is no
// word at all — nearly half of everything the job collected. A letter that is
// a word of the job's language — в, с, и; a, i — is a word the searcher may
// well have meant, and "кофе машина дома в москве" is kept, as is a digit,
// which is a model or a year as often as not.
func Echoes(v SuggestVariant, lang, completion string) bool {
	if v.Letter == "" || len(v.Echo) == 0 || standsAlone(lang, v.Letter) {
		return false
	}
	words := strings.Fields(strings.ToLower(completion))
	if len(words) < len(v.Echo) {
		return false
	}
	for i, w := range v.Echo {
		if words[i] != w {
			return false
		}
	}
	return true
}

// oneLetterWords are the words of one letter each language writes, which a
// completion keeps standing alone because they mean something there. A
// language not listed has the ones of English, which its default alphabet is.
var oneLetterWords = map[string]string{
	"en": "a i",
	"ru": "в с и к у о а",
	"uk": "в у і й з о а",
	"bg": "в и с к у о а",
	"de": "",
	"es": "a e o u y",
	"fr": "a à y",
	"pt": "a e o à é",
	"it": "a e i o è",
	"nl": "u",
	"pl": "a i o u w z",
	"cs": "a i k o s u v z",
	"sk": "a i k o s u v z",
	"sl": "a i k o s v z",
	"hr": "a i k o s u",
	"ro": "a e o",
	"sv": "i å ö",
	"no": "i å",
	"da": "i å",
}

// standsAlone says whether a letter is a word in its own right in the
// language, or a digit.
func standsAlone(lang, letter string) bool {
	if r, size := utf8.DecodeRuneInString(letter); size == len(letter) && unicode.IsDigit(r) {
		return true
	}
	words, ok := oneLetterWords[strings.ToLower(lang)]
	if !ok {
		words = oneLetterWords["en"]
	}
	for _, w := range strings.Fields(words) {
		if strings.EqualFold(w, letter) {
			return true
		}
	}
	return false
}

// SuggestVariants is every question asked about one key, in the generator's
// order: each pattern in turn, the ones with a letter once per letter of the
// alphabet, and then, for a key of more than one word when multiword is on, a
// letter put between each two of its words.
//
// The key is taken as the generator took a line of its file: trimmed, and its
// words split on single spaces exactly as written.
func SuggestVariants(key string, alphabet []string, multiword bool) []SuggestVariant {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	var out []SuggestVariant
	for _, p := range suggestPatterns {
		if !strings.Contains(p, "[A]") {
			out = append(out, SuggestVariant{Key: key, Text: strings.ReplaceAll(p, "[KEY]", key), Cursor: 1})
			continue
		}
		for _, a := range alphabet {
			text := strings.NewReplacer("[KEY]", key, "[A]", a).Replace(p)
			v := SuggestVariant{Key: key, Text: text, Cursor: 1}
			switch p {
			case "[KEY] [A]":
				v.Letter, v.Echo = a, lowerWords(key+" "+a)
			case "[A] [KEY]":
				v.Letter, v.Echo = a, lowerWords(a)
			}
			out = append(out, v)
		}
	}
	// A key of one word has no place between two words, and asks nothing more.
	if !multiword {
		return out
	}
	words := strings.Split(key, " ")
	for i := 1; i < len(words); i++ {
		for _, a := range alphabet {
			typed := make([]string, 0, len(words)+1)
			typed = append(typed, words[:i]...)
			typed = append(typed, a)
			typed = append(typed, words[i:]...)
			text := strings.Join(typed, " ")
			// The generator's str_occur(' ', i, …) + 2: the i-th space, counted
			// from nought, and two on from it — the cursor just past the first
			// character of the letter put in.
			out = append(out, SuggestVariant{
				Key: key, Text: text, Cursor: nthSpace(text, i) + 2,
				Letter: a, Echo: lowerWords(strings.Join(typed[:i+1], " ")),
			})
		}
	}
	return out
}

// lowerWords is a text's words, lower-cased, as a completion is compared.
func lowerWords(text string) []string { return strings.Fields(strings.ToLower(text)) }

// nthSpace is where the n-th space of a text stands, counted from nought, the
// way the generator's str_occur finds it: in bytes, as PHP's strpos counts.
func nthSpace(text string, n int) int {
	at := -1
	for i := 0; i < n; i++ {
		next := strings.IndexByte(text[at+1:], ' ')
		if next < 0 {
			return -1
		}
		at += next + 1
	}
	return at
}

// The alphabets the generator read from its files beside it, one file per
// language and the letters separated by |. A language it had no file for got
// a to z and nought to nine; so does one here.
//
// The files' letters are kept as written, with two changes the generator's
// reading never made: each is trimmed, because the generator sent the line end
// after the last letter of a file and the spaces around the Arabic letters as
// part of the letter, and an empty one is dropped.
var suggestAlphabets = map[string]string{
	"ru": "й|ц|у|к|е|н|г|ш|щ|з|ф|ы|в|а|п|р|о|л|д|я|ч|с|м|и|т|ь|х|ъ|ж|э|ю|б",
	"de": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|ä|ö|ü|ß",
	"es": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|ñ|rr|ll|ch",
	"fr": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|é|à|è|ùâ|ê|î|ô|û|ë|ï|ü|ÿ|ç",
	// The generator's file for Portuguese was named pg.
	"pt": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|á|â|ã|à|ç|é|ê|í|ó|ô|õ|ú",
	"ar": "ألف | باء | تاء | ثاء | جيم | حاء | خاء | دال | ذال | راء | زاي | سين | شين | صٓاد | ضاد | طاء | ظاء | عين | غين | فاء| قاف | كاف | لام | ميم | نون | واو | هاء | ياء",

	// Languages the generator had no file for, from the operator's A-Parser
	// completion parser, which carried an alphabet for each.
	"it": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|à|è|é|ì|ò|ù|0|1|2|3|4|5|6|7|8|9",
	"nl": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|é|ë|ï|ó|ö|ü|0|1|2|3|4|5|6|7|8|9",
	"sv": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|å|ä|ö|0|1|2|3|4|5|6|7|8|9",
	"da": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|æ|ø|å|0|1|2|3|4|5|6|7|8|9",
	"no": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|æ|ø|å|0|1|2|3|4|5|6|7|8|9",
	"fi": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|ä|ö|å|0|1|2|3|4|5|6|7|8|9",
	"pl": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|ą|ć|ę|ł|ń|ó|ś|ź|ż|0|1|2|3|4|5|6|7|8|9",
	"cs": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|á|č|ď|é|ě|í|ň|ó|ř|š|ť|ú|ů|ý|ž|0|1|2|3|4|5|6|7|8|9",
	"sk": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|á|ä|č|ď|é|í|ĺ|ľ|ň|ó|ô|ŕ|š|ť|ú|ý|ž|0|1|2|3|4|5|6|7|8|9",
	"hu": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|á|é|í|ó|ö|ő|ú|ü|ű|0|1|2|3|4|5|6|7|8|9",
	"ro": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|ă|â|î|ș|ţ|ț|0|1|2|3|4|5|6|7|8|9",
	"hr": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|č|ć|đ|š|ž|0|1|2|3|4|5|6|7|8|9",
	"sl": "a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|č|š|ž|0|1|2|3|4|5|6|7|8|9",
	"tr": "a|b|c|ç|d|e|f|g|ğ|h|ı|i|j|k|l|m|n|o|ö|p|r|s|ş|t|u|ü|v|y|z|q|w|x|0|1|2|3|4|5|6|7|8|9",
	"uk": "а|б|в|г|ґ|д|е|є|ж|з|и|і|ї|й|к|л|м|н|о|п|р|с|т|у|ф|х|ц|ч|ш|щ|ь|ю|я|0|1|2|3|4|5|6|7|8|9",
	"bg": "а|б|в|г|д|е|ж|з|и|й|к|л|м|н|о|п|р|с|т|у|ф|х|ц|ч|ш|щ|ъ|ь|ю|я|0|1|2|3|4|5|6|7|8|9",
	"el": "α|β|γ|δ|ε|ζ|η|θ|ι|κ|λ|μ|ν|ξ|ο|π|ρ|σ|ς|τ|υ|φ|χ|ψ|ω|0|1|2|3|4|5|6|7|8|9",
	"he": "א|ב|ג|ד|ה|ו|ז|ח|ט|י|כ|ך|ל|מ|ם|נ|ן|ס|ע|פ|ף|צ|ץ|ק|ר|ש|ת|0|1|2|3|4|5|6|7|8|9",
	"hi": "अ|आ|इ|ई|उ|ऊ|ए|ऐ|ओ|औ|क|ख|ग|घ|ङ|च|छ|ज|झ|ञ|ट|ठ|ड|ढ|ण|त|थ|द|ध|न|प|फ|ब|भ|म|य|र|ल|व|श|ष|स|ह|0|1|2|3|4|5|6|7|8|9",
	"th": "ก|ข|ค|ฆ|ง|จ|ฉ|ช|ซ|ฌ|ญ|ฎ|ฏ|ฐ|ฑ|ฒ|ณ|ด|ต|ถ|ท|ธ|น|บ|ป|ผ|ฝ|พ|ฟ|ภ|ม|ย|ร|ล|ว|ศ|ษ|ส|ห|ฬ|อ|ฮ|0|1|2|3|4|5|6|7|8|9",
	"vi": "a|b|c|d|đ|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|á|à|ả|ã|ạ|ă|ắ|ằ|ẳ|ẵ|ặ|â|ấ|ầ|ẩ|ẫ|ậ|é|è|ẻ|ẽ|ẹ|ê|ế|ề|ể|ễ|ệ|í|ì|ỉ|ĩ|ị|ó|ò|ỏ|õ|ọ|ô|ố|ồ|ổ|ỗ|ộ|ơ|ớ|ờ|ở|ỡ|ợ|ú|ù|ủ|ũ|ụ|ư|ứ|ừ|ử|ữ|ự|ý|ỳ|ỷ|ỹ|ỵ|0|1|2|3|4|5|6|7|8|9",
	"ja": "あ|い|う|え|お|か|き|く|け|こ|さ|し|す|せ|そ|た|ち|つ|て|と|な|に|ぬ|ね|の|は|ひ|ふ|へ|ほ|ま|み|む|め|も|や|ゆ|よ|ら|り|る|れ|ろ|わ|を|ん|ア|イ|ウ|エ|オ|カ|キ|ク|ケ|コ|サ|シ|ス|セ|ソ|タ|チ|ツ|テ|ト|ナ|ニ|ヌ|ネ|ノ|ハ|ヒ|フ|ヘ|ホ|マ|ミ|ム|メ|モ|ヤ|ユ|ヨ|ラ|リ|ル|レ|ロ|ワ|ヲ|ン|a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|0|1|2|3|4|5|6|7|8|9",
	"ko": "ㄱ|ㄴ|ㄷ|ㄹ|ㅁ|ㅂ|ㅅ|ㅇ|ㅈ|ㅊ|ㅋ|ㅌ|ㅍ|ㅎ|ㅏ|ㅑ|ㅓ|ㅕ|ㅗ|ㅛ|ㅜ|ㅠ|ㅡ|ㅣ|a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q|r|s|t|u|v|w|x|y|z|0|1|2|3|4|5|6|7|8|9",
}

// SuggestLanguages are the languages with an alphabet of their own, for a form
// to offer.
func SuggestLanguages() []string {
	out := make([]string, 0, len(suggestAlphabets))
	for lang := range suggestAlphabets {
		out = append(out, lang)
	}
	return out
}

// SuggestAlphabet is the alphabet the generator would have used for a
// language. It read the file named by the first three characters of what it
// was given; here the language's own two letters, before any region, name it.
// Anything without one of its own is a to z and nought to nine.
func SuggestAlphabet(lang string) []string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexAny(lang, "-_"); i > 0 {
		lang = lang[:i]
	}
	if lang == "pg" {
		lang = "pt"
	}
	letters, ok := suggestAlphabets[lang]
	if !ok {
		return strings.Split("abcdefghijklmnopqrstuvwxyz0123456789", "")
	}
	var out []string
	for _, a := range strings.Split(letters, "|") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// suggestSerpClient is the client the generator's links name. It answers in the
// shape the search page's own box reads: the list with the typed part plain and
// the rest in <b>, behind an XSSI guard.
const suggestSerpClient = "gws-wiz-serp"

// SuggestURL is the generator's link for one variant, on the host of the
// query's country, in the generator's order of parameters and with its encoding
// — spaces as plus signs, as PHP's urlencode writes them. A job's language and
// country, which the generator's links never carried, follow as hl and gl, so
// the completions are the ones a searcher there is offered.
func SuggestURL(q Query, v SuggestVariant) string {
	host := "www.google.com"
	country := strings.ToLower(strings.TrimSpace(q.Country))
	if h, ok := ccTLD[country]; ok {
		host = h
	}
	var b strings.Builder
	b.WriteString("q=" + url.QueryEscape(v.Text))
	b.WriteString("&cp=" + strconv.Itoa(v.Cursor))
	b.WriteString("&client=" + suggestSerpClient + "&xssi=t&authuser=0")
	b.WriteString("&pq=" + url.QueryEscape(v.Key))
	b.WriteString("&dpr=1.5")
	if lang := strings.ToLower(strings.TrimSpace(q.Language)); lang != "" {
		b.WriteString("&hl=" + url.QueryEscape(lang))
	}
	if country != "" {
		b.WriteString("&gl=" + url.QueryEscape(country))
	}
	u := url.URL{Scheme: "https", Host: host, Path: "/complete/search", RawQuery: b.String()}
	return u.String()
}

// ErrNotSuggestions is an answer from the completion address that is not a
// list of completions: a challenge page, a refusal, or something else.
var ErrNotSuggestions = errors.New("google: not a list of completions")

// xssiGuard is the line Google puts before the JSON so it cannot be loaded as a
// script.
const xssiGuard = ")]}'"

var markup = regexp.MustCompile(`<[^>]+>`)

// ParseSuggestions reads the completions out of a gws-wiz-serp answer:
// [[["text",0,[…]],…],{…}] behind the XSSI guard. The <b> around the part
// Google added is taken off and the entities decoded, so a completion is the
// text a searcher would see.
func ParseSuggestions(body []byte) ([]string, error) {
	s := strings.TrimPrefix(string(body), "\ufeff")
	s = strings.TrimSpace(s)
	s = strings.TrimSpace(strings.TrimPrefix(s, xssiGuard))
	var envelope []json.RawMessage
	if err := json.Unmarshal([]byte(s), &envelope); err != nil || len(envelope) == 0 {
		return nil, ErrNotSuggestions
	}
	var items []json.RawMessage
	if err := json.Unmarshal(envelope[0], &items); err != nil {
		return nil, ErrNotSuggestions
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		var fields []json.RawMessage
		if json.Unmarshal(item, &fields) != nil || len(fields) == 0 {
			continue
		}
		var text string
		if json.Unmarshal(fields[0], &text) != nil {
			continue
		}
		text = strings.TrimSpace(html.UnescapeString(markup.ReplaceAllString(text, "")))
		if text != "" {
			out = append(out, text)
		}
	}
	return out, nil
}

// Complete asks one variant and reads its completions.
//
// An answer that is not a list is classified the way a search's is, so the
// caller can tell Google refusing the address — a challenge, a rate limit —
// from the road failing, and move the request on: HTTP 429 or a page that is
// not JSON is a wall, 403 a refusal, any other status of its own.
func (s *Suggester) Complete(ctx context.Context, q Query, v SuggestVariant) ([]string, error) {
	client := s.Client
	if client == nil {
		client = &http.Client{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, SuggestURL(q, v), nil)
	if err != nil {
		return nil, fmt.Errorf("google: build completion request: %w", err)
	}
	req.Header.Set("Accept", "*/*")
	if want := q.AcceptLanguage(); want != "" {
		req.Header.Set("Accept-Language", want)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google: completions: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSuggestBody))
	if err != nil {
		return nil, fmt.Errorf("google: read completions: %w", err)
	}
	refused := func(class Class, why error) error {
		return &ResponseError{Class: class, Query: v.Text, op: "complete", err: why}
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, refused(ClassWall, fmt.Errorf("%w: HTTP %d", ErrNotSuggestions, resp.StatusCode))
	case resp.StatusCode == http.StatusForbidden:
		return nil, refused(ClassBanned, fmt.Errorf("%w: HTTP %d", ErrNotSuggestions, resp.StatusCode))
	case resp.StatusCode != http.StatusOK:
		return nil, refused(ClassHTTP, fmt.Errorf("%w: HTTP %d", ErrNotSuggestions, resp.StatusCode))
	}
	out, err := ParseSuggestions(body)
	if err != nil {
		// A 200 that is not the list is the challenge page served in its
		// place, or the address being sent to /sorry and following there.
		return nil, refused(ClassWall, err)
	}
	return out, nil
}
