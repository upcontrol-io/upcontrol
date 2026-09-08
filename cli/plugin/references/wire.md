# "This is not a Node app" - the wire, from any language

The SDK is a convenience for JavaScript, not the product. Everything upcontrol
does is reachable over one HTTP endpoint, so a Go service, a Rust worker, a
Python job, a PHP app or a static site sends the same things a Node app does -
including funnels, retention, A/B tests and breakdowns, which are computed on
the server and need no client library at all.

Read this topic when the repository is not Node, when only part of it is (a
Vite front over Go services is the common shape), or when the user asks how to
send from something the SDK does not cover.

## The endpoint

    POST https://upcontrol.io/i
    X-Upcontrol-Key: uc_live_...
    Content-Type: application/x-ndjson

One JSON object per line, newline-TERMINATED. The response is always a
structured receipt on sane input; the only refusals are 401 (the key), 429 (a
public key past its budget) and 503 (spool full, with Retry-After).

A log line is any object with a message:

```
{"ts":"2026-01-01T00:00:00Z","level":"error","msg":"checkout failed","order":"1042"}
```

A named event - the thing funnels and breakdowns are made of - adds
`"uc.event": true`. The message IS the event name:

```
{"ts":"...","msg":"checkout_started","uc.event":true,"uc.actor":"u_1042","plan":"pro"}
```

## `uc.actor` is the whole trick

`uc.actor` names the person behind an event. That one field is what makes a
funnel, a retention grid, an A/B test and a dimension readable - the server
counts DISTINCT actors over the events it already stores.

- Send the app's own stable user id, or a hash of it if you would rather
  upcontrol never hold the real one. The value is opaque to us.
- The SAME string for the same person, every time. A value that rotates - a
  session id, a request id, a cookie that resets - produces a number nobody
  experienced.
- For anonymous traffic, mint one id per visitor and keep it (a cookie, a
  localStorage entry). Do not fall back to an address: everyone behind one
  office or one carrier NAT becomes one person.
- An event with no `uc.actor` is a real event with nobody behind it. It counts
  wherever events are counted and never in a count of people. That is the right
  answer for a server-side event like `nightly_job_finished`.

`uc.` is a reserved namespace. Other fields are yours; keep them
low-cardinality (a plan name, a country code, a normalised route) because they
become labels. The actor is the one high-cardinality field, and it is lifted
into a column of its own rather than becoming a label.

## What each feed is, on the wire

There is no declaration step. A funnel is defined by the card that draws it,
from event names - so all a sender does is emit the events.

| Feed | What to send |
|---|---|
| funnel | one named event per step, each with `uc.actor` |
| retention | any event carrying `uc.actor`; the cohort is the week that actor was first seen |
| A/B test | the same event name with `uc.variant` and `uc.stat` (`exposed` / `converted`) |
| breakdown | an event with the dimension as an ordinary field (`"country":"FI"`) |

## Go

```go
body := `{"msg":"checkout_started","uc.event":true,"uc.actor":"` + userID + `"}` + "\n"
req, _ := http.NewRequest("POST", "https://upcontrol.io/i", strings.NewReader(body))
req.Header.Set("X-Upcontrol-Key", os.Getenv("UPCONTROL_API_KEY"))
req.Header.Set("Content-Type", "application/x-ndjson")
go func() { resp, err := http.DefaultClient.Do(req); if err == nil { resp.Body.Close() } }()
```

Whatever the language: build the line with the standard JSON encoder rather
than by hand, send it OFF the request path (a goroutine, a channel, a queue),
and never let a failure to send reach the user. Telemetry that can break a
checkout is worse than no telemetry. Batch lines when they are frequent - one
POST may carry many.

## From a browser

A secret key must never reach a bundle: it writes anything, and a bundle is
public. Ask the user to mint a PUBLIC key instead - prefix `uc_pub_`, listed
against the origins it may be sent from. It writes named events only, refuses a
plain log line, and is rate limited.

If the project has no public key yet, say so and stop rather than reaching for
the secret one. Naming a `VITE_`-prefixed variable for a secret key ships it to
every visitor of the site; that is a security incident, not a bad diff.

```js
navigator.sendBeacon('https://upcontrol.io/i?key=' + PUBLIC_KEY,
  new Blob([JSON.stringify({msg:'page_view', 'uc.event':true, 'uc.actor':visitorId}) + '\n'],
           {type:'application/x-ndjson'}));
```

## Rules that still apply

Everything in `npx upcontrol skills rules` holds here too: one line per point,
no wrappers, nothing in a hot loop, the key only in the environment and only
after `.gitignore` provably covers it, and every change staged as a diff for
the user to review. The transport changed; the discipline did not.
