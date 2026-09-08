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
visitToPaid.step('signup', user.id);
```

A request is fingerprinted on your own server (a salted hash of the address and
the user-agent, with known crawlers skipped) so a raw address never leaves, and
a string id passes through unchanged. Whichever it is rides the event as
`uc.actor`. The address is the framework's own `ip` first (behind a proxy, set
its trust-proxy so that is the client's), then the first `x-forwarded-for` hop,
then the socket; a request with none is not counted. Each `step()` line is one
event named by the step, and the server counts a person once per step from the
events it stores - nothing is deduped in the process, and the count stays
honest anyway. An event carrying `uc.actor` is all a sender needs, from any
language.

## A/B tests, retention and breakdowns

Three more feeds on the same events: each call is one event, and the server
counts distinct people from the `uc.actor` on it.

```ts
import { experiment, retention, breakdown } from '@upcontrol/sdk';

// 2 to 8 arms, control first. A person counts once per arm per stat.
const cta = experiment('checkout CTA', ['control', 'B']);
cta.expose('B', req);
cta.convert('B', req);

// Weekly cohorts. The id must be your own stable user id, the same string
// every week for the same person: an address is not, a week later.
const signups = retention('signups');
signups.seen(user.id);

// Counts events; pass a who to count distinct people instead.
const pages = breakdown('page');
pages.value('/pricing');
pages.value('/pricing', user.id);
```

`expose` and `convert` take the same `who` a funnel step takes, a request or a
string id. `seen()` takes a string id only. Never feed a breakdown something
unbounded like a request id or a URL with a query string.

Install and instrumentation are normally driven by your coding agent via
`npx upcontrol init` - see the [upcontrol package](https://www.npmjs.com/package/upcontrol).
