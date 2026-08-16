# google-serp-parser

Open-source **Google SERP parser and rank tracker in Go** — web UI, exports,
and a SerpApi-compatible API. Powered by [BlankTrail Proxy](https://blanktrail.com).

> **This program only works through BlankTrail Proxy.** Google serves a
> JavaScript shell — zero results — to any plain HTTP client, no matter how many
> proxies you put behind it. Measured, not assumed: seven client variants, both
> direct and proxied, all returned a 91 KB shell with no results in it. A
> BlankTrail licence that includes **Challenge Breaker** is a hard requirement.

## Status

The repository skeleton and the `blanktrail` SDK are in place, and the `google`
package now parses result pages: organic results with their exact host and
link form, the three ad placements, and related searches. Every response is
classified before it is parsed, so a genuine empty answer from Google is told
apart from a page that is not results at all — a challenge, a refusal, or the
JavaScript shell that arrives with HTTP 200 and no results in it.

Storage, the web UI and the API land in later milestones.

## Licence

MIT. See `LICENSE`.

## Legal note

This tool reads publicly available search results. You are responsible for
complying with Google's terms of service and with applicable law.

Русская версия: [README.ru.md](README.ru.md)
