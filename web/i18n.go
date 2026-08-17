// SPDX-License-Identifier: MIT

package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// Lang is one of the languages the interface is written in, named by its
// two-letter code because that is what a browser and a link both carry.
type Lang string

// The languages this program is built to say everything in. A bare binary with
// no file beside it answers in both.
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
// switcher offers them: the ones built in first, then whatever was read from
// files, in the order the directory named them.
//
// Each caller gets its own slice, so one that sorts what it was handed cannot
// reorder the switcher for everybody after it.
func Languages() []Lang { return slices.Clone(spoken.Load().langs) }

// Name is what a language calls itself.
//
// It is not translated: the switcher is read by someone who cannot read the
// page they are looking at, and a label they cannot read tells them nothing.
//
// A language read from a file gives its own name under langNameKey. One that
// does not is offered by its code, which is at least what its reader would have
// typed to ask for it.
func (l Lang) Name() string {
	if name := spoken.Load().names[l]; name != "" {
		return name
	}
	return string(l)
}

// T is the text of one key in this language, or the key itself when there is
// none.
//
// The key comes back rather than an empty string because an empty string on a
// page is invisible, and a key is ugly and reports itself, which is what an
// unfinished translation should do.
func (l Lang) T(key string) string {
	if text, ok := spoken.Load().say[l][key]; ok {
		return text
	}
	return key
}

// catalogue is everything the interface says, in every language built into this
// program. It is what a file beside the program adds to and overrides, and it is
// never written to: a file is merged into a copy.
//
// Keys name the place the words appear rather than the words themselves. A key
// named after its wording outlives that wording by exactly one edit, and then
// says the opposite of what it is called.
var catalogue = map[Lang]map[string]string{
	LangEN: {
		"nav.language":         "Language",
		"nav.pages":            "Screens",
		"jobs.title":           "Jobs",
		"jobs.delete":          "Delete",
		"jobs.delete.sure":     "Delete this job and everything it gathered? This cannot be undone.",
		"jobs.delete.running":  "This job is running or waiting to. Stop it first.",
		"jobs.none":            "No jobs yet.",
		"jobs.name":            "Name",
		"jobs.started":         "Started",
		"jobs.queries":         "Queries",
		"jobs.done":            "Done",
		"jobs.failed":          "Failed",
		"jobs.left":            "Left",
		"jobs.dropped":         "Repeats dropped",
		"jobs.state":           "State",
		"job.state.finished":   "finished",
		"job.state.unfinished": "unfinished",
		"job.state.starting":   "reaching the identities",
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

		"job.title":            "Job",
		"job.progress":         "Progress",
		"job.reshape":          "Save these settings",
		"job.reshape.why":      "Saving changes the job, not the ports that are open now. A job already running keeps the ports it started with; what is saved here is what it will open next time it starts — after a stop, or when the queue reaches it.",
		"job.reshape.done":     "Saved. The job will come up on this next time it starts.",
		"job.reshape.finished": "This job has run, so there is nothing left for a pool to be raised for.",
		"job.settings":         "Set up as",
		"job.stop":             "Stop",
		"job.resume":           "Carry on",
		"job.results":          "Results",
		"job.result.title":     "Title",
		"job.results.none":     "Nothing has been captured yet.",
		"job.results.sample":   "The newest results only, at most this many:",
		"job.export":           "Export",

		// What an index job established. Both answers say what was found and
		// neither is dressed as good news or bad: whether an address missing from
		// the index is a problem is the reader's to say.
		"job.verdicts":       "Addresses",
		"job.verdicts.none":  "No address has been checked yet.",
		"job.verdict":        "In the index",
		"job.verdict.held":   "found",
		"job.verdict.absent": "not found",

		// Where a position check found its site. A phrase the site was not found
		// for says so, and a phrase nobody has reached yet is not on the list: the
		// two are different answers and neither is dressed as news.
		"job.standings":      "Positions",
		"job.standings.none": "No phrase has been checked yet.",
		"job.rank.none":      "not found",

		"new.title":             "New job",
		"form.name":             "Name",
		"form.queries":          "Queries, one per line",
		"form.pages":            "Pages per query",
		"form.device":           "Result page",
		"form.device.desktop":   "Desktop",
		"form.device.mobile":    "Mobile",
		"form.country":          "Country",
		"form.language":         "Search language",
		"form.threads":          "Threads",
		"form.ports":            "Ports per thread",
		"form.estimateonly":     "These two change the estimate and nothing else. A job runs on the pool this server was started with, at the threads it was given.",
		"form.estimate":         "Estimate",
		"form.start":            "Start",
		"form.name.required":    "The job needs a name to be filed under.",
		"form.queries.required": "There is not one query in the list.",
		"form.pages.positive":   "A query is taken to at least one page.",
		// What the job asks Google. The lines of the list mean different things
		// under the two, which is why the choice stands above the list rather
		// than beside the depth.
		"form.kind":          "Kind of job",
		"form.kind.parse":    "Parsing",
		"form.kind.position": "Position check",
		"form.kind.index":    "Index check",
		"form.kind.why":      "Parsing reads the list as phrases and writes down everything each one came back with. A position check reads the same list and reports where one site stood for each phrase, or that it was not in the pages taken. An index check reads the list as addresses and asks Google whether it holds each one; it takes a single page per address, whatever the depth says, because the first page settles it.",
		"form.kind.unknown":  "That is not one of the things a job asks.",
		// The site a position check is about. The sentence says what cannot be
		// changed later and why, because a reader who finds that out afterwards
		// has a run to do again.
		"form.target":          "Site to look for",
		"form.target.why":      "A position check needs this and the other two kinds ignore it. A bare site counts any page of it, including its subdomains; an address with a path counts that address alone. It is fixed when the job starts: the positions already found were measured against it.",
		"form.target.required": "A position check needs the site it is about.",
		// What the job throws away as it writes, and how many it threw. Both say
		// what happened and neither judges it: whether six repeats in ten is what
		// the reader meant is theirs to say. The sentence names the part that
		// cannot be taken back, because it cannot — a dropped result is never
		// written, and having it takes another run.
		"form.from":           "Where the phrases come from",
		"form.from.box":       "Typed in below",
		"form.from.file":      "From a file",
		"form.from.why":       "Both boxes are here whichever is chosen, because a box that appeared and disappeared would need a script and this page has none. The one the switch names is the one that is read; the other is ignored. A file is read as it arrives, so its size is not bounded by this machine's memory.",
		"form.keep":           "What to keep of each result",
		"form.keep.why":       "Only what is ticked is written down, so a job that wants a list of addresses writes a fraction of what it would otherwise — on ten million results that is the difference between one file and several. The place a result stood is always kept: without it a file is a bag rather than a result page. Untick the site and this job's results will never be found on the History screen, which searches by site.",
		"form.keep.none":      "A job has to keep something of each result.",
		"form.keep.needsurl":  "Dropping repeats by url needs the url kept.",
		"form.keep.needshost": "Dropping repeats by domain needs the domain kept.",
		"field.ads":           "Paid placements",
		"field.related":       "Related searches",
		"job.export.sep":      "Separator for txt",
		"job.export.sep.why":  "Left empty, a txt file separates its columns with a tab. The other formats ignore it.",
		"job.export.ads":      "paid placements",
		"job.export.related":  "related searches",
		"field.title":         "Title",
		"field.url":           "Address",
		"field.link":          "Address as the page carried it",
		"field.host":          "Domain",
		"field.snippet":       "Snippet",
		"field.path":          "Path as the page drew it",
		"form.pool":           "The pool this job runs on",
		"form.tries":          "Tries per phrase",
		"form.pause":          "Pause on one identity, seconds",
		"job.reshape.total":   "Ports opened is threads times ports per thread.",
		"form.tries.why":      "How many identities one phrase may be taken to before it is written off. A poor list refuses most requests, and a limit set low loses the job rather than the request. These four are the job's own: a pool is raised for it when it starts and taken down when it lets go, and they can be changed on the job's own page.",
		"form.pause.why":      "How long one identity waits before it is asked a second time. It was a setting of this machine and is a property of the run: how hard a list may be pushed depends on the list and on what is being asked of it. Nought lets the pool work one out from its own size.",
		"form.unique":         "Dropping duplicates",
		"form.unique.off":     "Keep every result",
		"form.unique.url":     "Unique by url",
		"form.unique.host":    "Unique by domain",
		"form.unique.why":     "A repeat is dropped as the results arrive and is never written down, so it is not in the history and not in the export. This holds within this job alone — a site caught last month turns up again tonight — and it cannot be undone once the job has run. How many were dropped is shown on the job's own page.",
		"form.unique.unknown": "That is not one of the ways of dropping repeats.",
		"form.norunner":       "This server was started to read the history, so it cannot run a job.",
		"form.notsetup":       "There is nothing to run a job on yet. Open the settings and set the connection up.",
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
		"estimate.floor":         "Not sooner than",

		// The screen an operator watches. Every phrase here names a number or a
		// thing, and not one of them says whether the number is good. See
		// statePage for why that is the whole design of this screen.
		"state.title":            "Status",
		"state.running":          "Running now",
		"state.elapsed":          "Elapsed",
		"state.speed":            "Queries a minute now",
		"state.speed.pages":      "Pages a minute now",
		"job.asking":             "Fetching now",
		"state.answered":         "Answered",
		"state.answered.of":      "Of the queries settled so far",
		"state.rest":             "Left at this pace",
		"state.idle":             "Nothing is running.",
		"state.invite":           "Set up a job",
		"state.failures":         "Refused",
		"state.failures.of":      "Of the queries settled so far",
		"state.failures.settled": "Settled so far",
		"state.pool":             "Ports",
		"state.pool.alive":       "Answering",
		"state.pool.ports":       "Ports open",
		"state.pool.warm":        "Of them warm",
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
		"settings.title":             "Settings",
		"settings.connection":        "Connection",
		"settings.address":           "Address",
		"settings.key":               "Key",
		"settings.key.saved":         "The key that is saved ends in",
		"settings.key.keep":          "Leave this empty to keep the key that is saved.",
		"settings.check":             "Check the connection",
		"settings.check.title":       "What the check found",
		"settings.check.good":        "The connection works",
		"settings.check.good.detail": "The service answered, the key was accepted and the licence is active.",
		"settings.check.nothing":     "The check found nothing to report.",
		"settings.finding.ok":        "In order",
		"settings.finding.warn":      "Note",
		"settings.finding.fail":      "Fail",
		"settings.source":            "Proxy settings",
		"settings.source.where":      "Read from",
		"settings.source.none":       "Not used",
		"settings.source.file":       "A file on this machine",
		"settings.source.url":        "An address",
		"settings.source.at":         "Where",
		"settings.source.refresh":    "Read again every, minutes",
		"settings.source.forms":      "One address to a line, written any of the usual ways: host and port, those two followed by a user name and a password, or preceded by them, or the two joined by an @. A scheme in front is read and may be left out. Blank lines are skipped, as are lines opening with # // or ;",
		"browse.title":               "Choose a list",
		"browse.up":                  "Up one",
		"browse.left":                "Files not shown here:",
		"browse.none":                "Nothing here to choose.",
		"browse.back":                "Back to the settings",
		"browse.unreadable":          "This machine will not let this program read that folder.",
		"settings.source.choose":     "Choose",

		"settings.hot":              "Identities kept warm",
		"settings.hot.ports":        "How many",
		"settings.hot.why":          "Kept open between jobs and given one ordinary search every quarter of an hour, so they answer at once instead of meeting a challenge first. Nought keeps none. A job of this kind of page runs on them and opens what more it needs; a job of the other kind opens its own.",
		"settings.hot.count":        "How many identities to keep warm has to be a whole number, and nought keeps none.",
		"settings.hot.kind":         "That is not a kind of result page this program opens identities for.",
		"settings.language":         "Interface language",
		"settings.language.reader":  "Chosen automatically",
		"settings.save":             "Save",
		"settings.unreadable":       "The settings file is there and could not be read. What is below is what this program starts from; saving writes it over the file.",
		"settings.opened.nothing":   "The settings are saved, and nothing could be opened with them, so the work goes on where it was. The reason is in the log this server writes.",
		"settings.address.unusable": "That is not an address this program can reach.",
		"settings.refresh.length":   "How often a list is read again is a length of time, and no shorter than none.",
		"settings.source.unknown":   "That is not one of the places a list can be read from.",
		"settings.language.unknown": "This interface is not written in that language.",
	},
	LangRU: {
		"nav.language":         "Язык",
		"nav.pages":            "Экраны",
		"jobs.title":           "Задания",
		"jobs.delete":          "Удалить",
		"jobs.delete.sure":     "Удалить задание и все собранные им данные? Это не отменить.",
		"jobs.delete.running":  "Задание идёт или стоит в очереди. Сначала остановите его.",
		"jobs.none":            "Заданий пока нет.",
		"jobs.name":            "Название",
		"jobs.started":         "Начато",
		"jobs.queries":         "Запросов",
		"jobs.done":            "Готово",
		"jobs.failed":          "Ошибок",
		"jobs.left":            "Осталось",
		"jobs.dropped":         "Отброшено повторов",
		"jobs.state":           "Состояние",
		"job.state.finished":   "завершено",
		"job.state.unfinished": "не завершено",
		"job.state.starting":   "выходим на связь",
		"job.state.running":    "выполняется",
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

		"job.title":            "Задание",
		"job.progress":         "Ход",
		"job.reshape":          "Сохранить настройки",
		"job.reshape.why":      "Сохранение меняет задание, а не уже открытые порты. Идущее задание доработает на тех, с которыми стартовало; сохранённое здесь применится при следующем запуске — после остановки или когда до задания дойдёт очередь.",
		"job.reshape.done":     "Сохранено. В следующий раз задание поднимется на этом.",
		"job.reshape.finished": "Это задание отработало, и поднимать пул больше не для чего.",
		"job.settings":         "Как заведено",
		"job.stop":             "Остановить",
		"job.resume":           "Продолжить",
		"job.results":          "Результаты",
		"job.result.title":     "Заголовок",
		"job.results.none":     "Пока ничего не снято.",
		"job.results.sample":   "Здесь только последние результаты, не больше чем:",
		"job.export":           "Выгрузка",

		"job.verdicts":       "Адреса",
		"job.verdicts.none":  "Ни одного адреса ещё не проверено.",
		"job.verdict":        "В индексе",
		"job.verdict.held":   "найден",
		"job.verdict.absent": "не найден",

		"job.standings":      "Позиции",
		"job.standings.none": "Ни одной фразы ещё не проверено.",
		"job.rank.none":      "не найден",

		"new.title":             "Новое задание",
		"form.name":             "Название",
		"form.queries":          "Запросы, по одному в строке",
		"form.pages":            "Страниц на запрос",
		"form.device":           "Тип выдачи",
		"form.device.desktop":   "Desktop",
		"form.device.mobile":    "Mobile",
		"form.country":          "Страна",
		"form.language":         "Язык поиска",
		"form.threads":          "Потоков",
		"form.ports":            "Портов на поток",
		"form.estimateonly":     "Эти два меняют только оценку. Задание выполняется на пуле, с которым поднят сервер, и на его потоках.",
		"form.estimate":         "Оценить",
		"form.start":            "Запустить",
		"form.name.required":    "Заданию нужно название, под которым оно ляжет в историю.",
		"form.queries.required": "В списке нет ни одного запроса.",
		"form.pages.positive":   "Запрос берётся хотя бы на одну страницу.",

		"form.kind":          "Тип задания",
		"form.kind.parse":    "Парсинг",
		"form.kind.position": "Проверка позиций",
		"form.kind.index":    "Проверка индексации",
		"form.kind.why":      "Парсинг читает список как фразы и записывает всё, что вернулось по каждой. Проверка позиций читает тот же список и сообщает, на каком месте по каждой фразе стоит один сайт — или что на взятых страницах его нет. Проверка индексации читает список как адреса и спрашивает Google, держит ли он каждый; на адрес берётся одна страница, какую бы глубину ни выставили: первая страница вопрос закрывает.",
		"form.kind.unknown":  "Такого задания не бывает.",

		"form.target":          "Искомый сайт",
		"form.target.why":      "Проверке позиций он нужен, остальным двум типам не нужен вовсе. Голый сайт засчитывает любую его страницу, включая поддомены; адрес с путём — только этот адрес. После запуска он не меняется: уже найденные места отмерены по нему.",
		"form.target.required": "Проверке позиций нужен сайт, о котором она.",

		"form.from":           "Откуда берутся фразы",
		"form.from.box":       "Из поля ниже",
		"form.from.file":      "Из файла",
		"form.from.why":       "Оба поля стоят здесь при любом выборе: поле, которое появляется и исчезает, требует сценария, а этой странице сценарий не нужен. Читается то, которое названо переключателем, второе не читается. Файл читается по мере поступления, поэтому его размер не упирается в память этой машины.",
		"form.keep":           "Что хранить от каждого результата",
		"form.keep.why":       "Записывается только отмеченное, поэтому задание, которому нужен список адресов, занимает в разы меньше — на десяти миллионах результатов это разница между одним файлом и несколькими. Место результата хранится всегда: без него файл превращается из выдачи в мешок. Снимите домен — и результаты этого задания никогда не найдутся на экране «История», который ищет по домену.",
		"form.keep.none":      "Задание должно хранить хоть что-то от результата.",
		"form.keep.needsurl":  "Чтобы удалять дубли по url, нужно хранить url.",
		"form.keep.needshost": "Чтобы удалять дубли по домену, нужно хранить домен.",
		"field.ads":           "Реклама",
		"field.related":       "Похожие запросы",
		"job.export.sep":      "Разделитель для txt",
		"job.export.sep.why":  "Если оставить пустым, txt разделяет колонки табуляцией. Остальные форматы его не смотрят.",
		"job.export.ads":      "реклама",
		"job.export.related":  "похожие запросы",
		"field.title":         "Заголовок",
		"field.url":           "Адрес",
		"field.link":          "Ссылка, как её несла страница",
		"field.host":          "Домен",
		"field.snippet":       "Сниппет",
		"field.path":          "Путь, как его рисовала страница",
		"form.pool":           "Пул, на котором идёт это задание",
		"form.tries":          "Попыток на фразу",
		"form.pause":          "Пауза на одной личности, с",
		"job.reshape.total":   "Открывается портов = потоки × порты на поток.",
		"form.tries.why":      "Сколько личностей пробует одна фраза, прежде чем её спишут. Плохой список отказывает на большей части запросов, и низкий предел теряет не запрос, а задание. Эти четыре — свойство задания: пул поднимается под него при запуске и закрывается, когда задание его отпустило, а изменить их можно на странице задания.",
		"form.pause.why":      "Сколько одна личность ждёт, прежде чем её спросят второй раз. Это была настройка машины, а на деле свойство прогона: насколько сильно можно давить на список, зависит от списка и от того, что у него просят. Ноль — пусть пул считает паузу сам по своему размеру.",
		"form.unique":         "Удаление дублей",
		"form.unique.off":     "Оставлять все результаты",
		"form.unique.url":     "Уник по url",
		"form.unique.host":    "Уник по домену",
		"form.unique.why":     "Повтор отбрасывается по ходу поступления результатов и не записывается вовсе, поэтому его нет ни в истории, ни в выгрузке. Отбор действует в пределах этого задания — сайт, попавшийся месяц назад, сегодня покажется снова — и отменить его после прогона нельзя. Сколько отброшено, видно на странице задания.",
		"form.unique.unknown": "Так повторы не отбрасываются.",
		"form.norunner":       "Этот сервер поднят читать историю и выполнять задания не может.",
		"form.notsetup":       "Выполнять задание пока не на чем. Откройте настройки и настройте связь.",

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
		"estimate.floor":         "Не раньше чем",

		"state.title":            "Состояние",
		"state.running":          "Выполняется сейчас",
		"state.elapsed":          "Прошло",
		"state.speed":            "Запросов в минуту сейчас",
		"state.speed.pages":      "Страниц в минуту сейчас",
		"job.asking":             "Сейчас запрашивается",
		"state.answered":         "Успешных запросов",
		"state.answered.of":      "От завершённых запросов",
		"state.rest":             "Осталось по этой скорости",
		"state.idle":             "Ничего не идёт.",
		"state.invite":           "Завести задание",
		"state.failures":         "Отказы",
		"state.failures.of":      "От завершённых запросов",
		"state.failures.settled": "Завершено запросов",
		"state.pool":             "Порты",
		"state.pool.alive":       "Отвечает",
		"state.pool.ports":       "Портов открыто",
		"state.pool.warm":        "Из них прогреты",
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

		"settings.title":             "Настройки",
		"settings.connection":        "Связь",
		"settings.address":           "Адрес",
		"settings.key":               "Ключ",
		"settings.key.saved":         "Сохранённый ключ оканчивается на",
		"settings.key.keep":          "Оставьте пустым, чтобы сохранённый ключ остался прежним.",
		"settings.check":             "Проверить связь",
		"settings.check.title":       "Что показала проверка",
		"settings.check.good":        "Связь есть",
		"settings.check.good.detail": "Служба ответила, ключ принят, лицензия активна.",
		"settings.check.nothing":     "Проверке нечего сообщить.",
		"settings.finding.ok":        "В порядке",
		"settings.finding.warn":      "Замечание",
		"settings.finding.fail":      "Отказ",
		"settings.source":            "Настройки прокси",
		"settings.source.where":      "Читать",
		"settings.source.none":       "Не используются",
		"settings.source.file":       "Из файла на этой машине",
		"settings.source.url":        "По адресу",
		"settings.source.at":         "Откуда",
		"settings.source.refresh":    "Перечитывать раз в, мин",
		"settings.source.forms":      "По одному адресу в строке, любой привычной записью: хост и порт, эти два и следом имя с паролем, или имя с паролем впереди, или те и другие через @. Схема впереди читается, но не обязательна. Пустые строки пропускаются, как и строки, начатые с # // или ;",
		"browse.title":               "Выбор списка",
		"browse.up":                  "На уровень выше",
		"browse.left":                "Файлов не показано:",
		"browse.none":                "Выбирать здесь нечего.",
		"browse.back":                "Назад к настройкам",
		"browse.unreadable":          "Эта машина не даёт программе прочитать эту папку.",
		"settings.source.choose":     "Выбрать",

		"settings.hot":              "Держать прогретыми",
		"settings.hot.ports":        "Сколько",
		"settings.hot.why":          "Держатся открытыми между заданиями и получают один обычный запрос раз в четверть часа, поэтому отвечают сразу, а не после проверки. Ноль — не держать. Задание этого типа выдачи работает на них и открывает недостающие; задание другого типа открывает свои.",
		"settings.hot.count":        "Сколько личностей держать прогретыми — это целое число, ноль означает не держать.",
		"settings.hot.kind":         "Это не тот тип выдачи, под который программа открывает личности.",
		"settings.language":         "Язык интерфейса",
		"settings.language.reader":  "Авто выбор",
		"settings.save":             "Сохранить",
		"settings.unreadable":       "Файл настроек есть, и прочитать его не удалось. Ниже — то, с чего эта программа начинает; сохранение перезапишет файл.",
		"settings.opened.nothing":   "Настройки сохранены, и открыть по ним ничего не удалось, поэтому работа выполняется там же, где и раньше. Причина — в журнале сервера.",
		"settings.address.unusable": "По этому адресу программа обратиться не может.",
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
//
// It reads the languages the catalogue itself holds rather than the ones the
// switcher offers, and the difference is the whole of the rule: this is a check
// on text that ships in this binary, where a missing phrase is a mistake in this
// repository. A language read from a directory beside the program is somebody
// else's, and being short of phrases there is not a reason to refuse to start.
func checkCatalogue(c map[Lang]map[string]string) error {
	for written := range c {
		for other := range c {
			for key := range c[written] {
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

// texts is everything the interface can say right now, and in which languages.
//
// It is one value rather than three variables because it is replaced whole: a
// language, its name and its phrases arrive together, and a page drawn while
// they were being put in one at a time would show a switcher offering a
// language whose words were not there yet.
type texts struct {
	langs []Lang
	names map[Lang]string
	say   map[Lang]map[string]string
}

// spoken is what the interface says. It is read on every phrase of every page,
// from as many goroutines as there are readers, and replaced when a directory
// of translations is read — so it is swapped as a pointer rather than edited in
// place.
var spoken atomic.Pointer[texts]

// builtInNames is what each language built into this program calls itself.
var builtInNames = map[Lang]string{LangEN: "English", LangRU: "Русский"}

// langNameKey is where a file says what its language calls itself. It is the
// one key whose text is read by this program rather than shown as a phrase, and
// a file that leaves it out is offered by its code.
const langNameKey = "lang.name"

func init() { spoken.Store(builtIn()) }

// builtIn is what this program says with nothing beside it.
//
// The phrase maps are shared with the catalogue rather than copied, and nothing
// below writes to a map it did not make: the catalogue is this repository's own
// text and a file must not be able to edit it for the rest of the process.
func builtIn() *texts {
	t := &texts{
		langs: []Lang{LangEN, LangRU},
		names: make(map[Lang]string, len(builtInNames)),
		say:   make(map[Lang]map[string]string, len(catalogue)),
	}
	for l, name := range builtInNames {
		t.names[l] = name
	}
	for l, phrases := range catalogue {
		t.say[l] = phrases
	}
	return t
}

// ErrUnreadableTranslation reports a file in the translations directory that
// this program could not read a language out of.
var ErrUnreadableTranslation = errors.New("web: a translation file could not be read")

// translationExt is the extension a translation is written with. The format is
// JSON because a translation is a list of phrases against their keys and
// nothing else, and JSON is the one shape in the standard library that says
// exactly that, in any alphabet, with a reader that rejects a damaged file
// rather than guessing at it.
const translationExt = ".json"

// maxTranslationFile is how much of a file is read before it is called
// something other than a translation. Everything this program says fits in a
// small fraction of it; the limit is there because the directory is somebody
// else's and a program that reads whatever it is pointed at can be pointed at a
// disk.
const maxTranslationFile = 1 << 20

// LoadTranslations reads a directory of translations and puts what it finds
// into use, adding languages this program was not built with and overriding the
// ones it was.
//
// Nothing here is a reason to stop. The directory is the operator's, not this
// repository's, and the rule the built-in languages are held to — every key in
// every language or the server does not start — is a rule about text this
// program ships. A file short of keys says what it says and the rest is shown
// in English, and which keys those were comes back in incomplete so somebody
// can be told. The error is the same kind of report: it names the files that
// could not be read at all, and the ones that could are already in use.
//
// A directory that is not there is the ordinary case and not an error: the
// program that ships with two languages and no files beside it is the one
// almost everyone runs.
//
// Reading twice gives what the directory says the second time, not what it said
// the first time with the second laid over it. A language whose file was
// deleted between the two goes away.
func LoadTranslations(dir string) (added []Lang, incomplete map[Lang][]string, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrUnreadableTranslation, err)
	}

	t := builtIn()
	var complaints []error
	var fromFiles []Lang
	for _, entry := range entries {
		// A directory named like a translation is not one, and neither is
		// anything else the filesystem can hold that is not a plain file.
		if !entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		code, isTranslation := translationName(name)
		if !isTranslation {
			continue
		}
		phrases, readErr := readTranslation(filepath.Join(dir, name))
		if readErr != nil {
			complaints = append(complaints, fmt.Errorf("%w: %s: %v", ErrUnreadableTranslation, name, readErr))
			continue
		}
		lang := Lang(code)
		if _, built := t.say[lang]; !built {
			t.langs = append(t.langs, lang)
			added = append(added, lang)
		}
		t.say[lang] = merge(t.say[lang], phrases)
		fromFiles = append(fromFiles, lang)
		if self := strings.TrimSpace(t.say[lang][langNameKey]); self != "" {
			t.names[lang] = self
		}
	}

	// English is filled in from last, after every file has had its say, so a
	// language falls back on the English this machine shows rather than on the
	// English this program was built with.
	for _, lang := range fromFiles {
		if missing := fillFromEnglish(t.say[lang], t.say[LangEN]); len(missing) > 0 {
			if incomplete == nil {
				incomplete = make(map[Lang][]string)
			}
			incomplete[lang] = missing
		}
	}

	spoken.Store(t)
	return added, incomplete, errors.Join(complaints...)
}

// translationName reads a language code off a file name, and says whether the
// name is one this program will read at all.
//
// Only two or three lowercase letters and the extension are a translation.
// Everything else in the directory is somebody else's business — notes, a
// readme, a half-finished file renamed out of the way. The narrowness is also
// what keeps the name from being a path: every way of naming a file outside
// this directory needs a character this rejects.
func translationName(file string) (code string, ok bool) {
	code = strings.TrimSuffix(file, translationExt)
	if code == file {
		return "", false
	}
	if len(code) < 2 || len(code) > 3 {
		return "", false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 'a' || code[i] > 'z' {
			return "", false
		}
	}
	return code, true
}

// readTranslation reads one file as a list of phrases against their keys.
//
// A phrase that is there and blank is not a phrase, and it is left out so the
// key falls back to something a reader can read. An empty string on a page is
// invisible, which is the one outcome worse than the wrong language.
func readTranslation(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	body, err := io.ReadAll(io.LimitReader(f, maxTranslationFile+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxTranslationFile {
		return nil, errors.New("it is larger than everything this program says")
	}
	var phrases map[string]string
	if err := json.Unmarshal(body, &phrases); err != nil {
		return nil, err
	}
	if phrases == nil {
		return nil, errors.New("it holds no phrases against their keys")
	}
	for key, text := range phrases {
		if strings.TrimSpace(text) == "" {
			delete(phrases, key)
		}
	}
	return phrases, nil
}

// merge lays a file over a language, phrase by phrase.
//
// A file that names one key changes one phrase. It is merged rather than put in
// place because a translator correcting a single line would otherwise delete
// every other line of that language, and the page would answer in a language
// nobody chose.
//
// The result is a new map. The one underneath may be the catalogue's own, and
// this program's built-in text has to still be there the next time a directory
// is read.
func merge(under, over map[string]string) map[string]string {
	merged := make(map[string]string, len(under)+len(over))
	for key, text := range under {
		merged[key] = text
	}
	for key, text := range over {
		merged[key] = text
	}
	return merged
}

// fillFromEnglish gives a language the English of everything it does not say,
// and reports what those were.
//
// English rather than the key, because the key is this repository's shorthand
// and means nothing to a reader; and a phrase rather than nothing, because a
// button with no words on it cannot be pressed by anybody.
func fillFromEnglish(phrases, english map[string]string) []string {
	var missing []string
	for key, text := range english {
		if _, said := phrases[key]; said {
			continue
		}
		phrases[key] = text
		missing = append(missing, key)
	}
	slices.Sort(missing)
	return missing
}
