# "Add a funnel" - a journey counted step by step, once per person

A funnel is a journey the user names, plus its steps in order: how many people
reached each one. Each step() line is one event, and the server counts a person
once per step from the events it stores.

The request usually arrives as a sentence copied from the board's form:

> Add an UpControl funnel "visit to paid" with the steps visit, signup,
> checkout and paid, counting each person once per step.

Read two things out of it, in this order: the name (the user's own words, 1 to
60 characters) and the steps (2 to 12, each 1 to 40 characters, in journey
order, again the user's own words). Do not tidy either into something else - the
board shows what was declared. A declaration outside those limits is ignored
whole, with one line on stderr, so keep to them.

## Step 1 - declare the funnel once

In a module the call sites can import (`src/funnels.ts`, or beside the app
entry point):

```ts
import { funnel } from '@upcontrol/sdk';

export const visitToPaid = funnel('visit to paid', ['visit', 'signup', 'checkout', 'paid']);
```

## Step 2 - the anonymous step takes the request

The first step happens before anyone is signed in, so pass the request object
itself: the SDK reads the address and the user-agent off it.

Express - a middleware if every request counts, the page route if only one page
does:

```ts
app.use((req, res, next) => { visitToPaid.step('visit', req); next(); });
```

Fastify and Koa hand you the framework's request, which carries the client's
address once the app trusts its proxy (`trustProxy` in Fastify, `app.proxy` in
Koa; `trust proxy` in Express). Pass that, not the raw Node request:

```ts
fastify.addHook('onRequest', (request, reply, done) => { visitToPaid.step('visit', request); done(); });
app.use(async (ctx, next) => { visitToPaid.step('visit', ctx.request); await next(); });
```

Next.js: a route handler's `request`, or a server component with the request
headers (the root layout sees every page). The SDK needs Node (`node:crypto`),
so not `middleware.ts` unless it declares `runtime: 'nodejs'`:

```ts
export async function GET(request: Request) { visitToPaid.step('visit', request); /* existing handler stays */ }
```

```ts
import { headers } from 'next/headers';
visitToPaid.step('visit', { headers: await headers() }); // in the root layout, before its return
```

## Step 3 - the known-user steps take the id

One line each, where the moment truly happens: after the user exists, not at
form render, and inside the payment provider's success branch for the paid
step.

```ts
visitToPaid.step('signup', user.id);     // after the account row is created
visitToPaid.step('checkout', user.id);   // where checkout truly starts
visitToPaid.step('paid', userId);        // the payment success branch, not the redirect
```

## The rules

- One line per step. No wrappers, no control-flow changes, no `await` -
  `step()` never throws and never blocks.
- The request or the user id, never an email, never a session token.
- Steps go in journey order in the declaration. The declaration scopes and
  validates the calls in code; the board's card names the step events itself,
  and the card is what defines the journey.
- Keep it out of per-item loops - it belongs at the moment, not inside the
  work.
- What leaves the process is one event per step, the person on it as `uc.actor`.
  A request is fingerprinted on the user's own server (a salted hash of the
  address plus the user-agent: the framework's `ip` first, then the first
  `x-forwarded-for` hop, then the socket), so the raw address never leaves; a
  string id passes through unchanged as the caller's own identifier. A request
  with no address at all is not counted (one warning on stderr). Known crawler
  user-agents (bot, crawl, spider, slurp, headless) are not counted either.
- The events carry `uc.actor`, which is what lets the same numbers be produced
  from any language - see `npx upcontrol skills wire`.

## Verify

Ask the user to run the app. The step events go out with the next batch, and
`npx upcontrol verify` proves the key, transport and scrubber as usual. The
board's funnel card reads the same events, so the journey appears there as they
land; while a project has no live feed the Dashboard draws sample data behind a
`Sample` badge, so do not send the user there for proof. Report what is
proven: the funnel is declared and its events are on the wire.

## Report

```
+1 funnel · 4 steps · staged for your review
areas: middleware · auth · checkout
```

The real name, the real step count, the real areas.
