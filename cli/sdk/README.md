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
- `UPCONTROL_STATE_DIR` - optional. Where a funnel keeps the people it has seen
  (default `node_modules/.cache/upcontrol/`). Point it at a volume when a deploy
  rebuilds `node_modules`.

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
the user-agent, with known crawlers skipped) and never sent, and a string id is
hashed the same way, so only the counts leave the process. The address is the
framework's own `ip` first (behind a proxy, set its trust-proxy so that is the
client's), then the first `x-forwarded-for` hop, then the socket; a request with
none is not counted. Each step counts a person once, and its value is a counter
since the funnel was first declared on that machine: it grows while the process
lives and picks up from its last save, at most a minute behind, after a restart.
The counts go out every minute as one metric reading per step, so the board can
show any range as a slice of that series. The people seen are kept in the state
dir, up to a million per step, and each process keeps its own, so a cluster of
workers counts a visitor once per worker. A filesystem that does not survive a
deploy starts the counts again, which is what `UPCONTROL_STATE_DIR` is for.

Install and instrumentation are normally driven by your coding agent via
`npx upcontrol init` - see the [upcontrol package](https://www.npmjs.com/package/upcontrol).
