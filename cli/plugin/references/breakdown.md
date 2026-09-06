# "Add a breakdown" - a dimension ranked by how often each value happens

A breakdown is a dimension the user names: how often each value of it occurs.
It counts events, not people - it is a ranking of volume, not a count of
users. upcontrol receives each value's count and nothing else.

The request usually arrives as a sentence copied from the board's form:

> Add an UpControl breakdown "page" counting how often each page is viewed.

Read one thing out of it: the name of the dimension (the user's own words, 1
to 60 characters). Do not tidy it into something else - the board shows what
was declared. A declaration outside those limits is ignored whole, with one
line on stderr, so keep to them.

## Step 1 - declare it once

In a module the call sites can import (`src/breakdowns.ts`, or beside the app
entry point):

```ts
import { breakdown } from '@upcontrol/sdk';

export const pageViews = breakdown('page');
```

## Step 2 - value() where the thing happens

One line at the moment the event happens - for page views, the request
handler; for another dimension, wherever the value is known. `value()` takes
the value and nothing else: no request, no user id - events are not people.

```ts
pageViews.value('/pricing');
```

The value must be bounded. The SDK keeps at most 200 distinct values per
dimension; a new value past that is ignored, with one line on stderr. Never
feed it something unbounded - a request id, an email, a full URL with its
query string. Normalise before counting:

```ts
const path = new URL(req.url, 'http://localhost').pathname; // /checkout?plan=growth -> /checkout
pageViews.value(path);
```

## The rules

- One line. No wrappers, no control-flow changes, no `await` - `value()`
  never throws and never blocks.
- Events, not people: ten views by one person are ten. The board ranks
  volume; it does not count distinct people.
- Bounded values only: a normalised path, a country code, a plan name. Never
  a request id, an email, a full URL with a query string - the value names
  itself on the board, so keep personal data out of it.
- At most 200 distinct values per dimension; past that, new values are
  ignored with one line on stderr.
- The same name declared twice in one process is one breakdown: the first
  declaration wins.

## State and cadence

The counters run since the breakdown was first declared on that machine, and
they go out every minute as one metric reading per value, like the funnel, so
the board can slice any range. State is kept in memory and on disk under
`node_modules/.cache/upcontrol/`; point `UPCONTROL_STATE_DIR` at a path on a
volume when the deploy rebuilds `node_modules`. Without a key the breakdown
still counts locally and sends nothing, like the rest of the SDK.

## Verify

Ask the user to run the app. Within a minute the SDK sends the readings;
`npx upcontrol verify` proves the key, transport and scrubber as usual. The
board's breakdown card reads them once its live feed lands (the SDK is ahead
of the board here); until then the Dashboard draws sample data behind a
`Sample` badge, so do not send the user there for proof. Report what is
proven: the dimension is declared and its counters are on the wire.

## Report

```
+1 breakdown · staged for your review
areas: routes
```

The real dimension, the real areas.
