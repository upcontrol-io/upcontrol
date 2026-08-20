# Canonical event dictionary

A closed set of 24 names, frozen. Sending one of these as the message gives the
event a baseline, alerting rules and correlation. Anything else is a tier-4
ordinary log line: kept in the window and incident slices, never the trigger of
anything. That makes a naming mistake cheap - but only canonical names unlock
detection, so prefer them whenever one fits.

Names are snake_case and case-insensitive on the wire; they are stored in the
canonical spelling. The `uc.` prefix is reserved for upcontrol itself - never
send it; it is dropped with a `reserved_prefix` warning.

## How to emit

```ts
import { track } from '@upcontrol/sdk';
track('payment_succeeded', { provider: 'stripe', currency: 'usd', livemode: true });
```

The first argument is the event name. Labels ride as the second argument.
Required labels are REQUIRED: an event missing one is useless to its detector
and must not be placed at all.

## Tier 1 - baselined, alertable, may wake the user

| Event | Required labels | Optional labels | Default rule |
|---|---|---|---|
| `payment_succeeded` | `provider`, `currency`, `livemode` | `amount_minor`, `plan` | absence: zero longer than the backfilled p99 gap for this hour of week |
| `payment_failed` | `provider`, `reason_code` | `currency` | failure share vs baseline, sustained |
| `refund_issued` | `provider`, `currency` | `amount_minor` | count spike vs 90-day baseline |
| `subscription_cancelled` | `provider` | `plan` | spike vs baseline |
| `job_failed` | `job` | `error_type`, `duration_ms` | any occurrence for jobs with no prior failures; else rate |
| `heartbeat` | `job` | | missed window, derived from the declared cron expression |
| `unhandled_exception` | `error_type` | `route`, `fingerprint` | new fingerprint, or spike on a known one |
| `request_failed` | `status_class`, `route` | `duration_ms` | 5xx share over baseline, sustained |
| `external_api_failed` | `provider`, `status_class` | `route` | sustained failure of one provider says "it's Stripe, not you" |
| `email_failed` | `provider` | `reason_code` | any occurrence - silently dying mail is classically invisible |
| `login_failed` | | `route` | spike = credential stuffing |
| `app_started` | `version`, `env` | `instance` | restart storm: N starts in M minutes = crash loop |

## Tier 2 - baselined, on the dashboard, no waking by default

`job_started`, `job_done`, `checkout_started`, `subscription_created`,
`external_api_slow`, `email_sent`, `signup`, `upload_finished`,
`import_finished`, `app_stopped`.

Labels mirror their tier-1 counterparts: `job_*` carry `job`;
`checkout_started` and `signup` may carry `route` and `plan`;
`external_api_slow` carries `provider` and `duration_ms`; `email_sent` carries
`provider`; `app_stopped` carries `version`, `env`.

## Tier 3 - never alerted, highest correlation value

| Event | Required labels | Purpose |
|---|---|---|
| `deploy` | `version`, `env` (+ `commit_sha`, `actor`) | the correlation axis of every incident screen |
| `install_verified` | `version`, `env` | emitted once by the SDK at first successful connection; proves the chain end to end |

You never place `install_verified` by hand - the SDK sends it.

## The tier ladder for everything else

| Tier | What | Baseline | Rules | May wake | Where visible |
|---|---|---|---|---|---|
| T1 | the 12 names above | yes | yes, by default | yes | alert, digest, incident, window |
| T2 | the 10 names above | yes | only if explicitly configured | no | dashboard, digest, window |
| T3 | `deploy`, `install_verified` | no | no | never | correlation axis, window |
| T4 | everything else | no | no | never | window and incident slices, as an ordinary line |
| T5 | `uc.*` | - | - | - | reserved; client-sent ones are dropped with a warning |

## Label discipline

Labels are a closed set of low-cardinality keys: `route` (a PATTERN like
`/users/:id`, never a concrete path), `provider`, `job`, `status_class`, `env`,
`version`, `reason_code`, `currency`, `livemode`, `plan`, `error_type`,
`fingerprint`, `amount_minor`, `duration_ms`, `instance`, `commit_sha`, `actor`.
Never put free text, IDs, emails or URLs into a label value - high-cardinality
labels destroy the series they feed. Free detail belongs in ordinary log lines.
