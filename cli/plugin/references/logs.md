# "Send all my logs to upcontrol"

Two moves: (1) mirror the existing logger into upcontrol, (2) add the SDK's
automatic events. Do NOT replace the user's logger - wrap it. Their console/file
output keeps working; upcontrol receives a copy, scrubbed client-side.

Ordinary log lines land in the log window (a ring of the last N lines) and in
incident slices. They are tier 4: kept, searchable, never alerting. Canonical
events (topic `dictionary`) are what alert - offer to add those as a second
pass once the logs flow.

## Step 1 - SDK

`npx upcontrol init` has already added `@upcontrol/sdk` to package.json and put
the key in `.env`. In the app entry point, first line:

```ts
import '@upcontrol/sdk/auto';
```

## Step 2 - mirror the existing logger

### pino

```ts
import { upcontrolLine } from '@upcontrol/sdk';
import pino from 'pino';

const logger = pino({
  hooks: {
    logMethod(args, method, level) {
      upcontrolLine(this.levels.labels[level], args[0], args[1]);
      return method.apply(this, args);
    },
  },
});
```

### winston

```ts
import { UpcontrolTransport } from '@upcontrol/sdk/winston';
logger.add(new UpcontrolTransport());
```

### console only (no logger library)

```ts
import { mirrorConsole } from '@upcontrol/sdk';
mirrorConsole(); // console.log/warn/error keep printing AND get mirrored
```

`mirrorConsole()` patches the three console methods to tee into upcontrol; the
original output is untouched. Prefer a real logger hook when one exists -
console mirroring is the fallback for apps with no logger at all.

## Step 3 - verify

Ask the user to run the app, then `npx upcontrol verify`. Lines should appear
within seconds of the first request.

## What NOT to do

- Do not add log lines to code as part of "send my logs" - that request is
  about the logs that already exist. Placing new points is the `funnel` /
  `jobs` topics, a separate (offered, not assumed) step.
- Do not pipe stdout wholesale from outside the process (`node app | ...`):
  the SDK path preserves levels, timestamps and structure; a pipe loses all
  three and dies with the shell.
- Do not send secrets knowingly. The SDK scrubs known secret shapes (tokens,
  JWTs, card numbers, emails, connection strings) before the wire and the
  server scrubs again, but scrubbing is defense, not permission.
