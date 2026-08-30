# Changelog

Every release of a published package gets an entry here (repo rule: a bad
deploy is rolled back, a bad published version is on other people's machines).

## Unreleased

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
