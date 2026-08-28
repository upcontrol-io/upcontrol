---
name: upcontrol
description: Wire monitoring and logging into this app with upcontrol - uptime checks, log window, incident alerts. Use when the user asks to send logs to upcontrol, add logging/observability/monitoring, track user behavior or churn, watch payments, cron jobs or errors, or to set up, verify or debug an upcontrol integration.
---

# upcontrol

upcontrol is monitoring that arrives with the "why", not just "down": the user's
app pushes log points and named events; upcontrol correlates them into
incidents with context. You (the coding agent) place the log points. The
upcontrol CLI and SDK do everything else.

## Source of truth

**Required: before doing any upcontrol work, run `npx upcontrol skills` to list
the current reference topics, then read the ones your task needs with
`npx upcontrol skills <topic>`. Do not rely on memory or this file alone - the
CLI ships the up-to-date topics and recipes.**

Check state first: `npx upcontrol status` prints one JSON line - endpoint, where
the API key was found (env / .env / none), and whether data has been verified.

## What the user's goal maps to

The user states a goal in plain language; you translate it to a topic:

| The user says something like       | Read topic | You will                              |
|------------------------------------|------------|---------------------------------------|
| "send all my logs to upcontrol"    | `logs`     | wrap their existing logger, add SDK   |
| "track user behavior / churn"      | `funnel`   | place events in checkout, billing, auth |
| "tell me when my app is down"      | `uptime`   | no code - point them at the app       |
| "my cron / queue died silently"    | `jobs`     | job_* events + heartbeat              |
| "catch errors / exceptions"        | `logs`     | SDK auto-captures; add request_failed points |

If the user has no specific goal, propose what you FOUND in their repo, not a
generic list: Stripe in dependencies -> propose payments; a queue -> jobs; a
mailer -> email events. Suggestions come from the repository, not a template.

## Hard rules (the full list is `npx upcontrol skills rules` - read it before editing)

1. Every change lands as a diff for the user's review. **Never commit, never
   push.** End your report with a counter: `+N log points · staged for your
   review` and the touched areas (`checkout · billing · dunning`).
2. One line per log point. No wrappers around existing logic, no control-flow
   changes, no new `await`, never touch a `catch` block beyond adding one line.
3. Event names are free-form: pick a clear one and keep it stable (the same
   moment, the same name at every call site). `deploy*` and `install_verified`
   have machinery behind them, `uc.*` is reserved and refused
   (`npx upcontrol skills dictionary`).
4. Labels stay low-cardinality: no IDs, emails or concrete paths in a label
   value - free detail belongs in the message.
5. Nothing inside hot loops. Log the outcome of an operation, not its steps.
6. The API key goes **only into `.env`, and only after you have checked `.env`
   is gitignored** - fix `.gitignore` first if not, and say you fixed it. Never
   print the key into chat, code, logs or commit messages. `npx upcontrol init`
   handles key placement for you - prefer it over touching the key yourself.
7. Do not declare success when the diff is applied. The install is finished when
   `npx upcontrol verify` reports data arriving - run it, and if it fails,
   follow its taxonomy (`npx upcontrol skills verify`).

## Standard flow

1. `npx upcontrol status` - if no skill/key/endpoint, `npx upcontrol init` first
   (it installs this skill, adds the pinned SDK dependency and provisions a key
   into `.env` without showing it to you).
2. Read the topic for the user's goal. Place log points per the rules.
3. Show the diff, report the counter, let the user apply it.
4. Ask the user to run the app (or wait for traffic).
5. `npx upcontrol verify` - report its verdict verbatim.

## Scope

- The SDK is `@upcontrol/sdk` - zero dependencies, `track()` never throws, so a
  log point cannot break the caller. Version is pinned exactly by init; do not
  loosen it to a range.
- This skill never sends the user's code anywhere. All intelligence is you; the
  SDK pushes only what the placed log points emit, scrubbed client-side first.
- Uptime checks, status pages and alert channels live in the upcontrol app, not
  in code. Do not scaffold monitor config files - there is no such thing here.
