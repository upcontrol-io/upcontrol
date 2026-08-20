# The API key

One project key (`uc_live_...`) authenticates everything the SDK sends. The
rules around it are the only part of this skill where a mistake is a security
incident, not a bad diff.

## How the key gets where it belongs

Preferred: **you never touch it.** `npx upcontrol init` resolves the key in
this order and never prints it:

1. `UPCONTROL_API_KEY` already in the environment - used as is.
2. `.env` in the project root already carries it - used as is.
3. Neither: init calls the anonymous-project endpoint, receives a fresh key,
   verifies `.env` is gitignored (fixing `.gitignore` if needed, and saying
   so), and writes the key into `.env` itself.

The anonymous project accepts data immediately and belongs to nobody yet. Init
prints a **claim URL** - signing in through it attaches the project to an
account. Claiming later never changes the key, so nothing deployed breaks. An
unclaimed project cannot notify anyone (there is nowhere to send alerts) -
that is a fact, not a nag; say it plainly when the user asks why claim.

If the user already has an upcontrol account, the key lives at
`/app/sources#key` in their dashboard; they can paste it into `.env`
themselves, or run `npx upcontrol init --key <key>` (the flag writes it to
`.env` and does not echo it back).

## Absolute rules

- Only into `.env`, only after confirming `.gitignore` covers `.env`. If it
  does not: fix `.gitignore` FIRST, tell the user you fixed it, then write.
- Never into a tracked file. Never into source code, docker-compose values,
  CI config, or documentation. In docker-compose, use `env_file: .env`, not a
  literal value.
- Never echo the key: not in chat, not in command output you quote, not in
  commit messages, not in logs. If the user pastes it into chat themselves,
  use it for the `.env` write and do not repeat it.
- Never send it to any tool or service other than writing `.env` locally.

## SDK behavior without a key

`track()` no-ops, one warning at startup, zero buffering, zero effect on the
app. The moment `UPCONTROL_API_KEY` appears in the environment, the next start
begins sending - no code change needed. So a missing key is never a reason to
block the instrumentation diff; place the points, note the key step.
