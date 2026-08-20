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

What it does on the wire: batches lines (1.5 s / 64 KB), keeps at most 8 MB in
memory (oldest lines are evicted WITH an explicit drop line - silent loss is
the one defect a monitoring tool may not have), retries with backoff and
byte-identical bodies (the server deduplicates, so a retry cannot double-
write), and scrubs known secret shapes (tokens, JWTs, card numbers, emails,
connection-string passwords, PEM blocks, cookies) before anything leaves the
process. The server scrubs again; the client is the primary layer.

Install and instrumentation are normally driven by your coding agent via
`npx upcontrol init` - see the [upcontrol package](https://www.npmjs.com/package/upcontrol).
