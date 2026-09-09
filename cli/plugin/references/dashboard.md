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

## The kinds

`w`/`h` are the sizes to use unless the user asks otherwise, `min` is the
smallest the board accepts, and a kind that is not ranged must carry no
`range`:

| kind | draws | w | h | min w/h | ranged | metrics | `source` each ref takes |
|---|---|---|---|---|---|---|---|
| `stat` | one number and its trend | 3 | 6 | 2 / 6 | yes | exactly one | `logs`, `check`, `event` or `metric` |
| `line` | series over time | 6 | 8 | 3 / 6 | yes | one or more | `logs`, `check`, `event` or `metric` |
| `bar` | counts per time bucket | 6 | 8 | 3 / 6 | yes | one or more | `logs`, `check`, `event` or `metric` |
| `donut` | how a total splits | 3 | 8 | 3 / 6 | yes | one or more | `logs`, `check`, `event` or `metric` |
| `logs` | the latest lines of one service | 6 | 16 | 4 / 12 | no | exactly one | `service` |
| `status` | uptime bars for the checks you pick | 6 | 12 | 3 / 6 | no | one or more | `check` |
| `network` | probe timings for one check | 6 | 12 | 3 / 6 | no | exactly one | `check` |
| `calendar` | daily volume of one metric | 6 | 10 | 2 / 8 | no | exactly one | `logs`, `check`, `event` or `metric` |
| `funnel` | a journey step by step | 6 | 12 | 3 / 8 | yes | exactly one | `people` |
| `experiment` | control against its variants | 6 | 8 | 3 / 6 | yes | exactly one | `people` |
| `retention` | weekly cohorts | 6 | 12 | 4 / 8 | no | exactly one | `people` |
| `breakdown` | the top of one dimension | 4 | 12 | 3 / 6 | yes | exactly one | `people` |

The last column is load-bearing and the validator does not check it: a card
whose ref carries the wrong source draws nothing at all, silently. A `logs`
card in particular reads its service from the ref's `name`, not from a
`where.service` filter.

`stat` and `donut` never exceed 6 columns: past that either is mostly card.

## What a widget draws - the ref

```json
{ "source": "...", "name": "...", "steps": ["..."], "cohort": "...", "field": "...", "where": { "...": "..." } }
```

`source` is one of `logs check event metric service people`. An optional
`label` names the pick on the card when the
catalog no longer lists it; it changes nothing about what is drawn. What
`name` and `where` mean, per source:

| source | `name` | `where` |
|---|---|---|
| `logs` | none | `service`; `level` (`info`, `warn`, `error`); `fingerprint`; `q`, a free-text match on the message; `attr.<key>` |
| `check` | `response` or `uptime` | `check`, the monitor's id |
| `event` | the event name you passed to `track()` | none |
| `metric` | the metric name | label equalities (a funnel step: `funnel`, `step`) |
| `service` | the service name | none |
| `people` | the event name, on a breakdown or an A/B test | none; `steps`, `cohort` and `field` pick the reading |

A `people` ref is the definition itself - the card is built from event names,
with nothing declared elsewhere to keep in sync. It counts distinct people: an
event counts only when it carries `uc.actor`. The shape of the ref picks the
reading:

| card | ref |
|---|---|
| `funnel` | `{ "source": "people", "steps": ["visit", "signup", "payment.succeeded"] }` - the step event names, in journey order, 2 to 12 |
| `retention` | `{ "source": "people", "cohort": "week" }` - the whole project; there is nothing to name |
| `breakdown` | `{ "source": "people", "name": "<event>", "field": "<a field of that event>" }` - `value`, as the SDK's `breakdown()` sends it |
| `experiment` | `{ "source": "people", "name": "<event>" }` - the arms come from `uc.variant` and `uc.stat` on that event's rows |

The board counts three levels, folding every other one into `info`; a `level`
outside the three is not a filter and the card draws every level.

**You may only reference what this project actually sends.** A widget bound to
an event name nobody emits draws nothing. The key reads the board but no
catalog, on purpose, so build the board out of what you just instrumented and
what `npx upcontrol board` shows you is already there - never out of a guess.
A check's id sits on the check cards already stored, in `where.check`.

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
  what is wrong (`widget "w_x" runs past the 12 columns`). Fix that and re-run
  rather than paraphrasing it at the user.

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
