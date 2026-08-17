# google-serp-parser

Open-source **Google SERP parser and rank tracker in Go** — web UI, exports,
and a SerpApi-compatible API. Powered by [BlankTrail Proxy](https://blanktrail.com).

> **This program only works through BlankTrail Proxy.** Google serves a
> JavaScript shell — zero results — to any plain HTTP client, no matter how many
> proxies you put behind it. Measured, not assumed: seven client variants, both
> direct and proxied, all returned a 91 KB shell with no results in it. A
> BlankTrail licence that includes **Challenge Breaker** is a hard requirement.

## Status

**There is a programmable interface, and it is documented in
[`docs/api.md`](docs/api.md).** `gserp serve` puts it on the same address as the
pages, over the same history and the same queue, so a job set up by a program is
the job the browser lists. Under `/api/v1/` a program sets a job going, watches
it, stops it and takes it up again; two addresses stream what was captured — a
job's results and a site's history — as one JSON object per line, so a job of a
million rows is read a line at a time and a transfer cut short is worth every
line before the break. A single search is answered inside the connection that
asked for it, in this program's own shape and, at `/search`, in the shape SerpApi
answers in: a program written against that service works after changing the base
address and nothing else. Access is by key — `gserp key new` issues one and only
a hash of it is stored — carried either in an `Authorization` header or, because
somebody else's client already sends it that way, in the query string, which the
documentation warns puts it into the access log of every proxy between you and
the server.

Three things about that interface are worth knowing before writing against it.
**`google` is the only engine**, and a request for another is refused with a 400
that names the engine asked for and lists the ones there are, rather than quietly
answering an image search with web results. **No advertising is handed over**,
and the answer says how many paid placements were on the page and were not
reported, so none of them go missing silently. And a single search does not wait
behind the job queue — it would otherwise sit behind a job of ten thousand
queries — but it does compete with a running job for ports and has a deadline of
its own; a search that reaches that deadline is told so plainly rather than left
hanging. There is no rate limiting, keys have no scopes, and a job is polled
rather than announced.

**There is a browser interface.** `gserp serve` opens it, and `start.bat` or
`start.sh` opens it and your browser with it. A job is set up in a form, which
says what it will cost before you start it; it then runs in front of you, stops
on a button, carries on from exactly where it stopped on another, and downloads
as CSV or JSON Lines from a link. Jobs queue: one runs at a time, on ports that
stay open between them, because opening a fresh set per job costs minutes
before the first answer.

**One screen shows what is happening now**: the job in flight with the time it
has taken standing beside the time that was estimated for it, how much of what
has been settled was refused and what came back instead, how many ports are
held and how many have been set aside, and what is waiting its turn. It shows
numbers and draws no conclusions from them — it never calls a run slow, because
it does not know what you know.

**The connection is set up in the browser** rather than on a command line: the
proxy address and key, how many queries run at once and how many ports each
gets, the pause between two requests, where the list of addresses comes from
and how often to read it again, and the language the interface answers in. A
button tries what is in the boxes and writes nothing. Saving while a job is
running asks which of the two prices to pay — stop that job now and take the
settings into use at once, or let it finish on what it started on — and neither
answer restarts the program.

**A list of any size arrives as a file.** It is read as it arrives and written
in batches, so nothing ever holds the whole list; a list that stopped arriving
part way leaves a job that can be seen and cannot be run, rather than one
quietly short. A job can drop repeats as they land — the same address twice, or
a second result from a site already seen — and says how many it dropped.
**A job is one of three things, and they differ by what is asked rather than by
what is written down.** Parsing is the ordinary one and the one the form starts
on: a list of phrases in, and everything each of them came back with written
down. A position check takes the same list and one site, and answers with the
place that site stood at for each phrase, or that it was not in the pages taken
— it stops walking at the page that answers, so a site on the first page costs
one page and not the depth. An index check takes a list of addresses and answers
whether Google holds each one. Both checks count only results that are the site
or the page asked about, so a site's other pages never stand in for it, and
neither of them can tell you "not found" about a line nobody reached: a line that
was never checked is absent from the answers rather than reported as an absence.

Every page is in English and Russian, and a further language can be added with
a file beside the program without rebuilding it — what such a file is missing
is printed when the program starts, not shown in the browser. Tabs switch
without reloading, and every screen is also a whole page the server draws, so
it can be opened cold and bookmarked. Nothing is loaded from anywhere and the
whole interface is inside the binary. With no key in the environment and no
connection saved, the same interface still reads the history; it says so rather
than offering a button that cannot work.

`gserp run` works end to end: it takes a list of queries, spreads them over
threads, writes each result to a database as it lands, and exports what it
found as CSV or JSON Lines. A run stopped with Ctrl+C says what is still to do
and the command that takes it up; `gserp run -resume` finishes exactly what was
left. `gserp run -dry-run` says what a job will cost before any of it is sent.

Underneath: the `google` package reads result pages — organic results with
their exact host and link form, the three ad placements, related searches —
and classifies every response before parsing it, so a genuine empty answer is
told apart from a challenge, a refusal, or the JavaScript shell that arrives
with HTTP 200 and no results in it. On top of that sit the engines: pagination,
a site's position, whether a page is indexed, and search completions. A refused
answer is carried to another identity rather than lost, and addresses that stop
working are replaced as the run goes, so a list accumulates the ones that work
by using them.

## Licence

MIT. See `LICENSE`.

## Legal note

This tool reads publicly available search results. You are responsible for
complying with Google's terms of service and with applicable law.

Русская версия: [README.ru.md](README.ru.md)
