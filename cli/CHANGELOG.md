# Changelog

Every release of a published package gets an entry here (repo rule: a bad
deploy is rolled back, a bad published version is on other people's machines).

## 2026-09-09 — upcontrol 0.2.1

- **The board topic stopped contradicting the wire topic.** `dashboard.md` told the agent that
  a funnel, an A/B test, a retention or a breakdown card takes a ref whose `source` is
  `funnel` / `experiment` / `retention` / `dimension` and whose `name` is "the name you declared
  it as in the SDK call", while `wire.md` said two pages later that there is no declaration
  step. The doc was wrong: those sources are refused, and a board built from it drew nothing at
  all, silently — the file's own warning says that column is load-bearing and unvalidated.
  Every analytics card now reads `source: "people"`, and the shape of the ref picks the reading:
  `steps` a funnel, `cohort` a retention grid, `name` + `field` a breakdown, `name` alone an
  A/B test whose arms ride `uc.variant` and `uc.stat`. Two worked examples added.
- `funnel.md`, `experiment.md`, `retention.md` and `breakdown.md` keep their SDK recipes — a
  helper's name becomes the event name on the wire — but stop claiming the board picks what was
  declared. An A/B card orders its arms by people and names `control` as the baseline; a
  breakdown counts PEOPLE, so a `value()` with no `who` has nobody to count.
- Needs core 0.26.1 or newer: before it, a board carrying any of these refs was refused by its
  own save.

## 2026-09-09 — upcontrol 0.2.0

- **A new topic, `wire`: how to send from a stack that is not Node.** The SDK is a JavaScript
  convenience, not the product — everything upcontrol does is one HTTP endpoint, and since
  the feeds are counted on the server there is no client library to have. Funnels, retention,
  A/B tests and breakdowns now work from Go, Rust, Python, PHP or a static page that can POST
  a line. The topic covers `uc.actor` (what makes a person countable), the rule that the id
  must be stable, sending off the request path, and why a browser needs a public key rather
  than the secret one — naming a `VITE_`-prefixed variable for a secret key ships it to every
  visitor, which is a security incident and not a bad diff.
- The goal table gains the row that routes a non-Node repository there, so an agent stops
  reading TypeScript recipes at a Go service.
- `SDK_PIN` moves to `1.0.0`.

## 2026-09-08 — @upcontrol/sdk 1.0.0

- **The feeds stop counting and start reporting.** The API does not change by a character —
  `funnel()`, `experiment()`, `retention()`, `breakdown()` and every method on them keep
  their signatures, so no caller edits a line — but each call is now one `track()` event
  carrying a reserved `uc.actor` attribute, and the server counts DISTINCT people from the
  events it already stores. The SDK no longer dedups: the same person stepping twice sends
  two events, and the count stays honest anyway. A request is still fingerprinted (address +
  user-agent, salted) so a raw address never leaves the process; a string id passes through
  unchanged as the caller's own stable identifier.
- **The state file is gone, with its whole class of bugs.** No more
  `node_modules/.cache/upcontrol/funnels.json`, no `UPCONTROL_STATE_DIR`, no unreadable-file
  restart, no unwritable-dir silent loss, no million-id memory ceiling, no ten-minute slow
  save, no v1→v2 state migration to carry. The salt is per-process now: nothing local is
  deduped against it, so it no longer has to survive a restart.
- 1.0.0 rather than 0.8.0 because a minor would lie about what a caller is upgrading into:
  the wire changes (events with `uc.actor` where metric readings used to be) and so do the
  storage semantics. Needs a server that understands the events (upcontrol.io already
  does; a self-hosted core needs the matching release). `SDK_VERSION` is `1.0.0`.
- This is also what makes the feeds reachable from any language: an event with `uc.actor`
  is all a Go or Python client has to send.

## 2026-09-08 — upcontrol 0.1.9

- `SDK_PIN` moves to `0.7.0`. The pin is exact, so a fresh `npx upcontrol init` was
  installing `0.5.0` — a version whose breakdowns cannot count people at all (no `who`
  argument) and whose every dimension value silently lost its first count. The two SDK
  releases below are only reachable through this bump.
- The bundled skill is rebuilt from `cli/plugin/`, so the breakdown recipe no longer tells
  an agent that `value()` "takes the value and nothing else: no request, no user id". It
  now teaches both modes, which is what the SDK has done since 0.6.0.
- The README no longer enumerates agent products by name; the badge links to the list the
  detector actually supports.

## 2026-09-08 — @upcontrol/sdk 0.7.0

- **A bucket that appears mid-flight now states its rise from zero, so its first count is
  no longer invisible.** The reader folds a counter by subtracting each reading from the one
  before it and counts the very first as nothing, having nothing to subtract. A funnel and an
  A/B test escape that by declaring their buckets up front and reporting them at 0 from the
  first minute — but a breakdown's values and a retention's cohorts appear as they are
  counted, so each one's first increment was dropped for good. A country with exactly one
  visitor never appeared at all. Such a bucket now reports a 0 a millisecond before its first
  real count. A bucket restored from the state file does NOT, because a zero under a running
  total is a drop, and a drop reads as a reset worth its whole value.

## 2026-09-08 — @upcontrol/sdk 0.6.0

- `breakdown()`'s `value(v, who?)` takes an optional second argument. Additive: a call
  without `who` counts events carrying the value, exactly as it always has, so existing
  code changes nothing. A call passing `who` counts DISTINCT PEOPLE carrying the value —
  a person once per value, deduped by the same salted hash the funnel, A/B test and
  retention feeds dedup by.

## 2026-09-07 — upcontrol 0.1.8

- `SDK_PIN` moves to `0.5.0`, so a fresh install gets the multi-instance fix below rather
  than a version that reports feeds the board cannot separate. Nothing else changes: same
  commands, same skill.

## 2026-09-07 — @upcontrol/sdk 0.5.0

- Every counter reading now carries a `uc.reporter` label naming the process that sent it,
  which is what lets the server keep two instances of one app apart when it folds the
  counters. Before this, an app running more than one instance reported feeds the board
  could not separate: the instances interleaved on one series and every dip between them
  read as a counter reset worth the counter's whole value, so a funnel, A/B test, retention
  grid or breakdown read far too high: not doubled, but inflated by roughly the counters'
  own size, an error that grew with uptime. No application code changes; the fix needs a
  server that understands the label (upcontrol.io already does; a self-hosted core needs
  0.19.0 or newer). `SDK_VERSION` is `0.5.0`.

## 2026-09-06 — upcontrol 0.1.7

- `SDK_PIN` moves to `0.4.0`. The bundled skill gains three topics beside `funnel`:
  `experiment` (declare the arms, `expose()` where the arm is chosen, `convert()` where it
  converts), `retention` (`seen(userId)`, and the page is blunt that it must be the app's own
  stable id, because an address is not the same person a week later) and `breakdown`
  (`value()`, counting events rather than people). Nothing else in the installer changed.

## 2026-09-06 — @upcontrol/sdk 0.4.0

- `experiment(name, variants)`, `retention(name)` and `breakdown(name)` join `funnel()` on
  the same counter machinery, extracted once instead of copied: counts that only grow, one
  report a minute, one state file. An experiment counts a person once per variant per stat
  (`expose`/`convert`; control is the first variant declared, and `i` carries the variant's
  declaration index so the board orders the card's rows by it). Retention buckets people
  into the Monday-week cohort they were FIRST seen in and reports cohort-by-week, keeping
  the last 12 weekly cohorts so the state cannot grow without limit. A breakdown counts
  events, not people (no `who`, no dedup), and keeps its first 200 distinct values so a
  dimension fed a request id cannot grow without limit either. The funnel's on-disk state
  (`node_modules/.cache/upcontrol/funnels.json`, or `UPCONTROL_STATE_DIR`) is read forward:
  a 0.3.0 file loads with its salt and its counts, and the file is written from now on in a
  new shape (`v: 2`) that carries every feed. `SDK_VERSION` is `0.4.0`.

## 2026-09-06 — upcontrol 0.1.6

- `SDK_PIN` moves to `0.3.0`. The skill's `funnel` topic is now the funnel feed (declare once,
  one `step()` line per step); the recipe for placing behaviour events (payments, churn,
  activation) moved to the `behavior` topic and the skill's table points there. Published
  after the SDK, as the pin requires.

## 2026-09-06 — @upcontrol/sdk 0.3.0

- `funnel(name, steps)` declares a journey and `step(name, who)` counts a person at a step
  once: a request is fingerprinted on the server (a salted hash of its first `x-forwarded-for`
  hop or socket address and its user-agent; known crawler user-agents are skipped), a string
  id is hashed the same way, and nothing about the person leaves the process. Every minute the
  funnel goes out as one metric reading per step (`metric: "funnel"`, labels `funnel`, `step`
  and a zero-padded `i`), a counter that grows for as long as the process lives and picks up
  from its last save after a restart (a reader treats a lower reading as a reset), so it never
  lands in the log window and the board can show any range as a slice. The people seen are
  kept in `node_modules/.cache/upcontrol/funnels.json` (or `UPCONTROL_STATE_DIR`), up to a
  million per step, written off the event loop; a filesystem that does not survive a deploy
  starts the counts again. A request with no resolvable address is not counted, and each
  process counts its own people.
- `SDK_VERSION` is `0.3.0`.

## 2026-08-30 — upcontrol 0.1.5

- `SDK_PIN` moves to `0.2.0`, so a fresh `init` pins the `@upcontrol/sdk` that sends
  `UPCONTROL_SERVICE`, the variable the bundled skill now documents. Publish the SDK
  first: the pin names a version that must already be on npm.

## 2026-08-30 — @upcontrol/sdk 0.2.0

- `UPCONTROL_SERVICE` names the process on every line the SDK sends (`track()`,
  `upcontrolLine()`, the console and winston mirrors, and the SDK's own
  `install_verified` / `upcontrol_buffer_dropped` lines). A `service` attribute the
  caller passes wins over it. Unset, nothing changes: the line has no `service` and is
  byte-identical to 0.1.1. The dashboard's service column and filter already read the
  field; only the SDK never sent it.
- `SDK_VERSION` is `0.2.0` and a test now asserts it equals `package.json`: 0.1.1 shipped
  reporting `js/0.1.0` in the `X-UpControl-Sdk` header and in `install_verified`.

## 2026-08-29 — upcontrol 0.1.4

- `status`, `--version` and the `cli_version` field every install sends report
  the real version again. `CLI_VERSION` is a constant of its own, because the
  built `dist` cannot import `package.json` (deliberately outside the package
  exports), and nothing enforced the two matching: 0.1.3 shipped announcing
  itself as 0.1.2. A test now asserts they are equal, and cli/ has a CI job for
  the first time, so neither package can be published untested again.

## 2026-08-28 — @upcontrol/sdk 0.1.1

- `trace` is no longer rewritten to `debug`. A `trace` line sent through the
  SDK now arrives and is stored as `trace`, a level of its own on the server
  alongside `debug`, `info`, `warn` and `error`. Before 0.1.1 the SDK rewrote
  the level before it left the process, so the level the caller chose never
  reached the server: the line was stored as `debug`, and `level_raw`
  recorded `debug` too.
- `prepack` builds before npm packs or publishes: `dist/` is gitignored and
  is the only thing `files` ships.

## 2026-08-28 — upcontrol 0.1.3

- `init` stops lying about success (self-host cold-install rehearsal,
  finding 7): a run that tried to establish a key and could not — a failed
  or unreachable `--token` redeem, a refused or throttled anonymous mint —
  now reports `"success": false` and exits 1. The skill and SDK pin still
  land, and the `key.note` line says exactly what to fix. An agent that read
  `"success": true` while the key never arrived wired an app that silently
  sent nothing — the one defect a monitoring tool may not have.
- `init` no longer collects or sends a project spec, and `--no-meta` is gone
  with it. Nothing is taken away from anyone: that code only ever existed in
  the unpublished 0.1.2. The five-field upload existed for exactly one reader,
  the Explain prompt, so that an answer about a log line knew the stack it
  came from, and Explain has been removed from the product, endpoint
  included: there is no `PUT /v1/project/meta` on a current server. The
  upload was best effort by contract (a refused or unreachable PUT never
  failed an install), so an
  older installer pointed at a current server keeps working exactly as
  before; its spec is simply ignored.
- The bundled agent skill no longer teaches the frozen event dictionary (it
  was removed from the product).
- `SDK_PIN` moves to `0.1.1`, so a fresh `init` pins `@upcontrol/sdk` exactly
  at the version that keeps `trace` intact.
- `prepack` builds before npm packs or publishes. `dist/` and `skill/` are
  gitignored and are the only things `files` ships, so a publish from a fresh
  clone would otherwise have put an empty tarball on npm.
- Publish order: `@upcontrol/sdk` must be published before `upcontrol`. The
  installer pins an exact SDK version, and a pin to an unpublished version
  fails every user's install.

## 2026-08-17 — upcontrol 0.1.2 (never published)

This version was tagged in the repository and never reached npm, which went
0.1.1 straight to 0.1.3. Everything below was written and then removed again
before any release carried it, so no user ever ran it. Kept for the record.

- `init` collects a five-field project spec — `{name, description, framework,
  runtime, language}` from `package.json`, an ordered framework map
  (next/nuxt/nest/express/fastify/koa/svelte/vue/react; first match in deps or
  devDeps wins) and `tsconfig.json` presence. Never dependency lists,
  versions, file paths, git remotes, env values or code. It prints the exact
  spec before sending (`project spec (sent so AI log analysis knows your
  stack; nothing else is read):` … `  (skip with --no-meta)`) and PUTs it to
  `PUT /v1/project/meta` with the project key in `X-Upcontrol-Key`.
  The spec prints AFTER init's own summary (it is a detail of the install,
  not the headline), values are flattened to one line and capped at the 200
  runes the server accepts, and a `package.json` that says nothing about the
  product — no name, no description, no framework — uploads nothing at all,
  because `PUT` replaces the whole spec.
- `--no-meta` skips collection and upload entirely; a refused, failed or
  unreachable upload (and an uncollectible spec) skips silently — meta can
  never fail or delay an install. The upload waits at most 3s. The spec
  follows the key this run established (`--token`, `--key`, a fresh mint)
  rather than whatever `UPCONTROL_API_KEY` happens to name. The server stores
  the spec scrubbed and reads it only as Explain context.
- Every API call now reads its response body and closes its connection, and
  the CLI sets `process.exitCode` instead of calling `process.exit()`. Exiting
  on the spot tore the process down on top of a live socket, which on Windows
  aborted with `Assertion failed: !(handle->flags & UV_HANDLE_CLOSING)` (exit
  `0xC0000409`) whenever the backend refused an upload — and truncates piped
  stdout in general.
- `@upcontrol/sdk` unchanged at 0.1.0.

## 2026-08-15 — upcontrol 0.1.1

- `init --token uct_...` — redeems the one-time token the dashboard's install
  card generates (`POST /v1/install/token`, front-distribution-alignment.md
  §1) and lands the key of THAT account's project in `.env`. A refused token
  (spent/expired) never falls back to the anonymous mint: landing logs in a
  project that is not yours is the exact surprise the token exists to prevent.
- Backend counterparts (back/, same pass): `POST /v1/install/token`
  (session-authed, TTL 10 min, single-use) and `POST /v1/install/redeem`
  (burns the token, issues an ADDITIONAL api_key — not a rotation), migration
  015 (`install_token` table). The Sources install card now generates the
  command on click; the dashboard never shows a bare `npx upcontrol`.
- `@upcontrol/sdk` unchanged at 0.1.0.

## 2026-08-15 — upcontrol 0.1.0 · @upcontrol/sdk 0.1.0

The first real release (0.0.1 was a name-reservation placeholder).

**upcontrol** (the installer CLI):
- `init` (default command): installs the agent skill into `.claude/skills/` +
  `.agents/skills/` (`--copilot` adds `.github/skills/`), byte-compared and
  refreshed on re-run; pins `@upcontrol/sdk` exactly in package.json;
  provisions a key — env → `.env` → anonymous mint (`POST /v1/projects/
  anonymous`), written only after `.gitignore` provably covers `.env`, never
  echoed anywhere; prints the claim URL once.
- Agent detection (env markers: Claude Code, Cursor, Codex, Gemini CLI,
  Copilot, Windsurf, amp, aider, cline, opencode; `AI_AGENT` override): agents
  and pipes get one-line JSON, humans get prose + a starter prompt.
- `skills [topic]` — serves the bundled references (dictionary, rules, logs,
  funnel, jobs, uptime, key, verify) so the installed skill can defer to the
  CLI as its source of truth.
- `verify [--timeout N] [--json]` — polls `GET /v1/install/status`; exit 0
  verified / 2 no key / 3 unreachable / 4 failed, with the §8 failure taxonomy.
- `status` — one JSON line: endpoint, key source, skill freshness, verified.
- Zero runtime dependencies. Default endpoint `https://upcontrol.io`,
  overridable via `UPCONTROL_ENDPOINT` / `--endpoint`.

**@upcontrol/sdk** (the push library):
- `track()` / `flush()` + logger bridges: `upcontrolLine` (pino hook),
  `UpcontrolTransport` (winston, dependency-free), `mirrorConsole`.
- `@upcontrol/sdk/auto`: `app_started`, `unhandled_exception` (via
  `uncaughtExceptionMonitor` — observes, never alters the crash), best-effort
  flush on a draining loop. No signal handlers by design.
- Wire: NDJSON to `POST /i`, key in `X-Upcontrol-Key`, batches 1.5 s / 64 KB,
  8 MB in-memory ring with an explicit `upcontrol_buffer_dropped` line on
  eviction, byte-identical retries (server dedups by body hash), honors the
  receipt's sampling instruction, sends `install_verified` in the first batch.
- Client-side scrubbing before the wire (hand-written scanner, no regexes):
  cloud keys, vendor token prefixes, Bearer/JWT, connection-string passwords,
  PEM blocks, Luhn-validated card numbers, emails, cookies — replaced with
  `[redacted:type:len]` markers.
- Zero runtime dependencies, Node >= 18, ESM + CJS.

Backend counterparts shipped in the same pass (back/, not published):
`POST /v1/projects/anonymous` (per-IP throttled), `POST /v1/claim`,
`GET /v1/install/status`; migration 014 (tenant claim columns); two new
ring-query builders (`EventSeen`, `RecentEvents`).

Known gaps, deliberate: no `/claim/{token}` front page yet (the endpoint
exists; the URL 404s until the front route ships), no `connect` pairing
command, no Python SDK, `expires_hint` omitted from the mint response until an
unclaimed-project reaper actually exists.
