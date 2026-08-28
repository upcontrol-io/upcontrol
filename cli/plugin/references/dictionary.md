# Event names

Names are free-form: pick one that reads well (`payment_succeeded`, not
`pay_ok`) and keep it stable - the same moment should carry the same name at
every call site. A name unlocks nothing: detection runs on the level (`error`)
and on the fingerprint (the hash of the message with numbers, hex runs and
quoted strings masked), never on the name.

Two names have machinery behind them today:

- the `deploy` family (prefix `deploy`, or containing `deployment`) is stored
  in the `events` table, which post-deploy alert suppression reads back;
- `install_verified` is stored in `events` and counted by the admin dashboard.
  The SDK emits it once at first successful connection - you never place it
  by hand.

The `uc.` prefix is reserved for upcontrol itself. A client-sent name carrying
it is refused with a `reserved_prefix` warning.

Every other name is an ordinary log line. That is not a downgrade: ordinary
lines are stored, searchable and part of incident log slices, and a line at
level `error` alerts like any other.

## How to emit

```ts
import { track } from '@upcontrol/sdk';
track('payment_succeeded', { provider: 'stripe', currency: 'usd', livemode: true });
```

The first argument is the event name. Labels ride as the second argument.

## Label discipline

Labels are low-cardinality keys (`route` is a pattern like `/users/:id`, never
a concrete path). Never put free text, IDs, emails or URLs into a label value -
high-cardinality labels destroy the series they feed. Free detail belongs in
the message of an ordinary log line.
