# Verification - the install is not done until data arrives

An installer that reports success and leaves gives the user a silent product
and no way to tell "all quiet" from "nothing works". Never declare success
after the diff applies; the finish line is `npx upcontrol verify` reporting
data.

## Running it

```
npx upcontrol verify            # polls up to 120s, prints progress
npx upcontrol verify --timeout 300
npx upcontrol verify --json     # one JSON line, for you to parse
npx upcontrol verify --web      # a site-only install: page views pass too
```

Exit codes: `0` verified (install_verified seen, or with `--web` page views
arriving); `4` verification failed within the timeout; `2` no key found
(nothing to verify with); `3` cannot reach the endpoint at all. In `--json`,
`verified` is the SDK marker alone and page views ride in `web.views`, so a
`--web` pass on page views exits 0 with `verified: false`.

The SDK emits `install_verified` automatically on its first successful
connection - starting the app once with the key in place proves key validity,
network reachability, transport and scrubber in one step. No real traffic
needed.

A web-only install has no SDK in the chain: the script tag is the whole
install. It verifies with `npx upcontrol verify --web`, where `page views
arriving: N` counts as data arriving and one opened page is enough to produce
it. Ask the user to open the site once, then run it. The dev address counts
only if it was passed to the web run that minted the key; otherwise the tag
must be deployed before verify can see a view. Without `--web` page views never
pass: they would hide an SDK that never connected, so verify times out and
says that page views are arriving.

## The failure taxonomy - diagnose, do not shrug

"Nothing arrived" is useless; distinguish these five states and act:

| Observation | Diagnosis | What you do |
|---|---|---|
| the app did not start | import error, version conflict, build | read the runtime output yourself and fix or report it - never "check it yourself" |
| app runs, no `install_verified` | key not picked up, or no connectivity | `npx upcontrol status` shows where the key was found; if `none`, the env/.env is not loaded by the app's runner (dotenv not read? different working dir?). If key ok, verify prints which network step failed |
| `install_verified` arrived, no events after | points sit in code paths that have not executed yet | name the events you are waiting for and suggest the user action that triggers each ("open /checkout once") |
| events arrive, but not the expected names | names drifted from the dictionary | compare what arrived (verify prints recent names) against what you placed; a free name is fine, a near-canonical typo is not - fix the diff |
| data arrives with receipt warnings | a wire-level problem invisible to the user | verify surfaces the closed warning list (`ts_absent`, `level_unknown`, `key_in_body`, `scrubbed`, `reserved_prefix`, `cardinality_capped`, `attr_key_capped`, `field_cap_exceeded`); explain the specific one and fix the emitting side if it is your diff |

## Reporting

Quote verify's output to the user rather than paraphrasing it, then one line of
your own: what is proven ("key, transport and scrubber work; payment events
will appear with the first real checkout") or what remains ("waiting on the
first request to /checkout - the points are placed but that path has not run").
