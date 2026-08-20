# Google SERP Parser

Open-source **Google search results parser and rank tracker**, written in Go.
Browser interface, CSV/JSON exports, and a SerpApi-compatible HTTP API.
Powered by [BlankTrail Proxy](https://blanktrail.com).

**Русская версия: [README.ru.md](README.ru.md)**

> ⚠️ **This program only works through BlankTrail Proxy with Challenge Breaker.**
> Google serves a JavaScript shell — zero results — to any plain HTTP client, no
> matter how many proxies you put behind it. This was measured, not assumed:
> seven client variants, both direct and proxied, all came back with a 91 KB
> shell containing no results. Results appear only after the solver has carried
> the session through the challenge. A BlankTrail licence including **Challenge
> Breaker** is a hard requirement, not a recommendation.

![The status screen: a job in flight, what it has settled, the pool and the queue](assets/screenshots/status-en.png)

---

## 📜 Recent changes

- Identities are now reached over **SOCKS5**, so QUIC and far-side DNS work
  through them. HTTP is still selectable as a fallback.
- New **Proxies** tab: live pool figures, failures broken down by kind,
  counters you can zero for a clean measurement, and a one-press ban reset.
- A run is paced by **its own settings**. A job that asks for no pause keeps
  none — the pool it runs on is no longer paced by the standing set's numbers.
- A restart of the proxy service no longer bans the whole address list: a port
  that never answered is told apart from an address that failed.
- Measured on a live 15 000-address list: **500+ queries a minute at 100
  threads, 98% of requests answered.** Same pool, same hardware — the reference
  test client does 227.

---

## 📚 Features

### Core

- Three kinds of job:
  - **Parsing** — reads each line as a phrase and saves every result.
  - **Position check** — searches the same phrases and records where a given
    site stands, or that it was not found.
  - **Index check** — reads each line as an address and asks whether Google
    holds it.
- Organic results, ads and related queries, each exportable on its own.
- Pagination to any depth; country and interface language per job.
- Desktop and mobile result pages.
- Jobs queue and run one at a time, on ports that stay open between them.
- A job stops on a button and **carries on from exactly where it stopped**.
- Every result is written down as it lands, so a run that dies keeps everything
  it had established.
- History of every job, searchable and re-exportable at any time.

### Interface

- Browser interface — no command line needed for anything.
- **Status** screen: the job in flight, elapsed against estimated, share
  answered, what came back instead, ports held and ports set aside, queue.
- **Proxies** screen: addresses in the list, banned right now, ports open and
  warm; requests, attempts, failure share, address changes; failures broken
  down by kind, with counters you can zero at any moment.
- Live URL of the request going out right now.
- English and Russian, switchable in one click.
- The screens show numbers and draw no conclusions from them. They never call a
  run slow — they do not know what you know about your list.

### Proxies and identities

- Address list from a **file or a URL**, re-read on an interval you set.
- Formats accepted: `host:port`, `host:port:user:password`,
  `user:password:host:port`, `user:password@host:port`. A scheme in front
  (`socks5://`, `http://`) is optional; socks5 is assumed.
- Identities are kept **warm between jobs**: a cold identity meets a challenge
  and answers minutes later, a warm one answers in seconds.
- A failed address is banned for a set time and comes back on its own; the ban
  never covers more than three quarters of the list, so the pool cannot run out
  of addresses to try.
- Load is spread evenly: every address is used once before any is used twice.
- Failures counted apart, because the remedy differs — a dead address, a proxy
  port that never answered, a relay refusal, a wall from Google, our own
  timeout.

![The proxy screen: the pool as it stands, what failed and of what kind, and the address list](assets/screenshots/proxies-en.png)

*Three hundred identities, warmed, on a fifteen-thousand-address list: seven
per cent of attempts fail and nearly all of those are addresses that never
answered. The counts were cleared a few minutes before the shot, which is what
the button is for.*

### Export and API

- **CSV**, **JSON Lines**, **TXT** — results, ads and related queries.
- Deduplication by URL or by host, or none.
- HTTP API under `/api/v1/`: set a job going, watch it, stop it, resume it.
- Streaming endpoints: a job's results and a site's history, one JSON object
  per line, so a million rows read a line at a time.
- **SerpApi-compatible** `GET /search`: a program written against that service
  works after changing the base address and nothing else.
- Access by key, issued with `gserp key new`; only a hash is stored.

---

## 🚀 Quick start

No Go, no build, no dependencies. One file to download and one to double-click.

**1. Install and start BlankTrail Proxy** with a licence that includes
Challenge Breaker. Note its control API address (`http://127.0.0.1:8891` by
default) and issue an API key in it.

**2. Download the build for your system** from
[Releases](https://github.com/BlankTrail/google-serp-parser/releases/latest)
and unpack it anywhere:

| System | File |
|---|---|
| Windows, ordinary PC | `gserp-<version>-windows-amd64.zip` |
| Windows on ARM | `gserp-<version>-windows-arm64.zip` |
| Linux, ordinary PC or server | `gserp-<version>-linux-amd64.tar.gz` |
| Linux on ARM | `gserp-<version>-linux-arm64.tar.gz` |
| Mac with Apple silicon (M1 and later) | `gserp-<version>-macos-apple-silicon.tar.gz` |
| Mac with an Intel processor | `gserp-<version>-macos-intel.tar.gz` |

Each archive holds the program, the starter for that system, both READMEs and
the licence. The program is a single file that needs nothing installed
alongside it, and it keeps its history in a `gserp.db` next to itself.

**3. Start it.**

- **Windows** — double-click `start.bat`.
- **Linux and macOS** — `./start.sh` in a terminal.

Either way the program comes up and your browser opens at
`http://127.0.0.1:8080`. It listens on this machine only.

> macOS keeps programs downloaded from the internet quarantined. If it refuses
> to open the file, clear the mark once with
> `xattr -d com.apple.quarantine gserp` in the unpacked folder.

**4. Open Settings** and fill in the BlankTrail address and API key. Press
*Check the connection* — it says what it found rather than only whether it
worked.

**5. Open Proxies** and point it at your address list: a file on this machine
or a URL. Set how often to re-read it, and how long a failed address stays
banned.

**6. Open New job**, paste your phrases or upload a `.txt`, choose the kind of
job, the country, the depth, and the number of threads. The form says what the
run will cost before you start it.

![The new job form: kind of job, depth, country, what to keep, and the pool it runs on](assets/screenshots/new-job-en.png)

**7. Press Start.** The Status screen follows it. When it is done, download the
results as CSV or JSON Lines from the job's own page.

![A finished job: its counts, the settings it ran with, the export links and the results](assets/screenshots/job-en.png)

Prefer to build it yourself? See [Building from source](#-building-from-source).

---

## ⚙️ Settings

### Connection

| Setting | What it is |
|---|---|
| Control API address | Where BlankTrail answers, e.g. `http://127.0.0.1:8891` |
| API key | Issued in BlankTrail. Stored on this machine, never shown again |
| Identities kept warm | Ports held open between jobs. Nought keeps none |
| Result page | Which kind the warm identities are opened for: desktop or mobile |

### Proxies

| Setting | What it is |
|---|---|
| Address list | None, a file, or a URL |
| Path or address | Where the list is |
| Re-read every, minutes | How often the list is read again. Nought reads it once |
| Ban for, minutes | How long a failed address is left out. Nought is sixty |
| Connection to a port | SOCKS5 (default) or HTTP |

### Per job

| Setting | What it is |
|---|---|
| Threads | Queries taken at once |
| Ports per thread | Identities opened per thread. Threads × ports = pool size |
| Tries per phrase | How many identities one phrase may be taken to |
| Pause on one identity, seconds | Gap before an identity is asked again. **Nought means none** |
| Pages per query | Depth of pagination |
| Country, language | Two-letter codes, e.g. `de` |
| Deduplication | Keep everything, one row per URL, or one per host |

---

## 🖥 Command line

The browser interface needs none of this, but everything is scriptable.

```
gserp run [flags]        work a list of queries, saving each one as it lands
gserp serve [flags]      serve the browser interface and the API
gserp key new [flags]    issue an API key and print it the once it can be seen
gserp key list [flags]   list the keys that exist, without their secrets
gserp key revoke --id N  stop one key working
gserp doctor [flags]     check a BlankTrail instance against an intended run
gserp version            print the version
```

### `gserp run`

| Flag | Meaning |
|---|---|
| `--queries` | File with one query per line; blank lines and `#` are passed over |
| `--db` | History database to write (default `gserp.db`) |
| `--out`, `--format` | File to export into, and `csv` or `jsonl` |
| `--pages` | Result pages per query (default 1) |
| `--threads`, `--ports` | Queries at once, and ports each gets |
| `--country`, `--language` | Two-letter codes |
| `--name` | Name to file the job under |
| `--resume` | Take up the last unfinished job of this name |
| `--dry-run` | Print the estimate and send nothing |

### `gserp serve`

| Flag | Meaning |
|---|---|
| `--addr` | Address to listen on (default `127.0.0.1:8080`) |
| `--db` | History database to open |
| `--threads`, `--ports` | Defaults for a job that names no size |
| `--trace` | Log every request an identity makes: where it waited, what came back |

### Environment

| Variable | Meaning |
|---|---|
| `BLANKTRAIL_URL` | Control API base URL (default `http://127.0.0.1:8891`) |
| `BLANKTRAIL_API_KEY` | API key |
| `GSERP_PROXY_LIST_URL` | Address list to egress through; direct when unset |

The key and the address list are read from the environment and are not flags: a
key on a command line is a key in the shell history.

---

## 🔌 HTTP API

Full reference: [`docs/api.md`](docs/api.md).

Issue a key first:

```
gserp key new --name my-script
```

A single search, in this program's own shape:

```
GET /api/v1/search?q=coffee+grinder&gl=us&hl=en&num=10
Authorization: Bearer <key>
```

The same search in SerpApi's shape:

```
GET /search?q=coffee+grinder&gl=us&hl=en&api_key=<key>
```

Jobs and streams:

```
POST /api/v1/jobs                 set a job going
GET  /api/v1/jobs/{id}            watch it
GET  /api/v1/jobs/{id}/results    stream what it captured, one object per line
GET  /api/v1/history              stream a site's history the same way
```

Three things worth knowing before writing against it:

- **`google` is the only engine.** A request for another is refused with a 400
  that names what was asked for and lists what there is.
- **No advertising is handed over.** The answer says how many paid placements
  were on the page and were not reported, so none go missing silently.
- **A single search does not wait behind the job queue**, but it does compete
  with a running job for ports and has a deadline of its own.

There is no rate limiting, keys have no scopes, and a job is polled rather than
announced.

---

## 🏗 Building from source

Go 1.24 or newer. No cgo, no build tags, no code generation:

```
git clone https://github.com/BlankTrail/google-serp-parser
cd google-serp-parser
go build ./cmd/gserp
```

Cross-compiling is the ordinary Go way:

```
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o gserp ./cmd/gserp
```

Tests:

```
go test ./...
```

---

## 📈 Performance

Measured on a live backconnect list of 15 000 addresses, 100 threads,
300 identities, a real job of 400 000 phrases:

| | |
|---|---|
| Queries a minute, sustained | **500+** |
| Requests answered | **98%** |
| Attempts per result | 1.19 |
| Time a thread spends waiting for an identity | 0 |

What these numbers actually depend on is your address list and your BlankTrail
licence. A cold pool starts slow — every identity pays for one challenge — and
climbs for the first five to ten minutes. That is the shape to expect, not a
fault.

---

## 🛠 Feedback

Bugs and feature requests: please open an issue on GitHub. Include what you
did, what happened, and what you expected — and, if the run was slow, the
figures from the **Proxies** screen, which is what that screen is for.

`gserp serve --trace` logs every request an identity makes: where it waited and
what came back. That log is the fastest way to a diagnosis.

---

## ❤️ About BlankTrail

This parser is open source and free. It exists because Google no longer answers
plain HTTP clients at all, and because the piece that solves that —
[BlankTrail Proxy](https://blanktrail.com) — is worth showing at work rather
than describing.

---

## 📄 Licence

MIT. See [LICENSE](LICENSE).

The software is provided "as is", without warranty of any kind. You are
responsible for how you use it, including for observing the terms of service of
the sites you point it at and the law where you are.
