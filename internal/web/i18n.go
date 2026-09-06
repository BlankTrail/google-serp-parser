// SPDX-License-Identifier: MIT

package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
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
		"jobs.new":             "Set a job up",
		"jobs.collected":       "Collected",
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
		"job.reshape.why":      "Applies at the job's next start — after a stop, or when the queue reaches it. Ports already open are not changed.",
		"job.reshape.done":     "Saved. The job will come up on this next time it starts.",
		"job.reshape.finished": "This job has run, so there is nothing left for a pool to be raised for.",
		"job.settings":         "Set up as",
		"job.stop":             "Stop",
		"job.resume":           "Carry on",
		"job.retry":            "Try the failed ones again",
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

		"new.title":                "New job",
		"form.name":                "Name",
		"form.queries":             "Queries, one per line",
		"form.pages":               "Pages per query",
		"proxies.title":            "Proxies",
		"proxies.pool":             "The pool as it stands",
		"proxies.addresses":        "Addresses in the list",
		"proxies.resting":          "Banned right now",
		"proxies.ports":            "Ports open",
		"proxies.warm":             "Of them warm",
		"proxies.quarantined":      "Ports in quarantine",
		"proxies.since":            "Counted since",
		"proxies.requests":         "Requests",
		"proxies.attempts":         "Attempts on the wire",
		"proxies.failed":           "Of them failed",
		"proxies.share":            "Share that failed",
		"proxies.rotations":        "Address changes",
		"proxies.quarantines":      "Ports sent to quarantine",
		"proxies.revivals":         "Ports taken back",
		"proxies.reopenings":       "Ports opened again",
		"proxies.refused":          "Would not carry a port",
		"proxies.refused.name":     "Gateway",
		"proxies.refused.why":      "What the service said",
		"proxies.refused.why.note": "These were stepped over and the job ran on the rest. A gateway is dropped from this list when it carries a port again.",
		"proxies.rejections":       "Answers refused as unusable",
		"proxies.kind":             "Kind of failure",
		"proxies.count":            "Count",
		"proxies.of.failures":      "Of all failures",
		"proxies.kind.transport":   "Did not reach the address",
		"proxies.kind.port":        "The proxy port did not answer",
		"proxies.kind.relay":       "The address swaps the certificate",
		"proxies.kind.wall":        "Google refused",
		"proxies.kind.timeout":     "Ran out of time",
		"proxies.kind.other":       "Other",
		"proxies.reset":            "Clear the counts",
		"proxies.release":          "Clear the ban",
		"proxies.reset.why":        "Zeroes the counts to start a fresh measurement. Bans stay as they are.",
		"proxies.release.why":      "Unbans every address. Use it after a proxy restart or a network outage, when the bans are not the addresses fault. Counts stay as they are.",
		"form.device":              "Result page",
		"form.wire":                "Connection to a port",
		"form.device.desktop":      "Desktop",
		"form.device.mobile":       "Mobile",
		"form.country":             "Country",
		"form.language":            "Search language",
		"form.threads":             "Threads",
		"form.ports":               "Ports per thread",
		"form.estimateonly":        "These two change the estimate and nothing else. A job runs on the pool this server was started with, at the threads it was given.",
		"form.estimate":            "Estimate",
		"form.start":               "Start",
		"form.name.required":       "The job needs a name to be filed under.",
		"form.queries.required":    "There is not one query in the list.",
		"form.pages.positive":      "A query is taken to at least one page.",
		// What the job asks Google. The lines of the list mean different things
		// under the two, which is why the choice stands above the list rather
		// than beside the depth.
		"form.kind":              "Kind of job",
		"form.kind.parse":        "Parsing",
		"form.kind.position":     "Position check",
		"form.kind.index":        "Index check",
		"form.kind.parse.why":    "Parsing — reads the line as a phrase and saves every result it returns.",
		"form.kind.position.why": "Position check — searches the same phrases and records where one site stands, or that it was not found.",
		"form.kind.index.why":    "Index check — reads the line as an address and asks whether Google holds it. One page per address, whatever the depth.",
		"form.kind.unknown":      "That is not one of the things a job asks.",
		// The site a position check is about. The sentence says what cannot be
		// changed later and why, because a reader who finds that out afterwards
		// has a run to do again.
		"form.target":          "Site to look for",
		"form.target.why":      "Used by the position check only. A bare domain counts any of its pages, subdomains included; an address with a path counts that address alone. Fixed once the job has started.",
		"form.target.required": "A position check needs the site it is about.",
		// What the job throws away as it writes, and how many it threw. Both say
		// what happened and neither judges it: whether six repeats in ten is what
		// the reader meant is theirs to say. The sentence names the part that
		// cannot be taken back, because it cannot — a dropped result is never
		// written, and having it takes another run.
		"form.from":           "Where the phrases come from",
		"form.from.box":       "Typed in below",
		"form.from.file":      "From a file",
		"form.from.why":       "Only the chosen source is read; the other is ignored. A file is loaded line by line, so its size is not limited by this machine's memory.",
		"form.keep":           "What to keep of each result",
		"form.keep.why":       "Only ticked fields are saved, which cuts the size of a large job considerably. The position is always kept. Without the site, this job's results cannot be found on the History screen — it searches by site.",
		"form.keep.none":      "A job has to keep something of each result.",
		"form.keep.needsurl":  "Dropping repeats by url needs the url kept.",
		"form.keep.needshost": "Dropping repeats by domain needs the domain kept.",
		"field.ads":           "Paid placements",
		"field.related":       "Related searches",
		"job.export.sep":      "Separator for txt",
		"job.export.sep.why":  "Empty means a tab. Used by txt only.",
		"job.export.ads":      "paid placements",
		"job.export.related":  "related searches",
		"field.title":         "Title",
		"field.url":           "Address",
		"field.link":          "Address as the page carried it",
		"field.host":          "Domain",
		"field.snippet":       "Snippet",
		"field.path":          "Path as the page drew it",
		"form.pool":           "The pool this job runs on",
		"form.profile":        "Proxy profile",
		"form.tries":          "Tries per phrase",
		"form.pause":          "Pause on one identity, seconds",
		"job.reshape.total":   "Ports opened = threads × ports per thread.",
		"form.tries.why":      "Tries per phrase — how many identities one phrase is taken to before it is written off as failed. On a poor proxy list a low limit loses phrases.",
		"form.pause.why":      "Pause — how long one identity waits before it is asked again. Nought lets the pool work it out from the number of ports.",
		"form.pool.why":       "These four belong to the job and can be changed on its own page. Ports opened = threads × ports per thread.",
		"form.unique":         "Dropping duplicates",
		"form.unique.off":     "Keep every result",
		"form.unique.url":     "Unique by url",
		"form.unique.host":    "Unique by domain",
		"form.unique.why":     "Repeats are dropped as results arrive and reach neither the history nor the export. The filter works within this job alone and cannot be undone after the run. The job's page shows how many were dropped.",
		"form.unique.unknown": "That is not one of the ways of dropping repeats.",
		"form.norunner":       "This server was started to read the history, so it cannot run a job.",
		"form.notsetup":       "There is nothing to run a job on yet. Open the settings and set the connection up.",
		// The file. Every one of these says what happened and what is left behind,
		// because a list that stopped arriving leaves a job in the history and the
		// reader has to know it is there.
		"form.list.title":        "A list from a file",
		"form.list":              "The file, one query per line",
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
		"state.title":                "Status",
		"state.running":              "Running now",
		"state.elapsed":              "Elapsed",
		"state.speed":                "Queries a minute now",
		"state.speed.pages":          "Pages a minute now",
		"job.asking":                 "Current url",
		"state.answered":             "Answered",
		"state.answered.of":          "Of the queries settled so far",
		"state.rest":                 "Left at this pace",
		"state.idle":                 "Nothing is running.",
		"state.invite":               "Set up a job",
		"state.failures":             "Refused",
		"state.failures.of":          "Of the queries settled so far",
		"state.failures.settled":     "Settled so far",
		"state.pool":                 "Ports",
		"state.pool.alive":           "Answering",
		"state.pool.ports":           "Ports open",
		"state.pool.warm":            "Of them warm",
		"state.pool.queueing":        "Threads waiting for an egress",
		"settings.perupstream":       "Threads per proxy",
		"settings.perupstream.why":   "How many threads may work through one address, or one gateway, at the same time. One by default. Threads with nothing left to take wait their turn, and the status screen says how many are waiting. Each whole ip:port counts as one, so two ports on one machine are two.",
		"settings.perupstream.count": "Threads per proxy: a whole number, one or more.",
		"state.pool.rotate":          "Address changes",
		"state.pool.aside":           "Set aside",
		"state.pool.back":            "Back after resting",
		"state.pool.none":            "This server holds no ports.",
		"state.queue":                "Waiting their turn",
		"state.queue.none":           "Nothing is waiting.",
		"state.class.wall":           "a challenge or a rate limit",
		"state.class.banned":         "a refusal",
		"state.class.shell":          "a page carrying no results",
		"state.class.http":           "another status",
		"state.class.empty":          "nothing found",
		"state.class.serp":           "a page of results",
		"state.class.silent":         "no answer at all",

		// The quick start. Each step names what it is for before it names a box:
		// somebody filling in a key they do not understand the purpose of will
		// fill it in wrongly once and blame the program for the rest of the week.

		// What this program last learned about the service it runs on, as the
		// header says it, and as a banner says it where it is worth stopping the
		// reader over. The banner names the state and never the remedy: the press
		// beside it is the remedy.
		"link.good":     "connected",
		"link.unset":    "not set up",
		"link.silent":   "not answering",
		"link.refused":  "key refused",
		"link.inactive": "licence inactive",

		"notice.unset":    "The connection to BlankTrail has not been set up yet, so nothing can be run.",
		"notice.silent":   "BlankTrail is not answering, so nothing can be run.",
		"notice.refused":  "BlankTrail would not take the key it was given.",
		"notice.inactive": "The BlankTrail licence is not active, so no port will open.",
		"notice.settings": "Open the settings",

		// The exits a job runs through when it names none. A profile with nothing
		// in it is what a machine has on the day it is installed, and a run on it
		// goes out from the address the operator is sitting at.
		"notice.profile.none":  "There is no proxy profile, so a job has nothing to go out through.",
		"notice.profile.empty": "The default proxy profile names no exits, so a job would go out from this machine's own address.",
		"notice.profile.press": "Set the exits up",

		// The lights. The switch draws two marks and no words, so this is the name
		// on it: what a reader who cannot see it is told, and what the pointer
		// rests on for everybody else.
		//
		// It names what the press does rather than the theme already on. A switch
		// labelled with where it is gets pressed by everybody who wants to stay
		// there.
		// The walk through the interface. One sentence a stop and nothing longer:
		// somebody being shown round is reading while standing, and a paragraph is
		// a paragraph they close.
		"tour.title":        "Quick start",
		"tour.press":        "Press what is outlined",
		"tour.close":        "Close the walk",
		"tour.screens":      "The four screens",
		"tour.screens.said": "What is happening, the jobs, the exits, and every result kept.",
		"tour.key":          "The key",
		"tour.key.said":     "Paste the key from BlankTrail here. Nothing runs without it.",
		"tour.exits":        "The exits",
		"tour.exits.said":   "A profile is a named set of proxies. Every job goes out through one.",
		"tour.new":          "A new job",
		"tour.new.said":     "Jobs are started from here.",
		"tour.queries":      "What to ask",
		"tour.queries.said": "Type the phrases, or hand over a file of them.",
		"tour.depth":        "How deep, and where",
		"tour.depth.said":   "Pages per phrase, and the country and language to ask as.",
		"tour.reach":        "The connection",
		"tour.reach.said":   "A tick here means BlankTrail is answering. A cross means it is not.",

		"theme.dark":  "Switch to the dark theme",
		"theme.light": "Switch to the light theme",

		// Setting the machine up. The key is spoken of by its last few characters
		// and never shown, which is why two phrases are needed where one box
		// stands.
		"settings.title":               "Settings",
		"settings.connection":          "Connection",
		"settings.address":             "Address",
		"settings.key":                 "Key",
		"settings.key.saved":           "The key that is saved ends in",
		"settings.key.keep":            "Leave this empty to keep the key that is saved.",
		"settings.check":               "Check the connection",
		"settings.check.title":         "What the check found",
		"settings.check.good":          "The connection works",
		"settings.check.good.detail":   "The service answered, the key was accepted and the licence is active.",
		"settings.check.nothing":       "The check found nothing to report.",
		"settings.finding.ok":          "In order",
		"settings.finding.warn":        "Note",
		"settings.finding.fail":        "Fail",
		"settings.source":              "Proxy settings",
		"settings.source.where":        "Read from",
		"proxies.profiles":             "Proxy profiles",
		"proxies.profiles.why":         "A profile is a named set of exits: where the addresses come from and how the ports on them are used. A job names one when it is set up, so two lists no longer mean editing this screen between two runs.",
		"proxies.profile.name":         "Name",
		"proxies.profile.default":      "used by default",
		"proxies.profile.open":         "Edit",
		"proxies.profile.makedefault":  "Make default",
		"proxies.profile.delete":       "Delete",
		"proxies.profile.stats":        "Statistics",
		"proxies.profile.cancel":       "Leave it as it was",
		"proxies.reading":              "What has been going through",
		"proxies.reading.none":         "Nothing has been run since this program started, so there is nothing to read yet.",
		"proxies.reading.elsewhere":    "What is running is going out through another profile:",
		"proxies.profile.new":          "New profile",
		"proxies.profile.editing":      "Profile:",
		"proxies.profile.needs.name":   "A profile needs a name: it is what a job says to run through it.",
		"proxies.profile.name.taken":   "Another profile is called that already.",
		"proxies.profile.last":         "This is the last profile, and something has to be default. Empty its source instead of deleting it.",
		"settings.source.none":         "Not used",
		"settings.source.file":         "A file on this machine",
		"settings.source.url":          "An address",
		"settings.source.gateways":     "VPN gateways stored in BlankTrail",
		"proxies.gateways.loose":       "Uploaded on their own",
		"proxies.gateways.missing":     "Chosen and no longer on the service:",
		"proxies.gateways.unreachable": "The gateways could not be asked for: nothing answered.",
		"proxies.gateways.refused":     "The gateways could not be asked for: the service would not take the API key.",
		"proxies.gateways.unknown":     "This version of the service answers nothing about gateways.",
		"proxies.gateways.failed":      "The gateways could not be asked for: the service answered with an error.",
		"proxies.gateways.asked":       "Asked at",
		"proxies.gateways.ping.ms":     "ms",
		"proxies.gateways.ping.gone":   "did not answer",
		"proxies.gateways.ping.never":  "not measured",
		"proxies.gateways.all":         "All",
		"proxies.gateways.title":       "Gateways",
		"settings.renew":               "Change identity every, minutes",
		"settings.renew.why":           "How often a port is opened again with a fresh fingerprint and an empty cookie jar. Nought never does, which suits a long address list. A short list of gateways is the other case: a dozen identities held for hours become a dozen an origin knows, and choosing the gateways offers ten minutes.",
		"settings.renew.length":        "Say the minutes between changes of identity as a whole number, or nought for never.",
		"proxies.gateways.refresh":     "Refresh",
		"proxies.gateways.taken":       "read at",
		"proxies.gateways.none":        "None",
		"proxies.gateways.unavailable": "The service holds no usable gateways.",
		"settings.source.at":           "Where",
		"settings.source.refresh":      "Read again every, minutes",
		"settings.source.forms":        "One address per line, in any of these forms:",
		"settings.source.form.plain":   "host:port",
		"settings.source.form.after":   "host:port:user:password",
		"settings.source.form.before":  "user:password:host:port",
		"settings.source.form.at":      "user:password@host:port",
		"settings.source.scheme":       "A scheme in front (socks5://, http://) is optional; socks5 is assumed.",
		"settings.source.skipped":      "Blank lines, and lines beginning with # // or ;, are skipped.",
		"browse.title":                 "Choose a list",
		"browse.up":                    "Up one",
		"browse.left":                  "Files not shown here:",
		"browse.none":                  "Nothing here to choose.",
		"browse.back":                  "Back to the settings",
		"browse.unreadable":            "This machine will not let this program read that folder.",
		"settings.source.choose":       "Choose",

		"settings.hot":                "Identities kept warm",
		"settings.hot.ports":          "How many",
		"settings.wire.why":           "SOCKS5 carries UDP, so QUIC and DNS at the far end work. HTTP is TCP only, and is here to fall back to.",
		"settings.ban":                "Ban for, minutes",
		"settings.ban.why":            "How long an address is left out after it fails. A short ban brings the same dead addresses back inside one job; a long one takes more of the list out at once. Nought leaves nobody out at all. Sixty is what a machine nobody has configured starts with.",
		"settings.ban.length":         "Ban for: minutes, a whole number, nought or more.",
		"settings.hot.why":            "Ports kept open between jobs, each given one ordinary search every fifteen minutes so it stays warm and answers at once. Nought keeps none. A job of the same result type runs on them and opens whatever more it needs; a job of the other type opens its own.",
		"settings.hot.count":          "How many identities to keep warm has to be a whole number, and nought keeps none.",
		"settings.hot.kind":           "That is not a kind of result page this program opens identities for.",
		"settings.lan":                "Reaching this from another machine",
		"settings.lan.open":           "Answer the network, not this machine alone",
		"settings.lan.password":       "Password",
		"settings.lan.saved":          "A password is saved. Leave the box empty to keep it.",
		"settings.lan.why":            "Off, the interface answers this machine only. On, it answers every address this machine has, and asks for the password before it shows anything: the pages carry no key of their own, and they hold the settings, the queue and everything every job has collected. It cannot be turned on without a password. Takes effect at the next start.",
		"settings.lan.plain":          "The password travels in every request and is not encrypted: this program speaks plain HTTP. Use it on a network you trust, or reach the machine over a VPN.",
		"settings.lan.password.short": "Password: it could not be hashed. Try another one.",
		"settings.lan.needs.password": "Answering the network needs a password. Type one, or leave the network switch off.",
		"settings.language":           "Interface language",
		"settings.language.reader":    "Chosen automatically",
		"settings.save":               "Save",
		"settings.unreadable":         "The settings file is there and could not be read. What is below is what this program starts from; saving writes it over the file.",
		"settings.opened.nothing":     "The settings are saved, and nothing could be opened with them, so the work goes on where it was. The reason is in the log this server writes.",
		"settings.address.unusable":   "That is not an address this program can reach.",
		"settings.refresh.length":     "How often a list is read again is a length of time, and no shorter than none.",
		"settings.source.unknown":     "That is not one of the places a list can be read from.",
		"settings.language.unknown":   "This interface is not written in that language.",
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
		"jobs.new":             "Новое задание",
		"jobs.collected":       "Собрано",
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
		"job.reshape.why":      "Применится при следующем запуске задания — после остановки или когда до него дойдёт очередь. Уже открытые порты не меняются.",
		"job.reshape.done":     "Сохранено. В следующий раз задание поднимется на этом.",
		"job.reshape.finished": "Это задание отработало, и поднимать пул больше не для чего.",
		"job.settings":         "Как заведено",
		"job.stop":             "Остановить",
		"job.resume":           "Продолжить",
		"job.retry":            "Повторить неудавшиеся",
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

		"new.title":                "Новое задание",
		"form.name":                "Название",
		"form.queries":             "Запросы, по одному в строке",
		"form.pages":               "Страниц на запрос",
		"proxies.title":            "Прокси",
		"proxies.pool":             "Пул сейчас",
		"proxies.addresses":        "Адресов в списке",
		"proxies.resting":          "Забанено сейчас",
		"proxies.ports":            "Портов открыто",
		"proxies.warm":             "Из них прогрето",
		"proxies.quarantined":      "Портов в карантине",
		"proxies.since":            "Счёт с",
		"proxies.requests":         "Запросов",
		"proxies.attempts":         "Попыток на проводе",
		"proxies.failed":           "Из них с ошибкой",
		"proxies.share":            "Доля с ошибкой",
		"proxies.rotations":        "Смен адреса",
		"proxies.quarantines":      "Портов уведено в карантин",
		"proxies.revivals":         "Портов возвращено",
		"proxies.reopenings":       "Портов переоткрыто",
		"proxies.refused":          "Не приняли порт",
		"proxies.refused.name":     "Шлюз",
		"proxies.refused.why":      "Что ответил сервис",
		"proxies.refused.why.note": "Их обошли, задание пошло на остальных. Шлюз уходит из списка, когда снова принимает порт.",
		"proxies.rejections":       "Ответов отвергнуто как непригодные",
		"proxies.kind":             "Тип ошибки",
		"proxies.count":            "Счёт",
		"proxies.of.failures":      "Доля от всех ошибок",
		"proxies.kind.transport":   "Запрос не дошёл до адреса",
		"proxies.kind.port":        "Порт прокси-службы не ответил",
		"proxies.kind.relay":       "Адрес подменяет сертификат",
		"proxies.kind.wall":        "Google отказал",
		"proxies.kind.timeout":     "Не дождались ответа",
		"proxies.kind.other":       "Прочее",
		"proxies.reset":            "Обнулить счётчики",
		"proxies.release":          "Сбросить бан",
		"proxies.reset.why":        "Обнуляет счётчики, чтобы начать замер заново. Баны не снимаются.",
		"proxies.release.why":      "Снимает бан со всех адресов. Нужно после перезапуска прокси-службы или обрыва сети, когда адреса не виноваты. Счётчики не обнуляются.",
		"form.device":              "Тип выдачи",
		"form.wire":                "Связь с портом",
		"form.device.desktop":      "Desktop",
		"form.device.mobile":       "Mobile",
		"form.country":             "Страна",
		"form.language":            "Язык поиска",
		"form.threads":             "Потоков",
		"form.ports":               "Портов на поток",
		"form.estimateonly":        "Эти два меняют только оценку. Задание выполняется на пуле, с которым поднят сервер, и на его потоках.",
		"form.estimate":            "Оценить",
		"form.start":               "Запустить",
		"form.name.required":       "Заданию нужно название, под которым оно ляжет в историю.",
		"form.queries.required":    "В списке нет ни одного запроса.",
		"form.pages.positive":      "Запрос берётся хотя бы на одну страницу.",

		"form.kind":              "Тип задания",
		"form.kind.parse":        "Парсинг",
		"form.kind.position":     "Проверка позиций",
		"form.kind.index":        "Проверка индексации",
		"form.kind.parse.why":    "Парсинг — читает строку как фразу и сохраняет все её результаты.",
		"form.kind.position.why": "Проверка позиций — ищет по тем же фразам один сайт и записывает его место или отсутствие.",
		"form.kind.index.why":    "Проверка индексации — читает строку как адрес и проверяет, есть ли он в индексе. Одна страница на адрес, независимо от глубины.",
		"form.kind.unknown":      "Такого задания не бывает.",

		"form.target":          "Искомый сайт",
		"form.target.why":      "Нужен только проверке позиций. Голый домен — засчитывается любая его страница, включая поддомены; адрес с путём — только он сам. После старта не меняется.",
		"form.target.required": "Проверке позиций нужен сайт, о котором она.",

		"form.from":           "Откуда берутся фразы",
		"form.from.box":       "Из поля ниже",
		"form.from.file":      "Из файла",
		"form.from.why":       "Читается только выбранный источник, второй игнорируется. Файл загружается построчно, поэтому его размер не ограничен памятью машины.",
		"form.keep":           "Что хранить от каждого результата",
		"form.keep.why":       "Сохраняются только отмеченные поля — на большом задании это заметно уменьшает объём. Позиция сохраняется всегда. Без домена результаты этого задания не найти на экране «История»: поиск там идёт по домену.",
		"form.keep.none":      "Задание должно хранить хоть что-то от результата.",
		"form.keep.needsurl":  "Чтобы удалять дубли по url, нужно хранить url.",
		"form.keep.needshost": "Чтобы удалять дубли по домену, нужно хранить домен.",
		"field.ads":           "Реклама",
		"field.related":       "Похожие запросы",
		"job.export.sep":      "Разделитель для txt",
		"job.export.sep.why":  "Пусто — табуляция. Действует только для txt.",
		"job.export.ads":      "реклама",
		"job.export.related":  "похожие запросы",
		"field.title":         "Заголовок",
		"field.url":           "Адрес",
		"field.link":          "Ссылка, как её несла страница",
		"field.host":          "Домен",
		"field.snippet":       "Сниппет",
		"field.path":          "Путь, как его рисовала страница",
		"form.pool":           "Пул, на котором идёт это задание",
		"form.profile":        "Профиль прокси",
		"form.tries":          "Попыток на фразу",
		"form.pause":          "Пауза на одной личности, с",
		"job.reshape.total":   "Открывается портов = потоки × порты на поток.",
		"form.tries.why":      "Попыток на фразу — сколько личностей она пробует, прежде чем её спишут в ошибки. На плохом списке прокси низкое значение теряет фразы.",
		"form.pause.why":      "Пауза — сколько одна личность ждёт до следующего запроса. Ноль — пул подберёт её сам по числу портов.",
		"form.pool.why":       "Эти четыре — параметры задания, их можно изменить на его странице. Открывается портов = потоки × порты на поток.",
		"form.unique":         "Удаление дублей",
		"form.unique.off":     "Оставлять все результаты",
		"form.unique.url":     "Уник по url",
		"form.unique.host":    "Уник по домену",
		"form.unique.why":     "Дубли отбрасываются на лету и не попадают ни в историю, ни в выгрузку. Фильтр действует в пределах одного задания и после запуска не отменяется. Сколько отброшено — на странице задания.",
		"form.unique.unknown": "Так повторы не отбрасываются.",
		"form.norunner":       "Этот сервер поднят читать историю и выполнять задания не может.",
		"form.notsetup":       "Выполнять задание пока не на чем. Откройте настройки и настройте связь.",

		"form.list.title":        "Список файлом",
		"form.list":              "Файл, по одному запросу в строке",
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

		"state.title":                "Состояние",
		"state.running":              "Выполняется сейчас",
		"state.elapsed":              "Прошло",
		"state.speed":                "Запросов в минуту сейчас",
		"state.speed.pages":          "Страниц в минуту сейчас",
		"job.asking":                 "Текущий url",
		"state.answered":             "Успешных запросов",
		"state.answered.of":          "От завершённых запросов",
		"state.rest":                 "Осталось по этой скорости",
		"state.idle":                 "Ничего не идёт.",
		"state.invite":               "Завести задание",
		"state.failures":             "Отказы",
		"state.failures.of":          "От завершённых запросов",
		"state.failures.settled":     "Завершено запросов",
		"state.pool":                 "Порты",
		"state.pool.alive":           "Отвечает",
		"state.pool.ports":           "Портов открыто",
		"state.pool.warm":            "Из них прогреты",
		"state.pool.queueing":        "Потоков ждут выхода",
		"settings.perupstream":       "Потоков на прокси",
		"settings.perupstream.why":   "Сколько потоков одновременно работают через один адрес или один шлюз. По умолчанию один. Потокам, которым не досталось, ждут своей очереди, а экран состояния показывает, сколько их. За единицу считается вся строка ip:port, поэтому два порта на одной машине — это два выхода.",
		"settings.perupstream.count": "Потоков на прокси: целое число, один или больше.",
		"state.pool.rotate":          "Смен адреса",
		"state.pool.aside":           "Отложено",
		"state.pool.back":            "Вернулось после отлёжки",
		"state.pool.none":            "На этом сервере портов нет.",
		"state.queue":                "Ждут очереди",
		"state.queue.none":           "Никто не ждёт.",
		"state.class.wall":           "проверка или ограничение частоты",
		"state.class.banned":         "отказ",
		"state.class.shell":          "страница без результатов",
		"state.class.http":           "другой ответ",
		"state.class.empty":          "ничего не найдено",
		"state.class.serp":           "страница выдачи",
		"state.class.silent":         "ответа не было",

		"link.good":     "связь есть",
		"link.unset":    "не настроена",
		"link.silent":   "не отвечает",
		"link.refused":  "ключ отклонён",
		"link.inactive": "лицензия неактивна",

		"notice.unset":    "Связь с BlankTrail ещё не настроена — запустить ничего нельзя.",
		"notice.silent":   "BlankTrail не отвечает — запустить ничего нельзя.",
		"notice.refused":  "BlankTrail не принял ключ.",
		"notice.inactive": "Лицензия BlankTrail неактивна — порты не откроются.",
		"notice.settings": "Открыть настройки",

		"notice.profile.none":  "Профилей прокси нет — заданию не через что выходить.",
		"notice.profile.empty": "В профиле по умолчанию не указаны выходы — задание пойдёт с адреса этой машины.",
		"notice.profile.press": "Настроить выходы",

		"tour.title":        "Быстрый старт",
		"tour.press":        "Нажмите обведённое",
		"tour.close":        "Закрыть гид",
		"tour.screens":      "Четыре экрана",
		"tour.screens.said": "Что происходит, задания, выходы и всё, что собрано.",
		"tour.key":          "Ключ",
		"tour.key.said":     "Сюда вставьте ключ из BlankTrail. Без него не запустится ничего.",
		"tour.exits":        "Выходы",
		"tour.exits.said":   "Профиль — именованный набор прокси. Каждое задание идёт через один из них.",
		"tour.new":          "Новое задание",
		"tour.new.said":     "Задания заводятся отсюда.",
		"tour.queries":      "Что спрашивать",
		"tour.queries.said": "Впишите фразы или передайте файл с ними.",
		"tour.depth":        "Глубина и регион",
		"tour.depth.said":   "Сколько страниц на фразу и какой выдачи просить.",
		"tour.reach":        "Связь",
		"tour.reach.said":   "Галка — BlankTrail отвечает. Крестик — нет.",

		"theme.dark":  "Перейти на тёмную тему",
		"theme.light": "Перейти на светлую тему",

		"settings.title":               "Настройки",
		"settings.connection":          "Связь",
		"settings.address":             "Адрес",
		"settings.key":                 "Ключ",
		"settings.key.saved":           "Сохранённый ключ оканчивается на",
		"settings.key.keep":            "Оставьте пустым, чтобы сохранённый ключ остался прежним.",
		"settings.check":               "Проверить связь",
		"settings.check.title":         "Что показала проверка",
		"settings.check.good":          "Связь есть",
		"settings.check.good.detail":   "Служба ответила, ключ принят, лицензия активна.",
		"settings.check.nothing":       "Проверке нечего сообщить.",
		"settings.finding.ok":          "В порядке",
		"settings.finding.warn":        "Замечание",
		"settings.finding.fail":        "Отказ",
		"settings.source":              "Настройки прокси",
		"settings.source.where":        "Читать",
		"proxies.profiles":             "Профили прокси",
		"proxies.profiles.why":         "Профиль — это именованный набор выходов: откуда берутся адреса и как используются порты на них. Задание выбирает профиль при создании, так что два списка больше не значат правки этого экрана между двумя прогонами.",
		"proxies.profile.name":         "Название",
		"proxies.profile.default":      "по умолчанию",
		"proxies.profile.open":         "Редактировать",
		"proxies.profile.makedefault":  "Сделать основным",
		"proxies.profile.delete":       "Удалить",
		"proxies.profile.stats":        "Статистика",
		"proxies.profile.cancel":       "Оставить как было",
		"proxies.reading":              "Что шло через профиль",
		"proxies.reading.none":         "С запуска программы ничего не шло — читать пока нечего.",
		"proxies.reading.elsewhere":    "То, что сейчас идёт, выходит через другой профиль:",
		"proxies.profile.new":          "Новый профиль",
		"proxies.profile.editing":      "Профиль:",
		"proxies.profile.needs.name":   "Профилю нужно название: именно его задание называет, чтобы идти через него.",
		"proxies.profile.name.taken":   "Профиль с таким названием уже есть.",
		"proxies.profile.last":         "Это последний профиль, а какой-то должен быть основным. Очистите его источник вместо удаления.",
		"settings.source.none":         "Не используются",
		"settings.source.file":         "Из файла на этой машине",
		"settings.source.url":          "По адресу",
		"settings.source.gateways":     "Шлюзы VPN из BlankTrail",
		"proxies.gateways.loose":       "Загружены отдельно",
		"proxies.gateways.missing":     "Отмечено, но на сервере больше нет:",
		"proxies.gateways.unreachable": "Не удалось запросить шлюзы: по этому адресу никто не ответил.",
		"proxies.gateways.refused":     "Не удалось запросить шлюзы: сервис не принял ключ API.",
		"proxies.gateways.unknown":     "Эта версия сервиса про шлюзы ничего не отвечает.",
		"proxies.gateways.failed":      "Не удалось запросить шлюзы: сервис ответил ошибкой.",
		"proxies.gateways.asked":       "Спрашивали по адресу",
		"proxies.gateways.ping.ms":     "мс",
		"proxies.gateways.ping.gone":   "не ответил",
		"proxies.gateways.ping.never":  "не замерен",
		"proxies.gateways.all":         "Все",
		"proxies.gateways.title":       "Шлюзы",
		"settings.renew":               "Менять личность раз в, минут",
		"settings.renew.why":           "Как часто порт открывается заново — со свежим отпечатком и пустыми куками. Ноль — никогда, и это верно для длинного списка адресов. Короткий список шлюзов — другой случай: десяток личностей, живущих часами, становится десятком знакомых сайту, поэтому при выборе шлюзов предлагается десять минут.",
		"settings.renew.length":        "Минуты между сменами личности — целым числом, ноль — никогда.",
		"proxies.gateways.refresh":     "Обновить",
		"proxies.gateways.taken":       "получен в",
		"proxies.gateways.none":        "Никого",
		"proxies.gateways.unavailable": "На сервере нет пригодных шлюзов.",
		"settings.source.at":           "Откуда",
		"settings.source.refresh":      "Перечитывать раз в, мин",
		"settings.source.forms":        "По одному адресу в строке, в любом из этих видов:",
		"settings.source.form.plain":   "host:port",
		"settings.source.form.after":   "host:port:user:password",
		"settings.source.form.before":  "user:password:host:port",
		"settings.source.form.at":      "user:password@host:port",
		"settings.source.scheme":       "Схема впереди (socks5://, http://) не обязательна; по умолчанию socks5.",
		"settings.source.skipped":      "Пустые строки и строки, начатые с # // или ;, пропускаются.",
		"browse.title":                 "Выбор списка",
		"browse.up":                    "На уровень выше",
		"browse.left":                  "Файлов не показано:",
		"browse.none":                  "Выбирать здесь нечего.",
		"browse.back":                  "Назад к настройкам",
		"browse.unreadable":            "Эта машина не даёт программе прочитать эту папку.",
		"settings.source.choose":       "Выбрать",

		"settings.hot":                "Держать прогретыми",
		"settings.hot.ports":          "Сколько",
		"settings.wire.why":           "SOCKS5 переносит UDP, поэтому работают QUIC и DNS на дальней стороне. HTTP — только TCP, оставлен для отката.",
		"settings.ban":                "Бан на, минут",
		"settings.ban.why":            "Сколько адрес не выдаётся после отказа. Короткий бан возвращает те же мёртвые адреса внутри одного задания, длинный держит вне оборота большую часть списка. Ноль — не выводить из оборота вовсе. Шестьдесят — то, с чего начинает ненастроенная машина.",
		"settings.ban.length":         "Бан на: минуты, целое число, ноль или больше.",
		"settings.hot.why":            "Порты остаются открытыми между заданиями и раз в 15 минут получают один обычный запрос, чтобы не остывать и отвечать сразу. Ноль — не держать. Задание с тем же типом выдачи работает на них и открывает недостающие; с другим типом — открывает свои.",
		"settings.hot.count":          "Сколько личностей держать прогретыми — это целое число, ноль означает не держать.",
		"settings.hot.kind":           "Это не тот тип выдачи, под который программа открывает личности.",
		"settings.language":           "Язык интерфейса",
		"settings.lan":                "Доступ с другой машины",
		"settings.lan.open":           "Отвечать в сеть, а не только этой машине",
		"settings.lan.password":       "Пароль",
		"settings.lan.saved":          "Пароль сохранён. Оставьте поле пустым, чтобы не менять его.",
		"settings.lan.why":            "Выключено — интерфейс отвечает только этой машине. Включено — отвечает на всех адресах машины и спрашивает пароль, прежде чем что-либо показать: своих ключей у страниц нет, а за ними настройки, очередь и всё, что собрали задания. Без пароля не включается. Применится при следующем запуске.",
		"settings.lan.plain":          "Пароль передаётся в каждом запросе и не шифруется: программа говорит по обычному HTTP. Пользуйтесь в доверенной сети либо подключайтесь к машине по VPN.",
		"settings.lan.password.short": "Пароль: не удалось захешировать. Попробуйте другой.",
		"settings.lan.needs.password": "Ответ в сеть требует пароля. Задайте его или оставьте переключатель выключенным.",
		"settings.language.reader":    "Авто выбор",
		"settings.save":               "Сохранить",
		"settings.unreadable":         "Файл настроек есть, и прочитать его не удалось. Ниже — то, с чего эта программа начинает; сохранение перезапишет файл.",
		"settings.opened.nothing":     "Настройки сохранены, и открыть по ним ничего не удалось, поэтому работа выполняется там же, где и раньше. Причина — в журнале сервера.",
		"settings.address.unusable":   "По этому адресу программа обратиться не может.",
		"settings.refresh.length":     "Как часто перечитывать список — это время, и не короче нуля.",
		"settings.source.unknown":     "Список не читают из такого места.",
		"settings.language.unknown":   "На этом языке интерфейс не написан.",
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
