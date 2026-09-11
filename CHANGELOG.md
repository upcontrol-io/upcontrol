# Changelog

All notable changes to the self-hosted package. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[semver](https://semver.org/).

## [Unreleased]

## [0.30.0] — 2026-09-11

### Added
- **`GET /public/status-directory`** answers the directory's list as JSON (`{pages: [{host,
  slug, sentence?}]}`), from the same predicate as the HTML directory and the sitemap, and
  `[]` under the kill switch. A browser's `/status` is now a front page reading it, split at
  the edge by User-Agent exactly like `/status/{slug}`: crawlers keep the HTML directory.

### Changed
- The HTML directory is the crawler copy again: back to the minimal achromatic sheet, with no
  header or footer (0.29.1's look-alike ones only ever served humans). It keeps the viewport,
  the description and the `ItemList` JSON-LD, and its title is the front's own:
  `Status pages directory | UpControl`.

## [0.29.1] — 2026-09-11

### Changed
- **The status directory (`GET /status`) matches the rest of the site.** A header (brand link
  + "status directory" tag) and footer (Home/Docs/Pricing/Privacy/Terms, the same operator
  line the front's footer carries) now bracket the list, styled with the product's real dark
  (default) and light palettes via `prefers-color-scheme` instead of the old fixed light-only
  sheet. Still one dependency-free HTML string — no asset pipeline, no JS. Added the missing
  `<meta name="viewport">`, a `<meta name="description">`, and `ItemList` JSON-LD for the
  listed pages. `noindex, follow` is unchanged, and each project's own `/status/{slug}` page
  is untouched.

## [0.29.0] — 2026-09-11

A plan now buys live capacity, and the data stays the customer's. Migration 010 adds
`project.frozen_at`, `monitor.paused_by` and `status_page.domain_lapsed_at`, and makes the
cadence ladder (`_uc_effective_interval`) ignore frozen projects' subscriptions.

### Added
- **One `plan-capacity` ucworker job** runs at start and then every minute, in order: the
  project freeze, the check budget, the custom-domain grace.
- **Project freeze.** A workspace over `plan_entitlement.projects` keeps that many projects live
  and freezes the rest as snapshots. A live project stays live, so the pick is made once;
  between equals the project with more running checks wins, then the newest log or event. A
  frozen project keeps its configuration and keeps ingesting, but its checks stop and nothing
  is delivered from it (queued deliveries die as `project_frozen`). Its incidents and its
  dashboard history (`series_1h`) up to the freeze are exempt from the purge and the trim.
  Incidents on targets that are still probed keep feeding its public status page, and an
  outage still open when the project thaws is delivered then. `GET /v1/projects` rows carry
  `frozen`. `POST /v1/project/switch` into a frozen project
  answers 402, and PATCH or DELETE of its checks answers 402 too. The owner's 402 names the
  cheapest plan that carries every project; a guest's names none. The session resolver, `/v1/me`
  and sign-in never land on a frozen project: with no live project `/v1/me` answers
  `"project": null`, and `DELETE /v1/project` and `POST /v1/monitors` answer 409
  `no_current_project`. Deleting the live project promotes a snapshot in the same request.
  `/v1/me` carries a top-level `owner`, so the owner is named even when `project` is null.
- **`POST /v1/projects/{id}/keys/revoke`** revokes every working key of one of the caller's
  own projects, frozen ones included, so a key that leaked from a snapshot is withdrawn
  without buying a plan. Owner only; the rows are kept. Issuing and rotating a frozen
  project's keys stay walled.
- **Check budget.** Checks past `plan_entitlement.http_checks` are paused, newest first, and
  resume within a minute of the plan carrying them again. `Monitor` rows carry `paused`, and
  `pausedBy: "plan"` marks the budget's own pause. `PATCH {"paused": false}` on such a check
  answers 402. A check the budget pauses has its open incident closed as `plan_paused` ("Paused
  by the plan's check limit"), because nothing measures it any more. Nothing measured a
  recovery either, so the public status page leaves such an incident out and its 15-minute
  follow-up dies as `closed_unmeasured`. `GET /v1/plan` says what the plan holds back:
  `httpChecks.pausedByPlan` (the budget's pauses in live projects) and `projects.frozen`,
  both absent at zero.
- **Telegram seats and rooms at delivery.** Past `telegram_recipients`, the newest
  destinations are muted (the oldest keep their seats), and groups and channels are muted while
  the plan has no `telegram_rooms`. This applies to incident pages and to error-log alerts. The
  channel list marks such a destination `mutedBy: "plan"`, and its test send answers 402. An
  unreadable plan mutes nothing. The seats are counted over each project's own destinations,
  while the connect gate counts the whole workspace, so a workspace with several live projects
  can keep more destinations audible than the plan's seats after a downgrade.
- **Custom-domain grace.** When the plan stops carrying custom domains, a status page's domain
  keeps answering for 3 days (the Caddy ask included) and is then unbound; the page stays on
  its own address. The settings read carries `domainLapsesAt` while the grace runs.

### Changed
- **Contract:** `Monitor.paused`, `ProjectListItem.frozen` and `MeResponse.owner` are required. New 402s:
  `PATCH /v1/checks/{id}` and `PATCH`/`DELETE /v1/monitors/{id}` (frozen project, plan-paused
  unpause), `POST /v1/project/switch` (frozen project) and `POST /v1/channels/{id}/test` (muted
  destination). New 409 `no_current_project` on `POST /v1/monitors` and `DELETE /v1/project`.
  `PATCH /v1/sources/{id}` declares its 404. New optional fields: `Monitor.pausedBy`,
  `AlertChannel.mutedBy`, `StatusPageResponse.domainLapsesAt`, `PlanResponse.httpChecks.pausedByPlan`,
  `PlanResponse.projects.frozen`.
- **The check count skips frozen projects.** The create gate and `GET /v1/plan`'s
  `httpChecks.used` count only live projects' checks, the same set the budget ranks.
  `projects.used` counts live projects only.
- **`POST /public/watch` spends the check budget.** Every arm that subscribes into an existing
  workspace (the e-mail arm, the signed-in arm, a reused unclaimed page) creates new checks
  only while the plan's `http_checks` has room. Rows the project already watches cost nothing,
  the rest are left out, and `watching` counts what the project watches.
- **`POST /public/watch` signed in creates the project in the caller's own workspace**, like
  `POST /v1/projects`, never in the one the session stands in, and the session follows it
  there. A guest standing in somebody else's workspace no longer spends its owner's projects
  and checks.
- **`POST /v1/project/switch` is open to every role.** It changes where the session stands,
  not a project, so a Member may switch, and a guest whose every project in a workspace is
  frozen can switch back out.
- **The public status page lists an open incident only while its target is measured** (within
  three of its intervals, at the slower of the cadence at open and the target's current one).
  It leaves an unmeasured one out and never shows it as resolved.
- **`PATCH` and `DELETE /v1/sources/{id}` reach only the current project's sources** (404
  otherwise).

### Self-hosting
- The `Self-hosted` plan carries 1000 checks and unlimited projects, so nothing freezes there
  and the budget pauses only past 1000 checks. **A tenant on `Free` gets Free's limits**: one
  live project and three running checks. `UC_SELF_HOSTED=1` puts NEW tenants on
  `Self-hosted`, but it does not move existing ones. Set it, and move tenants created without
  it before upgrading:
  `UPDATE tenant SET plan = 'Self-hosted' WHERE plan = 'Free';`
- **Rolling back to 0.28.x:** older code never resumes a check the budget paused. Run
  `UPDATE monitor SET paused = false WHERE paused_by = 'plan';` before pinning an older core.
  Migration 010's Down runs it first, before it drops `paused_by`.

## [0.28.1] — 2026-09-10

### Changed
- **The crawler's status page drops the disclaimer.** The footer of `GET /status/{slug}` keeps
  "Measured from one location outside {host} by UpControl." and no longer says "Not affiliated
  with {host}. Created automatically." The state sentence, the doors and the robots meta are
  unchanged.

## [0.28.0] — 2026-09-10

### Added
- **One URL is checked once, for everybody.** A monitor is now a subscription to a shared
  `probe_target`: two projects watching `https://example.com` cost one fetch, and each still
  sees its own bars and its own incidents. A subscriber who joins an outage gets its incident
  and alert within one result. Heartbeats get a private target each. Liveness and cadence are
  derived in the lease query, so pausing, deleting or releasing a monitor needs no recount, and
  paying subscribers lease first. `www.` folds into the bare host, default ports and a lone `/`
  drop out of the key.
- **A permanent public page per watched host.** `/status/{slug}` for a host minted from the
  landing check keeps its root probe running for good: deleting the root check, or the whole
  project, empties the project and the page lives on. Watching a host whose page somebody
  already claimed mints a second page on the same probe, never a second probe. The reaper
  spares host pages.
- **A crawler gets a real page, a chat gets a picture.** `GET /status/{slug}` serves
  server-rendered HTML with the state sentence, canonical, Open Graph and JSON-LD, and
  `/public/status/{slug}/og.png` renders a 1200×630 card (Inter, OFL). New public doors: the
  directory `/status`, `/sitemap-status.xml`, `/bot` and `/status/policy`.
- **An index gate, not an index flood.** A page enters the sitemap only after 72 hours of
  continuous, measured, mostly-ok checks on a registrable host without wildcard DNS, at a daily
  ramp and under a cap, and leaves with hysteresis. A claimed page needs a DNS TXT proof and
  the owner's "List in search engines". Knobs: `UC_INDEX_RAMP_PER_DAY`, `UC_INDEX_MAX_PAGES`,
  and `UC_INDEX_DISABLED=1` as the kill switch. Removal is self-serve through a DNS TXT token.
- **Mint ceilings and a seed door.** `UC_MINT_PER_IP_PER_DAY`, `UC_MINT_PER_DAY` and
  `UC_HOST_PAGES_MAX` bound anonymous minting; `POST /internal/seed-host` (node token) seeds
  pages for outreach, which stay out of the index until a person interacts with them.
- **ucprobe fetches up to 20 checks at once**, and the server paces the fleet from queue depth:
  2 s after a full batch, 30 s after a partial one, 5 s on an empty queue.

### Changed
- **Could-not-measure is a third state:** HTTP 401, 403, 429, and a bot filter's challenge at
  any status, recognised by header (`cf-mitigated`, `x-amzn-waf-action`, `x-vercel-mitigated`).
  No incident, drawn as no data, never counted against uptime, and the target is asked less
  often. The landing check says "no data" for it instead of "down".
- **A check's target and keyword are immutable:** `PATCH /v1/monitors/{id}` answers 400
  `target_immutable`. Delete and re-create to move a check.
- **`checks` is partitioned by day**, and retention drops whole partitions; each row records
  the effective interval it was taken at.
- The probe protocol carries `target_id`; a result without one is dropped and counted.

### Fixed
- **A host that hangs is a timeout,** not "connection refused".
- **Saving status page settings created a second page** once the project gained a domain.
- **Switching "List in search engines" off now unlists the page** at once.
- A finished check closes its idle connection instead of holding a socket for 30 s.

### Upgrading
- **Migration 009 rewrites `checks` into daily partitions** and folds identical monitor URLs
  into shared targets; it takes time in proportion to `checks`. It fails if one project has
  two checks on the same normalised URL and keyword, for example `example.com` and
  `https://www.example.com/`: delete one of them before upgrading.
- **Upgrade ucapi and ucprobe together.** A probe older than this release sends no
  `target_id`, and its results are dropped.

## [0.27.0] — 2026-09-09

### Added
- **A breakdown can count events, not only people** (`SeriesQuery.count`, `DashboardMetricRef.count`).
  A dimension nobody is behind — a delivery outcome, an email status, an HTTP status, a queue
  name — has no actors at all, and every people read filters `actor <> ''`. So such a card read
  0 and said nothing about why: the one silent-empty failure left in the analytics kinds, and
  the reason four cards on our own board had been dark since the feeds moved to events.
  `count: events` counts rows instead, **and drops the actor filter with the aggregate** —
  counting events among rows that happen to have a person answers a third question nobody
  asked. Absent means people, so every board saved before this keeps its meaning.
  **Nothing switches the axis by itself.** The form asks, the ref carries the answer, and the
  card prints `· events` in its caption because only the unusual unit needs saying: a card that
  changed meaning the day its first actor arrived would be worse than an empty one. It is
  refused on a funnel, a retention grid and an A/B test — those ARE people by definition, and a
  conversion rate over rows is not a rate.

## [0.26.1] — 2026-09-09

### Fixed
- **A board holding any analytics card could not be saved at all** — in the app or through an
  ingest key. `DashboardMetricRef` is the schema a board is STORED as and the write decodes it
  strictly, but it never learned the `people` source: no `steps`, no `cohort`, no `field`, and
  `people` missing from its enum. So `PUT /v1/dashboard` answered `400 unknown_field` to a
  document the server itself had just served, and `validateLayout` would have refused the
  source behind it. Proved against production by reading one project's stored board and sending
  it back unchanged. Live since 0.22.0, when the agent's board doors made both writes strict —
  a funnel card has been unsavable for as long as funnels have been built from events.
  `TestReadLayout_KeepsEveryRefACardCanCarry` asserts the round trip, one case per shape, and
  the live suite now saves one of every analytics card against a real server: the fixture
  backend accepts any body, so nothing in `npm run test` could ever have caught this.

## [0.26.0] — 2026-09-09

### Fixed
- **The retention, breakdown and A/B pickers were empty for every sender, in every language.**
  `GET /v1/dashboard/catalog` built `funnels`, `experiments`, `retentions` and `dimensions` from
  the counter metrics the SDK stopped writing in 1.0.0, when the feeds moved onto events. So the
  three pickers offered nothing, and the form's answer to an empty list was a prompt that
  instruments an SDK feed, which emits events, which never filled the list. All four cards now
  read the `people` source over events: a funnel is its step event names, a retention grid is the
  whole project, a breakdown is one event folded by one of its own fields, and an A/B test is one
  event folded by `uc.variant` and `uc.stat`. The four dead catalog lists are gone.
- **The A/B read never matched what the wire writes.** `ExperimentArms` took the arm label key as
  `name` and TWO event names as its group, while the wire (and the SDK) write one event name
  carrying `uc.variant` and `uc.stat`. It now folds one event name by two label keys, and its
  rows come back labelled by exactly the keys the query asked for, as every other grouped read
  already did.
- **A breakdown could only ever fold by `value`.** The field is the card's now, so an event
  carrying `country` or `plan` ranks by that, which is what `npx upcontrol skills wire` has been
  promising senders outside Node.

### Added
- `CatalogEvent.fields`: the label keys an event's recent rows carried, most-used first, so a
  breakdown is picked rather than typed. Reserved `uc.`-prefixed keys are left out — that
  namespace is the wire's, not a dimension anybody ranks people by.

### Changed
- `npx upcontrol skills dashboard` teaches `people` refs and no longer tells the agent to name a
  feed "as you declared it in the SDK call". A board built from the old doc drew nothing at all,
  silently: that column is load-bearing and the validator does not check it.

## [0.25.1] — 2026-09-09

### Fixed
- **Rotation no longer retires the public key it cannot replace.** `POST /v1/keys/rotate` filtered
  on `state = active` alone, so a project holding a browser key would have had it retired
  alongside the secret ones and replaced by a `uc_live_` key that cannot ship in a bundle — the
  site working for 24 hours and then going quiet, with nothing connecting the silence to the
  button. Rotation is the everything-now lever for SECRET keys; a public key is replaced
  instead, because that means redeploying a site.
- **Rotation issued one new key per retired key.** `old` returns a row each and `INSERT..SELECT`
  ran over all of them, so two active keys became two replacements. Latent since the key set
  shipped; `LIMIT 1` closes it. A project with no active secret key now answers 409
  `nothing_to_rotate` rather than 500 from an empty insert.

## [0.25.0] — 2026-09-09

### Added
- **The four analytics feeds are readable from any language.** `events` gained an `actor`
  column, lifted at the ingest from a reserved `uc.actor` attribute, and `POST /v1/series`
  gained `source: people`, which counts DISTINCT actors over the events a project already
  sends. A funnel, a retention grid, an A/B test and a dimension therefore need nothing but an
  event with an actor on it — no client library, nothing computed on the sender's side. Go,
  Rust, Python, PHP and a static page are now first-class; the npm SDK is a convenience rather
  than the only door.
- **A funnel is defined by the card that draws it**, as a list of event names (`SeriesQuery.steps`),
  so there is nothing to keep in sync between the code that emits events and the board that
  reads them. `cohort: week` reads the retention grid the same way.
- **Public API keys** (`kind`, `origins`; prefix `uc_pub_`) — the credential a browser may hold.
  It writes named events only, is accepted solely from an origin its owner listed (byte-exact,
  no wildcards), and is rate limited per key and address. A public key with no origin is refused
  at mint: that is the unscoped key it exists to replace. Until now `api_key` had no scope at
  all, which is why no single-page app could ever use this product.
- **CORS on the ingest.** There was none anywhere in the backend, so a browser could not have
  posted even with a correct key. `OPTIONS /i` answers the preflight.

### Changed
- The `metric`-source counter reads stay exactly as they were, so boards saved before this
  release keep drawing. They are the compatibility path, not the future one.
- `@upcontrol/sdk` 1.0.0 moves with this: the API does not change by a character, but the
  counting is the server's now, and the on-disk state, the cumulative counter, `uc.reporter`
  and the reset-detection fold are gone from the client.

## [0.24.0] — 2026-09-08

### Fixed
- **A dynamically-appearing counter bucket no longer loses its first count**
  (`@upcontrol/sdk` 0.7.0). `FunnelBuckets` folds a counter by subtracting each reading from
  the one before it and counts the very first as nothing, having nothing to measure it
  against — correct for a counter that has been running, but a breakdown's values and a
  retention's cohorts appear as they are counted, so each one's first increment was invisible
  for good. A dimension value with exactly one person never drew at all. The SDK now reports
  a 0 a millisecond before such a bucket's first real count, the same rise from zero a funnel
  gets by declaring its steps up front. A bucket restored from the state file is untouched: a
  zero under a running total is a drop, and the fold reads a drop as a reset worth its whole
  value.

## [0.23.0] — 2026-09-08

### Changed
- **A breakdown can count distinct people, not only events** (`@upcontrol/sdk` 0.6.0).
  `value(v, who?)` takes the same `who` a funnel step takes, and dedups through the same
  salted-hash id set the funnel, the A/B test and retention have always used — `breakdown()`
  was the one feed on that machine passing `null`, so a dimension could only ever rank
  volume. Additive: a call without `who` counts events exactly as before, so no existing
  instrumentation changes meaning, and the 200-distinct-value ceiling applies in both modes.

## [0.22.0] — 2026-09-08

### Added
- **The agent can build, rebuild and extend the dashboard at any point, not only once.**
  `npx upcontrol board` reads the project's board, `--apply` replaces it and `--add <file|->`
  appends widgets to it, all on the ingest key already in `.env` — no second credential and
  nothing for the reader to paste. `references/dashboard.md` (`npx upcontrol skills dashboard`)
  documents the document: the 12-column grid, `h` in half-rows, every kind's default and floor
  size, the `source` each kind's refs must take, and the exact shape of what the command
  expects. The skill offers the board once `verify` reports data arriving, which is the moment
  it knows what the board should hold.
- `POST /v1/dashboard/widgets` — the additive door. The incoming block lands whole below the
  board's bottom and cannot collide with it; a colliding widget id is re-minted rather than
  refused. Appending never asks for confirmation, because it removes nothing.
- `GET /v1/dashboard/proposal` and `DELETE /v1/dashboard/proposal` — the session's side of a
  layout the key offered.

### Changed
- **An ingest key's board write is gated by PROVENANCE, not by existence.** `dashboard`
  gained `written_by` (migration 006, default `'session'`), and `PUT /v1/dashboard` with a key
  now replaces a board a key wrote instead of refusing it. A board a SESSION saved is still
  never overwritten by a credential that lives in `.env`: the layout is kept in
  `dashboard.proposed` and answered **202** `{"status":"proposed"}`, and the app resolves it in
  one click. The old 409 `board_exists` is gone — it approximated "did a human curate this
  board?" with "does one exist?", and the column answers that directly.
- The key may now `GET /v1/dashboard`. The catalog and the series stay session-only: the
  catalog reports what actually arrived, including attribute values, while the board's refs
  name events the agent itself declared.

### Fixed
- **`POST /v1/recipients/{id}/resend` was dead in production**, and had been since it shipped.
  `http.ServeMux` matches patterns segment by segment, so `POST /v1/recipients/{id}` never
  reached the sub-path: the handler's resend arm was unreachable and the app's Resend button
  answered 404. Unrelated to the board work — found while checking that the board's own new
  paths were wired, which they were not either.
- **Nothing tested that a contract path is actually routed.** Handler tests drive
  `writeAPI.ServeHTTP` and skip the mux entirely, so a path can be in `openapi.yaml`, answered
  by a handler, covered by a green test, and still 404 on a running server.
  `internal/api/routes_test.go` now reads every path out of the contract and fails when one is
  missing from `cmd/ucapi/main.go`.
- **The empty board answered `version: 1`**, left over from before the row was halved. An agent
  that read an empty board, filled it in and sent it back would have returned version-2 sizes
  under a version-1 label, which the front draws at twice the height asked for. An empty board
  has no heights, so the field says nothing except which unit the next writer should use.
- **An append no longer resolves a pending proposal.** Clearing it is right for a session
  write, which is the reader answering; an append is the agent writing, so a second agent run
  was deleting the offer the first one had told the reader to go and press Review on.
- **`npm run typecheck` in `cli/installer` had rotted red.** `test/version.test.ts` imported
  `CLI_VERSION` from `../dist/net.js` while `tsconfig.json` sets `declaration: false`, so no
  `.d.ts` is ever emitted beside it. The test now spawns the built binary and compares
  `--version` against package.json, which is what the claim is actually about. CI gained a
  `Typecheck` step for both cli packages — it ran only `npm test`, which is how this stayed
  invisible.
- Appending onto a board still stored at `version: 1` is refused with a sentence naming the
  fix, instead of silently drawing the agent's cards at twice their height — the agent writes
  version-2 heights and the front doubles a version-1 document whole. The server does not
  convert between the units, because `h` is a drawing decision and the front is what draws.

## [0.21.1] — 2026-09-07

### Security
- **Four advisories closed, one critical.** `github.com/getkin/kin-openapi` 0.142.0 → 0.149.0
  (GHSA-r277-6w6q-xmqw critical, CVE-2026-73502 medium), `google.golang.org/grpc` 1.82.1 →
  1.83.2 (CVE-2026-84304 high) and `github.com/moby/go-archive` 0.2.0 → 0.3.3 (CVE-2026-17106
  high, an indirect dependency of testcontainers and so test-only). Generation is unchanged by
  the kin-openapi bump: `api.gen.go` regenerates byte for byte.

### Fixed
- **CI is green again.** Two jobs had been red since before 0.19.1, and no release since looked
  at them. `lint`: an import group separator in `internal/api/keys.go`, a tagged switch on
  `r.URL.Path` in the same file, and a dead `sumPoints` in `internal/api/dashboard.go`, deleted
  (`pgstore.FunnelBuckets`, its only caller, now has no production consumer either — named, not
  removed, because that is a code change and not a lint fix). `front`: the OSS app's e2e fixture
  still answered `GET /v1/keys` with the pre-key-set shape, so `Settings.tsx` read `keys.length`
  off `undefined` and the whole route threw — which is why both failures pointed at a missing
  `Settings` heading rather than at a key. The fixture now answers the contract, one row per
  key state.

### Changed
- **`DashboardLayout.version` is `1 | 2`.** The board's row halved to 14px and its resize step
  to 26px, so a card can be tuned to what it holds; every stored height doubled with it. A card
  of H rows is `26H - 12` and the old one of h rows was `52h - 12`, so `H = 2h` is the same
  pixels and no existing board changes shape. The front's `normalizeLayout` is the one place a
  version 1 document gains its factor of two, once, on the way in; a save rewrites it as 2. **A
  server older than this refuses a version 2 layout**, which is what makes this release a
  prerequisite for the board work rather than a companion to it.

### Fixed
- **A new project reaches its owner.** `POST /v1/projects` now seeds that project's e-mail
  channel from the creator's own address, as sign-up and the invitation redeem already did.
  A project created by hand had no destination at all, so nothing it detected was ever sent.
  An owner with no address (Telegram-only) still gets no row, and no existing project is
  backfilled — a channel deleted on purpose stays deleted.
- **`POST /v1/channels` no longer answers `201 Created` for a write that failed.** The INSERT's
  error was discarded, so a failure came back as Created with a zero UUID and the row was
  simply absent from the next read. It answers 500 `internal` instead.

## [0.17.1] — 2026-09-06

### Fixed
- The two-instance HA test seeds its channel with a project: migration 004 made
  `alert_channel.project_id` mandatory and the seed predated it. No runtime change.

## [0.17.0] — 2026-09-06

### Changed
- **A project owns its team, its channels and its status page.** Migration 004:
  `tenant.owner_person_id` names the workspace's one owner, who needs no member row and is
  `login` in every project of the workspace; `project_member(project_id, person_id,
  tenant_id, role, status)` replaces `tenant_member`, and existing members land on the
  workspace's oldest project only; `alert_channel`, `telegram_invite` and `error_alert_state`
  carry `project_id`. The session's workspace follows the current project on every switch and
  sign-in, so every read a screen makes is one project's: monitors, incidents, sources, keys
  (a rotate touches one project's key), channels, recipients, the status page and its public
  page, overview, incident evidence and metric tiles. An incident pages its own project's
  channels, the error scanner remembers per project, the delivery failover stays inside the
  project, and a Telegram chat answers for the projects it is connected to.
- **A person may be a member of projects in other people's workspaces.** `GET /v1/projects`
  lists every reachable project, own ones first, each with `owned`, `role` and `ownerEmail`;
  `POST /v1/project/switch` may cross workspaces; `POST /v1/projects` always creates in the
  caller's own workspace (created on first use) against their own plan. `GET /v1/me` carries
  `project.owned` and `project.ownerEmail`, `GET /v1/plan` carries `owned`, and the owner's
  row in `GET /v1/recipients` carries `owner: true`. Sign-in lands on the project an
  invitation just activated, else where the person last worked, else their own project.
- **Owner-only doors.** `DELETE /v1/project` and `GET /v1/export` answer 403 `owner_only` to
  anyone but the workspace's owner; releasing a project also drops its channels, invites,
  members and scanner state.

## [0.16.0] — 2026-09-06

### Added
- **The board is stored on the server.** `GET /v1/dashboard` answers the current project's
  saved layout, or an empty board when it never saved one, and `PUT /v1/dashboard` replaces
  it whole. Any member reads it; only a `login` member writes it. Only the envelope is
  checked (version 1, unique non-empty widget ids, a known kind, a known range when one is
  present, and a widget that fits the 12 columns, under 64 KB), because a widget's refs are
  the front's to interpret. Last write wins: the board moves between browsers and machines
  instead of living in one of them.

## [0.15.0] — 2026-09-06

### Added
- **Two reads for the dashboard board.** `GET /v1/dashboard/catalog` lists what a project
  can draw over the last 7 days (services, message groups by fingerprint with a sample line,
  attribute pairs, checks, events, metrics, funnels with their steps) and `POST /v1/series`
  answers up to 40 bucketed queries in one round trip (logs, checks, events, metrics; a
  funnel step as counter deltas). Both are session reads of the current project.
- **History is a plan axis: how far back the board reads.**
  `plan_entitlement.history_days` (Free 1, Indie 7, Growth 31, Agency 365; NULL on
  Self-hosted is unlimited) travels as `historyDays` on `GET /v1/plan`, and a
  `POST /v1/series` range deeper than it is refused whole with the same 402 every
  other paid axis uses. Behind it, ingest keeps an hourly rollup of the line counts
  per service, level and message fingerprint (`series_1h`), which is what the 7d,
  31d and 365d ranges read instead of scanning the raw lines; ucworker's new
  `history-trim` job drops rollup rows past the tenant's depth. A bucket older than
  the first row the store actually holds draws `null`, never 0: an upgrade starts
  counting from the day it happens, and what was never kept was never zero.

## [0.14.0] — 2026-09-04

### Changed
- **The schema starts over at one migration.** `001_init` now carries the
  whole schema and replaces the earlier 001–006: every ALTER is folded into the
  CREATE it patched, `plan_entitlement` seeds its final numbers, and the log
  partitions are dated from install day. A fresh install never sees the
  difference. A database migrated through the old 001–006 already holds the same
  schema plus leftovers, and goose still remembers the six versions, so bring it
  in line once, by hand, after deploying this release (nothing here touches data):

  ```sql
  BEGIN;
  ALTER TABLE tenant DROP COLUMN billing;
  ALTER TABLE plan_entitlement DROP COLUMN regions, DROP COLUMN retain_mult;
  DROP TABLE zz_dead_tenant_line_ledger;
  DROP TABLE zz_dead_project_window;
  DELETE FROM goose_db_version WHERE version_id BETWEEN 2 AND 6;
  COMMIT;
  ```

  Without the last line the next migration this project ships (`002_…`) would
  count as "missing" against a version table that still says 6.

### Removed
- **Everything about money.** `billing_subscription` and `tenant.billing` are
  gone, and so is `billing` on `/v1/me`'s account: the core knows the plan word
  and nothing about how it is paid for. The hosted product's billing sidecar
  owns its own tables outside this schema.
- **The retired ring-retention tables** (`tenant_line_ledger`, `project_window`,
  renamed `zz_dead_*` since 0.12.0) and the unread `plan_entitlement.regions`
  and `retain_mult` columns. Nothing read them; nothing changes.
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
