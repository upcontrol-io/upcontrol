# Changelog

All notable changes to the self-hosted package. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[semver](https://semver.org/).

## [0.1.0] — unreleased

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
  `Powered by Upcontrol` default (removable in Settings).
- Single-user mode (`UC_AUTH=none`, the package default) and magic-link
  sign-in (`UC_AUTH=magic-link`).
- `infra/install.sh`: preflight, secrets, four questions, `--update`,
  `--from-source`.
