# Google SERP Parser

Free and open-source **Google search results parser and rank tracker**, written
in Go. Scrapes Google SERPs through your own proxies or through **VPN gateways**,
checks whether a page is **indexed**, and tracks where a site ranks. Browser
interface, CSV/JSON exports, and a SerpApi-compatible HTTP API. Powered by
[BlankTrail Proxy](https://blanktrail.com).

**Русская версия: [README.ru.md](README.ru.md)**

> ⚠️ **This program only works through [BlankTrail Proxy](https://blanktrail.com) with [Challenge Breaker](https://blanktrail.com/#features)** — a hard
> requirement, not a recommendation. **The solver passes reCAPTCHA:** 1899 of 1936
> challenges solved on a live run of 8514 queries, 98 per cent, and the session
> carries on working afterwards. Everything else is set up in this program, not in that one.

![The status screen: a job in flight, what it has settled, the pool and the queue](assets/screenshots/status-en.png)

---

## 🚀 Quick start

This is the whole road from nothing to a finished job. Everything below it is
reference; this part is what you do.

Two programs are involved and **only one of them is configured**.
[BlankTrail Proxy](https://blanktrail.com) carries the sessions through Google's
challenge; this parser drives it. Nothing is set up on the BlankTrail side — no
proxy list, no ports, no profiles, no gateways chosen there. It has to be
running, and it has to hand over one key. The address list, the identities, the
threads, the jobs and the exports all live in this program's own screens.

No Go, no build, no dependencies. One file to download and one to double-click.

### 1. Start BlankTrail Proxy and copy its key

Install [BlankTrail Proxy](https://blanktrail.com) with a licence that includes
[Challenge Breaker](https://blanktrail.com/#features), and start it. Then take
two things from it:

- **the control API address** — `http://127.0.0.1:8891` unless you have moved
  it;
- **the API key** — *Settings → API key* in BlankTrail. Issue one if there is
  none yet.

That is all it is asked for. You do **not** create ports there, you do **not**
paste your proxy list there, and you do **not** tick VPN configurations there:
the parser opens and closes ports through the control API itself, points each
one at the address or gateway it chose, and closes them again when the job ends.
Its CA certificate is fetched by the parser without anybody having to export it.

Leave BlankTrail running while you work. If it is stopped, the parser says so in
as many words rather than failing quietly.

> Why it cannot be done without it: Google serves no parsable results to a plain
> HTTP client at all. This was measured, not assumed: seven client variants,
> direct and proxied, all came back with a 91 KB JavaScript shell holding no
> results. Results appear only after the solver has carried the session through
> the challenge.

### 2. Download the build for your system

Every link here always points at the newest release.

**Windows — one file, nothing to unpack:**

| System | Download |
|---|---|
| Windows, ordinary PC | **[gserp.exe](https://github.com/BlankTrail/google-serp-parser/releases/latest/download/gserp.exe)** |
| Windows on ARM | **[gserp-arm64.exe](https://github.com/BlankTrail/google-serp-parser/releases/latest/download/gserp-arm64.exe)** |

Put it anywhere and double-click it. Everything the program needs is inside
that file — the pages, the icon, the lot — and it writes its history into a
`gserp.db` beside itself. Put it in a folder of its own: that database, and the
settings file next to it, are the program's whole state.

**Linux and macOS:**

| System | Download |
|---|---|
| Linux, ordinary PC or server | [gserp-linux-amd64.tar.gz](https://github.com/BlankTrail/google-serp-parser/releases/latest/download/gserp-linux-amd64.tar.gz) |
| Linux on ARM | [gserp-linux-arm64.tar.gz](https://github.com/BlankTrail/google-serp-parser/releases/latest/download/gserp-linux-arm64.tar.gz) |
| Mac with Apple silicon (M1 and later) | [gserp-macos-apple-silicon.tar.gz](https://github.com/BlankTrail/google-serp-parser/releases/latest/download/gserp-macos-apple-silicon.tar.gz) |
| Mac with an Intel processor | [gserp-macos-intel.tar.gz](https://github.com/BlankTrail/google-serp-parser/releases/latest/download/gserp-macos-intel.tar.gz) |

Each archive holds the program, a starter script, both READMEs and the licence.
Windows zips are there as well — [amd64](https://github.com/BlankTrail/google-serp-parser/releases/latest/download/gserp-windows-amd64.zip),
[arm64](https://github.com/BlankTrail/google-serp-parser/releases/latest/download/gserp-windows-arm64.zip) — for whoever wants the READMEs
beside the program.

Which version a download is, is on the
[release page](https://github.com/BlankTrail/google-serp-parser/releases/latest)
and in `gserp version`.

### 3. Start it

- **Windows** — double-click `gserp.exe`. The console goes away, an icon
  appears in the notification area, and your browser opens at
  `http://127.0.0.1:8080`. The icon is how you close it again.
- **Linux and macOS** — `./start.sh` in a terminal, or `./gserp serve`, then
  open `http://127.0.0.1:8080` yourself.

It listens on this machine only. Reaching it from another one is a setting, and
it is off until you turn it on — see
[Running it on a server](#-running-it-on-a-server).

On a machine nothing has been run on yet it **walks you through the interface**:
seven stops, each a note beside the thing it is talking about — the screens, the
key, the exits, where a job is started, what a job asks for, and where to see
that the machine can reach anything at all. One sentence a stop.

It does not move the program for you. A stop on another screen outlines the
press that leads there and waits; you press it, and the note follows you across
and settles beside the next thing. Close it with the cross at any point; it is in
the header afterwards for whenever it is wanted.

### Windows says it protected your PC

It will, and the builds here cannot stop it. SmartScreen judges a program by the
reputation of whoever signed it, and these are unsigned: there is no certificate
to have a reputation. Press *More info* → *Run anyway*.

What you can do instead of taking that on trust is check that the file you have
is the file that was published. Every release carries a `SHA256SUMS.txt`, and
the sum of your copy should be in it:

```
Get-FileHash .\gserp.exe -Algorithm SHA256
```

Windows also marks anything downloaded, which is what raises the prompt. The
mark can be cleared once, per file:

```
Unblock-File .\gserp.exe
```

> macOS keeps downloaded programs quarantined in the same way. If it refuses to
> open the file, clear the mark once with `xattr -d com.apple.quarantine gserp`
> in the unpacked folder. On Linux and macOS the sums are checked with
> `sha256sum -c SHA256SUMS.txt` or `shasum -a 256 -c SHA256SUMS.txt`.

### 4. Settings: point it at BlankTrail

Open **Settings** — the link at the right-hand end of the strip of tabs. Fill in
the two things from step 1:

- **Control API address** — `http://127.0.0.1:8891`;
- **API key** — paste it. It is kept on this machine and never shown again;
  afterwards the screen shows only its last characters, so you can tell one key
  from another without the key being readable over a shoulder.

Then press **Check the connection**. It says what it found rather than only
whether it worked, and each answer is a different thing to go and do:

| What it says | What it means |
|---|---|
| *BlankTrail is not answering* | The service is not running, or not on that address |
| *BlankTrail rejected the API key* | The key is wrong or has been reissued. Copy it again from *Settings → API key* |
| *The BlankTrail licence is not activated* | The licence really is the problem — activate it in the dashboard |
| *Challenge Breaker is not included in this tariff* | The plan has no solver, and without one Google answers with a challenge page instead of data |
| *Challenge Breaker is entitled but switched off* | The plan includes it, but no solver processes are configured |
| *More ports than Challenge Breaker processes* | The run will work and will be slower: challenges queue |
| Nothing to report | The connection works |

The rest of this screen can be left alone on a first run:

| Setting | What it is |
|---|---|
| Identities kept warm | Ports held open between jobs, so the next job does not start cold. Nought keeps none |
| Result page | Which kind those warm identities are opened for: desktop or mobile |
| Reaching this from another machine | Off by default. Turning it on asks for a password |
| Interface language | English or Russian. Chosen here and nowhere else |

### 5. Proxies: say where the exits come from

Open **Proxies**. At the top are the **profiles**: a profile is a named set of
exits — where the addresses come from and how the ports on them are used — and a
job names one when it is set up. One profile is marked default: it is what a job
that named none runs on, what the identities kept warm are raised on, and what
the HTTP API's own search goes through.

A machine being upgraded finds one profile already there, called `Default`,
holding the settings it was set up with. Nothing about a run changes until you
make a second one.

Under the list is the form that edits the profile you have open, and under that
the live state of the pool while a job runs. **Read from** chooses the source,
and there are three:

- **A file on this machine** — one address a line. All of these are read:

  ```
  host:port
  host:port:user:password
  user:password:host:port
  user:password@host:port
  ```

  A scheme in front (`socks5://`, `http://`) is optional and `socks5` is
  assumed. Blank lines, and lines starting with `#`, `//` or `;`, are skipped —
  so a list can carry comments.

- **An address** — the same list fetched from your provider over HTTP and
  re-read on the interval you set, which is how a rotating list stays current
  without anybody pasting it again.

- **VPN gateways stored in BlankTrail** — the configurations your subscription
  already carries. They appear grouped by subscription with the round trip last
  measured to each, and you tick one, a whole subscription, or all of them at
  once. This is where gateways are chosen; nothing is chosen on the BlankTrail
  side. *Refresh* asks the service for the list again and re-measures.

A list — a file or an address — also has **Connect through**: the road its ports
take to the addresses. *Nothing* goes straight to each address. *SOCKS5 proxy*
sends every port through that proxy first, written the way an address on a list
is, login and password included. *VPN gateway* sends them through one of the
gateways the service holds, chosen from its list; only the box belonging to the
choice is shown. It is for a list this machine reaches badly: measured on a
wingate list, straight from here the challenge solver could not open a connection
through any address and every search came back as Google's JavaScript check;
through a SOCKS5 proxy in front of them 46 searches in 48 were answered. The
address stays the exit Google sees, so a session made on it stays the same
session. A profile on gateways connects through nothing of its own — a gateway's
own road is set on it in BlankTrail.

The rest of the form is how that profile's pool behaves — each of these belongs
to the profile, so two lists can be banned for different lengths and reached over
different protocols:

| Setting | What it does | Start with |
|---|---|---|
| Re-read every, minutes | How often a list at a URL is read again | 30 |
| Ban for, minutes | How long an address that failed is left out. Nought leaves nobody out | 60 |
| Threads per proxy | How many threads may share one address or one gateway at a time | 1 for a long list, more for a short one |
| Connection to a port | SOCKS5 carries UDP, so QUIC and far-side DNS work. HTTP is the fallback | SOCKS5 |

This screen keeps what belongs to the profile: requests, attempts on the wire,
the share that failed, and failures broken down by kind, so an address that
never answered is told apart from Google refusing. *Clear the counts* starts a
fresh measurement; *Clear the ban* puts every address back into rotation after a
restart or an outage that was not their fault. What the pool is doing this
minute — addresses in the list, banned right now, ports open, warm and in
quarantine — is on the page of the job it is doing it for: a pool is raised for
one job and taken down when that job lets go.

### 6. New job: the first run

Open **New job**. The form is one screen and it says what the run will cost
before you start it.

| Field | What to put in it |
|---|---|
| Name | Anything you will recognise in the history |
| Kind of job | **Parsing** — collect the results. **Position check** — where one site stands for each phrase. **Index check** — whether Google holds an address at all |
| Site to look for | Only for a position check: the domain whose place you want |
| Where the phrases come from | Paste them, one per line, or upload a `.txt` |
| What to keep of each result | Organic results, ads, related queries — each can be kept or left |
| Country, language | Two-letter codes, e.g. `de`, `en` |
| Pages per query | Depth of pagination. One page is the first ten results |
| Result page | Desktop or mobile |
| Dropping duplicates | Keep everything, one row per URL, or one per host |
| Browser, system, version | The fingerprint the job's sessions wear. Nothing chosen spreads them over every browser and system the program knows, at the newest releases |
| Proxy profile | Which set of exits the job goes out through |
| Threads | How many phrases are taken at once. Past a hundred they start five a second rather than all at once |
| Tries per phrase | How many identities one phrase may be carried to before it is called failed |
| Session rest, seconds — from, to | How long one session rests between two of its requests, drawn afresh each time between the two. **Thirty to sixty** by default: on the same 8514 queries it was both faster and cheaper in checks than sixty to a hundred and twenty or fifteen to thirty. The thread does not wait with it — it carries another session meanwhile |

Threads decide how many sessions the run keeps in work at once; a session new to the run pays
one check to Challenge Breaker, and the solver's processes are what your
licence counts.

![The new job form: kind of job, depth, country, what to keep, and the pool it runs on](assets/screenshots/new-job-en.png)

**Press Start.**

### 7. While it runs, and when it is done

The **Status** screen follows the job: queries done and left, the current URL,
queries and pages a minute — the average of the last five minutes — and the
pool behind it.

A job's own page carries its controls under the summary: **Stop**, which is
answered at once (the job reads *stopping* until it has let go of its
sessions), carrying on from where it stopped, and the **export**. Beside the
counts it keeps the **captchas** the job has cost over all its runs; below them
the run's **proxy and session statistics** — sessions working and resting,
Google's checks per thousand pages, what the solver has in hand, and where the
threads are standing.

The first minutes are the slow ones. Every session that rested between jobs is
checked again on its first request, so a run climbs for ten to twenty minutes
and then settles — a run that starts slowly is not a run that is broken. At the
end the last queries are finished without the rest, so a job does not linger on
its tail.

The **Export** tab lays the job's findings out field by field before they are
downloaded: which parts, which columns in which order, separators and line
ends, a byte-order mark for Excel — with a preview of how the file begins. A job
that ended with failures can be told to try the failed phrases again rather than
started over.

![A job at a hundred threads: its counts, its sessions, captchas per thousand pages, the settings it runs with and the newest results](assets/screenshots/job-en.png)

Prefer to build it yourself? See [Building from source](#-building-from-source).


---

## 📜 Recent changes

### 0.3.0

- **A run goes through the program's own sessions** — a Google identity kept
  with its cookies, fingerprint, address and TLS tickets, written down after
  every page. The pages of a query belong to the session that opened it, and a
  session rests thirty seconds to a minute between two of its requests while the
  thread carries another one.
- **A job that keeps addresses runs at full speed.** Google hides a result's
  address behind an encrypted `/goto` link on most of the list — nine to a
  page. A port read for them carries ten at once now, warmest first and never
  resting: **1105 pages a minute** at a hundred threads against 821, the same job
  in 33 minutes against 48. Results linked through Google Translate are read out
  of the translator's link.
- **The end of a job does not linger**: the last queries are asked without the
  rest and their addresses read with all the room there is — the last hundred
  went from seven and a half minutes to one and a half.
- **Big runs**: past a hundred threads they start five a second, and the brake
  that slows a run while the solver is behind tightens and eases by steps.
- **An export tab**: a job's file laid out field by field, with a preview.
- **A first hop**: a list profile can reach its addresses through a SOCKS5 proxy
  or a BlankTrail gateway, and its check takes the same road.
- **VDNS and its resolver** are offered as BlankTrail's own interface offers
  them, in the same order and words.
- **BlankTrail 1.4.987**: its 525 *origin handshake failed* is read as the
  handshake, not the address, and no address is blamed for the TLS defects the
  service has mended.

### Before

- **A light interface, and a dark one by choice.** The interface is light, and
  the dark is a switch in the header — sun and moon, the knob on the side in use
  — remembered per browser. It used to follow
  whatever the machine round the browser said at the time, so an operator on a
  dark desktop had no way of reading this program in the light.
- **The connection is on every screen.** The header says what this program last
  learned about BlankTrail — connected, not answering, key refused, licence
  inactive — and where that is something to act on, a banner above the screen
  leads straight to the settings. It is asked in the background, so no page ever
  waits on a network to be drawn.
- **A job with no way out says so.** The profile written on the first start
  comes from settings that named nothing, so a fresh machine sent every request
  from its own address and no screen said as much. There is a banner now, and
  its press opens that profile's own boxes.
- **What a job has collected** is a count on its own screen, beside the counts of
  queries: ten thousand queries done says nothing about how much there is.
- **A walk through the interface** on a machine nothing has been run on: seven
  stops, one sentence each, standing beside the thing they are about.
- **A thread works several queries at once, a page at a time.** The pause a
  reader sets is the gap between two requests on one identity, and it was being
  taken between two *queries* — so a query walked to a hundred pages was a
  hundred requests through one identity with nothing between them, and the
  number that was set applied to none of them. It is now taken where it belongs,
  which on a hundred-page walk is ninety-nine places it never used to be. And
  the thread no longer stands still through it: it holds several walks at once,
  each on an identity of its own, and works the others while one rests. A query
  keeps its identity for the whole of its walk, which is the one thing that does
  not change — a visitor paging through results does not change address between
  page one and page two. How many a thread holds is nobody's setting: it takes
  another whenever it is about to wait and the pool has one spare, so the count
  settles wherever the pause and the speed of the answers put it.
- **A page Google would not show is asked for again where the session stands.**
  Such a page is Google's check on the address handed back unsolved: the service
  passes a check of that kind in a browser of its own through that same address,
  so the page is that browser failing to open a connection through the exit —
  which it may manage a minute later. It used to condemn the address at once:
  the session was taken off it, landed somewhere else, and paid a check to be
  let in there, which is a page, an address and a check for one refusal. Now the
  page is asked for once more through the same session and the same address, and
  only a second one condemns. Measured live on a list of fifteen thousand,
  sixteen phrases an arm: condemning at once answered 14 of 16 and condemned
  five addresses, asking again answered 16 of 16 and condemned one. The second
  asking counts against the tries the phrase is allowed, so an address that
  answers nothing else is not asked for ever.
- **The hidden addresses are read through ports of their own.** Some regions
  put no address in the markup: the link is a redirector and the address is read
  out of the `Location` header it answers with. Those lookups were going out
  through the same ports the searches do — carrying the challenge solver, which
  a tariff holds only so many of, and writing into the cookie jar of a session
  built for searching — and on such a region they are most of the requests a job
  makes. Measured on a live list, the same links through a searching port,
  through one with the solver switched off, and through one with neither solver
  nor cookie jar read 9 of 9, 9 of 9 and 11 of 11, at 2.33, 2.33 and 2.27
  attempts each; every failure in all three was the address dropping the
  connection. So a lookup needs none of it, and a job that keeps addresses now
  opens a second set of ports that carry none of it. One address, one identity,
  a fresh one for every attempt, and nothing kept between them.
- **Spend the whole proxy list.** A tick beside "ports per thread", and the job
  stops running on a pool of a fixed size: it opens a port of its own for every
  address the list can spare, as it asks for identities, until the list runs out
  or the service has no room left to stand on — and only then hands back a port
  it already has, the one that has rested longest. Every request settles through
  a different address. It is off by default and what it costs is measured: a
  port on a fresh address pays for a challenge on its first request, one to
  three minutes against a second or two through one that has already answered,
  and on a large cheap list three addresses in four carry nothing at all. It is
  there for a run that must not be seen coming from a handful of exits.
- **A redirect is an answer, and the port that carried it is credited with one.**
  A hidden address is read out of the `Location` header of a redirect, so every
  lookup that works answers 302 — and 302 was also the shape the pool took for
  Google's block page. Three lookups in a row, three that had just worked, took
  the address that was carrying them out of the port; the searches that followed
  went out through whatever the list offered next; and a session's first
  request, answered with a redirect from one country domain to another, cost an
  address every time a single miss followed it. On a region that hides its
  addresses that is most of the requests a job makes, so the run was taking its
  own pool apart as fast as it filled it. Reading a refusal off a status was
  right about what a redirect can mean and wrong about who to blame: the port
  carried the request, and what the answer means belongs to the layer that knows
  what a Google page says — which reads the page it lands on and puts the
  identity out of the rotation, as it always did.
- **An address that is Google's own is not an address.** A lookup answered with
  a redirect that stays on Google is the identity being sent to a challenge, or
  the link handed on to another redirector. The header was taken at face value,
  and a live run wrote a result whose address was the redirector itself. That is
  worse than an empty one: an empty address says nobody could reach it, and that
  one says the ranking site is Google, to every export and rank history
  downstream. Such an answer is now no address at all, the link is carried to
  another identity, and the one that met it is put out of the rotation.
- **A page is worked at until every address is had.** The lookups gave up after
  three identities, and three is not a number this kind of list has any time
  for. Measured against a live fifteen-thousand-address list, one link at a
  time, one identity per attempt: 29 hidden addresses cost 114 attempts to read
  all 29 — one attempt in four — and every single failure was the address
  dropping the connection rather than the far end answering something else.
  Three identities read 55% of those addresses, eight read 93%, fifteen read all
  of them. A page is now carried to as many identities as a query is, and for
  the same measured reason. A round asks only for what is still missing, so a
  page down to its last address costs one request a round, and a region that
  states its addresses costs nothing at all.
- **Proxy profiles.** A profile is a named set of exits, and a job names one when
  it is set up. Two lists no longer mean editing one screen between two runs, and
  a finished job can say which exits it went out through. A machine being
  upgraded finds its own settings already in a profile called `Default`, and
  nothing about a run changes until a second one is made.
- **A refused API key says so.** The check read the refusal off the one endpoint
  the service answers without a key, so it always arrived at the next call and
  was reported as a licence that could not be read. Two support rounds were spent
  looking at a licence that was fine.
- **The addresses Google hides are looked up.** Some regions answer with an
  encrypted link that carries no address at all; the step that fills those in
  existed and was called by nothing, so every result of such a page was recorded
  with an empty address.
- **The pause reaches the run.** Every job set up in the interface ran with no
  pause between two requests on one identity, whatever was typed: the box was
  missing from the door a browser actually posts to.
- A lease goes to an identity that has **answered before**, and never waits for
  one. Measured at ten threads for twenty minutes an arm, at the same minute:
  three ports a thread answered 259 against 164 for one port, and 154 against 54
  in the second half, once the identities were warm.
- A job **waits for an identity** rather than spending a query on not having
  one. A pool with everything set aside almost always has something to give
  shortly, and the screen counts who is queueing.
- A port whose listener has gone is **opened again**, and a gateway whose tunnel
  has died is restarted before it is left. Neither used to happen: a restart of
  the proxy service took every port in the job with it.
- **VPN gateways held in BlankTrail** can be used instead of an address list,
  grouped by subscription and ticked one, one subscription, or all at once,
  each showing the round trip the service last measured to it.
- **Threads per proxy** is a setting now. One `ip:port` or one gateway is one
  upstream however many ports sit on it, and threads that find none free wait
  their turn where the status screen can be seen counting them.
- Identities are now reached over **SOCKS5**, so QUIC and far-side DNS work
  through them. HTTP is still selectable as a fallback.
- New **Proxies** tab: live pool figures, failures broken down by kind,
  counters you can zero for a clean measurement, and a one-press ban reset.
- A run is paced by **its own settings**. A job that asks for no pause keeps
  none — the pool it runs on is no longer paced by the standing set's numbers.
- A restart of the proxy service no longer bans the whole address list: a port
  that never answered is told apart from an address that failed.
- Measured on a live 15 000-address list of middling datacentre proxies:
  **500–800 queries a minute at 100 threads, peaking at 1 500, 99% of
  queries answered.** Same pool, same hardware — the reference test client
  does 227.

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
- **Sessions of its own**: Google identities kept with their cookies, their
  fingerprint and the address they go out through, rested between requests and
  written down as they go. A query's pages are walked by the session that opened
  it.
- Hidden result addresses — Google's encrypted `/goto` links — read on ports of
  their own, ten lookups to a port, while the search goes on.
- A job stops on a button and **carries on from exactly where it stopped**.
- Every result is written down as it lands, so a run that dies keeps everything
  it had established.
- History of every job, searchable and re-exportable at any time.

### Interface

- Browser interface — no command line needed for anything.
- **Status** screen: the job in flight, elapsed against estimated, share
  answered, what came back instead, identities free to hand out, threads waiting
  for a free proxy, queue. What the pool is doing this minute is on the running
  job's own page.
- **Proxies** screen: requests, attempts, failure share, address changes, ports
  opened again; failures broken down by kind, with counters you can zero at any
  moment.
- A running job's own page reads the identities under it: addresses in the list
  and banned right now, ports open, warm and in quarantine, sessions — how many
  there are, how many are in work, how many are resting and how many this job
  had made for it — and whether it is still taking sessions on, running at its
  own speed, or out of addresses to open one on. A thread makes a session only
  when none of the ones there are is ready for it, so a run that has stopped
  making them has as many as its threads can keep busy: the speed on the screen
  is then the speed the job runs at rather than one it is still climbing to.
- The same page reads Google's checks: how many this job has been made to pass,
  how many the solver is working on, how many are waiting for it, and **how many
  checks each thousand pages have cost** the run so far — right from its first
  minute, the checks a fresh session pays to be let in included.
- And it says **where its threads are standing**: every one of them is in
  exactly one of six places at each instant — waiting for a port, taking a
  session, held back by the brake, asking Google, giving the session back,
  writing the result down, or waiting for something to be due — so the shares
  are the whole of the run's time and can be read against each other. Asking is
  the wait that is the work and everything else is a place to go and look at.
  The last column is how long the thread that has been in a place longest has
  been there, which tells a place threads pass through from one they are stuck
  in. It is what a slow run is taken apart with: measured on a job doing a tenth
  of its speed, the threads were not idle anywhere — they were asking, and every
  ask ended in a refusal within fifteen seconds.
- A profile's **list can be checked** from the profile's own screen, as many
  addresses at once as the box says, along the road that profile's ports take.
  The service is asked to reach each address itself, and where the profile names
  a first hop both roads are reported: measured on a live list of fifteen
  thousand, 75 of 80 addresses answered through the hop and 48 of 80 without it.
  A check that took the road nobody uses would call a working list dead, which
  is why there was no list check at all until there was one that could take the
  right road. The sample is spread across the whole list and shuffled — the
  first hundred of a list are the same hundred every time.
- The gateway list is read once and held for a couple of minutes, with a
  **Refresh** that asks the service again — for when a configuration has just
  been added or the tunnels have just been measured.
- A job that failed can be told to **try the failed queries again**, which is
  not the same button as carrying on with what is left.
- A **walk through the interface** for a machine nothing has been run on: seven
  stops, each a note beside the thing it is about, on the screens themselves
  rather than on a page describing them.
- What this program last learned about **BlankTrail** in the header of every
  screen, and a banner with one press where it is something to act on.
- Live URL of the request going out right now.
- **Light by default, dark by choice** — a switch in the header with a sun and a
  moon on it, remembered per browser. It is a link, so it works with no script
  running at all.
- English and Russian: what the browser asks for, or whichever is chosen in the
  settings.
- The screens show numbers and draw no conclusions from them. They never call a
  run slow — they do not know what you know about your list.

### Proxies and identities

- Address list from a **file or a URL**, re-read on an interval you set.
- Formats accepted: `host:port`, `host:port:user:password`,
  `user:password:host:port`, `user:password@host:port`. A scheme in front
  (`socks5://`, `http://`) is optional; socks5 is assumed.
- A **first hop** in front of every address of a list — a SOCKS5 proxy or a
  BlankTrail gateway — for a list that can only be reached from somewhere in
  particular.
- **VDNS and the resolver** set per profile, offered as BlankTrail's own
  interface offers them.
- **VPN gateways held in BlankTrail** as a third source. The program asks the
  service what it holds and lays the configurations out by the subscription
  they arrived with; tick one, tick a whole subscription, or tick the lot. Each
  shows what it answered when the service last measured it — a number, or the
  word for a gateway that did not answer, or for one nobody has measured.
- Identities are kept **warm between jobs**: a cold identity meets a challenge
  and answers minutes later, a warm one answers in seconds.
- A failed address is banned for a set time and comes back on its own; the ban
  never covers more than three quarters of the list, so the pool cannot run out
  of addresses to try.
- Load is spread evenly: every address is used once before any is used twice.
- **Threads per proxy**, one by default — and worth raising to two or three on a
  long address list, where the spare identities are asked less often and last
  longer. On a short list of gateways it is the other way: more identities
  behind one exit is more for that exit to answer for. A unique `ip:port` is one
  upstream and so is one gateway, even where several ports sit on the same machine. Threads
  that find no free upstream wait their turn rather than doubling up on one,
  and the status screen says how many are waiting.
- Failures counted apart, because the remedy differs — a dead address, a proxy
  port that never answered, a relay refusal, a wall from Google, our own
  timeout.
- A port the proxy service is no longer listening on is opened again, and a
  gateway whose tunnel has died is restarted on the same gateway before being
  left: the service runs a gateway only while a port holds it.
- Nothing is spent on an empty pool. A job with no identity to take queues for
  one instead of writing the query down as failed, so a service that goes away
  costs the time it is away and not the rest of the list.

![The proxy screen: the profiles this machine has, which one is default, and one press each to open a profile or read what has been going through it](assets/screenshots/proxies-en.png)

*Three hundred identities on a fifteen-thousand-address list, a hundred of them
warm: a fifth of the attempts fail and 99 per cent of those are addresses that
carried nothing at all. That is what a datacentre list looks like from the
inside, and it is why the failures are counted apart — nothing there is Google
refusing, and nothing there would be mended by trying more politely.*

Choosing the gateways instead puts the subscription's own configurations on the
same screen, grouped by subscription, each with the round trip the service last
measured to it:

![The proxy screen reading VPN gateways: the configurations a subscription carries, by subscription, with what each last answered](assets/screenshots/proxies-gateways-en.png)

### Export and API

- **CSV**, **JSON Lines**, **TXT** — results, ads and related queries.
- An **export tab** that lays a job's file out field by field — parts, columns,
  order, separators, line ends, a byte-order mark — with a preview. TXT writes a
  single field one per line and nothing else.
- Deduplication by URL or by host, or none.
- HTTP API under `/api/v1/`: set a job going, watch it, stop it, resume it.
- Streaming endpoints: a job's results and a site's history, one JSON object
  per line, so a million rows read a line at a time.
- **SerpApi-compatible** `GET /search`: a program written against that service
  works after changing the base address and nothing else.
- Access by key, issued with `gserp key new`; only a hash is stored.

---

## ⚙️ Settings

### Connection

| Setting | What it is |
|---|---|
| Control API address | Where BlankTrail answers, e.g. `http://127.0.0.1:8891` |
| API key | Issued in BlankTrail. Stored on this machine, never shown again |
| Identities kept warm | Ports held open between jobs. Nought keeps none |
| Result page | Which kind the warm identities are opened for: desktop or mobile |
| Answer the network | Off by default — see [Running it on a server](#-running-it-on-a-server) |
| Password | What the pages ask for from another machine |
| Interface language | English or Russian. Chosen here, and nowhere else |

### Proxies

| Setting | What it is |
|---|---|
| Read from | Nothing, a file, a URL, or the VPN gateways held in BlankTrail. The form takes the shape of the choice: a path and a browse link for a file, an address box for a URL, the list itself for the gateways |
| File on this machine / Address of the list | Where the list is |
| Re-read every, minutes | How often the list is read again. Nought reads it once |
| Ban for, minutes | How long a failed address is left out. Nought leaves nobody out; sixty is where a fresh install starts |
| Threads per proxy | How many threads share one address or one gateway. One by default |
| Profile | Which named set of exits this is. A job names one; the default one is what a job naming none runs on |
| Connection to a port | SOCKS5 (default) or HTTP |
| Gateways | Which stored configurations to use, when the source is the gateways |

### Per job

| Setting | What it is |
|---|---|
| Proxy profile | Which set of exits the job goes out through. Changeable afterwards on the job's own page |
| Threads | Queries taken at once. Past a hundred they start five a second |
| Tries per phrase | How many identities one phrase may be taken to |
| Session rest, seconds — from, to | How long one session rests between two of its requests. Thirty to sixty by default |
| Browser, system, version | The fingerprint the job's sessions wear |
| Pages per query | Depth of pagination |
| Country, language | Two-letter codes, e.g. `de` |
| Deduplication | Keep everything, one row per URL, or one per host |

---

## 🌐 Running it on a server

By default the interface answers **this machine only**: it listens on
`127.0.0.1`, and nothing on the network can reach it. That is the right default
because the pages carry no key of their own — they hold the settings, the queue
and everything every job has collected, and they hand it to whoever opens them.

To use the interface from another machine, open **Settings → Reaching this from
another machine**, tick *Answer the network* and set a password. It takes effect
at the next start, and from then on the program listens on every address this
machine has and asks for the password before showing anything.

- **It cannot be turned on without a password.** The settings refuse the switch
  on its own, and a program started with a switch and no password stays on
  loopback and says so in its first line.
- **This machine is not asked.** A browser on the machine itself goes straight
  in: a password there is a lock on a door you are already inside.
- **The password is not encrypted in transit.** This program speaks plain HTTP,
  so the password travels in every request and anything between can read it.
  Use it on a network you trust, or reach the machine over a VPN or an SSH
  tunnel.
- What is written down is a salt and a PBKDF2-SHA256 derivation, never the
  password. A settings file somebody photographs does not hand it over.

`--addr` still wins over the setting, for the operator who wants a particular
address:

```
gserp serve --addr 0.0.0.0:8080
```

That one opens the port without asking for anything, so put it behind something
that does.

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

Go 1.26 or newer. No cgo, no build tags, no code generation:

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

With [`just`](https://github.com/casey/just) installed, `just --list` shows the
same things under shorter names: `just build`, `just test`, `just lint`.

### Reproducing a release

The published builds are made by [`scripts/dist.sh`](scripts/dist.sh), which is
what the release workflow runs — there is no separate recipe kept somewhere
only CI can see. To build the same artefacts yourself:

```
git checkout v0.1.0
./scripts/dist.sh 0.1.0
cd dist && sha256sum -c SHA256SUMS.txt
```

The sums are checked from inside `dist/` because the file names in them carry
no directory — which is what lets the same file check a release you downloaded
into a folder of your own.

Binaries are built with `-trimpath`, so they carry no trace of the machine that
built them and two people building one tag get one file. The archives are not
byte-for-byte reproducible — they record the moment they were packed — so
compare the binaries inside them rather than the archives. Your build will
differ from the published one in one way on purpose: it carries no integration
key, which the next section explains.

### Attribution

BlankTrail credits a subscription to whoever's work brought the customer, and a
program says which one it is by stamping an *integration key* into the running
service — it travels in the body of the service's licence requests, never in a
link, and it is not a secret.

The downloads above are stamped with this project's key, so a subscription
bought because of the parser is credited to it. A binary you build yourself
carries no key and stamps nothing. Whichever key is in the build, three rules
hold, and you can read them in [`cmd/gserp/integration.go`](cmd/gserp/integration.go):

- a build with no key of its own leaves the service alone;
- a key already stamped by another integrator is never written over;
- nothing about any of it can stop a run — if the service refuses, the parser
  says so once and carries on.

To stamp a build with your own key:

```
go build -ldflags "-X github.com/blanktrail/google-serp-parser/internal/version.integrationKey=dk_yours" ./cmd/gserp
```

`scripts/dist.sh` reads the same key from `GSERP_INTEGRATION_KEY`, and stamps
nothing when it is unset — which is what a build made by anybody but this
project does, here and in a fork's own release workflow.


---

## 📈 Performance

Measured on a live list of 15 000 addresses — ordinary datacentre proxies of
middling quality, about one in twelve answering at any moment — through
BlankTrail 1.4.987, on 8514 queries up to ten pages deep with the result
addresses kept:

| | 100 threads | 300 threads |
|---|---|---|
| Pages a minute, once the start has passed | **~1100** | **~1570** |
| Pages a minute, the whole job | 890–950 | 1235 |
| Whole job, 8514 queries | 33–49 min | 26 min |
| Captchas per 1000 pages, the whole job | 30–38 | 38 |
| Challenges solved | 98% | — |
| Hidden addresses read, per page | ~9 | ~9 |

How long the whole job takes depends mostly on how deep Google lets the queries
go: the same list came back at 3.7 pages a query on one run and 5.1 on another.

The start is the slow part. Every session that rested between jobs is checked
again on its first request, so a run climbs for ten to twenty minutes and then
settles. The end is not: the last queries are finished without the rest.

What limits it past three hundred threads was not this program. At five hundred
the machine running BlankTrail had every core busy — about sixteen of them on
the challenge solver — and five hundred threads were slower than three hundred.
What these numbers depend on beyond that is your address list, your BlankTrail
licence and the machine it runs on.

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
