// SPDX-License-Identifier: MIT

package google

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Related says whether a completion still has something of its key in it: a
// word of the key, as written or in another of its forms.
//
// It is the cheap test of whether a completion is about the key at all. Most
// of what a job collects is — on one job of 7773 Russian keys, 329 of 482
// thousand completions carried every word of their key and 103 thousand some
// of them — but 47 thousand carried none, and three in four of those were
// what a letter typed after the key led Google to instead: a word that merely
// begins the same way, a town, a song. The fourth that were not are what
// Google does with a key besides finishing it, and each is kept here:
//
//   - another form of a word: кофемашина, кофемашины, кофемашину;
//   - a misspelling put right, a letter or two apart: expresso → espresso;
//   - a Russian word typed in Latin letters, or the other way about:
//     kofemashina → кофемашина;
//   - two words joined or one split: coffee maker → coffeemaker,
//     coffeemaker → coffee maker, coffee_maker → coffee maker.
//
// Measured on the same job it marks 26.5 thousand of the 482 thousand, 5.5 per
// cent, and a sample of them read as the letter-led leftovers above. What it
// cannot keep is a translation or a synonym with no letters in common — фото
// редактор → photo editor, нейросеть → искусственный интеллект — about one
// completion in a hundred. Nor can it tell a completion that keeps one word of
// the key and turns to another subject with it: that is a matter of meaning,
// not of letters, and needs a model that knows it.
//
// The words that carry no subject — как, для, how, for — are not counted, so a
// completion sharing only "для" with its key is not related by it. Short words
// — ai, 3d — are counted and must match whole: left out, a job of keys that
// all end in "ai" lost as many completions about its keys as it set aside
// leftovers.
func Related(key, completion string) bool {
	keyWords := significant(wordsOf(key))
	if len(keyWords) == 0 {
		// A key of nothing but words like как and для has nothing to compare;
		// whatever comes back is let through rather than all of it refused.
		return true
	}
	words := wordsOf(completion)
	for _, k := range keyWords {
		for _, w := range words {
			if sameWord(k, w) {
				return true
			}
		}
	}
	// Joined or split, word for word: coffee maker → coffeemaker,
	// coffeemaker → coffee maker. Compared whole words at a time, so jpl airport is not pl ai.
	keyRuns, runs := joins(keyWords), joins(words)
	for joined, n := range runs {
		if m, ok := keyRuns[joined]; ok && (n > 1 || m > 1) {
			return true
		}
	}
	// A key written as one long word, a brand or an address, of which the
	// completion spells out a word: bestcoffeegrinder → grinder for
	// espresso. Four letters at least, so a word as short as
	// art is not found inside every key that has it.
	for _, k := range keyWords {
		for _, w := range words {
			if n := utf8.RuneCountInString(w); n >= 4 && utf8.RuneCountInString(k) > n && strings.Contains(k, w) {
				return true
			}
		}
	}
	return false
}

// joins is every run of up to four words next to each other written as one,
// with how many words went into it — the most, where two runs read the same.
func joins(words []string) map[string]int {
	out := map[string]int{}
	for i := range words {
		joined := ""
		for n := 1; n <= 4 && i+n <= len(words); n++ {
			joined += words[i+n-1]
			if n > out[joined] {
				out[joined] = n
			}
		}
	}
	return out
}

// wordsOf is a text's words, lower-cased and in Latin letters: a word is a run
// of letters and digits, so coffee_maker is two.
func wordsOf(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for i, f := range fields {
		fields[i] = latin(f)
	}
	return fields
}

// significant is the words that say what a text is about: the words that do
// not are left out, and so are single letters.
func significant(words []string) []string {
	out := words[:0:0]
	for _, w := range words {
		if utf8.RuneCountInString(w) < 2 || stopWords[w] {
			continue
		}
		out = append(out, w)
	}
	return out
}

// sameWord says whether two words are one word: the same, the same stem, or a
// letter or two apart. Short words must match whole — ai is not air — and the
// longer a word, the more of it two forms may differ in.
func sameWord(a, b string) bool {
	if a == b {
		return true
	}
	la, lb := utf8.RuneCountInString(a), utf8.RuneCountInString(b)
	if la < 4 || lb < 4 {
		return false
	}
	// A stem in common: the first four letters and most of the shorter word.
	// фото and фотографий, нейросеть and нейросети.
	common := commonPrefix(a, b)
	shorter := min(la, lb)
	if common >= 4 && common*3 >= shorter*2 {
		return true
	}
	// A misspelling, or the same word in the other script spelled a little
	// differently: one letter apart for a word of up to six, two beyond.
	allowed := 1
	if shorter >= 7 {
		allowed = 2
	}
	return withinEdits(a, b, allowed)
}

// commonPrefix is how many letters two words begin with in common.
func commonPrefix(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	n := 0
	for n < len(ra) && n < len(rb) && ra[n] == rb[n] {
		n++
	}
	return n
}

// withinEdits says whether two words are at most n letters put in, taken out
// or changed apart.
func withinEdits(a, b string, n int) bool {
	ra, rb := []rune(a), []rune(b)
	if d := len(ra) - len(rb); d > n || -d > n {
		return false
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			best = min(best, cur[j])
		}
		if best > n {
			return false
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)] <= n
}

// latin writes a Cyrillic word in Latin letters, the way a searcher typing a
// Russian or Ukrainian word on a Latin keyboard spells it, so нейросети and
// neyroseti are one word apart by a letter at most. A word in any other script
// is left as it is.
func latin(word string) string {
	var b strings.Builder
	for _, r := range word {
		if t, ok := cyrillic[r]; ok {
			b.WriteString(t)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

var cyrillic = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh",
	'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o",
	'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "h", 'ц': "ts",
	'ч': "ch", 'ш': "sh", 'щ': "sch", 'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu",
	'я': "ya", 'і': "i", 'ї': "yi", 'є': "ye", 'ґ': "g",
}

// stopWords are the words that say nothing about what a text is about, in
// Latin letters as wordsOf leaves them: Russian, Ukrainian and English.
var stopWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		как для что это или при без над под про через чем где кто когда какой какая какие
		мне мой моя все всё так уже еще ещё тоже только можно нужно надо есть был была
		на по из от до за со во ко не ни же ли бы то об
		як для що це або при без над під про через чим де хто коли який яка які
		на по із від до за зі не ні же чи би то
		the of for to in on at by with from and or how what why who where when which
		is are be can do does an my your it its this that`) {
		m[latin(w)] = true
	}
	return m
}()
