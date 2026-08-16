# HTTP API

`gserp serve` puts two interfaces on one address:

* **`/api/v1/…`** — this program's own REST: set a job going, watch it, stream
  what it captured, read a site's history, run one search.
* **`/search`** — the same single search in the shape **SerpApi** answers in, so
  a program written against that service works after changing the base address
  and nothing else.

Everything below is JSON. Every refusal, from every address, is one object with
one field: `{"error": "..."}`. Parse that once.

The server listens on `127.0.0.1:8080` by default. It has **no rate limiting**,
keys have **no scopes**, and there are **no webhooks** — a job is polled.

## Keys

Issue one with the command line:

```
gserp key new -name nightly     # prints the secret once — it is not recoverable
gserp key list
gserp key revoke -id 3
```

Only a hash of a key is stored, so a lost key is replaced rather than read back.
Every address below is behind the key check; without a good key the answer is
`401` with the same sentence whether the key was never issued, was revoked, or
was never sent.

A key is accepted **two ways**:

```
Authorization: Bearer <key>          ← use this one
GET /search?q=…&api_key=<key>        ← accepted because other clients send it
```

> **A key in the query string ends up in other people's logs.** The query string
> is part of the address, and addresses are what intermediaries write down: every
> proxy, gateway, load balancer and CDN between you and this server records that
> line, in the clear, in its access log, for as long as its logs are kept. It
> also lands in browser history and in `Referer` headers. A header is recorded by
> none of them. `?api_key=` exists only so somebody else's client works unchanged
> — if you can set headers, set the header, and treat a key that has travelled in
> an address as a key that has been read by strangers.

## SerpApi-compatible search — `GET /search`

```
GET /search?q=iphone+13&gl=us&hl=en
Authorization: Bearer <key>
```

| Parameter | Meaning |
|---|---|
| `q` | what to search for. Required. |
| `gl` | country. The Google domain follows from it. |
| `hl` | interface language. |
| `engine` | must be `google` or absent. See below. |
| `device` | must be `desktop` or absent. |
| `google_domain` | must be the domain `gl` already sends the search to. |

Answer:

```json
{
  "search_metadata": {
    "id": "…", "status": "Success",
    "created_at": "2026-08-16 21:04:11 UTC",
    "processed_at": "2026-08-16 21:04:13 UTC",
    "total_time_taken": 1.83
  },
  "search_parameters": {
    "engine": "google", "q": "iphone 13", "google_domain": "google.com",
    "gl": "us", "hl": "en", "device": "desktop"
  },
  "organic_results": [
    {"position": 1, "title": "…", "link": "…", "displayed_link": "…", "snippet": "…"}
  ],
  "related_searches": [{"query": "iphone 14", "link": "…"}],
  "ads_omitted": 3
}
```

### Seven things that differ from the service this shape copies

**1. `link` is sometimes a Google address rather than the site's.** Google writes
a result's address three ways and chooses between them itself; two captures of
one query minutes apart have differed. Two of the three carry the destination in
the page, and `link` is then the destination. The third carries only ciphertext,
and the exact destination behind it costs one extra request per result to learn —
which this address will not spend, because it has a deadline to answer inside. So
for those results `link` is **Google's own redirector for that result**, made
absolute. It goes where a click goes, but **the host you read out of it is
Google's**. If you need the destination host, read `displayed_link` — it is
always the site — or set a job going, which resolves the links.

**2. Advertising is not returned at all.** Google draws three paid products and
this program reads only a title from one of them; rendering them into paid
records whose price, seller and address fields are empty because nobody looked
would read as a page that carried none of those things. Instead, `ads_omitted`
says **how many paid placements were on the captured page and are not reported**.
It is not one of SerpApi's field names, so their clients ignore it. **The field is
absent when the page carried no advertising at all.**

**3. `position` is the place on that one page of results, counted from 1**, with
no gaps: this address captures a single page, and nothing is dropped on the way
out. It is not a rank across a multi-page walk. The cross-page rank lives in a
job's stored rows (`rank` in `/api/v1/jobs/{id}/results`) and not here.

**4. Four things are refused with `400` that the copied service would answer.**
Each of them would otherwise hand you results that are not the ones you asked
for, and you could not tell:

| Refused | Why |
|---|---|
| `engine` other than `google` | see below |
| `device` other than `desktop` | the layout is recorded and acts on nothing sent, so a mobile capture would be the desktop page under another name |
| `google_domain` that `gl` does not send the search to | one country's results under another country's name |
| no `q` | nothing was asked for |

An empty or absent `engine` is fine — `google` is the copied service's default
and your program is entitled never to have set it. Nothing is asked of the
identities before these refusals: a refused request spends nothing.

**5. The key is taken two ways**, with the warning above.

**6. `search_metadata.id` is unique per answer, and nothing is stored under it.**
The copied service keeps every search and hands the identifier back so you can
fetch it again; this program keeps a search answered inside the request nowhere
at all. The identifier is worth having as the key of your own log line, and
**you cannot fetch a search again by it**. If you need results kept, set a job
going.

**7. The refusal codes are the same ones `/api/v1/search` uses** — `400`, `401`,
`429`, `502`, `503`, `504`. See the table at the end.

### Engines

```
GET /search?q=iphone&engine=google_images
→ 400 {"error": "engine=google_images is not supported by this server.
        Supported engines: google. …"}
```

**`google` is the only engine, and that is the state of the product rather than a
temporary gap.** Images, News, Shopping, Video, Maps, Trends and Translate are
deferred to future releases; nothing here reads any of those pages. The refusal
names the engine you asked for and lists the ones that exist, so a program that
asks for images learns it at once instead of filing ordinary web results as image
results and discovering it a week later.

## This program's own REST

### `GET /api/v1/search`

The same single search in this program's own shape. Same parameters `q`, `gl`,
`hl`; same limits and same refusal codes.

```json
{
  "query": "iphone 13", "country": "us", "language": "en",
  "took_seconds": 1.83,
  "results": [
    {"position": 1, "title": "…", "url": "…", "host": "apple.com",
     "displayed_path": "iphone", "snippet": "…", "resolved": true}
  ],
  "related": ["iphone 14"]
}
```

**`resolved` is the named answer to the unresolved-link problem.** Where the
captured page carried no destination, `url` is empty and `resolved` is `false`.
It is carried as its own field rather than left to be inferred from an empty
string, so a program cannot read "the page did not say" as "this capture
failed". `host` and `displayed_path` are exact either way.

### Jobs

```
POST   /api/v1/jobs                 set a job going
GET    /api/v1/jobs?limit=50        newest first
GET    /api/v1/jobs/{id}            one job and how far it has got
POST   /api/v1/jobs/{id}/stop       end the job in flight
POST   /api/v1/jobs/{id}/resume     take up a job left part way
```

Body of a creation:

```json
{"name": "nightly", "queries": ["iphone 13", "# a note is skipped", "golang"],
 "pages": 2, "country": "us", "language": "en",
 "ports": 6, "threads": 2}
```

Blank lines and lines beginning with `#` are notes and are dropped, exactly as in
the browser form and in a query file. `ports` and `threads` describe the pool you
run; they shape the **estimate only** and change nothing about how the job runs.

`201` answers with the job and what it will cost:

```json
{"job": {"id": 7, "name": "nightly", "created_at": "…", "finished_at": null,
         "finished": false, "pages": 2, "country": "us", "language": "en",
         "spec": "", "total": 2, "done": 0, "failed": 0, "pending": 2,
         "running": false, "queued": true},
 "estimate": {"queries": 2, "pages": 2, "searches": 4, "requests": 6,
              "max_requests": 12, "ports": 6,
              "expected_seconds": 214.0, "floor_seconds": 88.0}}
```

**One job runs at a time**, whether it was set going here or from the browser:
both go through the same queue, on the same identities, into the same history.
Poll `GET /api/v1/jobs/{id}` and watch `running`, `queued`, `done`, `failed`,
`pending`, `finished`.

### Streams

```
GET /api/v1/jobs/{id}/results
GET /api/v1/history?host=example.com
```

Both answer `application/x-ndjson`: **one JSON object per line**, never one
array, so a job of a million rows is readable a line at a time and a transfer cut
short is worth every line before the break. Read a line, parse it, start the
next.

A job's rows:

```json
{"ordinal": 0, "query": "iphone 13", "page": 1, "rank": 1,
 "title": "…", "url": "…", "host": "apple.com", "snippet": "…"}
```

Here `rank` **is** the position across the whole walk — page 2's first result is
rank 11, not rank 1. This is the number `position` in `/search` is not.

A site's history, oldest run first:

```json
{"job_id": 7, "job_name": "nightly", "taken_at": "2026-08-16T21:04:11Z",
 "query": "iphone 13", "rank": 4, "url": "…"}
```

A site that has never ranked is answered with **nothing, at `200`**. That a site
does not rank is an answer, not a missing thing.

The status is chosen before the first byte of a stream goes out, so a `404` or a
`401` on these addresses is always final and never arrives half way down. If the
connection breaks mid-stream you get a short body and no error object — count the
lines you got.

## Response codes

| Code | Meaning | What to do |
|---|---|---|
| `200` | answered | — |
| `201` | job created and queued | poll it |
| `400` | the request asks for something that cannot be answered truthfully | fix the request |
| `401` | no key, an unknown key, or a revoked one | — |
| `404` | no such job, or no such address | — |
| `409` | the job is already running, is not the one running, or has nothing left | — |
| `429` | as many searches are already under way as this server allows at once | retry shortly; it is refused immediately rather than queued |
| `500` | the server could not read its own records | — |
| `502` | the search came back unreadable | look at the query, then retry |
| `503` | the server was started without the means to search or to run jobs, or nothing can answer at the moment | come back later |
| `504` | the search did not finish inside the time this server allows | **not a refusal** — the same request may be answered next time |

`429` is a hard, immediate refusal, not a queue. A caller refused in a
millisecond can back off; a caller held for a minute against a full limiter
reaches its own timeout and retries anyway, having spent the wait and the place.

## What to expect of a single search

A search answered inside the request does **not** wait behind the job queue — it
would otherwise sit behind a job of ten thousand queries — but it does compete
with a running job for the same capacity, and it has a deadline it must answer
inside (five minutes unless configured otherwise).

These are measurements from live runs against eight identities, not guarantees.
Each row is one probe. Both runs are shown because the difference between them
is the point: the second was made after the deadline was raised from one minute
to five, and the searches are the same searches.

| search, in the order sent | under a 1m deadline | under a 5m deadline |
|---|---|---|
| 1st — identities that had never searched | `504` at 1m0s | `504` at 5m0s |
| 2nd | `504` at 1m0s | `502` at 3m19s |
| 3rd | `504` at 1m0s | `200` in 2m31s |
| 4th | `504` at 1m0s | `200` in 2m8s |
| 5th | `504` at 1m0s | `200` in 4.3s |
| 6th | `200` in 29.8s | — |
| sent while a job of ten queries was running | `200` in 6.4s | `200` in 3.0s |
| three at once against a limit of one | 2 × `429` in 2–3ms | 2 × `429` in 2–3ms |

Three things to plan for:

* **The first minutes after a start are the pool finding identities that
  answer, not the search being slow.** That cost has been measured separately
  at 27s to 9m10s. Once it is paid, the same endpoint answers in 3–6 seconds.
* **The deadline is five minutes because the solver's own bound on one
  challenge is 300 seconds.** A shorter deadline hangs up on work that was
  still going to succeed and reports it as a search that did not finish —
  which is exactly what the first column is. Raising it turned four straight
  failures into three answers, and it did **not** make the first search
  succeed: on a cold pool, expect to spend the warm-up whatever the deadline
  is.
* **A running job is not the problem.** A search sent while a job was running
  came back in 3.0s, because by then the identities were warm. If your program
  cannot spend the warm-up, send searches to a server that has been up a while,
  or set a job going instead.

The server does **not** warm the pool when it starts, and that is a decision
rather than an omission. A job of any size warms it within its first handful of
queries, so the warm-up gets paid by work somebody asked for; a server started
to serve the pages would otherwise spend identities and solver time on searches
nobody wanted. If your program's first call must be fast, set a small job going
first.
