# Dashboard - the board as one document, read and written by `npx upcontrol board`

The board is the project's dashboard page: cards on a grid the user can also
edit by hand in the app. This topic is how you read it, compose a layout and
land it without disturbing what the user arranged.

## The three commands

```
npx upcontrol board                    # the current board, as JSON
npx upcontrol board --apply <file|->   # replace the board
npx upcontrol board --add <file|->     # append widgets to it
```

"Build me a dashboard" and "reorganize it" are `--apply`: you compose the whole
board. "Add the metrics we just instrumented" is `--add`. Prefer `--add`
whenever the request is additive - it lands its block below what is already
there and never disturbs what the user arranged, while `--apply` overwrites the
layout whole.

## Several boards

A project can hold more than one board, as many as its plan carries, and each
command above can name which one it means:

```
npx upcontrol board --list                     # every board: id, name, frozen, widgets
npx upcontrol board --board <id> --add <file>  # the three commands, on one board
npx upcontrol board --new "Payments"           # a new board (--apply gives it a layout)
```

Read `--list` first, every time you are about to write. It answers
`{ "boards": [...], "max": 3 }`: `max` is how many boards this plan carries per
project, absent when the plan is unlimited; `frozen` on a board means the plan
stopped carrying it, so it is kept whole but refuses every read and write until
the plan carries it again; `proposed` means a layout is already waiting there
for the user. Without `--board`, every command means the project's first board:
the first entry of `--list` (its oldest), whatever it is called now.

Name a board by its id rather than its name: the alias `main` outranks a board
actually named `main`, and a rename leaves the id alone.

Create a board only for an audience or a topic the user named ("a board for the
payments team", "one for the website"), and add to an existing board otherwise.
Never create a board to get around a proposal: a 202 means the user curates that
board, and a second board nobody asked for hides your cards instead of showing
them. Past the plan's count the create comes back as a refusal whose sentence
names what lifts it - give the user that sentence as it is, and do not retry.

## The document

The board is one JSON object:

```json
{ "version": 2, "widgets": [ ... ] }
```

`version` is always `2` when you write. Every widget:

| field | type | meaning |
|---|---|---|
| `id` | string | unique on the board, opaque, yours to invent (`w_errors`) |
| `kind` | enum | which card, from the table below |
| `title` | string | the heading the reader sees |
| `metrics` | array | what it draws, see the ref shape below |
| `range` | enum, optional | only for kinds with a time axis: `1h 4h 12h 24h 7d 31d` |
| `x`, `y` | integer | grid position, from 0; `x + w` never exceeds 12 |
| `w`, `h` | integer | size in grid cells |

The grid is 12 columns wide and unbounded downward. `h` counts half-rows: a
card of height H draws `26H - 12` pixels, so `h: 6` is a short card and
`h: 16` a tall one. Widgets must not overlap - the board renders the grid
literally - so lay them out in reading order, left to right and then down. The
whole document stays under 64 KB, far more than a screenful of cards needs.

The server decodes the document strictly: a field on a widget or on a ref that
the tables in this topic do not list is refused by name, not ignored, and so is
a `count` other than `people` or `events`.

## The kinds

`w`/`h` are the sizes to use unless the user asks otherwise, `min` is the
smallest the board accepts, and a kind that is not ranged must carry no
`range`:

| kind | draws | w | h | min w/h | ranged | metrics | `source` each ref takes |
|---|---|---|---|---|---|---|---|
| `stat` | one number and its trend | 3 | 6 | 2 / 6 | yes | exactly one | `logs`, `check`, `event`, `metric`, `web` or a bare-name `people` |
| `line` | series over time | 6 | 8 | 3 / 6 | yes | one to three | `logs`, `check`, `event`, `metric`, `web` or a bare-name `people` |
| `bar` | counts per time bucket | 6 | 8 | 3 / 6 | yes | one to three | `logs`, `check`, `event`, `metric`, `web` or a bare-name `people` |
| `donut` | how a total splits | 3 | 8 | 3 / 6 | yes | one or more | `logs`, `check`, `event` or `metric` |
| `logs` | the latest lines of one service | 6 | 16 | 4 / 12 | no | exactly one | `service` |
| `status` | uptime bars for the checks you pick | 6 | 12 | 3 / 6 | no | one or more | `check` |
| `network` | probe timings for one check | 6 | 12 | 3 / 6 | no | exactly one | `check` |
| `calendar` | daily volume of one metric | 6 | 10 | 2 / 8 | no | exactly one | `logs`, `check`, `event` or `metric` |
| `funnel` | a journey step by step | 6 | 12 | 3 / 8 | yes | exactly one | `people` |
| `experiment` | control against its variants | 6 | 8 | 3 / 6 | yes | exactly one | `people` |
| `retention` | weekly cohorts | 6 | 12 | 4 / 8 | no | exactly one | `people` |
| `breakdown` | the top of one dimension | 4 | 12 | 3 / 6 | yes | exactly one | `people` |
| `heatmap` | top pages, a Heatmap button per row | 4 | 12 | 3 / 6 | yes | exactly one | `people` |
| `ratio` | one count as a share of another | 3 | 8 | 3 / 6 | yes | exactly two, in order | `logs`, `event` or a bare-name `people` |
| `table` | one event's values by a field: events, people, share, and revenue when it carries money | 6 | 12 | 3 / 9 | yes | exactly one | `people` |

The last column is load-bearing and the validator does not check it: a card
whose ref carries the wrong source draws nothing at all, silently. A `logs`
card in particular reads its service from the ref's `name`, not from a
`where.service` filter.

`stat`, `donut` and `ratio` never exceed 6 columns: past that each is mostly
card.

A `line` or a `bar` draws its first three refs and prints `3 of N shown` for the
rest, so write at most three; a `donut` draws every ref it is given as a slice.
A bare-name `people` ref is a `people` ref with only a `name`, the unique-people
count described below.

## What a widget draws - the ref

```json
{ "source": "...", "name": "...", "steps": ["..."], "cohort": "...", "field": "...", "count": "...", "where": { "...": "..." } }
```

`source` is one of `logs check event metric service people web`. An optional
`label` names the pick on the card, as its legend entry, in place of the name
the board would work out; it changes nothing about what is drawn. What
`name` and `where` mean, per source:

| source | `name` | `where` |
|---|---|---|
| `logs` | none | `service`; `level` (`info`, `warn`, `error`); `fingerprint`; `q`, a free-text match on the message; `attr.<key>` |
| `check` | `response` (the average), `uptime`, `p50` or `p95` | `check`, the monitor's id |
| `event` | the event name you passed to `track()`; with `"measure": "amount"` and a `"currency"` (`USD`) it sums the money the events carried instead of counting them (topic `wire`) | the event's own fields, as text (below) |
| `metric` | the metric name | label equalities (a funnel step: `funnel`, `step`) |
| `service` | the service name | none |
| `people` | the event name, on a breakdown, an A/B test or the `heatmap` card, optionally on a retention grid, or alone for a unique count | the event's own fields, on a `name` alone (below); none on the rest. `steps`, `cohort` and `field` pick the reading, and `count` picks what a breakdown counts |
| `web` | `engaged` (average engaged seconds per page view) or `reach` (the percent of page views that stayed engaged that long), on `stat`, `line` or `bar` only, and only when the site tag is installed | `path`, `device`, `segment` (a name the site marks visitors with) and `source` (`search`, `social`, `campaign`, `referral`, `direct`) narrow either; `reach` needs `after`: `10`, `30`, `120` or `300` |

A `people` ref is the definition itself - the card is built from event names,
with nothing declared elsewhere to keep in sync. It counts distinct people: an
event counts only when it carries `uc.actor`. The shape of the ref picks the
reading:

| card | ref |
|---|---|
| `funnel` | `{ "source": "people", "steps": ["visit", "signup", "payment.succeeded"] }` - the step event names, in journey order, 2 to 12. A step can be one page: `{ "source": "people", "steps": ["uc.pageview", "cta_click", "uc.pageview"], "stepWhere": [{ "path": "/pricing" }, {}, { "path": "/signup" }] }` - one object per step, an empty object for none, at most 3 fields each |
| `retention` | `{ "source": "people", "cohort": "week" }` - every event the project sent. `name` is optional: `{ "source": "people", "cohort": "week", "name": "<event>" }` counts only the people of that one event, the name `retention()` was given. Their first such event is the cohort, and the same event in a later week a return. A name starting `uc.` reads nothing |
| `breakdown` | `{ "source": "people", "name": "<event>", "field": "<a field of that event>" }` - `value`, as the SDK's `breakdown()` sends it. A `where` of that event's fields narrows the rows: `{ "source": "people", "name": "uc.pageview", "field": "from", "where": { "path": "/pricing" } }` is where visitors of /pricing came from, and `{ "source": "people", "name": "uc.pageview", "field": "path", "where": { "from": "/pricing" } }` where they went next. Add `"count": "events"` when nobody is behind the dimension - a delivery outcome, an HTTP status - or the card counts distinct people and reads 0. |
| `experiment` | `{ "source": "people", "name": "<event>" }` - the arms come from `uc.variant` and `uc.stat` on that event's rows; the card compares each arm with the one named `control`, and what it claims is topic `experiment` |
| `heatmap` | `{ "source": "people", "name": "uc.pageview", "field": "path", "count": "events" }` - fixed: the web script's page views, nothing to pick |
| `table` | `{ "source": "people", "name": "payment", "field": "plan", "measure": "amount", "currency": "USD" }` - a breakdown's ref; `measure` and `currency` add the Revenue column (money the event carried, topic `wire`), a `where` narrows the rows |

The board counts three levels, folding every other one into `info`; a `level`
outside the three is not a filter and the card draws every level.

**You may only reference what this project actually sends.** A widget bound to
an event name nobody emits draws nothing. The key reads the board but no
catalog, on purpose, so build the board out of what you just instrumented and
what `npx upcontrol board` shows you is already there - never out of a guess.
A check's id sits on the check cards already stored, in `where.check`.

### An event's own fields

An event ref's `where` names the event's own labels, so one event can be cut by
what it carried. This ref counts only the signups that carried `plan` = `pro`:

```json
{ "source": "event", "name": "signup", "where": { "plan": "pro" } }
```

The labels are the attributes the `track()` call (or the wire line) sent, except
the keys the decoder takes as the line's own fields: `level`, `severity`, `host`,
`hostname`, `ts`, `time`, `timestamp`, `@timestamp`, `msg`, `message`, `service`
and `svc`. A dimension that goes by one of those names is not a label, so a
filter on it matches nothing: send it under another name (`tier` for `level`,
`site` for `host`). Only the sender's service comes back: the core adds it to the
event's labels as `service`, whichever of `service`, `svc` or `UPCONTROL_SERVICE`
named it (an event stored before the core began to add that one has none). Take
the label names and values from the `track()` calls you just read - a filter on
a value nobody sends draws zeros.

- Every pair must match. A value is compared as text, exactly, case included,
  and a `where` value is always a string: write a number or a boolean as its
  text (`"42"`, `"true"`).
- At most 3 pairs, a key of 1 to 60 bytes, a value of at most 256 bytes, and no
  control character in either. A `where` past those bounds leaves the card a
  Sample: the board draws sample data behind a `Sample` badge instead of the
  project's own.
- The same `where` narrows a bare-name `people` ref (next). It is not read yet
  on a funnel, a breakdown, the `heatmap`, an A/B test or a retention grid: the
  card stays a Sample there, so leave it off.

### Unique people

An `event` ref counts rows, every time the event was sent. A `people` ref with
only a `name` counts the DISTINCT people behind the event instead (its
`uc.actor`):

```json
{ "source": "people", "name": "signup" }
```

It is a count per bucket on a line or a bar, and one count over the whole window
on a stat or a ratio, so a person who signed up on two days is one person, not
two. An event sent without `uc.actor` is a row and never a person. The legend
adds `· people`. Read it on a `stat`, a `line`, a `bar` or a `ratio`; a `donut`
or a `calendar` does not read it and stays a Sample. Give it a `where` to count
the unique people who did the event with a given label. On an `experiment` card
the same ref is read as its arms.

### A check's percentiles

A `check` ref named `p50` or `p95` draws the median or the 95th percentile of
that check's response time, in ms:

```json
{ "source": "check", "name": "p95", "where": { "check": "<the monitor's id>" } }
```

95 percent of the successful probes answered at least that fast: the slow tail
an average hides. Both read the successful probes only, and a bucket with none
has no reading. `response` stays the average.

### Splitting an event: up to three filtered refs

A line or a bar draws up to three refs, so one event cut by a field is up to
three refs of that event, each with its own `where`:

```json
{
  "id": "w_signups_by_plan", "kind": "bar", "title": "Signups by plan",
  "metrics": [
    { "source": "event", "name": "signup", "where": { "plan": "free" } },
    { "source": "event", "name": "signup", "where": { "plan": "pro" } },
    { "source": "event", "name": "signup", "where": { "plan": "team" } }
  ],
  "range": "7d", "x": 0, "y": 0, "w": 6, "h": 8
}
```

The legend names each ref by its filter (`signup · plan = pro`) unless the ref
has a `label`. For a field with more than three values, a `breakdown` of that
field ranks the top ones instead.

## A share - the `ratio` card

A `ratio` is one count as a share of another: checkouts completed out of
checkouts started, error lines out of all lines, pro signups out of all signups.
It takes exactly two refs, **in this order: the count first, then what it is out
of**, and each must be a count - a `logs` ref (lines), an `event` ref (rows) or a
bare-name `people` ref (distinct people), narrowed by a `where` as above. Any
other ref leaves the card a Sample. The server refuses a stored ratio that does
not hold exactly two refs, with the sentence:

`widget "w_x": a ratio reads exactly two refs, a count and what it is out of`

Both refs count the same unit: people out of people, or rows out of rows (a
`logs` count and an `event` count are both rows). People out of rows, or rows
out of people, is not a share of anything, and a mixed pair leaves the card a
Sample.

```json
{
  "id": "w_checkout_rate", "kind": "ratio", "title": "Checkouts completed",
  "metrics": [
    { "source": "event", "name": "checkout_completed" },
    { "source": "event", "name": "checkout_started" }
  ],
  "range": "7d", "x": 0, "y": 0, "w": 3, "h": 8
}
```

The card prints the share of the two window totals with the fraction it comes
from, a sparkline of the share per bucket and its change in points against the
previous window; when the second total is 0 it says `no denominator in this
range` instead of a number. It does not check that the first count is a part of
the second, and a share past 100% is printed as it is, so pick a pair where it
is. Other pairs that work: two bare-name `people` refs (unique signups out of
unique visitors), or two `logs` refs of one `service`, the first with a `level`
of `error` and the second with none, which counts every level (the share of a
service's lines that are errors).

## Markers - drawn beside a line without being asked

A `line` or a `bar` marks what changed on its own clock - a dashed line for a
deploy or a release, a shaded span for an incident - so a reading that moved
shows the change next to it. There is nothing to write in the layout: markers
are a view the reader can switch off (`?marks=off`), never a field of a card, and
the board's bucket (by hour or by day, on a week or a month) is the same kind of
view. What feeds them is on the sending side:

- **Deploys.** An event named `deploy` (or any name that starts with `deploy` or
  contains `deployment`, with no whitespace in it) that carries the commit as
  `sha` - `track('deploy', { sha })`, or the same line from a pipeline (topic
  `wire`); `commit_sha` is read as `sha`. The mark is labelled by the first 7
  characters of the sha, else by the event's name, and deploy events less than 10
  minutes after the one before draw as a single deploy.
- **Releases.** The first time an `app_started` event carries a `release` not
  seen before, else a `version` not seen before (a `version` of `0` names
  nothing). The SDK, from 1.1.0, sends `release` itself: `UPCONTROL_RELEASE` if
  set, else the variable Vercel, Render, Railway, Fly and Cloud Run set for the
  running deploy, or Heroku's once `heroku labs:enable runtime-dyno-metadata` is
  on (it is off by default). On any other platform, and on Heroku without that
  flag, setting `UPCONTROL_RELEASE` to the commit sha or the build id where the
  app is deployed is the whole fix.
- **Incidents.** The project's incidents, from when each was detected until it
  was resolved.

A project that sends neither deploys nor releases draws no deploy or release
markers, and no card can change that: offer the line that sends them when the
user asks why a chart moved.

## The answer's shape

Be exact, because this is the half that fails silently:

- write the layout to a file, or pipe it: `npx upcontrol board --apply
  ./board.json`, or `cat board.json | npx upcontrol board --apply -`;
- `--add` takes the same widget objects wrapped as `{ "widgets": [ ... ] }` (a
  bare array works too), and the server places the block below whatever is
  already on the board, so the `y` you give is relative to your own block and
  never has to be looked up;
- the command prints what happened. `stored - N widgets.` means the board
  changed. The proposal lines mean the user had edited this board in the app,
  so your layout is waiting there behind one click - the command names the page
  and the button; tell the user to open the board and press Review, and do not
  treat it as a failure;
- a refused layout comes back on stderr with the server's own sentence naming
  what is wrong (`widget "w_x" runs past the 12 columns`), or `board: unknown
  field "colour"` for a field the tables in this topic do not list. Fix that and
  re-run rather than paraphrasing it at the user.

## A worked example

For a project that sends the service `api`, the events `signup`,
`checkout_started` (each row carrying `uc.variant` and `uc.stat`),
`checkout_completed`, and the event `page`, which carries its dimension value
on the `value` field:

```json
{
  "version": 2,
  "widgets": [
    {
      "id": "w_api_errors", "kind": "bar", "title": "API errors",
      "metrics": [{ "source": "logs", "where": { "service": "api", "level": "error" } }],
      "range": "24h", "x": 0, "y": 0, "w": 6, "h": 8
    },
    {
      "id": "w_checkout", "kind": "line", "title": "Checkout",
      "metrics": [
        { "source": "event", "name": "checkout_started" },
        { "source": "event", "name": "checkout_completed" }
      ],
      "range": "24h", "x": 6, "y": 0, "w": 6, "h": 8
    },
    {
      "id": "w_signups", "kind": "stat", "title": "Signups",
      "metrics": [{ "source": "event", "name": "signup" }],
      "range": "24h", "x": 0, "y": 8, "w": 3, "h": 6
    },
    {
      "id": "w_api_logs", "kind": "logs", "title": "API logs",
      "metrics": [{ "source": "service", "name": "api" }],
      "x": 3, "y": 8, "w": 6, "h": 16
    },
    {
      "id": "w_cta", "kind": "experiment", "title": "Checkout CTA",
      "metrics": [{ "source": "people", "name": "checkout_started" }],
      "range": "24h", "x": 0, "y": 24, "w": 6, "h": 8
    },
    {
      "id": "w_pages", "kind": "breakdown", "title": "Top pages",
      "metrics": [{ "source": "people", "name": "page", "field": "value" }],
      "range": "24h", "x": 6, "y": 24, "w": 4, "h": 12
    }
  ]
}
```

Two cards fill the first row side by side (6 + 6 = 12 columns), the stat and
the log tail the next (the log tail is `h: 16`, so the row under it starts at
`y: 24`), and the two `people` cards fill that one. To land these under what
the board already holds is `npx upcontrol board --add ./board.json`; to make
them the whole board is `--apply`.
