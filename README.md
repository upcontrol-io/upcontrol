# UpControl

Monitoring that arrives with the "why", not just "down" — uptime checks, application logs and AI incident explanations in one compose file, wired into your app by the coding agent you already use.

[![CI](https://github.com/upcontrol-io/upcontrol/actions/workflows/ci.yml/badge.svg)](https://github.com/upcontrol-io/upcontrol/actions/workflows/ci.yml)
[![npm](https://img.shields.io/npm/v/upcontrol?label=npx%20upcontrol)](https://www.npmjs.com/package/upcontrol)
[![License: AGPL-3.0](https://img.shields.io/badge/license-AGPL--3.0-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/upcontrol-io/upcontrol)](https://github.com/upcontrol-io/upcontrol/releases)

<!-- launch-asset: hero GIF — the incident card firing Explain and getting the
     triage answer. Shot list: docs/launch-assets.md -->
![UpControl incident explain](docs/assets/hero.gif)

Two things, and neither of them is an afternoon:

- **One click to watch a site.** Type a domain, get uptime checks, SSL and
  domain expiry, alerts and a public status page. Nothing touches your code.
- **One command to send logs.** `npx upcontrol` hands your coding agent the
  instructions, and you ask it in plain language to log what matters.

Run it on your own box with the compose file below, or use the hosted service
at [upcontrol.io](https://upcontrol.io) — same product, same CLI, same
contract. There is no query language to learn and nothing to assemble; if you
want a Grafana stack you should have a Grafana stack, and this is for the rest
of the time.

## Quickstart

```sh
git clone https://github.com/upcontrol-io/upcontrol
cd upcontrol/infra
./install.sh
```

Four questions, a few pulled images, and `https://localhost` is a working
instance: website checks every minute, a log ingest endpoint, Telegram and
email alerts, and an Explain button that reads an incident's own evidence and
answers what broke, why, and what to run next. No account with us, no phone
home — [no telemetry of any kind](#what-this-is-and-is-not).

**Requirements:** Docker with Compose v2, 4GB RAM recommended (2GB + active
swap minimum), 10GB free disk.

## One command wires your app up

[![Works with 10 coding agents](https://img.shields.io/badge/works%20with-10%20coding%20agents-blue)](cli/installer/src/detect.ts)

Checks need nothing from your code. Logs do, and that is what
[`npx upcontrol`](https://www.npmjs.com/package/upcontrol) is for — one
command, for every agent (Claude Code, Cursor, Codex, Gemini CLI, Copilot,
Windsurf, Amp, Aider, Cline, opencode):

```sh
npx upcontrol
```

It does four deterministic things and runs no AI of its own:

1. **Installs the skill** into `.claude/skills/` and `.agents/skills/`
   (`--copilot` for `.github/skills/`) — the canonical event dictionary,
   placement rules and stack recipes your agent follows.
2. **Pins `@upcontrol/sdk`** in `package.json` at an exact version — the
   library whose `track()` never throws and never blocks.
3. **Provisions a key**, no account required: an anonymous project is minted
   and the key written into `.env`, only after `.gitignore` is confirmed to
   cover it, and without ever printing it. Claim the project later (free, same
   key) from the printed link, or bring your own with `--key uc_live_…` or
   `UPCONTROL_API_KEY`.
4. **Verifies.** `npx upcontrol verify` waits until data provably arrives, and
   names the failure precisely when it does not.

Then talk to your agent in plain language:

> send all my logs to upcontrol
>
> track user behavior and tell me where customers churn
>
> alert me when my nightly cron dies silently

The agent reads the skill, stages a diff for your review — it never commits —
and finishes only when `verify` reports data flowing. Run inside an agent, the
command detects that and answers in JSON, so "set up upcontrol for me" works
as a single prompt too.

- `npx upcontrol skills` — the reference topics the agent reads
- `npx upcontrol status` — endpoint, key source, skill freshness, one JSON line

**Your code never leaves your machine.** The CLI talks only to the UpControl
API, the intelligence is your own agent, and the SDK sends only what the
reviewed log points emit — scrubbed client-side, before anything is sent.

That package is `cli/` in this repository, MIT-licensed, and it points wherever
you tell it to: `--endpoint https://your-box` for a self-hosted instance, or
the hosted service by default.

## Hosted cloud

We run this same product as a service at [upcontrol.io](https://upcontrol.io):
multiple probe regions, email delivery without your own SMTP, and nothing to
operate. The open-source core is the cloud's core — same contract, same UI,
same `npx upcontrol` — and it is fully usable on its own. Nothing here is a
demo of the paid thing.

Pick whichever fits; the honest advice for a one-box self-host is to let
*something* outside the box watch the box, and upcontrol.io's free plan covers
exactly that.

## What you get

- **Website checks** from 1-minute intervals, with SSL and domain expiry
  watched for free on every check — measured uptime, never asserted.
- **Log ingest** over one HTTP door (`POST /i`, NDJSON): the SDK and CLI wire
  an app up in one command, and a WAL makes the receipt durable.
- **Incidents with a "why"**: error-rate detection over your own logs,
  deploys and webhooks correlated onto the timeline, and an **Explain** button
  whose answer is the model's read of the evidence — labelled as a guess with
  its confidence, with runnable next steps. Bring your own OpenAI-compatible
  key (OpenAI, OpenRouter, a local gateway — model and endpoint are yours to
  pick) and paste it into Settings; without one, Explain says it is off
  instead of guessing.
- **Alerts** to Telegram, email (your SMTP), Slack, Discord or any webhook —
  a channel is a destination, one field to add.
- **A public status page** per project, with real uptime bars.

<!-- launch-asset: screenshot — the monitors list; screenshot — the public
     status page. Shot list: docs/launch-assets.md -->

## How it compares

|  | UpControl | Uptime Kuma | Grafana + Prometheus + Loki |
| --- | --- | --- | --- |
| Uptime checks | yes | yes | via exporters |
| Application logs | yes — one ingest endpoint | no | yes — Loki |
| AI incident explain | yes (bring your own OpenAI-format key) | no | no |
| Deploy/webhook correlation | yes | no | build it yourself |
| Setup | one compose file | **one container — lighter than us** | several services + query languages |
| Status pages | yes | yes | plugins |

Uptime Kuma is genuinely lighter if uptime checks are all you need. The
Grafana stack is more powerful if you have the time to assemble and query it.
UpControl's lane is the middle: logs, checks and the why of an incident in
one place, running on one box.

## What this is and is not

- **No telemetry.** The self-hosted package phones nothing home — no version
  pings, no usage beacons. If that ever changes it will be opt-in and loudly
  documented.
- **Single-user by default.** `UC_AUTH=none` boots one owner account with no
  sign-in — right for a private box. Set `UC_AUTH=magic-link` before exposing
  the instance to the internet.
- **Honest floors.** ClickHouse under 2GB of RAM without swap gets OOM-killed;
  the installer refuses to start there rather than fall over later.

## Repository layout

```
back/    Go services: ucapi (API + ingest), ucworker (delivery, detection), ucprobe (checks)
front/   The web app (React + Vite)
db/      Postgres migrations (goose) + ClickHouse schema
cli/     npx upcontrol installer, @upcontrol/sdk, the agent skill  [MIT]
infra/   docker-compose, Caddy, install.sh
```

`back/api/openapi.yaml` is the contract: both sides generate from it, and a
hand-written type on either side is how the two start disagreeing silently.

## How the pieces talk

Six runtime pieces, one contract, no hidden paths:

```mermaid
flowchart LR
    Browser["Browser / status page"] --> Front["front, the web app"]
    Front -->|"REST /v1, HttpOnly session cookie, one origin"| UCAPI["ucapi"]
    SDK["SDK / CLI, npx upcontrol"] -->|"POST /i, NDJSON"| UCAPI
    UCPROBE["ucprobe"] <-->|"connect-rpc: lease checks, submit results"| UCAPI
    UCAPI -->|"tenants, monitors, incidents, deliveries"| PG[("Postgres")]
    UCAPI -->|"logs, events, checks: ingest door + availability detector"| CH[("ClickHouse")]
    UCWORKER["ucworker"] --> PG
    UCWORKER -->|"log detectors, incident lifecycle, rollups"| CH
    UCWORKER -->|"alerts"| CHANNELS["Telegram bot, SMTP email, webhooks"]
    UCAPI -->|"Explain, only with a configured key"| AI["AI provider"]
```

| Component | What it is responsible for |
| --- | --- |
| front | The web app (React + Vite): the account app and the public status pages. It talks to ucapi only, on the same origin, with an HttpOnly session cookie. |
| ucapi | The public door: the `/v1/*` API, public and hook surfaces, the schemaless ingest endpoint `POST /i`, and the ProbeService the probe fleet calls. It owns the whole ingest pipeline into ClickHouse, all auth doors, and enqueues deliveries (never processes the queue, so replicas stay safe). |
| ucworker | The background driver: the delivery queue (Telegram, Discord, Slack, email), retention recompute, expired-batch purging, the notification scanner and the log detector orchestrator. Every job runs under a Postgres advisory lock, so N replicas never duplicate work. |
| ucprobe | The stateless check runner: leases checks from ucapi, runs each through the SSRF-guarded HTTP executor, submits results back. It holds no database credentials and stores nothing. |
| Postgres | The system of record: tenants, monitors, incidents, deliveries, entitlements. |
| ClickHouse | The telemetry store: logs, events, checks; the availability detector reads it on submit, ucworker's detectors read it on their ticker. |
| SDK / CLI | `npx upcontrol` and `@upcontrol/sdk` (MIT): the installer wires an app up, the SDK's `track()` never throws and never blocks, everything goes to `POST /i` as NDJSON. |
| Delivery channels | Telegram bot, SMTP email, Slack, Discord, any webhook. A channel is a destination, not a rule engine. |
| AI provider | Optional and yours: Explain works only when an OpenAI-compatible key is configured; without one it says it is off instead of guessing. |

The full package map, data flows and storage writers live in
[docs/architecture.md](docs/architecture.md).

## Development

```sh
cd back && go build ./... && go test ./...     # Go services
cd front && npm install && npm run dev          # web app on :5199
cd front && npm run test                        # Playwright suite
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full setup, the test matrix
and the CLA process.

## Licensing

The repository is [AGPL-3.0](LICENSE), with one deliberate exception:
everything under `cli/` (the `upcontrol` installer, `@upcontrol/sdk` and the
agent skill) is MIT — it ships inside YOUR application, and a copyleft
license has no business there. Each `cli/` package carries its own LICENSE
file.

## Security

Found a vulnerability? Please do not open an issue —
see [SECURITY.md](SECURITY.md).
