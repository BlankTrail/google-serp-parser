// SPDX-License-Identifier: MIT

package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Lang is one of the languages the interface is written in, named by its
// two-letter code because that is what a browser and a link both carry.
type Lang string

// The languages this program says everything in.
const (
	LangEN Lang = "en"
	LangRU Lang = "ru"
)

// langQuery is the name of the parameter a reader switches language with. It
// travels in the address so a switched page can be bookmarked and sent on.
const langQuery = "lang"

// langCookie is where the chosen language is written down. The name carries the
// program's own prefix because every program served from this machine shares
// one cookie jar, and a bare "lang" would be fought over.
const langCookie = "gserp_lang"

// langMemory is how long a chosen language is remembered. It outlasts the
// session on purpose: choosing a language is a fact about the reader, not about
// the visit, and asking again next week is asking again for nothing.
const langMemory = 365 * 24 * time.Hour

// ErrMissingText reports a language short of text another language has.
var ErrMissingText = errors.New("web: a language is missing text the other has")

// Languages is every language the interface is written in, in the order the
// switcher offers them.
//
// Each caller gets its own slice, so one that sorts what it was handed cannot
// reorder the switcher for everybody after it.
func Languages() []Lang { return []Lang{LangEN, LangRU} }

// Name is what a language calls itself.
//
// It is not translated, and it is not in the catalogue: the switcher is read by
// someone who cannot read the page they are looking at, and a label they cannot
// read tells them nothing.
func (l Lang) Name() string {
	if l == LangRU {
		return "Русский"
	}
	return "English"
}

// T is the text of one key in this language, or the key itself when there is
// none.
//
// The key comes back rather than an empty string because an empty string on a
// page is invisible, and a key is ugly and reports itself, which is what an
// unfinished translation should do.
func (l Lang) T(key string) string {
	if text, ok := catalogue[l][key]; ok {
		return text
	}
	return key
}

// catalogue is everything the interface says, in every language it says it in.
//
// Keys name the place the words appear rather than the words themselves. A key
// named after its wording outlives that wording by exactly one edit, and then
// says the opposite of what it is called.
var catalogue = map[Lang]map[string]string{
	LangEN: {
		"nav.language":         "Language",
		"jobs.title":           "Jobs",
		"jobs.none":            "No jobs yet.",
		"jobs.name":            "Name",
		"jobs.started":         "Started",
		"jobs.queries":         "Queries",
		"jobs.done":            "Done",
		"jobs.failed":          "Failed",
		"jobs.left":            "Left",
		"jobs.state":           "State",
		"job.state.finished":   "finished",
		"job.state.unfinished": "unfinished",
		"history.title":        "History",
		"history.host":         "Site",
		"history.look":         "Show",
		"history.none":         "This site has not ranked in any run yet.",
		"history.taken":        "Taken",
		"history.job":          "Job",
		"history.query":        "Query",
		"history.rank":         "Position",
		"history.address":      "Address",

		"new.title":             "New job",
		"form.name":             "Name",
		"form.queries":          "Queries, one per line",
		"form.pages":            "Pages per query",
		"form.country":          "Country",
		"form.language":         "Search language",
		"form.spec":             "Profile",
		"form.threads":          "Threads",
		"form.ports":            "Ports",
		"form.estimate":         "Estimate",
		"form.start":            "Start",
		"form.name.required":    "The job needs a name to be filed under.",
		"form.queries.required": "There is not one query in the list.",
		"form.pages.positive":   "A query is taken to at least one page.",
		"form.norunner":         "This server was started to read the history, so it cannot run a job.",
		"estimate.title":        "What this will cost",
		"estimate.searches":     "Queries × pages",
		"estimate.requests":     "Requests leaving this machine",
		"estimate.worst":        "At worst",
		"estimate.expected":     "Expected time",
		"estimate.caveat":       "from per-request costs measured on one list, on one day, whose own spread is a factor of twenty",
		"estimate.floor":        "Floor, counting the pauses alone",
	},
	LangRU: {
		"nav.language":         "Язык",
		"jobs.title":           "Задания",
		"jobs.none":            "Заданий пока нет.",
		"jobs.name":            "Название",
		"jobs.started":         "Начато",
		"jobs.queries":         "Запросов",
		"jobs.done":            "Готово",
		"jobs.failed":          "Ошибок",
		"jobs.left":            "Осталось",
		"jobs.state":           "Состояние",
		"job.state.finished":   "завершено",
		"job.state.unfinished": "не завершено",
		"history.title":        "История",
		"history.host":         "Сайт",
		"history.look":         "Показать",
		"history.none":         "Этот сайт ещё не занимал мест ни в одном задании.",
		"history.taken":        "Снято",
		"history.job":          "Задание",
		"history.query":        "Запрос",
		"history.rank":         "Позиция",
		"history.address":      "Адрес",

		"new.title":             "Новое задание",
		"form.name":             "Название",
		"form.queries":          "Запросы, по одному в строке",
		"form.pages":            "Страниц на запрос",
		"form.country":          "Страна",
		"form.language":         "Язык поиска",
		"form.spec":             "Профиль",
		"form.threads":          "Потоков",
		"form.ports":            "Портов",
		"form.estimate":         "Оценить",
		"form.start":            "Запустить",
		"form.name.required":    "Заданию нужно название, под которым оно ляжет в историю.",
		"form.queries.required": "В списке нет ни одного запроса.",
		"form.pages.positive":   "Запрос берётся хотя бы на одну страницу.",
		"form.norunner":         "Этот сервер поднят читать историю и выполнять задания не может.",
		"estimate.title":        "Во что это обойдётся",
		"estimate.searches":     "Запросов × страниц",
		"estimate.requests":     "Запросов уйдёт с этой машины",
		"estimate.worst":        "В худшем случае",
		"estimate.expected":     "Ожидаемое время",
		"estimate.caveat":       "по стоимости запроса, измеренной на одном списке за один день, с разбросом в двадцать раз",
		"estimate.floor":        "Нижняя граница, считая одни паузы",
	},
}

// checkCatalogue refuses a catalogue whose languages do not hold the same keys.
//
// It runs before the first request rather than at the moment of rendering,
// because a phrase missing from one language shows up as a bare key on one page
// in one language, and only a reader of that language would ever see it.
func checkCatalogue(c map[Lang]map[string]string) error {
	for _, spoken := range Languages() {
		for _, other := range Languages() {
			for key := range c[spoken] {
				if _, ok := c[other][key]; !ok {
					return fmt.Errorf("%w: %s has no %q", ErrMissingText, other, key)
				}
			}
		}
	}
	return nil
}

// langOf reads a language out of whatever carried it, and says whether this
// program has it.
//
// The region is cut off because a browser asking for ru-RU is asking for
// Russian, and this program has one Russian.
func langOf(tag string) (Lang, bool) {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if cut := strings.IndexByte(tag, '-'); cut >= 0 {
		tag = tag[:cut]
	}
	for _, l := range Languages() {
		if string(l) == tag {
			return l, true
		}
	}
	return "", false
}

// fromAcceptLanguage reads the first language of the header this program has.
//
// A browser lists its languages in the order it prefers them, so the first one
// that can be answered is the best answer available, and the weights that
// follow the semicolons say nothing more. Reading them would mean sorting a
// list that already arrived sorted.
func fromAcceptLanguage(header string) (Lang, bool) {
	for _, offered := range strings.Split(header, ",") {
		if weight := strings.IndexByte(offered, ';'); weight >= 0 {
			offered = offered[:weight]
		}
		if l, ok := langOf(offered); ok {
			return l, true
		}
	}
	return "", false
}

// pickLang decides which language a request is answered in.
//
// The order is the reader's own words first, then what they said last time,
// then what their browser prefers. A language this program does not have is not
// an answer at any step, and falls through to the next: a mistyped address must
// not undo a choice the reader made.
func pickLang(r *http.Request) Lang {
	if l, ok := langOf(r.URL.Query().Get(langQuery)); ok {
		return l
	}
	if c, err := r.Cookie(langCookie); err == nil {
		if l, ok := langOf(c.Value); ok {
			return l
		}
	}
	if l, ok := fromAcceptLanguage(r.Header.Get("Accept-Language")); ok {
		return l
	}
	return LangEN
}

// rememberLang picks the language and, when the reader asked for one outright,
// writes it down so the next page needs no asking.
//
// Only an outright request is stored. Writing down what the browser preferred
// would freeze a guess into a decision the reader never made, and a browser
// reconfigured afterwards would go on being ignored.
//
// The cookie is closed to scripts because nothing in the browser reads it: the
// language is decided here, before a page exists.
func rememberLang(w http.ResponseWriter, r *http.Request) Lang {
	lang := pickLang(r)
	if _, asked := langOf(r.URL.Query().Get(langQuery)); asked {
		http.SetCookie(w, &http.Cookie{
			Name:     langCookie,
			Value:    string(lang),
			Path:     "/",
			MaxAge:   int(langMemory / time.Second),
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}
	return lang
}

// langLink is one language as the switcher offers it.
type langLink struct {
	Name    string
	URL     string
	Current bool
}

// switcher offers every language at the address being read right now.
//
// The rest of the address is carried across, because switching language is not
// a request to go somewhere else, and a switch that returns the reader to the
// front page loses whatever they were looking at.
func switcher(r *http.Request, now Lang) []langLink {
	langs := Languages()
	links := make([]langLink, 0, len(langs))
	for _, l := range langs {
		q := r.URL.Query()
		q.Set(langQuery, string(l))
		at := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
		links = append(links, langLink{Name: l.Name(), URL: at.String(), Current: l == now})
	}
	return links
}
