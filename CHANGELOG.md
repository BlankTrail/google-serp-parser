# Changelog

[← Overview](README.md) · [Русский](CHANGELOG.ru.md)

### 0.4.15

- **Addresses read as letters.** The results table of a job and the History
  showed an address with letters of other alphabets as percent escapes
  (`/%D0%BD%D0%B5…`). They now show them as letters, the way a browser's
  address bar does; the link, the database and every export keep the address
  exactly as it came. Escapes that would change what the address says (`%2F`,
  `%3F`, `%23`, `%25`, `%26`), spaces, and invisible or direction-changing
  characters stay escaped.

### 0.4.14

- **Refreshing the VPN gateways stays in the profile.** Pressing *Refresh* on a
  profile on the gateways went back to the bare proxies screen, which shows the
  default profile, so the profile being edited closed instead of its list being
  read again. It now comes back to that profile; a profile being made comes back
  as one being made, still on the gateways.

### 0.4.13

- **An index check of a page address reads the address behind Google's hidden
  links.** Where Google answers with encrypted links (`/goto`), a result carries
  its site but not its address, and a check of an address answered "not
  indexed" for every page there is — 1000 of 1000 addresses taken from Google's
  own `site:` results. The results of the asked-about site are now read before
  the verdict, through the same lookups a parse job uses; a check of a whole
  site needs none.

### 0.4.12

- **A phrase that never reached Google is asked again in the same run.** When
  every try of a phrase went to addresses that carried nothing, the run left it
  for the next start, and a big job on a list with many dead addresses ended
  *not finished* with its threads idle — on a test of 7773 phrases at 300
  threads and 3 tries, 1009 were left behind. Now, once the queue is out, such
  phrases get another pass with fresh tries, again and again while the run is
  getting answers. A pass in which nothing reached Google still leaves them for
  a later run, as before, rather than writing them down as failures. The same
  test now ends finished with none left.

### 0.4.11

- **A meaning filter for search suggestions.** A model downloaded once from the
  settings page (≈132 MB, minishlab/potion-multilingual-128M, MIT) scores every
  suggestion for how close it is to its key; old jobs are scored from their
  page. The export filters *by words* or *by meaning* with a threshold and says
  how much it leaves out. Measured on 7773 Russian keys: by meaning removes
  about two thirds of the junk for about 4% of the good suggestions.
- The history moves to schema version 32 on the first start.

### 0.4.10

- **Suggestions with no word of their key are marked** and left out of the
  export unless asked for; a *Related to the key* column says which is which.

### 0.4.9

- **The question Google hands back is left out**: a suggestion that keeps the
  letter typed on its own where it was typed is not collected.

### 0.4.8

- **A port BlankTrail has closed is opened again**, so a job survives the
  service restarting under it.

### 0.4.7

- **TLS sessions with BlankTrail are resumed.** A search opens a connection to
  the service for every page, and every one of them paid a full TLS handshake.
  Each port now keeps a small cache of its own TLS sessions, and a new
  connection resumes one. Checked against BlankTrail 1.4.1068; older builds of
  the service simply do not resume. Nothing of it reaches Google.
- Keep-alive for searches was examined and left off: a session lands on the
  same port about once in a hundred, and a kept connection would let a new
  session travel through the previous one's tunnel.

### 0.4.6

- **A search suggestions job and the hidden-address lookups keep their
  connections to BlankTrail alive** instead of opening one a request — TCP,
  SOCKS5 and a full TLS handshake each time. A port drops every connection it
  holds whenever it is moved to another address. The same suggestions job of
  200 requests: 127 of 175 on a connection used before, 9.1 seconds against
  14.4.

### 0.4.5

- **Query formats and macros** for a parsing job — `{query}`, `site:{query}`,
  `"{query}"`, `{ABC:a:z:2}`, `{num:1:1000}`, several formats a job — see
  [the first run](docs/GUIDE.md#first-job). A parsing job's list is now kept
  once: a line listed twice is searched once.
- A suggestions job made from the page receives its **Multiword**, its **cap on
  requests per key** and a choice to keep every repeat; they were lost on the
  way.
- A suggestions job's export names its columns **key number, key, suggestion**,
  and its part **suggestions**.

### 0.4.1 – 0.4.4

- A suggestions job reads as **keys a minute** and **requests a minute**; its
  results are the key and the suggestion, with no position and no address.
- **Repeats dropped by the suggestion's text** across the whole job, by default.
- A warning on the form for a suggestions job with **no search language**,
  which types its keys in Latin letters.
- **Tries per request** for suggestions, and no session rest, which that kind
  does not use.

### 0.4.0

- **Search suggestions**, a fourth kind of job: every completion Google's
  search box offers for each key, the key typed the way `run4linkgen.php`
  typed it — its seven patterns, its alphabets, its Multiword — with the links
  made as the job runs. Two keys with Multiword on twenty threads: 402
  requests in 12 seconds, 1052 and 1189 completions.
- **The end of a job does not wait on one slow check**: once nothing is left to
  hand out, a query whose request has waited a minute is started again through
  another session beside it, and the first to finish settles it. The last
  thirty queries of 8514 took 1.8 minutes instead of 3.9.

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
