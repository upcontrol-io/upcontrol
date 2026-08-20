# upcontrol plugin and library: SPEC

**Status: settled.** The specification of the plugin for coding agents and of the push library. This
is the product's main front door. The document is binding for `cli/` in its entirety: a PR that
contradicts §6 or §10 is rejected without discussing implementation details.

**Version 3.0, 12 August 2026.** Replaces version 2.0. The reason for the replacement is of the same
kind as last time: the entry surface changed. It was "we install a daemon on the customer's machine
through `npx`", it became **"a plugin inside somebody else's coding agent writes in the logging
points, the library ships them"**. Owner's decision after testing the npx/npm hypotheses: for now,
vibecoders only, through plugins. Rationale — `docs/plans/backend-v4-plugin-and-clickhouse.md` §0.1
(an internal document).

**Version 3.0 partially restores version 1.0.** Instrumenting sources with a coding agent, thrown
out in 2.0, comes back — but in a different form: it is not our CLI walking through somebody else's
code, it is **somebody else's agent following the instructions from the plugin**. That is why
version 1.0's S0–S10 flow is not resurrected: it described our deterministic installer, and now the
user's agent does the work.

**What survived both changes, and why.** Five things were right regardless of the collection method:
the canonical dictionary of 24 events (§4), the key and anonymous-project model (§7), live
verification with a failure taxonomy (§8), the compatibility commitments (§9.4), the security
contract (§10). The compatibility commitments **got stricter** in the process — §9.4.

**What was thrown out of version 2.0:** binary delivery through `optionalDependencies`, platform
prebuilds, environment detection (systemd/docker/compose/k8s), the enrollment protocol, daemon
configuration, the disk spool, the `upcontrol status/uninstall` commands. All of it comes back
together with the daemon (plan v4, M5) and until then is not built.

**Provenance legend.**

| Mark | Meaning |
|---|---|
| ⬤ | The number is **measured**. Reproducible by a benchmark in the repository. |
| (no mark) | Arithmetic over a ⬤-marked number, or a configuration limit, or a reference to code with `file:line`. |
| ⚠ | A number that must be re-measured before it becomes load-bearing. |

There is not a single ⬤ mark in this document yet: the code is not written.

**Scope and publication mode.** `cli/` is **open** in its entirety: both the plugin and the library.
`back/`, `probe/`, `db/`, `front/` are closed. Rationale — plan v4 §0.7 and §10, and also the open
question about this decision: the marketing site shipped the opposite (system-design §12).

Openness here is not marketing but a load-bearing element: the library reads the application's data
and sends it outside, and the only honest answer to "why should I let you do that" is "read its
code". Everything in §6 exists so that this answer holds up under inspection. For the plugin the
answer is cheaper still: it shows the diff before it is applied.

**Published 15 August 2026:** `upcontrol@0.1.0` (installer + skill) and
`@upcontrol/sdk@0.1.0`, license **MIT** (owner's decision, closes §12.5). The skill delivery
mechanism is `docs/plans/one-command-install.md` (an internal document): a single
`npx upcontrol` writes SKILL.md into `.claude/skills/` and `.agents/skills/`, which covers
Claude Code, Cursor, Codex, Gemini CLI and Windsurf with one file (this is the "one render" of
§2.2 implemented by means that became standard later than this version of the SPEC).

---

## 1. What this is and what it is not

### 1.1 Two artifacts

| Artifact | What | Language |
|---|---|---|
| **`cli/plugin/`** | the plugin for Claude Code, Cursor, Codex: the `/upcontrol:add` command, instructions, the dictionary, the instrumentation rules | markdown + manifest |
| **`cli/sdk/`** | the push library: `track`, batching, buffer, retry, scrubbing, `POST /i` | TypeScript; Python next |

The `ucagent` daemon from version 2.0 is deferred, not canceled (plan v4 §5.6). While it does not
exist, `agent/` does not exist either and is not described in this document.

### 1.2 What this is not

**This is not a source-code upload channel.** The plugin does not send code, does not ask us for
analysis, does not phone home. It is data and instructions; they are executed by the agent already
running on the user's machine. The landing promise "Nobody gets access to your code. It never leaves
your machine" rests on this, and it is an invariant, not a current state (§10).

It is also **not** an APM and not a tracer (we do not patch bytecode and do not inject into the
runtime), **not** an installer that requires an account (§7), and **not** an MCP server at launch
(§2.3).

### 1.3 Why a plugin and not a daemon — and what we pay for it

Version 2.0 contained a table "why a daemon and not an SDK". It **was not refuted** — the price is
accepted in exchange for a shorter entry. The reversal line by line, so that nobody treats the
question as unasked:

| Version 2.0 argument for the daemon | Version 3.0 answer |
|---|---|
| **Zero edits to somebody else's code** | Does not hold. Compensation: the diff goes to a human review, the point counter, `track()` never throws, not a single exception of the library escapes outside (§5.3). This bounds the risk, it does not remove it |
| **Works where there is no code** | Does not hold for code — **holds through door 1** (URL): availability, SSL, domain, dependencies, a status page without a single line |
| **Collects what an SDK does not see** (disk, OOM, restart loop) | Does not hold. Partly covered by the `app_started` event (restart storm); `saturation` and `oom` are lost entirely |
| **A vantage point inside the perimeter** | Does not hold. Comes back with the daemon |
| **One update surface** | Does not hold: the customer updates with their own release. Compensation — §9.4 |

What is **gained** in exchange, and what the trade was made for:

| Gain | Substance |
|---|---|
| **Entry an order of magnitude shorter** | `/plugin install upcontrol` instead of environment detection, choosing an installation method, privileges on the machine and a systemd unit |
| **Nothing of ours executes on somebody else's machine** | No binary — no question of "what is this process doing with my logs". There is a diff that a human read |
| **Logs appear where there were none** | The daemon tailed what was already being written. The plugin creates what does not exist — and the segment has nothing (`new-plan.md` §1) |
| **The human answers with a goal, not with files** | "help me find where customers churn" instead of a list of paths. He did not write this code and does not know the paths |
| **We are inside the agent ecosystem, not next to it** | Distribution through the plugin catalog — a channel the competitors do not have (`new-plan.md` §4.3) |

### 1.4 Where the approach is more fragile — honestly, with the compensation

| Fragility | Compensation |
|---|---|
| **We edit somebody else's production** | The main risk, back from version 1.0. Bounded by: the diff always goes to review, we never commit ourselves, one line per point, no wrappers around somebody else's logic, no edits to `catch` blocks |
| **Our call inside somebody else's handler** | `track()` **never** throws; a network failure does not block the thread; a serialization error is swallowed inside. A library that brings down somebody else's payment handler shuts the project down |
| **The quality of the instrumentation is the quality of somebody else's agent** | Tier T4 (§4.4) makes a mistake in a name cheap: a non-canonical name breaks nothing, it simply gets no baseline. Plus the dictionary travels in the plugin, not in the agent's head |
| **Updating the library is the customer's release** | §9.4: ingest accepts any version ever released, old shapes are never removed |
| **The library has to be written for every language** | TS first (the segment writes in it), Python second. The rest is covered by a direct `POST /i`, tolerant by construction |
| **A process restart loses the buffer** | Up to 8 MB. A deliberate narrowing against the daemon's disk spool: an application in a container often has no writable disk |

---

## 2. Distribution channels

### 2.1 The invariant

Two things are distributed: the **plugin** (instructions that somebody else's agent will execute)
and the **library** (code that will travel into somebody else's production). Both are ordinary
packages, nothing is downloaded at runtime.

**The plugin pins the library version and its integrity hash.** It writes the dependency into
somebody else's `package.json`, and the version there must be exact, not a range: "last month's
plugin installed a library released yesterday" is an unauditable chain on somebody else's machine.

### 2.2 The minimum set for launch

**The Claude Code plugin plus the library's npm package. Nothing else.**

Cursor and Codex are covered by a single render of the template (`upcontrol emit-rules`) and are
**outputs, not channels** — the launch copy carries the distinction: honest is "works with Cursor
and Codex", dishonest is "install the Cursor extension", which does not exist.

### 2.3 What we do not build for launch

**There is no MCP server.** A separate transport, a separate authentication surface, competing with
the plugin for the same job. Revisit after the install→verify conversion is measured.

**`npx upcontrol` is deferred.** It comes back together with the daemon; building it now means
building a second front door before the first one is measured.

**There are no apt/yum/brew packages** and now there will be no need for them at all: there is
nothing to install on the machine.

---

## 3. The installation flow

Five steps. The first four are executed by somebody else's agent following the plugin's
instructions, the fifth by us.

### S1 — Plugin installation

`/plugin install upcontrol`. Adds the `/upcontrol:add` command. Reads nothing and goes nowhere.

### S2 — The question about the goal

`/upcontrol:add` asks **one** question: "Where should I add logging?" — and takes the answer as a
goal, not as a list of files: "help me find where customers churn and grow my MRR".

Why a goal: the person in this segment does not know where his checkout is — he did not write this
code. What he does know is what he wants to understand. The question "where to log" is translated
into "what do you want to find out", the only phrasing he can answer.

If there is no answer, the agent offers **what it found, not something generic**: it saw Stripe in
the dependencies — it offers money; it saw a queue — it offers jobs. The list of suggestions is
built from the repository, not from a template.

### S3 — The key

The project key is taken from the account (the plugin asks to open `/app/sources#key`) or is issued
by a device-code flow. **Only into `.env`, only after checking that the file is in `.gitignore`**,
never into a tracked file, never into our own output. §7 in full.

If `.env` is not in `.gitignore`, the agent fixes it and says that it fixed it, or it stops.
Silently writing the key into a tracked file is not allowed under any circumstances.

### S4 — The diff

The agent reads the repository, finds the places, writes in the points and **shows the diff**. The
instrumentation rules are §5.2. Mandatory output: a counter of the changed places ("+13 log points ·
staged for your review") and a list of the affected areas ("checkout · billing · dunning").

**We never commit ourselves.** The human reviews and applies.

### S5 — Live verification

The installation does not count as finished until data has arrived. The agent asks to start the
application or to wait for the first request; `ucapi` emits `install_verified` on the first batch
carrying this key.

An installer that reports success and leaves strands the human with a silent product and no way to
tell "all quiet" from "nothing works". The failure taxonomy of this step is §8.

### Running it again

`/upcontrol:add` a second time is not a reinstall: the agent sees the already written points by the
import marker, does not duplicate them and offers to **add** where it did not go last time.

---

## 4. The canonical event dictionary

A closed set, **24 names**, frozen. The set is carried over from version 1.0 unchanged: it was right
regardless of the collection method, and renaming it today would cost a major version in somebody
else's production (§9.4).

Every canonical event lands as a row in `logs` **and** increments a series in `series_1m` (plan v3
§3.5). That is exactly what makes detection `O(series)` and not `O(lines)`.

**The counter plane is not affected by eviction from the raw window.** The window is a ring of
capacity N lines: new ones evict old ones, there is no monthly reset, no exhaustion, no "ingest
refused" state. Baselines, alerts and frozen incident snapshots live outside the ring. The
consequence to keep in mind when reading §8: **`verify` cannot fail because a limit ran out, because
the ring cannot go empty.**

Reserved prefix: `uc.*` is ours. The customer's names are free and do not collide.

### 4.1 Tier 1 — with a baseline, alertable, allowed to wake you up

| Event | Required labels | Optional | Default rule |
|---|---|---|---|
| `payment_succeeded` | `provider`, `currency`, `livemode` | `amount_minor`, `plan` | **absence**: zero for longer than the backfill p99 gap for this hour of the week |
| `payment_failed` | `provider`, `reason_code` | `currency` | share of attempts against the baseline, sustained |
| `refund_issued` | `provider`, `currency` | `amount_minor` | a spike in the count against the 90-day baseline |
| `subscription_cancelled` | `provider` | `plan` | a spike against the baseline |
| `job_failed` | `job` | `error_type`, `duration_ms` | any occurrence for jobs with no prior failures; otherwise rate |
| `heartbeat` | `job` | | **a missed window**, derived from the declared cron expression |
| `unhandled_exception` | `error_type` | `route`, `fingerprint` | a new fingerprint or a spike on a known one |
| `request_failed` | `status_class`, `route` | `duration_ms` | share of 5xx above the baseline, sustained |
| `external_api_failed` | `provider`, `status_class` | `route` | a sustained failure of one provider says "this is Stripe, not you" |
| `email_failed` | `provider` | `reason_code` | any occurrence — silently dying email is classically invisible |
| `login_failed` | | `route` | a spike = credential stuffing |
| `app_started` | `version`, `env` | `instance` | **a restart storm**: N starts in M minutes = crash loop |

### 4.2 Tier 2 — with a baseline, on the dashboard, do not wake you by default

`job_started`, `job_done`, `checkout_started`, `subscription_created`, `external_api_slow`,
`email_sent`, `signup`, `upload_finished`, `import_finished`, `app_stopped` — labels and purpose
as in version 1.0.

### 4.3 Lifecycle — never alertable, the highest correlation value

| Event | Required labels | Purpose |
|---|---|---|
| `deploy` | `version`, `env` (+`commit_sha`, `actor`) | the correlation axis for every incident screen |
| `install_verified` | `version`, `env` | emitted once by the agent on the first successful connection. Proves the chain end to end (§8.2) |

`install_verified` changed its source (the agent instead of the SDK), but not its meaning and not
its name. The name is not touched: it is already frozen, and a rename for cosmetics is exactly the
major version §9.4 forbids to be cheap.

### 4.4 The tier ladder for everything that is not canonical

| Tier | What it is | Baseline | Rule | Right to wake you | Where it is visible |
|---|---|---|---|---|---|
| **T1** | the 12 names of §4.1 | yes | yes, by default | **yes** | alert, digest, incident, window |
| **T2** | the 10 names of §4.2 | yes | only when explicitly configured | no | dashboard, digest, window |
| **T3** | the 2 names of §4.3 | no | no | never | correlation axis, window |
| **T4** | everything else | no | no | never | window and incident snapshot, as an ordinary line |
| **T5** | `uc.*` | — | — | — | reserved; anything arriving from a client is dropped with the warning `reserved_prefix` |

T4 makes a mistake in a name cheap: ingest never rejects content. A promotion T4 → canonical is a
minor version; a demotion does not exist.

### 4.5 New: rules for extracting events from tailed logs

Version 1.0 got canonical events from instrumented code. The agent gets them from lines, and that
requires a mechanism that did not exist before.

**A rule** is `(source, regular expression with named groups, event name, mapping of groups to
labels)`. Rules ship as a pack (ready-made for nginx, Caddy, Postgres, Redis, Rails, Django,
Express) and are extended by the customer. **The place where they are applied changed together with
the entry surface: rules now execute at ingest, not on the customer's machine** — there is nothing
left to tail somebody else's logs with. The audience narrowed too: it is whoever sends us the text
of an already existing logger through a direct `POST /i`, not the one whose code the plugin
instrumented (his names are canonical from the start).

Hard requirements, all three mandatory:

1. **A rule cannot drop a line.** It only adds `event=` and labels. A line that matched no rule
   arrives in full — as T4. A rule able to swallow would be silent data loss, and that is the worst
   defect a monitoring tool can have (§5.4).
2. **A time budget per line.** The rule set as a whole must fit under a ceiling; exceeding it
   disables the **rule**, not the acceptance of the line, and prints this as a warning in the
   receipt. A regex with catastrophic backtracking has no right to stop ingest.
3. **Labels come from a closed set of keys of bounded cardinality**: `route` (a pattern,
   `/users/:id`, never a concrete path), `provider`, `job`, `status_class`, `env`, `version`,
   `reason_code`. A rule that tries to put arbitrary text from the line into a label is rejected
   when the configuration loads — otherwise series cardinality explodes within a day.

### 4.6 What this costs in window

Screen S1 shows an estimate of lines per day per source ⚠, and S2 shows the sum of what was selected
against the plan's capacity as **depth**, not as a remainder:

```
Selected sources: ~57k lines/day
Your window (Free, 250 000 lines) holds about 4.4 days at this rate.
```

Not a word about a limit, a quota or a remainder: they do not exist in the model. A progress bar is
forbidden — it reads as a wall, which `new-plan.md` §5.1 explicitly disallows.

---

## 5. The library and the instrumentation rules

### 5.1 The library surface

Minimal and frozen:

```ts
track(event: string, attrs?: Record<string, string | number | boolean>): void
flush(): Promise<void>
```

Plus, automatically: interception of unhandled exceptions → `unhandled_exception`, sending
`app_started` on initialization, `flush()` on the termination signal.

Everything else is internal and not configurable: a batch of 1–2 s or 64 KB, an 8 MB in-memory ring
buffer, exponential backoff with jitter, an idempotent batch, scrubbing (§6).

### 5.2 The instrumentation rules — executed by somebody else's agent

The daemon's eleven rules are replaced by seven instrumentation rules. The plugin carries them
verbatim; an agent that violated any of them produced a bad diff.

1. **One line per point.** `track('payment_succeeded', {...})` and nothing around it.
2. **No wrappers around somebody else's logic.** Do not wrap existing code in `try`, do not change
   control flow, do not add `await` where there was none.
3. **Do not touch `catch` blocks** other than by adding one line. Editing error handling is editing
   behavior, not observability.
4. **Names come from the §4 dictionary.** If none fits — tier T4 and a free name; inventing
   something that merely looks canonical is forbidden.
5. **Required labels are required.** `payment_succeeded` without `provider` is useless to the
   detector and must not appear.
6. **Nothing in hot loops.** A point inside a loop over elements is a thousand lines per request;
   what gets logged is the result of the operation, not its steps.
7. **The key goes only into `.env`.** The repetition of §7 is deliberate: this is the only rule
   whose violation is a security incident rather than a bad diff.

### 5.3 The library's rules

1. **`track()` never throws.** Not on any serialization, network or overflow error.
2. **It does not block the calling thread.** Sending is always asynchronous.
3. **The buffer is bounded.** 8 MB, eviction of the oldest — **with an event of its own**: silent
   data loss by an observability tool turns monitoring into a source of false calm.
4. **It holds no connections.** No persistent sockets out of somebody else's application.
5. **It does not read the environment beyond the key and the endpoint.** An observability library
   that inventories environment variables is what people are afraid of from us.
6. **Zero runtime dependencies.** Every dependency of the library becomes a dependency of somebody
   else's production.
7. **It respects the instruction to sample** carried in the receipt (plan v4 §4.4).

### 5.4 What the library does not collect

Host metrics, container events, file tailing and checks inside the perimeter **do not exist** — that
is the daemon's job, and it is deferred (plan v4 §0.2.1). A library that goes off to read `/proc`
violates §10 and turns into exactly the opaque agent we rejected.

---

## 6. Scrubbing: the main line of defense, and it is on the customer's machine

Version 1.0 kept scrubbing on the server and honestly recorded that as a divergence from the
openness promise. There is no divergence: the scrubber lives in the open library and runs **before
the wire**.

The trust argument became twice as cheap as the daemon's. There are two of them: the human **saw the
logging points in the diff** — he knows exactly what is written, because he reviewed it; and the
library is open, its scrubber reads the same way the daemon's would have. The daemon had only the
second argument, and it demanded that the customer trust a binary he had never run his eyes over.

### 6.1 What is always cut out

Cloud provider keys (AWS, GCP, Azure), `Authorization: Bearer …`, JWTs by shape, database connection
strings with a password, PEM private keys, card numbers (a Luhn check, not just 16 digits), e-mail,
`Set-Cookie`, `session=`, tokens of the form `sk_live_…`/`ghp_…`/`xoxb-…`. Plus the customer's rules
from `agent.yaml`.

**With a hand-written scanner, not with regular expressions.** Regexes here are an order of
magnitude more expensive and catastrophic on pathological input, and pathological input in logs is
the norm, not the edge: a single 2 MB line from a stack-trace dump shows up on the very first day.

### 6.2 What is replaced rather than deleted

What is cut out is replaced by a marker naming the type and the length: `[redacted:jwt:184]`. The
reason: a line from which a chunk silently disappeared misleads an incident investigation more than
a line where it is visible that a secret was here. Plus the marker gives a counter — how many times
and what exactly was scrubbed, without the content.

### 6.3 What is verifiable

- the scrubber's rule list is the source of an open library, read directly;
- hit counters by type travel in the receipt and are visible on screen — **without the payload**,
  only "jwt: 412 in a day";
- the server scrubs again. This is defense in depth, not the main line: anyone can make a mistake,
  and two independent layers beat one perfect one.

**A regression in the scrubber is a security incident, not a bug.** The review standard matches: a
change in `cli/sdk/scrub/` requires a test for the specific vector and is not merged without one.

---

## 7. The key, the anonymous project, offline

The "value before registration" principle is violated most severely at the main door. Requiring
registration on the third screen loses everyone who would have seen the value on the eleventh.

### 7.1 The anonymous project key

```
POST /v1/projects/anonymous
  → { project_id, key: "uc_live_…", claim_token, claim_url, expires_hint }
```

- the request carries no e-mail, no name, no hostname, no path — only
  `{cli_version, agent_version, platform, arch}`;
- `key` is written to `/etc/upcontrol/agent.env`, mode `0600`; `claim_token` goes **only** into
  `~/.upcontrol/config.json`, mode `0600`, and never into `agent.yaml`;
- `claim_url` is printed once on screen and repeated in the final report;
- protection against abuse of anonymous issuance is the server's duty, named here so that it does
  not end up unplanned. With an open agent it has to be real: the issuance code will be read.

**An unclaimed project accepts data but cannot notify.** It has no delivery channels. This is not an
artificial restriction but a fact — there is nowhere to send to — and it is precisely the honest
pull towards claiming. The screen must say this plainly, not hint at "limited functionality".

### 7.2 Offline

Without a key the library **does not crash and does not make noise**: `track()` runs empty, the
buffer does not grow, a warning is printed once at initialization. Once a key appears in the
environment, sending starts from the next run, with no code changes.

The "collect into a spool and send later" mode is gone: the spool was on disk and belonged to the
daemon. The library's buffer lives in the process's memory and survives not a weekend but seconds of
a network failure (§5.3).

### 7.3 Claiming later

Claiming is a device code by e-mail or the deep link `t.me/<bot>?start=<claim_token>`.

- **claiming does not change the key** — rotation would require a release of somebody else's
  application; the only requirement in this section whose violation shows up at the customer's and
  not at ours;
- it is idempotent: a repeated `claim` prints the owner and exits with code 0;
- `claim_token` is single-use, deleted after success;
- all data accumulated before claiming stays with the project;
- Telegram gives identity and a delivery channel in one action, e-mail gives identity only.

### 7.4 Self-host: the section is gone

Version 1.0 had a §7.4 about `--endpoint` pointing at your own logflow. The server is closed (plan
v3 §0.7), self-host is not shipped, and the `--endpoint` flag **does not exist**. This is a
regression against the marketing site's promises, and it is recorded where it will be read: plan v3
§10.

### 7.5 A key in the repository — only one case

The daemon does not put the key into the repository: it lives in `/etc`. The only exception is the
`docker-compose` installation method, where the service travels into the customer's file. Then and
only then the `CLAUDE.md` rule applies: the key is written into `.env`, **only after checking that
`.env` is in `.gitignore`**; if it is not, we stop, fix it, ask again. What goes into
`docker-compose.yml` itself is `env_file`, not the value.

---

## 8. Verification and the failure taxonomy

The place where the product proves itself — or silently fails while continuing to look installed.

### 8.1 One gate instead of two

Version 1.0 had a static gate (`upcontrol check` over the diff) and a live one (`verify`). The
static one disappeared together with the code edit. The live one remained and became the only one —
and therefore mandatory.

**The mechanism that makes it cheap:** the agent emits `install_verified` once on the first
successful connection. There is no need to wait for a payment to prove the chain: starting the
daemon proves key validity, network reachability, the transport wiring and the scrubber's operation
in one step.

```
Waiting for the agent to report…                    0:07

✓ install_verified — 0:09    key ok, transport ok, host web-1
  stored as: {"event":"install_verified","host":"web-1","version":"0.3.1","env":"prod"}
  warnings: none

✓ metrics flowing — cpu, memory, 2 disks, 3 interfaces
✓ logs flowing   — nginx 41 lines, docker/api 12 lines

  Nothing else needed. Open it: https://upcontrol.io/p/pr_7f2a91
```

### 8.2 The failure taxonomy — it must be specific

"Nothing arrived" is useless. Five distinguishable states, each derivable from what did and did not
arrive:

| Observation | Diagnosis | What we print |
|---|---|---|
| the application did not start | an import error, a version conflict, the build | the runtime's output straight into our answer to the agent, not "go and look yourself" |
| the application is alive, there is no `install_verified` | the key was not picked up, or there is no connectivity | these are distinguishable: the key is missing from the environment / DNS / TCP / TLS / HTTP code — we print at which step it stopped |
| there is `install_verified`, there are no events | the points were written into code that has not run yet | name exactly which ones we are waiting for and suggest a path that will trigger them |
| there are events, but not the right ones | the agent picked names outside the dictionary | show what arrived against §4 — the mistake is cheap (T4), but it has to be seen |
| data arrived **with a warning in the receipt** | a problem on the wire, invisible to the user | print the closed dictionary: `ts_absent`, `level_unknown`, `key_in_body`, `field_cap_exceeded`, `scrubbed`, `reserved_prefix` |

The last row is the very mechanism that compensates for ingest's tolerance: a broken client forever
gets 2xx and never finds out, if nobody looks. `verify` looks.

**`verify` must be able to fail and to say so.** The failure this whole section exists to prevent is
a silent failure that looks installed. The failure is printed, the exit code is 4, `--resume` stays
available, and the status is mirrored to the Telegram bot: real installations end at a deploy an
hour later, not in the terminal.

---

## 9. State, idempotency, updates

### 9.1 State

`agent.yaml` is the configuration, edited by hand. `/var/lib/upcontrol/state.json` is the daemon's
state: positions in files (by inode, not by name — rotation), the journald cursor, the format
version, counters. **It never contains the key.**

`~/.upcontrol/config.json`, mode `0600` — the account token and `claim_token`, and nothing else.

### 9.2 Running it again

A reconciliation, not a fresh installation. Five states of a source:

| State | Meaning | Action |
|---|---|---|
| `present` | the source is in place and readable | no-op, silently |
| `drifted` | the path changed, the file was found by inode or by glob | update the state, do not touch the config |
| **`lost`** | the source disappeared | **report it, never remove it silently.** The customer may have deleted the service on purpose |
| `denied` | the source exists, but there are no permissions | name the command that grants them |
| `new` | a source appeared that was not there before | offer it |

`new` is the installer's recurring value: `upcontrol install` after rolling out a new service is a
natural move, not a one-off ritual.

### 9.3 A silent source

A source that is present in the configuration and gives no lines for 14 days is a **server-side**
observation, not the CLI's job; it surfaces in the dashboard and in the digest. It means either a
dead service or broken collection, and only a human can tell them apart. This closes a real hole:
without such detection a source that silently stopped arriving is indistinguishable from a source
that is legitimately quiet — that is, from exactly the failure the product exists to prevent.

### 9.4 Updates and two compatibility commitments

Updating the library is **the customer's release, not our command**, and that is the most expensive
consequence of moving off the daemon (§1.3). The daemon updated with one command; a library inside a
deployed application may go a year without an update. That is why both of version 1.0's commitments
are not merely kept but become load-bearing.

- **Adding a canonical event is a minor version. A rename or a removal is a major one, and the old
  name keeps working for 12 months.**
- **There is never a forced update.** The wire format is not the library version. Ingest accepts
  **any version ever released**: old body shapes are never removed, extensions only add fields. A
  2026 library works in 2029, because ingest is tolerant by contract. A library that stopped working
  because we rolled something out is our bug, and it is in the customer's production, where we
  cannot reach.

From the same place comes the wire versioning rule: the library version travels in the body of every
batch, and that is the only way to know who has what installed when a security fix has to be
shipped.

---

## 10. Security

We ask for one thing that it is right to be nervous about: the right to read logs and metrics on
somebody else's machine. The principle: **nothing here may require trust that cannot be verified in
ten minutes.**

### 10.1 What the agent must never do

**Reading**

- Never read a source not listed in `agent.yaml`.
- A hard deny-list applied **on top of** the explicit selection and regardless of anything else:
  `.env*`, `*.pem`, `*.key`, `*.p12`, `id_*`, `.npmrc`, `.pypirc`, `.netrc`, `.git/config`, `.aws/`,
  `.ssh/`, `.kube/`, `**/secrets/**`, `.bash_history`, `.zsh_history`, `/etc/shadow`, any file with
  a private-key header. An attempt to add such a path is rejected with a reason.
- `/var/log/auth.log` and its analogs — **only with explicit separate consent**, on its own
  screen: they are useful and full of user names and addresses.
- Never read the memory of somebody else's processes, never attach a debugger, never trace syscalls.
- **Never upload source code.** The agent reads logs, not repositories.

**Writing**

- Three paths and nowhere else: `/etc/upcontrol/`, `/var/lib/upcontrol/`, the unit file. Plus
  `~/.upcontrol/config.json` mode `0600`.
- Never touch CI configuration, the Dockerfile, Terraform, k8s manifests or secret managers without
  separate explicit confirmation. The only exception is the service in `docker-compose.yml`, shown
  as a diff and confirmed on its own screen.
- Never run `git add`, `git commit`, `git push`, never run somebody else's scripts, tests or builds.

**Network and execution**

- **The plugin does not open the network at all.** Zero outbound destinations, zero installation
  telemetry. This is an invariant, not a setting: the promise "the code never leaves the machine"
  rests on it.
- The library: exactly one outbound destination — `endpoint`. It never opens a listening socket,
  downloads nothing and executes nothing at runtime.
- The library does not read the environment beyond the key and the endpoint. An observability
  library inventorying environment variables is exactly what people are afraid of from us.

### 10.2 What is verifiable today, from the installed artifact

1. `npm view @upcontrol/sdk scripts` → empty, there is no `postinstall`. `dependencies` →
   **empty**.
2. Read the plugin: it is markdown and a manifest, there is no executable code.
3. `/upcontrol:add` on a repository with a traffic interceptor → zero outbound connections until the
   moment the application with the library goes to `endpoint` on its own.
4. The diff before applying → every line we propose to add is visible in full.
5. Read `cli/sdk/scrub/` → the full list of scrubbing rules, with tests for the vectors.
6. Start the application and look at the process's outbound connections → exactly one destination.

### 10.3 What becomes verifiable after publication

1. **npm provenance** — `npm publish --provenance` from a public workflow: a signed attestation
   binding the tarball to a specific commit and run.
2. **A reproducible build of the agent** — the reader builds from source and gets the same hash as
   in npm. For an open agent this is not decoration but the closing of the chain: without it "open
   source" does not prove that this is what runs.
3. **An open scrubber with a test corpus of vectors** — §6.
4. `SECURITY.md` with the deny-list, the telemetry payload, the scrubbing rules and a vulnerability
   disclosure address.
5. A paragraph on reverse injection: the agent reads somebody else's logs, which may contain hostile
   text. This used to say "logs are not fed into the model at any step" — with the arrival of
   Explain that stopped being true, and the boundary does not run there. What is true now:

   - **Neither ingest, nor the CLI, nor the SDK feed lines into the model.** Ingest, scrubbing and
     storage do without it; lines are scrubbed before they are written, the key never reaches the
     model.
   - **Only the lines a human selected and pressed Explain on go to the model**, and only those —
     not the whole window, not neighboring projects. With no provider configured anywhere (no
     `UC_AI_*` in the environment, nothing saved on the Settings screen) Explain is simply off:
     the endpoint answers 503 `ai_not_configured` and names the fix. There is no fallback answer.
     The network-free heuristic this paragraph used to describe was removed on 2026-08-20 by owner
     decision — the guess is the model's or it does not exist, and a canned one signed with the
     product's name is the worst of both.
   - **The selected lines are untrusted input and are handled as such.** They travel inside
     `<log-lines>` markers, the system prompt declares everything inside the markers to be data and
     not instructions, and any marker met in the lines themselves is neutralized before sending
     (`ai.UserMessage`) — the fence cannot be closed from the inside.
   - The model's answer is strict JSON of a fixed shape; it executes nothing and is written nowhere
     except the answer cache.

   Injection remains a risk of the level "the model may produce a wrong conclusion", not "somebody
   else's text gains control", and this has to be written down in advance, because an attentive
   reader will ask.

---

## 11. The plugin's contents

The plugin's job changed twice. In version 1.0 it instrumented sources; in 2.0 it explained to
somebody else's agent how to embed the daemon; in 3.0 it is **about instrumentation again**, but
through instructions for the user's agent rather than through our code.

Contents:

| Part | Content |
|---|---|
| Instructions | how to read a repository, how to tell business logic from infrastructure logic, where to put the points |
| Dictionary | the 24 canonical names (§4) plus the tier ladder for the non-canonical |
| Instrumentation rules | §5.2 verbatim |
| Key rules | §7 verbatim |
| Stack recipes | literal copyable initialization lines for Next.js, Express, Fastify, NestJS |
| Output rules | the diff goes to review, the point counter, never commit yourself |

Three rules from version 1.0 that are still right:

- **like a code review, not like an infection**: a dry run with the full diff, a separate commit or
  PR, an idempotent repeat;
- **recipes are literal copyable lines, never prose**: a model given prose invents a signature; a
  model given a line copies it;
- **completion is not "done" but verification against the fact of delivery** (§8). The plugin does
  not declare success until data has arrived.

One rule that version 1.0 did not have and that follows from the plugin's openness: **the
instructions are written as if not only the agent but also a human will read them**. A plugin
installed into somebody else's coding agent is text that executes over somebody else's code; it must
read as a reviewable document, not as prompt magic.

---

## 12. Open questions, in decreasing order of significance

1. **The extraction rules (§4.5) — whose corpus.** Ready-made rules for eight stacks have to be
   written and maintained. The question: do they go into the agent's open repository (then the
   community edits them and it is the best contribution channel) or into a closed package (then the
   quality is ours and there are no contributions). **Answer before the first release.**
2. **The rules' time budget (§4.5, requirement 2) — what number.** The per-line ceiling is not
   named. Measure it on a real corpus in M0, before the rules reach people.
3. **What the agent does on a downgrade.** The window capacity shrank — the cut-off moved, and part
   of what was already sent became invisible. The agent does not know about this and must not; the
   question is whether the server tells it "send less" or evicts silently. The second is more honest
   to the ring model, the first is cheaper on traffic.
4. **The lifetime of an unclaimed project.** The answer must be a number on screen, not a default in
   the code: the very first person who comes back a month later will find emptiness.
5. ~~**The license of the agent and the CLI.**~~ **Settled 15 August 2026: MIT** (owner's decision
   at the publication of 0.1.0).
6. **Windows.** Not in the list of platforms. The threshold is a request from people, not a guess.
7. **The upper bound on the number of sources per host.** Screen S2 is readable at ten and
   unreadable at a hundred (a machine with a hundred containers). Either a ceiling with grouping by
   label, or paginated selection.
