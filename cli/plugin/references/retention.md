# "Add retention" - weekly cohorts, each person once a week

Retention is a name the user gives plus weekly cohorts: of the people first
seen in a week, how many were seen again in each week after. Each seen() line
is one event carrying the person as `uc.actor`; the server works out the
cohorts from the actor's first event.

The request usually arrives as a sentence copied from the board's form:

> Add UpControl retention "signups" counting each person once a week from the
> week they first appeared.

Read one thing out of it: the name (the user's own words, 1 to 60
characters). Do not tidy it into something else - the board shows what was
declared. A declaration outside those limits is ignored whole, with one line
on stderr, so keep to them.

## seen() takes the stable user id - the one rule

`seen()` takes a string user id and nothing else - never a request, never an
address. A cohort is observed over weeks, and an address is not the same
person a week later. The id must be the customer's own stable user id: the
same string every week for the same person. A per-request address, a session
id or a cookie that rotates produces cohorts that fall apart and a churn
number nobody experienced. If the app has no stable id to give, say so - do
not substitute one.

## Step 1 - declare it once

In a module the call sites can import (`src/retention.ts`, or beside the app
entry point):

```ts
import { retention } from '@upcontrol/sdk';

export const signups = retention('signups');
```

## Step 2 - seen() where the app knows who it is talking to

One line, after auth or in the session middleware: the place the app already
knows which person the request belongs to. Not at the front door - an
anonymous hit has no id to give.

Express:

```ts
app.use((req, res, next) => { if (req.user) signups.seen(String(req.user.id)); next(); });
```

Fastify and Koa:

```ts
fastify.addHook('onRequest', (request, reply, done) => { if (request.user) signups.seen(String(request.user.id)); done(); });
app.use(async (ctx, next) => { if (ctx.state.user) signups.seen(String(ctx.state.user.id)); await next(); });
```

Next.js - in the root layout, once the session is read (it sees every page).
The SDK needs Node (`node:crypto`), so not `middleware.ts` unless it
declares `runtime: 'nodejs'`:

```ts
const session = await auth();
if (session?.user) signups.seen(String(session.user.id)); // in the root layout, before its return
```

## The rules

- One line. No wrappers, no control-flow changes, no `await` - `seen()`
  never throws and never blocks.
- The stable user id, never an email, never a session token, never a request
  object.
- A person counts once per ISO week (Monday). The week they were first seen
  starts their cohort; every later week they are seen again counts them once
  in that week.
- The events carry `uc.actor`, which is what lets the same numbers be produced
  from any language - see `npx upcontrol skills wire`.

## Verify

Ask the user to run the app and sign in. The events go out with the next
batch, and `npx upcontrol verify` proves the key, transport and scrubber as
usual. The board's retention card reads the same events, so the cohorts
appear there as they land; while a project has no live feed the Dashboard
draws sample data behind a `Sample` badge, so do not send the user there for
proof. Report what is proven: the cohort is declared and its events are on
the wire.

## Report

```
+1 retention cohort · staged for your review
areas: auth · session
```

The real name, the real areas.
