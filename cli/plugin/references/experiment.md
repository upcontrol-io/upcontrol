# "Add an A/B test" - arms counted once per person, exposure and conversion

An A/B test is a name the user gives plus its arms: how many people were
exposed to each arm and how many of those converted. Each expose() and
convert() line is one event; the server counts a person once per arm per stat
from the events it stores.

The request usually arrives as a sentence copied from the board's form:

> Add an UpControl A/B test "checkout CTA" with a control and a variant B,
> counting exposures and conversions to paid once per person.

Read two things out of it, in this order: the name (the user's own words, 1 to
60 characters) and the arms (2 to 8, each 1 to 40 characters, control first,
again the user's own words). Do not tidy either into something else - the
name becomes the event name on the wire, and the arms ride it as `uc.variant`.
A declaration outside those limits is ignored
whole, with one line on stderr, so keep to them.

## Step 1 - declare the test once

In a module the call sites can import (`src/experiments.ts`, or beside the app
entry point):

```ts
import { experiment } from '@upcontrol/sdk';

export const checkoutCta = experiment('checkout CTA', ['control', 'B']);
```

## Step 2 - expose() where the arm is chosen

The SDK does not assign arms. The app's own code decides who lands in which
arm (whatever it already runs - a cookie, a flag library, a config);
`expose()` counts the person against the arm that was chosen. If nothing in
the app chooses an arm, say so: the SDK will not invent the assignment, and
without one the test measures nothing.

**Pass the person's own stable id, and pass the same one to `convert()`.** The
card is a rate: the people who converted out of the people who were exposed.
That means something only when both calls name the same person with the same
string - the signed-in user's id, or the anonymous id the app already keeps for
a visitor (the one in its cookie or localStorage). Not a request: a request is
fingerprinted with a salt that is new in every server process, so the same
visitor is a different person after a restart, on a second instance, and in the
payment webhook that carries the conversion. A request on `expose()` and a user
id on `convert()` is the worst mix: the two counts describe different people and
the card has nothing to divide. If the app keeps no id for the people in this
test, say so rather than inventing one or falling back to the request.

One line at the branch where the arm is chosen - usually one route, not a
global middleware:

```ts
checkoutCta.expose(arm, 'u_' + user.id); // arm = what the app's own assignment chose; a signed-in user, id prefixed (see the rules)
checkoutCta.expose(arm, visitorId);      // a visitor: the anonymous id the app already keeps
```

## Step 3 - convert() where the conversion happens

The board sentence names the conversion ("to paid"); put the line where that
moment truly happens - inside the payment provider's success branch, not the
redirect. Pass the arm the person was exposed to, read from wherever the
assignment lives, and the same id `expose()` got:

```ts
checkoutCta.convert(arm, 'u_' + userId);  // the payment success branch: the arm they saw, the id they were exposed under
```

A webhook has no browser: read the arm and the id from what the payment is tied
to (the order, the customer, the checkout session's metadata). If the id cannot
be recovered there, say so rather than passing something else.

## The rules

- One line per call. No wrappers, no control-flow changes, no `await` -
  `expose()` and `convert()` never throw and never block.
- The user's id, or the anonymous id the app keeps - never an email, never a
  session token, never a request. It is a string, and a numeric id goes prefixed
  (`'u_' + user.id`, the same on both calls): the SDK scrubs every string it
  sends, `uc.actor` included, and a bare number of 13 to 19 digits that passes
  the card check becomes `[redacted:card:N]`, which folds every such user into
  one person. A hash of the id works as well when the user would rather upcontrol
  never held the real one, as long as both calls hash it the same way. An empty
  or missing id sends nothing.
- Control is first in the declaration. The board's card orders the arms by
  people, putting a variant literally named `control` first, and compares the
  other arms with that one: with no arm named exactly `control`, nothing is
  compared.
- The server counts a person once per arm per stat, so a reload does not
  inflate the rate and a second conversion does not move it. Keep the calls at
  the moment anyway, out of per-item loops.
- What leaves the process is one event per call, the id on it as `uc.actor`.
- The events carry `uc.actor`, which is what lets the same numbers be produced
  from any language - see `npx upcontrol skills wire`.

## What the card claims

Read this before promising the user anything about a result.

- **A difference, against `control`.** Each other arm shows its rate minus the
  control's, in percentage points: `+2.4 pt vs control`. No arm named exactly
  `control` means no comparison at all, only counts and rates.
- **An interval and a verdict, once there are enough people.** When every arm,
  control included, has at least 30 people exposed, at least 5 who converted and
  at least 5 who did not, each of the other arms also shows a 95% interval for
  its difference (Wald's, for two proportions) and one verdict in words:
  - `ahead of control` - the whole interval is above zero;
  - `behind control` - the whole interval is below zero;
  - `not clear yet, keep collecting` - the interval spans zero.

  A footnote says what the interval assumes:
  `Assumes the same person is exposed and converts.`
- **Below that floor**, each of the other arms says `too few to tell` and no
  interval is drawn. One arm under the floor is enough: the interval needs every
  arm.
- **When expose() and convert() named different people** - any arm with more
  people converted than exposed - the card shows the difference alone and says
  there is no interval. The cause is the id: see step 2.

The card never says `significant`. Tell the user what it says, in its words;
do not promise significance, a winner or a sample size, and do not report a
result while an arm is still under the floor.

## Verify

Ask the user to run the app. The events go out with the next batch, and
`npx upcontrol verify` proves the key, transport and scrubber as usual. The
board's A/B card reads the same events, so the arms appear there as they land;
while a project has no live feed the Dashboard draws sample data behind a
`Sample` badge, so do not send the user there for proof. Report what is
proven: the test is declared and its events are on the wire.

## Report

```
+1 A/B test · 2 arms · staged for your review
areas: checkout · billing
```

The real name, the real arm count, the real areas.
