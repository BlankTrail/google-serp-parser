# google-serp-parser

Open-source **Google SERP parser and rank tracker in Go** — web UI, exports,
and a SerpApi-compatible API. Powered by [BlankTrail Proxy](https://blanktrail.com).

> **This program only works through BlankTrail Proxy.** Google serves a
> JavaScript shell — zero results — to any plain HTTP client, no matter how many
> proxies you put behind it. Measured, not assumed: seven client variants, both
> direct and proxied, all returned a 91 KB shell with no results in it. A
> BlankTrail licence that includes **Challenge Breaker** is a hard requirement.

## Status

**There is a browser interface.** `gserp serve` opens it, and `start.bat` or
`start.sh` opens it and your browser with it. A job is set up in a form, which
says what it will cost before you start it; it then runs in front of you, stops
on a button, carries on from exactly where it stopped on another, and downloads
as CSV or JSON Lines from a link. Every page is in English and Russian, nothing
is loaded from anywhere, and the whole interface is inside the binary. Jobs
queue: one runs at a time, on ports that stay open between them, because
opening a fresh set per job costs minutes before the first answer. With no key
in the environment the same interface still reads the history; it says so
rather than offering a button that cannot work.

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

The SerpApi-compatible API lands in a later milestone.

## Licence

MIT. See `LICENSE`.

## Legal note

This tool reads publicly available search results. You are responsible for
complying with Google's terms of service and with applicable law.

Русская версия: [README.ru.md](README.ru.md)
