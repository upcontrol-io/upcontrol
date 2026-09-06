# "Add an A/B test" - arms counted once per person, exposure and conversion

An A/B test is a name the user gives plus its arms: how many people were
exposed to each arm and how many of those converted. The SDK counts a person
once per arm per stat on the user's own server; upcontrol receives counts and
nothing else.

The request usually arrives as a sentence copied from the board's form:

> Add an UpControl A/B test "checkout CTA" with a control and a variant B,
> counting exposures and conversions to paid once per person.

Read two things out of it, in this order: the name (the user's own words, 1 to
60 characters) and the arms (2 to 8, each 1 to 40 characters, control first,
again the user's own words). Do not tidy either into something else - the
board shows what was declared. A declaration outside those limits is ignored
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

One line at the branch where the arm is chosen - usually one route, not a
global middleware. `expose()` takes the same `who` a funnel step takes.

Express:

```ts
app.get('/checkout', (req, res, next) => { checkoutCta.expose(arm, req); next(); }); // arm = what the app's own assignment chose
```

Fastify and Koa hand you the framework's request, which carries the client's
address once the app trusts its proxy (`trustProxy` in Fastify, `app.proxy` in
Koa; `trust proxy` in Express). Pass that, not the raw Node request:

```ts
fastify.get('/checkout', (request, reply) => { checkoutCta.expose(arm, request); /* existing handler stays */ });
app.use(async (ctx, next) => { checkoutCta.expose(arm, ctx.request); await next(); });
```

Next.js: a route handler's `request`, or the page or layout that renders the
variant with the request headers. The SDK needs Node (`node:fs`,
`node:crypto`), so not `middleware.ts` unless it declares `runtime: 'nodejs'`:

```ts
export async function GET(request: Request) { checkoutCta.expose(arm, request); /* existing handler stays */ }
```

```ts
import { headers } from 'next/headers';
checkoutCta.expose(arm, { headers: await headers() }); // in the page that renders the variant, before its return
```

## Step 3 - convert() where the conversion happens

The board sentence names the conversion ("to paid"); put the line where that
moment truly happens - inside the payment provider's success branch, not the
redirect. Pass the arm the person was exposed to, read from wherever the
assignment lives, and the same kind of `who`:

```ts
checkoutCta.convert(arm, userId);  // the payment success branch, the arm they saw
```

## The rules

- One line per call. No wrappers, no control-flow changes, no `await` -
  `expose()` and `convert()` never throw and never block.
- The request or the user id, never an email, never a session token.
- Control is first in the declaration; the board draws the arms in that order.
- The same name declared twice in one process is one test: the first
  declaration wins.
- A person counts once per arm per stat, so a reload does not inflate the
  rate and a second conversion does not move it. Keep the calls at the moment
  anyway, out of per-item loops.
- The card reports counts and an uplift, and claims nothing more: no
  significance testing, no confidence intervals. Do not promise those to the
  user.
- What leaves the process is counts. A request is fingerprinted on the user's
  own server (the same salted hash of address plus user-agent a funnel uses),
  a string id is hashed with the same salt, and neither the hash nor its
  input is ever sent. A request with no address at all is not counted (one
  warning on stderr), and known crawler user-agents are not counted either.

## State and cadence

Each arm's exposure and conversion counters run since the test was first
declared on that machine: there is no window, they pick up from their last
save, at most a minute behind, after a restart, and they go out every minute
as one metric reading per arm per stat, so the board can slice any range. The
people already counted are kept in memory and on disk under
`node_modules/.cache/upcontrol/`; point `UPCONTROL_STATE_DIR` at a path on a
volume when the deploy rebuilds `node_modules`, otherwise the counts start
again after such a deploy. Each process counts its own people, so an app in
cluster mode counts a person once per worker; say so to the user if you see a
cluster. Without a key the test still counts locally and sends nothing, like
the rest of the SDK.

## Verify

Ask the user to run the app. Within a minute the SDK sends one reading per arm
per stat; `npx upcontrol verify` proves the key, transport and scrubber as
usual, and the readings arrive with the next batch. The board's A/B card reads
them once its live feed lands (the SDK is ahead of the board here); until then
the Dashboard draws sample data behind a `Sample` badge, so do not send the
user there for proof. Report what is proven: the test is declared and its
counters are on the wire.

## Report

```
+1 A/B test · 2 arms · staged for your review
areas: checkout · billing
```

The real name, the real arm count, the real areas.
