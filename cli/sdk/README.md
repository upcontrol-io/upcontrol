# @upcontrol/sdk

The upcontrol push library. Zero dependencies. `track()` never throws, never
blocks, and without a key it is a warned no-op - a log point can never break
the code it observes.

```ts
import { track } from '@upcontrol/sdk';

track('payment_succeeded', { provider: 'stripe', currency: 'usd', livemode: true });
```

One-line automatic setup (`app_started`, `unhandled_exception`, flush on a
draining event loop) - first line of your entry point:

```ts
import '@upcontrol/sdk/auto';
```

Configuration is environment-only:

- `UPCONTROL_API_KEY` - the project key (`uc_live_...`). Belongs in `.env`,
  which must be gitignored. `npx upcontrol init` places it for you.
- `UPCONTROL_ENDPOINT` - optional, defaults to `https://upcontrol.io`.
- `UPCONTROL_SERVICE` - optional. A short name for the process that sends the
  line (`api`, `worker`, `front`); the dashboard's service column and filter
  read it. A `service` attribute passed to `track()` or `upcontrolLine()` wins
  over it. Unset, the line carries no service.
- `UPCONTROL_RELEASE` - optional. A commit sha or a build id, trimmed and cut to
  64 characters. The auto entry sends it as `release` on `app_started`, which
  names the deploy so the board can mark it on its charts. It is read before the
  platform's own variable (`VERCEL_GIT_COMMIT_SHA`, `RENDER_GIT_COMMIT`,
  `RAILWAY_GIT_COMMIT_SHA`, `HEROKU_SLUG_COMMIT`, `FLY_IMAGE_REF`,
  `K_REVISION`), so set it where the platform is none of those. Heroku sets
  `HEROKU_SLUG_COMMIT` only after `heroku labs:enable runtime-dyno-metadata`
  (off by default); without that flag, set `UPCONTROL_RELEASE` there too. With
  none of them set, `app_started` carries no `release`.

What it does on the wire: batches lines (1.5 s / 64 KB), keeps at most 8 MB in
memory (oldest lines are evicted WITH an explicit drop line - silent loss is
the one defect a monitoring tool may not have), retries with backoff and
byte-identical bodies (the server deduplicates, so a retry cannot double-
write), and scrubs known secret shapes (tokens, JWTs, card numbers, emails,
connection-string passwords, PEM blocks, cookies) before anything leaves the
process. The server scrubs again; the client is the primary layer.

## Funnels

Declare a journey once, then count a person at each step with one `step()` line.

```ts
import { funnel } from '@upcontrol/sdk';
export const visitToPaid = funnel('visit to paid', ['visit', 'signup', 'checkout', 'paid']);
```

```ts
app.use((req, res, next) => { visitToPaid.step('visit', req); next(); });
visitToPaid.step('signup', 'u_' + user.id);
```

A request is fingerprinted on your own server (a salted hash of the address and
the user-agent, with known crawlers skipped) so a raw address never leaves, and
a string id is your own identifier (a numeric one goes prefixed, see the next
section). Whichever it is rides the event as `uc.actor`. The address is the
framework's own `ip` first (behind a proxy, set its trust-proxy so that is the
client's), then the first `x-forwarded-for` hop, then the socket; a request with
none is not counted. Each `step()` line is one event named by the step, and the
server counts a person once per step from the events it stores - nothing is
deduped in the process, and the count stays honest anyway. An event carrying
`uc.actor` is all a sender needs, from any language.

## A/B tests, retention and breakdowns

Three more feeds on the same events: each call is one event, and the server
counts distinct people from the `uc.actor` on it.

```ts
import { experiment, retention, breakdown } from '@upcontrol/sdk';

// 2 to 8 arms, control first. A person counts once per arm per stat.
const cta = experiment('checkout CTA', ['control', 'B']);
cta.expose('B', 'u_' + user.id);
cta.convert('B', 'u_' + user.id);

// Weekly cohorts. The id must be your own stable user id, the same string
// every week for the same person: an address is not, a week later.
const signups = retention('signups');
signups.seen('u_' + user.id);

// Counts events; pass a who to count distinct people instead.
const pages = breakdown('page');
pages.value('/pricing');
pages.value('/pricing', 'u_' + user.id);
```

`expose` and `convert` take the same `who` a funnel step takes, and an A/B test
needs the SAME stable string id on both: the card's interval assumes the person
exposed is the person who converts, and a request hashes differently from a user
id. A numeric id goes prefixed, as above: every string the SDK sends is
scrubbed, `uc.actor` included, and a bare number of 13 to 19 digits that passes
the card check becomes `[redacted:card:N]`, which folds every such user into one
person. `seen()` takes a string id only. Never feed a breakdown something
unbounded like a request id or a URL with a query string.

Install and instrumentation are normally driven by your coding agent via
`npx upcontrol init` - see the [upcontrol package](https://www.npmjs.com/package/upcontrol).
