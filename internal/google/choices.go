// SPDX-License-Identifier: MIT

package google

import "sort"

// Choice is one value a search axis can be set to: what goes in the URL, and
// what a person reading a list of them recognises it by.
type Choice struct {
	Code string
	Name string
}

// The names are written in the language they name — Deutsch, not German — and
// the countries in English. That is what a chooser does everywhere else, and it
// is the one rendering that needs no translating: a reader looking for their own
// language finds it written the way they write it, whichever of this program's
// two languages the rest of the screen is in.
//
// Neither list is everything Google accepts. The engine takes well over a
// hundred interface languages and answers for every country there is; what a
// chooser is for is the ones somebody actually reaches for, and a list of a
// hundred and fifty is a list nobody reads. Anything absent can still be typed
// in — the box takes a code, and the list beside it is a shortcut rather than a
// gate.
var languages = []Choice{
	{"ar", "العربية"},
	{"bg", "Български"},
	{"cs", "Čeština"},
	{"da", "Dansk"},
	{"de", "Deutsch"},
	{"el", "Ελληνικά"},
	{"en", "English"},
	{"es", "Español"},
	{"et", "Eesti"},
	{"fa", "فارسی"},
	{"fi", "Suomi"},
	{"fr", "Français"},
	{"he", "עברית"},
	{"hi", "हिन्दी"},
	{"hr", "Hrvatski"},
	{"hu", "Magyar"},
	{"id", "Bahasa Indonesia"},
	{"it", "Italiano"},
	{"ja", "日本語"},
	{"kk", "Қазақ тілі"},
	{"ko", "한국어"},
	{"lt", "Lietuvių"},
	{"lv", "Latviešu"},
	{"ms", "Bahasa Melayu"},
	{"nl", "Nederlands"},
	{"no", "Norsk"},
	{"pl", "Polski"},
	{"pt", "Português"},
	{"ro", "Română"},
	{"ru", "Русский"},
	{"sk", "Slovenčina"},
	{"sl", "Slovenščina"},
	{"sr", "Српски"},
	{"sv", "Svenska"},
	{"th", "ไทย"},
	{"tr", "Türkçe"},
	{"uk", "Українська"},
	{"vi", "Tiếng Việt"},
	{"zh-CN", "简体中文"},
	{"zh-TW", "繁體中文"},
}

// countries are the ones a search can be pinned to. Every entry this program
// knows a domain for is here — a country named without one is answered from
// google.com, which is a different page — followed by the rest of the ones a
// chooser is reached for.
var countries = []Choice{
	{"ar", "Argentina"},
	{"at", "Austria"},
	{"au", "Australia"},
	{"be", "Belgium"},
	{"br", "Brazil"},
	{"by", "Belarus"},
	{"ca", "Canada"},
	{"ch", "Switzerland"},
	{"cl", "Chile"},
	{"co", "Colombia"},
	{"cz", "Czechia"},
	{"de", "Germany"},
	{"dk", "Denmark"},
	{"es", "Spain"},
	{"fi", "Finland"},
	{"fr", "France"},
	{"gb", "United Kingdom"},
	{"gr", "Greece"},
	{"hu", "Hungary"},
	{"id", "Indonesia"},
	{"ie", "Ireland"},
	{"il", "Israel"},
	{"in", "India"},
	{"it", "Italy"},
	{"jp", "Japan"},
	{"kr", "South Korea"},
	{"kz", "Kazakhstan"},
	{"mx", "Mexico"},
	{"my", "Malaysia"},
	{"nl", "Netherlands"},
	{"no", "Norway"},
	{"nz", "New Zealand"},
	{"pe", "Peru"},
	{"ph", "Philippines"},
	{"pl", "Poland"},
	{"pt", "Portugal"},
	{"ro", "Romania"},
	{"ru", "Russia"},
	{"se", "Sweden"},
	{"sg", "Singapore"},
	{"th", "Thailand"},
	{"tr", "Türkiye"},
	{"ua", "Ukraine"},
	{"us", "United States"},
	{"vn", "Vietnam"},
	{"za", "South Africa"},
}

// Languages are the interface languages a search can be asked in, for a screen
// offering a choice.
func Languages() []Choice { return append([]Choice(nil), languages...) }

// Countries are the countries a search can be pinned to, for a screen offering
// a choice.
//
// Every domain this program knows is in the list. A country this program has no
// domain for is still allowed — it reaches google.com with gl set, which is a
// different page from the country's own domain and sometimes the one that was
// wanted — so the check is that nothing with a domain is missing from the
// chooser, not that nothing outside the chooser may be typed.
func Countries() []Choice { return append([]Choice(nil), countries...) }

// KnownDomains is every domain this program has of its own, with the codes that
// reach it. It is what a chooser is checked against: some domains are reached by
// more than one code — the United Kingdom answers to both gb and uk — and a
// chooser offering one of the two covers that domain. Offering both would be a
// list with the same country in it twice.
func KnownDomains() map[string][]string {
	out := map[string][]string{}
	for code, host := range ccTLD {
		out[host] = append(out[host], code)
	}
	for host := range out {
		sort.Strings(out[host])
	}
	return out
}
