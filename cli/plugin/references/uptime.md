# "Tell me when my site / app is down"

This one needs no code. Uptime checks (HTTP, SSL, domain expiry, heartbeat
pings) run from upcontrol's probe fleet against the user's URL - they are
created in the upcontrol app, not in the repository, and there is no monitor
config file to scaffold. Do not invent one.

What to tell the user:

1. Open the upcontrol app -> Status page -> add a check with the site's URL
   (or, with no account yet, type the URL into the check field on
   https://upcontrol.io - the result offers to watch it and provisions the
   account in one step).
2. Alerts go to the channels configured under Alerts (email now; a Telegram
   destination appears when the bot is connected).
3. SSL and domain expiry are watched automatically on every website check -
   nothing to add.

## Where code DOES help this goal

- `app_started` (automatic with the SDK) turns a restart loop into a named
  incident instead of a flapping uptime check.
- `request_failed` points inside the app separate "the process is up but
  erroring" from "the host is down" - the difference between an uptime page
  that says "fine" and one that tells the truth.
- `heartbeat` covers things a probe cannot reach (workers, crons, private
  networks) - see topic `jobs`.

If the user asked ONLY for uptime, do not place any code. Offer the SDK as a
follow-up ("want the app to report why it went down, not just that it did?"),
and take an explicit yes before touching files.
