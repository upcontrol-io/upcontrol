# Changelog

All notable changes to the self-hosted package. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[semver](https://semver.org/).

## [Unreleased]

### Changed
- **The billing table is provider-neutral.** Migration `006_billing_provider_neutral`
  recreates `billing_subscription` with string provider ids (`provider`,
  `provider_customer_id`, `provider_subscription_id`, `product_id`, `period_end`,
  `canceled_at`) in place of the LemonSqueezy integer columns. The table never held
  a production row, so nothing is migrated; the Down recreates the old shape empty.
  The core still reads none of it — the hosted billing sidecar is its only writer.

### Removed
- **AI Explain, in full.** The button that read a log selection or an
  incident's own evidence and answered what broke is gone from the product,
  and so is everything behind it: the OpenAI-compatible client, the scenario
  registry that held every prompt, cap and output contract, the quota
  accounting and the answer cache. Nothing takes its place — an incident card
  shows the facts the code computes and stops there.
- **The Explain endpoints.** `/v1/logs/explain`, `/v1/logs/explain/preview`,
  `/v1/incidents/{id}/explain`, `/v1/instance/ai` and `PUT /v1/project/meta`
  are off the contract and unmounted. `npx upcontrol` no longer uploads a
  project spec, which existed only as context for an explanation.
- **The AI configuration.** Every `UC_AI_*` variable and the
  `secrets/ai_api_key` file are gone from the stack, the Settings screen has
  no AI section left, and the instance settings that section wrote
  (`ai_api_key`, `ai_model`, `ai_base_url`) are deleted on upgrade.
- **The Explain tables, destructively.** Migration `027_remove_ai` drops
  `ai_call`, `ai_explain_cache` and `ai_usage`, and drops the columns
  `plan_entitlement.ai_explains` and `project.meta`. An operator upgrading
  loses the stored explain history for good: every cached answer, every
  ledger row, every quota count. The Down migration recreates the empty
  shapes so the chain can roll back — it restores no data, and nothing else
  restores it either.

## [0.4.0] — 2026-08-26

### Added
- **The invitation mail, on every transport.** Inviting a teammate now
  sends the invitation through the same mailer the sign-in door uses — the
  email agent, own SMTP, or the dynamic relay — and the mail is sent before
  the membership commits: a send failure rolls the whole write back and
  answers 503, so "The invite was not sent. Nobody was added." is the truth.
  The mail names the project and the person who invited them, and carries
  the one-time sign-in link; its text part is byte-identical on every
  transport, pinned from both sides (Go and the agent's template).
- **Resend.** A pending row carries a Resend button: the same invitation
  mail again, with nothing to insert and nothing to roll back. A resend
  inside the cooldown answers 429 — no second mail can go out while a link
  is still fresh, and a button that said "Sent!" without sending would be
  the lie this screen refuses elsewhere.
- **Signing in accepts the invitation and seeds the e-mail channel.** The
  invitation is a magic link: redeeming it marks the membership `active` and
  writes the invitee's address as an Email channel in the same breath — the
  Telegram redeem's symmetry, on the e-mail side. An accepted invitation
  leaves nobody a member whose address cannot be alerted, and no second
  "add a channel" step between the invite and the alerts.

### Changed
- **A pending person cannot be an e-mail destination.** The picker on Alerts
  offers only people who have signed in, and the server answers a pending
  address with the same refusal as an unknown one: no channel, and no test
  mail, can reach an inbox the person has not proven theirs yet.
- **A resend inside the cooldown answers 429** rather than 202, for the
  reason above: an empty success would show "Sent!" while nothing went out.

## [0.3.0] — 2026-08-26

### Added
- **Acknowledge and Resolve buttons in group alerts.** A group message used
  to carry no buttons at all, because the one button it wanted — the Mini
  App opener — is refused by the Bot API outside a private chat. A group now
  gets the two buttons that work anywhere, and a press is authorised by who
  pressed it: anyone on the project and only them, and someone else is told
  so privately. Open and Explain stay on personal alerts, where the Mini App
  can open; a detector spike carries Acknowledge only, in a group as in a
  private chat.
- **Chat labels on Alerts.** A Telegram row prints the person's name with
  their `@username`, or the group's title, instead of a raw chat id — the
  reader sees who a destination reaches, not its handle. Migration `025`
  backfills existing personal rows from the person's name.
- **`Link Telegram`, a person-bound invite.** A teammate's row on Team can
  mint a Telegram link addressed to that one person: it works once, only for
  them, and is shown once under the row, which turns to `Link pending`. The
  redeem refuses to merge — another person's Telegram is turned away, and so
  is a group chat — and a refusal rolls back, leaving the link valid for its
  person.
- **Muted channels are shown and liftable from the Alerts screen.** A
  channel muted with `/mute` says `muted until` and the time on its row, and
  `Unmute` lifts the window and releases the alerts it parked. Muting stays a
  chat command: the screen only lifts.
- **Explain is available to Members.** The three Explain calls left the write
  gate: they are reads a Member may spend the AI quota on, like every other
  read on a screen a Member already sees. Every write still demands an
  Admin.

### Changed
- **The Telegram invite no longer carries a role.** Minting takes no role
  and the contract carries none: whoever redeems a link joins as a Member.
  A Telegram invitee arrives with no e-mail, and an Admin must have one.
- **Invited addresses are normalised before the person is created.** The
  address is trimmed and lower-cased on invite, so `Bob@Example.com` and
  `bob@example.com` are one person, not two accounts for one reader, and the
  display name is derived from the normalised form.

### Removed
- `PATCH /v1/telegram/invites/{id}`. Its only job was changing the role a
  link would carry; the link carries no role now, so a link minted wrong is
  revoked and re-minted, never patched.

### Fixed
- **A removed person's e-mail channel no longer stays behind.** Removing a
  teammate deleted their membership, invites, sessions and Telegram
  channels, but the e-mail channel survived and kept receiving alerts at an
  address that no longer had a person. The delete now takes the e-mail
  channel too, matched by address.

## [0.2.0] — 2026-08-24

### Added
- **Google sign-in**, the authorization-code half: a second door beside the
  magic link, on the same session and the same account rules.
- **Alert email is rendered, not concatenated.** The backend posts the facts
  and a template name and the email agent renders both parts, the split the
  magic-link mail already used — so an alert arrives with its summary, its
  labelled facts and its log lines instead of four unstyled sentences. Its
  button deep-links the incident rather than the dashboard: by the time most
  alert mail is read the incident is resolved and no longer sits first on the
  screen the old link opened.
- **Telegram alerts read like the product.** A status emoji beside words that
  carry the same fact (colour is never the only channel, on a surface with no
  CSS), the measured summary sentence, label/value facts with machine output in
  `<code>`, raw lines in one `<pre>`, and a closing link chosen by the
  delivery's class — an outage deep-links its incident, a log alert opens the
  log group, the recovered follow-up closes quietly and carries the duration
  measured from the incident's own bounds.
- **Detector incidents alert.** An error-rate spike notifies channels the way
  an outage does, gated by the channel's error axis, which is off by default.
  Such incidents open as `check`, not `down`: these detectors read the log
  stream and never look at a monitor, so they report degradation without
  claiming the availability verdict. The alert quotes the weekly baseline only
  when one was measured, and says so when the project is younger than a week.
- **Alerts open the app.** Personal Telegram alerts carry a Mini App button —
  Open for an outage, Explain for a spike, which runs the AI read on arrival.
  `/unmute` lifts a mute window early and releases the pages it parked,
  `/status` names the open incidents (and says so when it could not read them),
  and the bot registers its command list and menu button at start.

### Fixed
- **A deleted check no longer leaves an incident nobody can close.**
  `incident.monitor_id` is `ON DELETE SET NULL`, so removing a monitor orphaned
  its open incident and nothing could ever resolve it: a status page went on
  announcing "Some systems are down" for a component it no longer listed. The
  delete closes it first, with `close_reason = monitor_deleted` and a timeline
  entry that says what ended it; a public page no longer lists the incidents of
  deleted checks. Migration `024` closes the ones already stranded, leaving
  detector incidents — which legitimately carry no monitor — alone.
- **Login CSRF on both sign-in doors.** Every other write here is protected by
  accident: `SameSite=Lax` keeps a victim's cookie off a cross-site request. The
  sign-in doors are the exception, because they need no cookie — they install
  one, so an attacker could cross-site POST a credential for an account they
  control and leave the victim signed into the attacker's tenant. Two checks now
  stand in the way, either sufficient alone: `Sec-Fetch-Site`, which a page
  cannot forge and a non-browser caller simply omits, and a JSON content type,
  which a cross-site form cannot produce.
- **One address, one account.** `person.email` is unique and was compared byte
  for byte with nothing normalised, so a phone's autocapitalisation or Google's
  own casing could open a second account for the same person.
- **No Telegram invite could be redeemed.** The mint stored `sha256` of the
  whole `inv_…` string and the bot looked up `sha256` of the tail, so every
  `/start` landed on "this invite link is no longer valid" — and both suites
  stayed green, because each side was only ever tested against itself. One
  hasher now serves both; invites minted before the fix redeem after it.
- **A Telegram alert could fail to deliver because of what it was alerting
  about.** The old renderer interpolated the title raw, and a log-alert title IS
  an error message: one `<` in it — any generic type — and the Bot API rejected
  the whole message as malformed HTML. Every dynamic string is escaped.
- `X-Upcontrol-Key` keeps the spelling it was published under. A brand sweep
  renamed the header along with the prose; nothing would have broken, but SDKs
  already installed in other people's projects send the original, and a spec
  advertising a different spelling for the same header is a discrepancy with no
  upside.
- The reporting addresses in the security and conduct documents are ones that
  exist.

### Changed
- `formatTelegram` says the same thing in half the lines — plain concatenation
  instead of forty `WriteString` calls, with byte-identical output pinned by the
  layout tests.
- The product spells itself UpControl throughout the docs, and the supported
  agent count is a badge over a complete list.

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
