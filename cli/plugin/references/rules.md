# Placement rules

Seven rules. A diff violating any of them is a bad diff - do not show it.
Rule 6 is the only one whose violation is a security incident rather than a bad
diff; treat it accordingly.

1. **One line per log point.** `track('payment_succeeded', {...})` and nothing
   around it. No helper functions "while you're at it", no refactors.

2. **No wrappers around existing logic.** Do not wrap existing code in `try`, do
   not change control flow, do not add `await` where there was none. `track()`
   is fire-and-forget by design - it never needs awaiting.

3. **Never touch `catch` blocks** except by adding one line inside. Changing
   error handling is changing behavior, not observability.

4. **Names are free-form; keep them stable.** Pick a name that reads well
   (`payment_succeeded`, not `pay_ok`) and reuse it for the same moment at
   every call site. `deploy*` and `install_verified` have machinery behind
   them (topic `dictionary`); `uc.*` is reserved and refused.

5. **Labels stay low-cardinality.** `route` is a pattern (`/users/:id`), never
   a concrete path; no IDs, emails or free text in a label value - free detail
   belongs in the message.

6. **The key goes only into `.env`, only after checking `.gitignore` covers it.**
   Prefer `npx upcontrol init`, which does this for you and never shows the key.
   If you must handle the key: fix `.gitignore` first, say you fixed it, and
   never echo the key anywhere - not in chat, not in code, not in a commit.
   The one key that belongs in code is the `uc_pub_` key inside the tag
   `npx upcontrol web` prints: it is public and bound to the site's addresses,
   so it goes into the page's HTML. A `uc_live_` key never does.

7. **Nothing in hot loops.** A point inside a per-item loop is a thousand lines
   per request. Log the outcome of the operation, not its steps.

## SDK initialization

Add the import once, at the app's entry point, before other imports that might
throw:

```ts
import '@upcontrol/sdk/auto';
```

That single line installs the automatic pieces: `app_started` on boot (it names
the `release` the process runs when the environment says which),
`unhandled_exception` on crashes (observed via `uncaughtExceptionMonitor` - the
process still crashes exactly as before), and a best-effort flush when the
event loop drains. Configuration is environment-only: `UPCONTROL_API_KEY`,
optionally `UPCONTROL_ENDPOINT`, and `UPCONTROL_SERVICE` - one short lowercase
name per deployable (`api`, `worker`, `front`) that every line carries and the
dashboard filters by. No config object, no init call.

The SDK reads the deploy variable of Vercel, Render, Railway, Fly and Cloud Run
by itself. Heroku sets its own (`HEROKU_SLUG_COMMIT`) only after
`heroku labs:enable runtime-dyno-metadata`, which is off by default. Everywhere
else, and on Heroku without that flag, set `UPCONTROL_RELEASE` (a config var on
Heroku) to the commit sha or build id where the app is deployed: the board marks
each new release beside its line and bar charts.

Without a key the SDK stays silent: `track()` becomes a no-op, one warning is
printed at startup, nothing accumulates. The app is never affected.

## Existing loggers

Found pino, winston or console-based logging? Wrap it as a transport (topic
`logs` has literal recipes) - never replace it. The user's logger keeps working
exactly as before; upcontrol receives a copy.

## Review output

Always end with:

```
+13 log points · staged for your review
areas: checkout · billing · dunning
```

- the real count and the real areas. Never commit; never `git add`. The human
applies the diff. On a re-run, detect existing points by the `@upcontrol/sdk`
import and the `track(` calls: do not duplicate them, propose additions where
the previous pass did not go.
