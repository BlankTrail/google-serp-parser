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
		"nav.pages":            "Screens",
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
		"job.state.running":    "running",
		"job.state.waiting":    "waiting its turn",
		// Said of a job whose file stopped arriving. It names what happened and
		// what follows from it, and it judges nothing: the queries that did arrive
		// are there to be looked at.
		"job.state.listunfinished": "the list never finished arriving, so nothing will run this",
		"history.title":            "History",
		"history.host":             "Site",
		"history.look":             "Show",
		"history.none":             "This site has not ranked in any run yet.",
		"history.taken":            "Taken",
		"history.job":              "Job",
		"history.query":            "Query",
		"history.rank":             "Position",
		"history.address":          "Address",

		"job.title":          "Job",
		"job.progress":       "Progress",
		"job.settings":       "Set up as",
		"job.stop":           "Stop",
		"job.resume":         "Carry on",
		"job.results":        "Results",
		"job.result.title":   "Title",
		"job.results.none":   "Nothing has been captured yet.",
		"job.results.capped": "Only the first results are drawn here. The export holds every one.",
		"job.export":         "Export",

		"new.title":             "New job",
		"form.name":             "Name",
		"form.queries":          "Queries, one per line",
		"form.pages":            "Pages per query",
		"form.country":          "Country",
		"form.language":         "Search language",
		"form.spec":             "Profile",
		"form.threads":          "Threads",
		"form.ports":            "Ports",
		"form.estimateonly":     "These two change the estimate and nothing else. A job runs on the pool this server was started with, at the threads it was given.",
		"form.estimate":         "Estimate",
		"form.start":            "Start",
		"form.name.required":    "The job needs a name to be filed under.",
		"form.queries.required": "There is not one query in the list.",
		"form.pages.positive":   "A query is taken to at least one page.",
		"form.norunner":         "This server was started to read the history, so it cannot run a job.",
		"form.notsetup":         "There is nothing to run a job on yet. Open the settings and set the connection up.",
		// The file. Every one of these says what happened and what is left behind,
		// because a list that stopped arriving leaves a job in the history and the
		// reader has to know it is there.
		"form.list.title":        "A list from a file",
		"form.list":              "The file, one query per line",
		"form.list.why":          "A file is written into the job line by line as it arrives, so its size is not this machine's memory. There is no estimate before it is read: how long the list is is what the file says, and this counts as it goes.",
		"form.upload":            "Upload and start",
		"form.upload.none":       "No file came with that.",
		"form.upload.unreadable": "That was not sent as a file, so there was nothing to read.",
		"form.upload.broke":      "The file stopped arriving part way. What reached this machine is kept as a job whose list never finished, and nothing will run it.",
		"form.upload.longline":   "One line of that file is longer than this program reads a line. What reached this machine is kept as a job whose list never finished, and nothing will run it.",
		"estimate.title":         "What this will cost",
		"estimate.searches":      "Queries × pages",
		"estimate.requests":      "Requests leaving this machine",
		"estimate.worst":         "At worst",
		"estimate.expected":      "Expected time",
		"estimate.caveat":        "from per-request costs measured on one list, on one day, whose own spread is a factor of twenty",
		"estimate.floor":         "Floor, counting the pauses alone",

		// The screen an operator watches. Every phrase here names a number or a
		// thing, and not one of them says whether the number is good. See
		// statePage for why that is the whole design of this screen.
		"state.title":            "Status",
		"state.running":          "Running now",
		"state.elapsed":          "Elapsed",
		"state.expected":         "The estimate quoted",
		"state.rest":             "Left at this pace",
		"state.idle":             "Nothing is running.",
		"state.invite":           "Set up a job",
		"state.failures":         "Refused",
		"state.failures.of":      "Of the queries settled so far",
		"state.failures.settled": "Settled so far",
		"state.pool":             "Ports",
		"state.pool.alive":       "Answering",
		"state.pool.ports":       "Ports open",
		"state.pool.rotate":      "Address changes",
		"state.pool.aside":       "Set aside",
		"state.pool.back":        "Back after resting",
		"state.pool.none":        "This server holds no ports.",
		"state.queue":            "Waiting their turn",
		"state.queue.none":       "Nothing is waiting.",
		"state.class.wall":       "a challenge or a rate limit",
		"state.class.banned":     "a refusal",
		"state.class.shell":      "a page carrying no results",
		"state.class.http":       "another status",
		"state.class.empty":      "nothing found",
		"state.class.serp":       "a page of results",
		"state.class.silent":     "no answer at all",

		// Setting the machine up. The key is spoken of by its last few characters
		// and never shown, which is why two phrases are needed where one box
		// stands.
		"settings.title":            "Settings",
		"settings.connection":       "Connection",
		"settings.address":          "Address",
		"settings.key":              "Key",
		"settings.key.saved":        "The key that is saved ends in",
		"settings.key.keep":         "Leave this empty to keep the key that is saved.",
		"settings.check":            "Check the connection",
		"settings.check.title":      "What the check found",
		"settings.check.nothing":    "The check found nothing to report.",
		"settings.finding.ok":       "In order",
		"settings.finding.warn":     "Note",
		"settings.finding.fail":     "Fail",
		"settings.source":           "Addresses to go out through",
		"settings.source.where":     "Read from",
		"settings.source.none":      "Not used",
		"settings.source.file":      "A file on this machine",
		"settings.source.url":       "An address",
		"settings.source.at":        "Where",
		"settings.source.refresh":   "Read again every, minutes",
		"settings.source.forms":     "One address to a line, written any of the usual ways: host and port, those two followed by a user name and a password, or preceded by them, or the two joined by an @. A scheme in front is read and may be left out. Blank lines are skipped, as are lines opening with # // or ;",
		"settings.pool":             "Ports",
		"settings.pause":            "Pause on one port between requests, seconds",
		"settings.language":         "Language of this interface",
		"settings.language.reader":  "Whichever the reader asks for",
		"settings.save":             "Save",
		"settings.running.asks":     "A job is running. These settings change where the work runs, so say what to do about that job.",
		"settings.running.cost":     "Stopping it costs a second warm-up and leaves it to be carried on. Waiting costs however long it has left.",
		"settings.running.now":      "Stop it and save",
		"settings.running.after":    "Save and wait for it to finish",
		"settings.unreadable":       "The settings file is there and could not be read. What is below is what this program starts from; saving writes it over the file.",
		"settings.opened.nothing":   "The settings are saved, and nothing could be opened with them, so the work goes on where it was. The reason is in the log this server writes.",
		"settings.address.unusable": "That is not an address this program can reach.",
		"settings.ports.count":      "The ports on a thread are a count of at least one.",
		"settings.threads.count":    "The threads are a count of at least one.",
		"settings.pause.length":     "A pause is a length of time, and no shorter than none.",
		"settings.refresh.length":   "How often a list is read again is a length of time, and no shorter than none.",
		"settings.source.unknown":   "That is not one of the places a list can be read from.",
		"settings.language.unknown": "This interface is not written in that language.",
	},
	LangRU: {
		"nav.language":         "Язык",
		"nav.pages":            "Экраны",
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
		"job.state.running":    "идёт",
		"job.state.waiting":    "ждёт очереди",

		"job.state.listunfinished": "список залит не до конца, выполнять это никто не станет",
		"history.title":            "История",
		"history.host":             "Сайт",
		"history.look":             "Показать",
		"history.none":             "Этот сайт ещё не занимал мест ни в одном задании.",
		"history.taken":            "Снято",
		"history.job":              "Задание",
		"history.query":            "Запрос",
		"history.rank":             "Позиция",
		"history.address":          "Адрес",

		"job.title":          "Задание",
		"job.progress":       "Ход",
		"job.settings":       "Как заведено",
		"job.stop":           "Остановить",
		"job.resume":         "Продолжить",
		"job.results":        "Результаты",
		"job.result.title":   "Заголовок",
		"job.results.none":   "Пока ничего не снято.",
		"job.results.capped": "Здесь показаны только первые строки. В выгрузке — все.",
		"job.export":         "Выгрузка",

		"new.title":             "Новое задание",
		"form.name":             "Название",
		"form.queries":          "Запросы, по одному в строке",
		"form.pages":            "Страниц на запрос",
		"form.country":          "Страна",
		"form.language":         "Язык поиска",
		"form.spec":             "Профиль",
		"form.threads":          "Потоков",
		"form.ports":            "Портов",
		"form.estimateonly":     "Эти два меняют только оценку. Задание выполняется на пуле, с которым поднят сервер, и на его потоках.",
		"form.estimate":         "Оценить",
		"form.start":            "Запустить",
		"form.name.required":    "Заданию нужно название, под которым оно ляжет в историю.",
		"form.queries.required": "В списке нет ни одного запроса.",
		"form.pages.positive":   "Запрос берётся хотя бы на одну страницу.",
		"form.norunner":         "Этот сервер поднят читать историю и выполнять задания не может.",
		"form.notsetup":         "Выполнять задание пока не на чем. Откройте настройки и настройте связь.",

		"form.list.title":        "Список файлом",
		"form.list":              "Файл, по одному запросу в строке",
		"form.list.why":          "Файл ложится в задание построчно по мере поступления, поэтому его размер не упирается в память этой машины. Оценки до чтения нет: длину списка знает файл, и она считается по ходу.",
		"form.upload":            "Залить и запустить",
		"form.upload.none":       "Файла с этим не пришло.",
		"form.upload.unreadable": "Это пришло не файлом, читать было нечего.",
		"form.upload.broke":      "Файл перестал поступать на середине. То, что дошло, осталось заданием с недозалитым списком, и выполнять его никто не станет.",
		"form.upload.longline":   "В этом файле есть строка длиннее, чем программа читает строку. То, что дошло, осталось заданием с недозалитым списком, и выполнять его никто не станет.",
		"estimate.title":         "Во что это обойдётся",
		"estimate.searches":      "Запросов × страниц",
		"estimate.requests":      "Запросов уйдёт с этой машины",
		"estimate.worst":         "В худшем случае",
		"estimate.expected":      "Ожидаемое время",
		"estimate.caveat":        "по стоимости запроса, измеренной на одном списке за один день, с разбросом в двадцать раз",
		"estimate.floor":         "Нижняя граница, считая одни паузы",

		"state.title":            "Состояние",
		"state.running":          "Идёт сейчас",
		"state.elapsed":          "Прошло",
		"state.expected":         "Оценка обещала",
		"state.rest":             "Осталось по этой скорости",
		"state.idle":             "Ничего не идёт.",
		"state.invite":           "Завести задание",
		"state.failures":         "Отказы",
		"state.failures.of":      "От завершённых запросов",
		"state.failures.settled": "Завершено запросов",
		"state.pool":             "Порты",
		"state.pool.alive":       "Отвечает",
		"state.pool.ports":       "Портов открыто",
		"state.pool.rotate":      "Смен адреса",
		"state.pool.aside":       "Отложено",
		"state.pool.back":        "Вернулось после отлёжки",
		"state.pool.none":        "На этом сервере портов нет.",
		"state.queue":            "Ждут очереди",
		"state.queue.none":       "Никто не ждёт.",
		"state.class.wall":       "проверка или ограничение частоты",
		"state.class.banned":     "отказ",
		"state.class.shell":      "страница без результатов",
		"state.class.http":       "другой ответ",
		"state.class.empty":      "ничего не найдено",
		"state.class.serp":       "страница выдачи",
		"state.class.silent":     "ответа не было",

		"settings.title":            "Настройки",
		"settings.connection":       "Связь",
		"settings.address":          "Адрес",
		"settings.key":              "Ключ",
		"settings.key.saved":        "Сохранённый ключ оканчивается на",
		"settings.key.keep":         "Оставьте пустым, чтобы сохранённый ключ остался прежним.",
		"settings.check":            "Проверить связь",
		"settings.check.title":      "Что показала проверка",
		"settings.check.nothing":    "Проверке нечего сообщить.",
		"settings.finding.ok":       "В порядке",
		"settings.finding.warn":     "Замечание",
		"settings.finding.fail":     "Отказ",
		"settings.source":           "Адреса, через которые выходить",
		"settings.source.where":     "Читать",
		"settings.source.none":      "Не используются",
		"settings.source.file":      "Из файла на этой машине",
		"settings.source.url":       "По адресу",
		"settings.source.at":        "Откуда",
		"settings.source.refresh":   "Перечитывать раз в, мин",
		"settings.source.forms":     "По одному адресу в строке, любой привычной записью: хост и порт, эти два и следом имя с паролем, или имя с паролем впереди, или те и другие через @. Схема впереди читается, но не обязательна. Пустые строки пропускаются, как и строки, начатые с # // или ;",
		"settings.pool":             "Порты",
		"settings.pause":            "Пауза на одном порту между запросами, с",
		"settings.language":         "Язык этого интерфейса",
		"settings.language.reader":  "Какой попросит читающий",
		"settings.save":             "Сохранить",
		"settings.running.asks":     "Идёт задание. Эти настройки меняют то, где выполняется работа, — скажите, что делать с ним.",
		"settings.running.cost":     "Остановка стоит повторного разогрева и оставляет задание к продолжению. Ожидание стоит того времени, что заданию осталось.",
		"settings.running.now":      "Остановить и сохранить",
		"settings.running.after":    "Сохранить и дождаться конца",
		"settings.unreadable":       "Файл настроек есть, и прочитать его не удалось. Ниже — то, с чего эта программа начинает; сохранение перезапишет файл.",
		"settings.opened.nothing":   "Настройки сохранены, и открыть по ним ничего не удалось, поэтому работа выполняется там же, где и раньше. Причина — в журнале сервера.",
		"settings.address.unusable": "По этому адресу программа обратиться не может.",
		"settings.ports.count":      "Портов на поток — хотя бы один.",
		"settings.threads.count":    "Потоков — хотя бы один.",
		"settings.pause.length":     "Пауза — это время, и не короче нуля.",
		"settings.refresh.length":   "Как часто перечитывать список — это время, и не короче нуля.",
		"settings.source.unknown":   "Список не читают из такого места.",
		"settings.language.unknown": "На этом языке интерфейс не написан.",
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

// pickLang decides which language a request is answered in on a server that
// has been told nothing about which one to prefer.
func pickLang(r *http.Request) Lang { return chooseLang(r, "") }

// chooseLang decides which language a request is answered in.
//
// The order is the reader's own words first, then what they said last time,
// then what this machine was set up to answer in, then what their browser
// prefers. A language this program does not have is not an answer at any step,
// and falls through to the next: a mistyped address must not undo a choice the
// reader made.
//
// The saved language stands below both of the reader's own answers and above
// the browser's, because it is a decision somebody made on this machine and the
// header is a guess about a reader this machine has never met.
func chooseLang(r *http.Request, saved Lang) Lang {
	if l, ok := langOf(r.URL.Query().Get(langQuery)); ok {
		return l
	}
	if c, err := r.Cookie(langCookie); err == nil {
		if l, ok := langOf(c.Value); ok {
			return l
		}
	}
	if l, ok := langOf(string(saved)); ok {
		return l
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
func (s *Server) rememberLang(w http.ResponseWriter, r *http.Request) Lang {
	lang := chooseLang(r, s.tongue())
	if _, asked := langOf(r.URL.Query().Get(langQuery)); asked {
		writeLang(w, lang)
	}
	return lang
}

// writeLang writes a language down for as long as a language is worth
// remembering.
//
// The cookie is closed to scripts because nothing in the browser reads it: the
// language is decided on the server, before a page exists.
func writeLang(w http.ResponseWriter, lang Lang) {
	http.SetCookie(w, &http.Cookie{
		Name:     langCookie,
		Value:    string(lang),
		Path:     "/",
		MaxAge:   int(langMemory / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
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
