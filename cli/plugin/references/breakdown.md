# "Add a breakdown" - a dimension ranked by how often each value happens

A breakdown is a dimension the user names: how often each value of it occurs.
By default it counts events, not people - it is a ranking of volume, not a
count of users. Passing a `who` to value() counts distinct people instead: each
call is one event, and the server does the counting.

The request usually arrives as a sentence copied from the board's form:

> Add an UpControl breakdown "page" counting how often each page is viewed.

Read one thing out of it: the name of the dimension (the user's own words, 1
to 60 characters). Do not tidy it into something else - the name becomes the
event name on the wire, and the card folds that event by one of its own
fields - `value`, as the SDK sends it. A declaration outside those limits is
ignored whole, with one line on stderr, so keep to them.

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
the value, and an optional `who` to count distinct people instead of events.

```ts
pageViews.value('/pricing');
```

The value must be bounded. Never feed it something unbounded - a request id,
an email, a full URL with its query string. Normalise before counting:

```ts
const path = new URL(req.url, 'http://localhost').pathname; // /checkout?plan=growth -> /checkout
pageViews.value(path);
```

## The rules

- One line. No wrappers, no control-flow changes, no `await` - `value()`
  never throws and never blocks.
- Events, not people, unless the call passes a `who`: ten views by one
  person are ten, and with a `who` they are one. The board's card counts
  people either way - pass the `who` or it has no one to count.
- Bounded values only: a normalised path, a country code, a plan name. Never
  a request id, an email, a full URL with a query string - the value names
  itself on the board, so keep personal data out of it.
- A `who` rides the event as `uc.actor`, which is what lets the same numbers
  be produced from any language - see `npx upcontrol skills wire`.

## Verify

Ask the user to run the app. The events go out with the next batch, and
`npx upcontrol verify` proves the key, transport and scrubber as usual. The
board's breakdown card reads the same events, so the ranking appears there as
they land; while a project has no live feed the Dashboard draws sample data
behind a `Sample` badge, so do not send the user there for proof. Report what
is proven: the dimension is declared and its events are on the wire.

## Report

```
+1 breakdown · staged for your review
areas: routes
```

The real dimension, the real areas.
