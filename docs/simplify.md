# Simplification candidates

Twelve places the system could get smaller, ordered by value per unit of risk.
Each entry says what to cut, what it costs, and — where the honest answer is
"don't" — why the complexity is load-bearing.

Written against the tree at `ae676cb`. Line counts are non-test unless stated.
The repository is ~51k lines total: 18.4k backend Go, 12k backend tests, 6k
generated, 10.7k front, 2.6k CLI, 1.8k SQL and infra.

> **STATUS, 2026-08-28.** Worked through in four passes. Each item below carries
> a status note; three landed differently from what is written here and say so.
> Only item 1's group is on `master` so far: items 2, 3, 11 and 12 sit on
> committed branches awaiting a merge, and item 8 is mid-flight.
>
> Three claims in this document turned out to be **wrong**, all the same way:
> they judge the product from inside the public `core/` repo, which cannot see
> the closed `admin/` dashboard. Corrected in place at items 2, 5 and 8.

## Summary

| #    | Change                                                   | Status |
|------|----------------------------------------------------------|--------|
| 1    | Split the README by audience                             | **DONE**, and then some: the heartbeat line was made true rather than deleted |
| 2    | One entity: Event, named or derived; drop the dictionary | **BUILT, narrow** — dictionary deleted; the `uc.event` marker ships; the read-API split stays |
| 3    | Fold ucworker into ucapi                                 | **DONE** |
| 4    | Drop the second Caddy                                    | **DECLINED** (owner, 2026-08-28) |
| 5    | Decide what analytics is for                             | **DECLINED** — and the premise is false, see the item |
| 6    | Move `discover/` out of `internal/probe/`                | **DONE** |
| 7    | Ship the WAL's replay path, or stop fsyncing             | **DONE** — the WAL was deleted |
| 8    | Postgres by default; drop ClickHouse                     | **IN PROGRESS** — group 1 of 5; risk is High, not Medium |
| 9    | Keep: ucprobe as its own binary                          | Kept, untouched |
| 10   | Store the client's level verbatim + `level_norm`         | **DONE, INVERTED** — `level` stayed normalized, raw went to `level_raw` |
| 11   | Keep scrubbing, add a `UC_SCRUB=0` off switch            | **DONE**, including the attr-scrubbing gap |
| 12   | Implement fingerprinting (gap, not a cut)                | **DONE**, including the attr caps; different warning codes |

Guiding principles for all of these live in [philosophy.md](./internal/philosophy.md).

---

## 1. Split the README by audience

> **DONE.** README split by audience; the diagram, component table and folder
> map moved to `docs/architecture.md`. Both wrong claims fixed: "no telemetry"
> reworded as a promise about us, and the nightly-cron line was made TRUE by
> building the heartbeat vertical (ping route, missed-window job, migration 028)
> rather than deleting it.

The README opens with NDJSON, WALs, event tiers, advisory locks, MTTA/MTTR and
connect-rpc. That vocabulary serves someone auditing the architecture. It
repels the person who wants to know whether their site is up — who is the
larger audience and the one who converts.

Two documents, not one. The README answers "what is this, what do I type, what
happens next" in plain sentences. Everything structural moves into
`docs/architecture.md`, which already exists and already does that job well.

While rewriting, fix the two claims that are wrong today:

- **"Alert me when my nightly cron dies silently."** There is no absence
  detector anywhere in `back/internal/detect`. `heartbeat` exists as a name in
  the event dictionary, but nothing watches for an event that fails to arrive.
  Either build it or delete the line.
- **"No telemetry."** Readers parse this as a statement about what the software
  does, when it is a promise about what it does not send us — confusing, in a
  product whose entire purpose is telemetry. Say "We never collect anything
  about you. The software doesn't call home."

**Trade-off:** none. No runtime behaviour changes.

## 2. One entity — Event; names declared or derived; drop the dictionary

> **BUILT (2026-08-29).** The dictionary and its five tiers are deleted
> (`normalize.go`, 108 lines to 52) and `EventTier` is gone — it was written on
> every row and read by nothing. The `named_by` model below is now built: a JSON
> line may carry `"uc.event": true`, which declares that its message is an event
> name rather than a log line. `normalize.Classify` honours the marker (the
> reserved `uc.*` prefix still wins over it), `track()` sends it, and the
> contract documents it on `POST /i`.
>
> The contradiction this item left open is resolved: a derived identity IS the
> fingerprint and is NOT addressable as a name in alert rules, so retuning
> item 12's masking can never silently rewrite someone's rules.
>
> The marker reaches users only at the next npm release, so the textual
> fallback stays and is what keeps the gap safe for every already-published
> SDK: the `deploy` family (post-deploy suppression reads it) and
> **`install_verified`**, whose only consumer is the closed `admin/` dashboard,
> outside this repo. Both remain guarded by tests because both failures are
> silent.
>
> Both sub-items below are DONE: `eventKind` reordered (a failure now outranks
> its subject, though `deploy` stays first), and `mergeTimeline` sorts on
> `time.Time`.

Decision taken 2026-08-27, refined same day. There is one domain entity:

    Event { name, named_by: client|derived, message?, level?, ts, attrs }

- `name` is required. A client that supplies one (`track('payment_failed')`)
  declares it; when absent — console mirroring, logger bridges, syslog/Loki/
  OTLP pipes, stack traces, senders with no human at the call site — ingest
  derives it: the message with variable parts masked ("user <n> not found").
  The fingerprint (item 12) is the hash of that same template; one mechanism.
- `named_by=client` rows are sparse business facts, kept long; they anchor
  the correlation timeline and post-deploy suppression.
- `named_by=derived` rows are the firehose, 24–48h retention; they feed the
  error-rate baseline, error grouping, and the log context an agent needs.
  Storing them is not optional: they are what catches failures nobody
  predicted, and the detector's denominator.
- The "logs vs. events" split disappears from the API, SDK and docs. Storage
  may keep two tables — cheap retention is DROP PARTITION, and you cannot
  drop a partition while keeping three rows inside it — but that is plumbing,
  not a concept users learn.

Deleted: the frozen 24-name dictionary and its tiers
(`internal/ingest/normalize`), the Tier 1–3 promotion gate (`ingest.go:269`),
and the double-write's conceptual weight. Kept as documented convention:
`deploy` as a reserved name (post-deploy suppression queries it,
`detect.go:158`) and the `uc.*` reserved prefix.

**Trade-off:** free-form names mean typos become distinct event types, and
derived-name quality depends on template masking (see item 12's tuning note).
The dictionary's tier-based alert severity is gone; alert rules must name
events explicitly. Correct trade for a tool whose clients are coding agents
reading their own names back.

**While you are in this area:** `eventKind` (`internal/api/read_api.go:632`)
classifies events for display by substring matching — order-dependent, so
`payment_failed` renders as a payment event rather than an error; fix its
ordering. And `mergeTimeline` (`read_api.go:667`) sorts a chronology by its
`"HH:MM"` display string — incidents spanning midnight render out of order,
invalid timestamps sort first. `EventsAround` already returns correct
`time.Time` order; carry the timestamp and sort on that.

## 3. Fold ucworker into ucapi

> **DONE.** Jobs extracted to `internal/worker`, called by both binaries;
> `cmd/ucworker/main.go` 296 lines to 39. `UC_WITH_WORKER` defaults on for
> self-host, off otherwise. The count below is stale: 8 jobs, not five. The
> stale rationale in `architecture.md` is corrected in two places, one of which
> a grep missed because the phrase wraps across a line break.

`ucworker` is a 246-line main that runs five looping jobs. Every one of them —
including delivery — is wrapped in `runWithLock`, a non-blocking
`pg_try_advisory_lock` that skips when another instance holds it
(`cmd/ucworker/main.go:155`). N replicas are already safe by construction.

Make it a flag on `ucapi`: `--with-worker`, defaulting on for self-host and off
for the hosted deployment where the split still earns its keep. One binary, one
image, one thing to restart.

Note that `docs/architecture.md` justifies the split by saying two ucapi
replicas with an inline delivery worker would duplicate deliveries. Given the
advisory locks, that rationale no longer holds and the doc should be corrected
alongside the change.

**Trade-off:** you lose the ability to scale or restart background work
independently of the API, and a panic in a worker job takes the API down with
it. Both matter at hosted scale; neither matters on one box. Keep the binary
building so the hosted deployment can still run it separately.

## 4. Drop the second Caddy

> **DECLINED (owner, 2026-08-28).** The trade is bad: minus one container that
> shares a base layer, plus a shared-volume coupling and a `front` image that
> stops being independently runnable.

`front/Dockerfile` builds the static bundle and serves it from `caddy:2-alpine`.
`infra/docker-compose.yml` then puts *another* Caddy in front as the TLS edge.
The edge can serve `/srv` directly from a volume.

**Trade-off:** the `front` image stops being independently runnable, which is
worth something if anyone deploys it outside this compose file. If nobody does,
it's a container for nothing.

Combined with #3, the compose file goes from 8 services to 5: postgres,
clickhouse, migrate (one-shot), ucapi, caddy — plus ucprobe where used.

## 5. Decide what analytics is for

> **DECLINED (owner, 2026-08-28) — and the premise below is FALSE.**
>
> "Nobody in this repository can read it" is true of `core/` and wrong about the
> product. `admin/src/queries.ts` reads `web_events` in 20 places; the ucadmin
> dashboard is the consumer. This is hosted-serving, not dead code, and "stop
> collecting" would have broken it.
>
> This is the first of three places where judging from inside the public repo
> missed the closed one. Do not action this item as a cut.

`internal/analytics` is a complete first-party web analytics engine — visitor
IDs via a `uc_vid` cookie, user-agent parsing, GeoIP, UTM capture, IP hashed
and never stored — writing to a `web_events` table with a 730-day TTL. The
recorder is live, wired into ucapi at `cmd/ucapi/main.go:124`.

There is no analytics page in `front/src/pages`. The system is collecting
visitor data that nobody in this repository can read.

Two honest options: ship the page, or stop collecting until you do. The current
state is the worst of both — the storage cost, the cookie, and the privacy
surface, with none of the value.

**Trade-off:** if this feeds the hosted product's own dashboards from a
different repo, it is not dead and should simply be documented as
hosted-serving. Confirm before deleting.

## 6. Move `discover/` out of `internal/probe/`

> **DONE.** `internal/probe/discover` moved to `internal/discover`, a pure
> rename; one importer, no behaviour change.

`internal/probe/discover` is ~1,000 lines that crawl robots.txt, sitemaps,
hosts and page links to *suggest what to monitor*. It is filed under
`internal/probe/`, which reads as though it is part of the check runner.

It is not. `ucprobe` never imports it. The only importer is `internal/api`
(`write_api.go:1912`), so the crawl runs inside ucapi, on a user request, and
always has.

So this is a directory rename — `internal/probe/discover` → `internal/discover`
— and nothing else. No behaviour changes, no binary changes, no security
posture changes. The only thing it fixes is that anyone sizing up the probe
currently counts a thousand lines the probe does not contain, and anyone
reading `internal/probe/` reasonably assumes discovery ships to the edge.

What the probe binary actually is: the HTTP executor (358), the SSRF guard
(95) and main (157) — about 610 lines of check running.

**Trade-off:** none beyond the import churn of a rename. This is the cheapest
item in the document and the one most likely to be skipped for being cosmetic;
it is worth doing precisely because the current layout has already misled at
least one reader about where the code runs.

## 7. Ship the WAL's replay path, or stop fsyncing

> **DONE - the WAL was deleted** (owner, 2026-08-28), replay not built. Worth
> recording why it was more than latency: nothing truncated `ingest.wal`, and
> `dirSpoolFiller` sizes the spool directory as the overload gauge, so every
> long-lived install was drifting toward the 60/75/90/100% shed steps and
> eventually refusing ingest with `spool_full`.

`ingest.go:155` appends and fsyncs every accepted batch before returning a
receipt. The comment directly above it is candid: *"No replay path exists yet;
the file is a durability record, not recovery."*

So each ingest request pays a disk sync for a file nothing reads. Either build
the replay — which is what makes the durability claim true — or drop the fsync
and let the in-memory batcher own the path, and stop describing the receipt as
durable.

**Trade-off:** these go in opposite directions. Replay is the right answer if
you want to keep promising that an accepted batch survives a crash; it is also
more code, which is not what this document is for. Dropping the fsync is the
simplification, and it costs you the guarantee. Pick deliberately — the one
thing not to do is keep paying for a guarantee you don't deliver.

## 8. Postgres by default; drop ClickHouse

> **IN PROGRESS — group 1 of 5.** Branch `fire/postgres-only`. Nothing merges
> until all five land: half a storage migration does not run.
>
> **The risk rating below is wrong: this is High, not Medium.** The survey found
> materially more than "22 queries": **14 non-test files** import `storage/ch`,
> **14 more raw-SQL sites** reach past it via `.Raw()`, `ring/query` is 291 lines
> with 12 log queries, and the ClickHouse schema is **18 objects** — 9 tables and
> 7 materialized views — not the 4 counted here. Plus `admin/`, in another repo.
>
> Owner decisions (2026-08-28) that make it tractable:
> - **The seq ring survives.** `project_window.cutoff_seq`/`retain_seq` keep
>   their exact meaning and every read keeps filtering `seq >= cutoffSeq`. Day
>   partitions are only the physical floor. Replacing the ring with time-based
>   partitions would have made retention global, silently gutting a paid axis.
> - **No data is worth keeping** (no customers, no OSS users), so there is no
>   migration path and the 28 migrations collapse to a fresh `001`.
> - **The prod reset is a manual, documented drop.** goose records version 28, so
>   a fresh `001` would be treated as already applied and skipped. An automated
>   `DROP DATABASE` in the deploy path would eventually wipe a self-hoster.
> - `admin/`'s ClickHouse queries get ported too, not dropped.
>
> Groups: (1) fresh schema + `pgstore` writer, (2) write path, (3) read path and
> the detector, (4) `admin/`, (5) teardown — delete `storage/ch`, drop the
> service from both compose files, lift `install.sh`'s 2GB floor, rewrite docs.
>
> Decided: the six rollups (`checks_1m`, `series_1h`, `checks_1h`, `metrics_5m`,
> `metrics_1h`, `baselines`) are dropped, not deferred. A survey found them read
> by nothing in core, cloud or admin; only `series_1m` was ever queried, and it
> was ported and is what the detector's median/MAD reads. Nothing is lost.
> Re-creating the materialized views as Postgres triggers would rebuild the
> complexity this item exists to delete.

Decision taken 2026-08-27: Postgres is the storage engine. ClickHouse is at
minimum optional and OFF by default; the working intent is to remove it
outright and re-add a column store only if a real high-volume customer forces
the question. The git history keeps the code; nothing is lost by deleting it.

This is the largest reduction in the list. It was originally rated High risk
on the assumption that the detector's baseline math needed a column store; a
closer look shows it does not, which drops the rating to Medium.

ClickHouse is why the RAM floor is 2GB, why `install.sh` refuses to start on
small boxes, and why "just run it" is a longer conversation than it should be.
What it actually serves, by query count: 13 queries against `logs` (search,
tail, histograms — `internal/ring/query`), 4 against `checks` (uptime history),
3 against `series_1m` (the 7-day error-rate baseline), 2 against `events`
(the incident timeline).

The design that works — trigger rollups on **age, not row count**:

- **Raw logs in Postgres**, partitioned by day, 24–48h retention. Retention is
  `DROP PARTITION`, not `DELETE`. Search, the incident timeline and tail all
  read this window from one source.
- **A `series_1m` counts table in Postgres**, maintained at ingest time by a
  batched `ON CONFLICT ... DO UPDATE SET n = n + ?` upsert at each `BatchSink`
  flush, 7-day TTL. The detector's median/MAD reads this — the same numbers
  ClickHouse's rollup holds today, so detection stays exact, not degraded.
- **No compaction job.** Count-triggered aggregation ("collapse past 1k rows
  per type") was considered and rejected: an incident burst is exactly what
  crosses the threshold, so it destroys the raw lines at the moment they
  matter; "event type" requires fingerprinting free-text logs (a new
  subsystem); every query grows a raw-vs-rollup fork; and retention becomes
  traffic-dependent, so nobody can say how far back raw lines go.

What is honestly lost: log search past the raw window (say so in the UI), and
histogram queries older than 48h come from 1-minute counts only. Write-side,
the upsert needs batching to avoid hot-row contention — the `BatchSink` flush
boundary already provides it.

**Trade-off:** at high ingest volume (multiple GB/day), Postgres log search
and histograms degrade where ClickHouse would not, and re-adding a column
store later is real work. Accepted: that customer does not exist today, and
holding a 2GB RAM floor on every install to be ready for them is the wrong
default. Removing rather than dual-backing also avoids the worst outcome —
maintaining two storage backends forever.

## 9. Keep: ucprobe as its own binary

> **KEPT**, untouched, as intended.

Listed so it isn't revisited. The probe leases checks, runs them, submits
results, and holds no database credentials. That last property is the point: it
is the component most exposed to hostile input, it is the one you would run in
another region, and a compromise of it leaks nothing. The separation buys
security and reach that a flag cannot.

The transport (`internal/rpc/probeservice.go`, 267 lines) is two calls — lease
work, submit results, on a loop. "Run periodically and report to the master
node" is already exactly what it does; RPC is only the name for those two
calls, and a probe in another region is the same binary at a different address.

The binary is ~610 lines and there is nothing in it to cut. See #6 for the
package that made it look larger.

## 10. Store the client's level verbatim, normalize into a second column

> **DONE, WITH THE COLUMNS INVERTED.** `level` stays the normalized value and
> the client string goes to a new `level_raw` (capped at 32 bytes). Same
> guarantee, opposite naming: `level` is what 14 readers, a set index and both
> rollup views group by, so renaming it meant rewriting all of them for nothing.
>
> `fatal`, `critical`, `panic`, `crit`, `emerg` and `alert` now map to `error`.
> Known levels are also lowercased, which incidentally fixed the overload shed
> mask: it keys on `level` and had been missing `ERROR` entirely.
>
> The SDK already mapped `fatal` to `error` client-side, so this bug only ever
> hit Loki, syslog, OTLP and hand-rolled senders.

Decision taken 2026-08-27. Today `normalizeLevel` (`decode.go:190`) rewrites
whatever the client sent into `info|warn|error|debug|trace`, and anything it
does not recognise — including `fatal` and `critical` — silently becomes
`info`, the one level the detector ignores. That is thinking for the user,
and in the worst case it downgrades their most serious line.

What changes: the `level` column stores the client's string exactly as sent
(`ERR`, `sev2`, whatever their system emits). A second column `level_norm`
holds the mapped value, computed once at ingest, and is what the detector,
overload shedding and histograms read. Unmappable levels get
`level_norm = 'error'`-adjacent handling decided explicitly, not a silent
`info`; the mapping must include `fatal` and `critical` → `error`.

**Trade-off:** one extra low-cardinality column, and the mapping table still
exists — it has to, because `level = 'error'` filters cannot fuzzy-match at
read time on every query. What is bought: the user's data is never rewritten,
only annotated.

## 11. Keep scrubbing, add an off switch

> **DONE.** `UC_SCRUB=0`, honoured only with `UC_SELF_HOSTED=1`; set without it
> the variable is ignored and the boot log says so. The attr-scrubbing gap is
> closed, so the comment is finally true.
>
> The config field is `ScrubOff`, negative on purpose: `Deps` is built from
> literals, so a positive `Scrub bool` would mean any literal that forgets it
> silently stops redacting. The zero value must scrub.

Decision taken 2026-08-27. Secret scrubbing (`internal/ingest/scrub`) stays —
the hosted service cannot store other people's bearer tokens and connection
strings — but a self-hosted operator logging to their own box is entitled to
turn it off.

Configuration today is environment-only: `UC_*` variables with `UC_*_FILE`
indirection for secrets (`platform/config/config.go`); there is no config
file and no CLI flags. So the switch is `UC_SCRUB=0`, default on, documented
next to `UC_SELF_HOSTED`.

**Close the attr-scrubbing gap while here.** The comment at `ingest.go:249`
says "Scrub the message and attribute values (defense in depth)" — the code
scrubs only the message; attrs pass through untouched. The SDK scrubs
client-side, but Loki labels, Alertmanager labels and hand-rolled NDJSON
arrive with no scrubbing at all: a connection string in a Loki label is
stored verbatim while the comment claims otherwise. Scrub attr values
server-side so the comment becomes true, behind the same `UC_SCRUB` switch.

**Trade-off:** scrubbing runs per attr value instead of once per message —
a real but small cost on the hot path; the scrubber is already a single-pass
matcher built for exactly this.

## 12. Implement fingerprinting — a gap, not a cut

> **DONE.** `ingest.Fingerprint` is FNV-64a over the scrubbed message with digit
> runs, hex runs of 8+ and quoted strings masked. Every row stored 0 before, so
> one group cooldown suppressed every other error in the project.
>
> Attr caps shipped too: 64 keys, 256B per key, 8192B per value, kept in SORTED
> key order. Go randomises map iteration, so an unsorted cap would keep a
> different 64 each time and an idempotency replay would disagree with its own
> original write.
>
> Warning codes differ from the text below: `attr_key_capped` and
> `field_cap_exceeded` were already in the contract and never emitted, so using
> them meant zero contract change instead of a third dead code.

Decision taken 2026-08-27. The `fingerprint` column exists in the schema with
a bloom-filter index, `ErrorGroups` consumes it, and the errorlog detector's
new-vs-repeat logic depends on it — but nothing on the write path ever
computes it, so every row stores 0 and all error lines collapse into one
group whose cooldown suppresses every other error in the project.

Implement it at ingest: mask the variable parts of the scrubbed message
(numbers, hex runs, quoted strings), and that masked template does double
duty — it is the derived `name` for uninstrumented events (item 2), and its
hash is the fingerprint. A template hash, not a raw hash, or every line with
a request ID is its own group. Stamp both on the `RowEnvelope`
(`ingest.go:269`).

**Cap attrs in the same ingest pass.** Today there are no server-side limits
on attrs at all: no cap on key count, key length or value length (the SDK's
1MB batch cap is client-side only). One buggy or hostile client can send a
megabyte value or ten thousand distinct keys per record — today that bloats
the ClickHouse Map dictionaries; in the JSONB design it bloats the GIN
index. Enforce at ingest, in the degrade-gracefully style the rest of the
pipeline uses: ≤64 keys, ≤256B per key, ≤8KB per value; drop or truncate the
excess and tally an `attrs_capped` warning in the receipt — never reject the
batch.

**Trade-off:** template-masking heuristics are never perfect; over-masking
merges distinct errors, under-masking splits identical ones. Start crude
(digits + hex) and tune on real data. The alternative — deleting the column
and both queries — was rejected: without grouping, "alert me on a new error"
cannot exist, and that feature is core to goal 2 in philosophy.md.

