# "Track user behavior / reduce churn / watch my revenue"

The goal decomposes into named events placed where the money and the users
actually move. You find the places by reading the code - the user often cannot
name them ("help me find where customers churn" is a complete, valid request).

Default scope is conservative: at most ~20 points, tests untouched. The user can widen it in words ("only payments and crons", "no more
than 10 places"). Honor those words exactly.

## The map: goal fragments to events

| Fragment of the goal | Events to place | Where to look |
|---|---|---|
| revenue, payments, MRR | `payment_succeeded`, `payment_failed`, `refund_issued` | payment provider callbacks/webhooks, charge routes |
| churn | `subscription_cancelled`, `payment_failed` (dunning), `login_failed` | subscription webhook handlers, billing portal, auth |
| activation, growth | `signup`, `checkout_started`, `subscription_created` | registration handler, checkout route |
| reliability behind behavior | `request_failed`, `external_api_failed`, `external_api_slow` | API error paths, outbound client wrappers |
| onboarding health | `upload_finished`, `import_finished` | upload/import completion paths |

## Placement examples (literal)

Stripe webhook handler - one line per branch that already exists:

```ts
case 'payment_intent.succeeded':
  track('payment_succeeded', { provider: 'stripe', currency: pi.currency, livemode: event.livemode, amount_minor: pi.amount });
  // ...existing handling stays untouched
case 'payment_intent.payment_failed':
  track('payment_failed', { provider: 'stripe', reason_code: pi.last_payment_error?.code ?? 'unknown' });
case 'customer.subscription.deleted':
  track('subscription_cancelled', { provider: 'stripe' });
```

Signup completion (after the user exists, not at form render):

```ts
track('signup', {});
```

Checkout entry (route or handler where checkout truly starts):

```ts
track('checkout_started', { route: '/checkout' });
```

Outbound API failure, inside the existing error branch:

```ts
track('external_api_failed', { provider: 'openai', status_class: String(res.status)[0] + 'xx' });
```

## Discipline reminders

- `route` is a pattern (`/users/:id`), never a concrete path with an ID in it.
- No user IDs, emails or amounts-with-currency-signs in labels; `amount_minor`
  is an integer in minor units.
- A provider's own event enum (Stripe's `payment_intent.succeeded`) maps to our
  name via the table above - do not forward provider names as event names.
- A stable name is the point: `payment_succeeded` at every success path gives
  the money moments one searchable, correlatable name. That is why label
  discipline matters.
