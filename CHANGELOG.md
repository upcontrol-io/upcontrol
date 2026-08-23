# Changelog

All notable changes to the self-hosted package. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[semver](https://semver.org/).

## [0.1.1] — 2026-08-21

### Fixed
- The write-ahead log leaked one file handle per checkpoint, and could not
  truncate at all on Windows. Both were latent — neither method has a caller
  in the running services yet — and both were invisible on Linux, where an
  open file still unlinks and truncation ignores the open flags.
- `go test ./...` failed on every Windows checkout: `.gitattributes` was
  missing, so the working tree arrived with CRLF and `gofmt` called the whole
  module unformatted.
- Explain answered 500 against a working provider when the configured model
  is not a reasoning model: the request carried a `reasoning_effort` the
  model refuses, and the client's parameter-adaptation did not know that
  spelling. It does now, beside `max_tokens` and `temperature`.
- On a hosted instance the "no AI key" message told the caller to add one in
  Settings — a door that answers 404 to every tenant there, by design. The
  hosted wording states the fact and asks for nothing; a self-host is
  unchanged, because there the door is real and the reader is the operator.

### Added
- The log strip zooms below a minute. `GET /v1/logs` takes `bucketSeconds`
  and answers with `detail`: the same counts bucketed finer, bounded to the
  range being read, and present only when finer than the minute the whole-ring
  histogram already draws. The server snaps the requested width to one it can
  serve and reports the width it used.

### Changed
- `UC_AI_MODEL` defaults to `gpt-5-nano-2025-08-07`. Nothing changes for an
  instance that sets it.
- The backend's full CI gate — generated-artifact drift, lint, and the
  integration and contract suites against a real Postgres — now runs in this
  repository. Plain `go test ./...` skips every build-tagged file, so the
  tagged suites had never run here.

## [0.1.0] — 2026-08-20

The first public release.

### Added
- Website checks (1m–1h intervals) with SSL and domain expiry watched on
  every check.
- Log ingest over `POST /i` (NDJSON, WAL-backed receipt) with the `upcontrol`
  CLI and `@upcontrol/sdk`.
- Error-rate incident detection over your own logs, with deploy/webhook
  events correlated onto the incident timeline.
- AI incident/log Explain: bring your own OpenAI-format provider — key,
  model and base URL pasted into Settings (stored encrypted) or set via env.
  Without a key Explain is off and says so; the client auto-adapts to the
  gateway's dialect (`max_tokens` vs `max_completion_tokens`).
- Settings-screen setup for the Telegram bot (token + username, stored
  encrypted): alerts and invites go live on save, no restart.
- Alert channels: Telegram, email (own SMTP), Slack, Discord, webhook.
- Public status page per project with measured uptime bars and an honest
  `Powered by UpControl` default (removable in Settings).
- Single-user mode (`UC_AUTH=none`, the package default) and magic-link
  sign-in (`UC_AUTH=magic-link`).
- `infra/install.sh`: preflight, secrets, four questions, `--update`,
  `--from-source`.
