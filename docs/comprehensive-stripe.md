# Comprehensive Stripe

> What Stripe is, how the Payment Intents API actually behaves, and exactly how
> this project will wire it in. Pairs with
> [comprehensive-go.md](./comprehensive-go.md) (the language) and
> [comprehensive-redis.md](./comprehensive-redis.md) (the hot path this
> payment flow hangs off of).
>
> **Status:** reference document. The code it describes is
> [Layer 2 item 7](../docs/product-roadmap.md) and is **not written yet**.

## How to read this

- **Parts 1-3** are the mental model. Read those if payments are new to you.
- **Parts 4-6** are the mechanics you need to write the integration: Payment
  Intents, webhooks, signature verification.
- **Part 7** is the plan *for this repo* — the migration, the routes, the
  decision points we haven't locked yet.
- If you only have 30 minutes: Part 1 (vocabulary) → Part 2 (Payment Intents) →
  Part 4 (webhooks) → Part 7 (our plan).

## Conventions used in this document

- `[In this repo]` callouts point at real files in this codebase.
- `[Gotcha]` flags a frequent stumbling block — several of these have cost real
  people real money.
- `[Locked]` marks a decision that's already been made. Don't relitigate
  without a reason.
- `[Open]` marks something we still have to decide. These are listed together
  in Part 8.
- Go samples use `stripe` as the import alias (the official SDK's own package
  name), and `stripego` where the local package would collide.
- **Everything below was verified against `stripe-go/v82.5.1` and Stripe's
  current docs.** Signature names, method names, and the thin-vs-snapshot event
  split are recent changes that most tutorials get wrong.

---

## Table of contents

- **Part 1 — The Mental Model**
  1. What Stripe actually is
  2. Vocabulary you must know before reading any docs
  3. Test mode vs live mode, and the three kinds of key
  4. Why you never talk to Stripe from the browser with a secret key
- **Part 2 — Payment Intents**
  5. The core idea: one object per checkout attempt
  6. The PaymentIntent lifecycle
  7. Creating a PaymentIntent
  8. The client_secret — what it is and what it isn't
  9. Amounts, currencies, and the zero-decimal trap
  10. Metadata — your reconciliation hook
- **Part 3 — Idempotency (Stripe's side)**
  11. Stripe's own Idempotency-Key
  12. How it differs from the Idempotency-Key you built in Layer 1
- **Part 4 — Webhooks**
  13. Why webhooks, not polling
  14. Snapshot vs thin events — the modern split
  15. Which events we actually need
  16. The three things webhooks are bad at (duplicates, ordering, slowness)
  17. Returning 2xx fast
  18. Automatic retries and the 3-day window
- **Part 5 — Signature Verification**
  19. Why verification is mandatory
  20. The signature scheme, by hand
  21. The SDK's ConstructEvent, and its four error cases
  22. The raw-body rule — the #1 cause of "invalid signature"
  23. Testing locally with the Stripe CLI
- **Part 6 — The Go SDK**
  24. Client construction and the V1Services shape
  25. The PaymentIntent service surface
  26. Params and the `stripe.String` / `stripe.Int64` helpers
  27. Error handling — stripe.Error and its subtypes
- **Part 7 — The plan for this repo**
  28. The migration
  29. The config
  30. The routes
  31. The webhook handler, end to end
  32. Tracing the webhook
  33. Refunds
  34. The state machine's new edges
  35. The failure matrix
- **Part 8 — Open questions**
- **Appendix A — Setup checklist**
- **Appendix B — Test card numbers**
- **Appendix C — Local testing recipes**

---

# Part 1 — The Mental Model

## 1. What Stripe actually is

Stripe is a payments company. You integrate it by making HTTPS calls to their
API from your server, and by exposing an HTTPS endpoint they call when
something happens.

That's it. There's no SDK magic, no agent, no daemon. Two directions:

```
                    ┌─────────────────────────────┐
   your server  ──► │  Stripe API                 │  "create a payment intent"
                    │  api.stripe.com/v1/...      │
                    └─────────────────────────────┘

                    ┌─────────────────────────────┐
   your server  ◄── │  Stripe webhooks            │  "the payment succeeded"
                    │  POST /webhooks/stripe      │
                    └─────────────────────────────┘
```

The critical asymmetry, and the thing this whole document is about:

> **Stripe's API tells you what you *asked* for. Stripe's webhooks tell you
> what actually *happened*.**

A successful `PaymentIntent` create call does not mean the customer paid. It
means you created an object. The payment is confirmed later, by the browser, on
your user's behalf. Only the webhook tells you the money moved.

## 2. Vocabulary you must know before reading any docs

Stripe's docs assume you know these. Learn them here first.


| Term                  | What it is                                                                                                                      |
| ----------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| **Account**           | Your Stripe business account. Holds your balance.                                                                               |
| **Customer**          | A stored buyer identity. Optional — you can do card payments with no Customer at all.                                          |
| **PaymentMethod**     | A tokenized instrument (card, bank debit, wallet). Reusable, non-sensitive. The card number never touches your server.          |
| **PaymentIntent**     | The central object. Tracks *one* checkout attempt from creation to final outcome.                                               |
| **client_secret**     | A per-PaymentIntent key that lets the browser confirm the payment. Scoped, not privileged.                                      |
| **Charge**            | An actual money movement, created when a PaymentIntent succeeds. One PaymentIntent can produce **many** Charges across retries. |
| **Refund**            | Money returned against a Charge. A Charge can be partially refunded many times.                                                 |
| **Dispute**           | A cardholder's chargeback. Not a refund — the bank takes the money back.                                                       |
| **Balance**           | Your money. **Not** the same as your pending balance — funds settle later.                                                     |
| **Payout**            | Moving balance to your bank account.                                                                                            |
| **Webhook event**     | A JSON notification Stripe POSTs to you.                                                                                        |
| **Event destination** | The modern name for a registered webhook endpoint.                                                                              |
| **`whsec_...`**       | A per-endpoint signing secret. Verify every event with it.                                                                      |
| **Test mode**         | A sandbox. Fake money, real API. Separate keys, separate data.                                                                  |
| **livemode**          | A boolean field on nearly every object telling you which mode produced it.                                                      |

**The Charge-vs-PaymentIntent distinction is the one that bites people.**
Stripe's own docs: *"A PaymentIntent might have more than one Charge object
associated with it if there were multiple payment attempts. For example,
retries can create multiple Charges."*

If your code assumes `payment_intent.latest_charge` is the *only* charge, a
user who retries after a decline will surprise you.

## 3. Test mode vs live mode, and the three kinds of key

There are two completely separate worlds. Data in test mode never touches real
money and never appears in your live-mode dashboard.


| Key prefix    | Visibility                        | Where it works               | Safe in git?                                              |
| --------------- | ----------------------------------- | ------------------------------ | ----------------------------------------------------------- |
| `sk_test_...` | **Secret** — full account access | Test mode API calls          | **No**                                                    |
| `pk_test_...` | **Publishable** — client-safe    | Test mode, browser           | Technically yes, still don't                              |
| `whsec_...`   | **Webhook signing secret**        | Verifying test-mode webhooks | **No**                                                    |
| `sk_live_...` | **Secret**                        | Live mode                    | **No, and never touch it until you have a real business** |

`[Gotcha]` **Test-mode and live-mode webhook secrets are different**, even for
the same URL. A `whsec_` from your test endpoint will never verify a live
event. If your signature verification suddenly starts failing after you flip
to live keys, that's why.

`[Gotcha]` The publishable key is named "publishable" because it's designed to
be embedded in frontend code. It can only *create* client-side artifacts — it
cannot read your customers, your charges, or your balance.

## 4. Why you never talk to Stripe from the browser with a secret key

Your frontend has the publishable key. Your backend has the secret key. The
asymmetry is the entire security model.

The browser confirms the PaymentIntent using the `client_secret`. That secret
grants exactly one capability: "confirm or collect payment methods for
*this one* PaymentIntent." It cannot list your customers. It cannot read your
balance. It cannot issue refunds.

```javascript
// BROWSER — has pk_test_, uses client_secret
const stripe = Stripe(pk_test_...);
await stripe.confirmPayment({ elements, clientSecret });

// SERVER — has sk_test_, does everything privileged
pi, err := client.V1PaymentIntents.Create(ctx, params);
```

`[Gotcha]` **Never log the `client_secret`.** It ends up in your server logs
constantly (error messages embed it). Stripe explicitly says: *"Don't log it,
embed it in URLs, or expose it to anyone other than the customer. Make sure
you have TLS on any page that includes the client secret."* If you're logging
whole `PaymentIntent` structs with `slog`, you're leaking it. Log `pi.ID`
instead.

---

# Part 2 — Payment Intents

## 5. The core idea: one object per checkout attempt

Payment Intents replaced Stripe's older Charges-first model because card
payments are genuinely multi-step. A single card payment may require:

1. The customer enters a card
2. The bank requires 3D Secure verification (a redirect, a phone app prompt)
3. The bank approves or declines
4. You capture, or the charge auto-captures

The old model made you track those steps manually. A `PaymentIntent` is Stripe
saying: *"I'll hold the whole state machine for you. You create it, the
browser completes it, and I'll tell you via webhook when it's done."*

Stripe's guidance is one PaymentIntent per order or per cart:

> *"We recommend that you create exactly one PaymentIntent for each order or
> customer session in your system."*

`[Locked]` For this repo: **one PaymentIntent per reservation.** The reservation
IS the order. This is the single most important modeling decision in the whole
integration, because it makes every later question (webhook lookup, retry,
refund, idempotency) a single-row operation.

## 6. The PaymentIntent lifecycle

A PaymentIntent walks through `status` values. This table is the thing to
internalize — most integration bugs are a mishandled status.


| `status`                  | Dashboard shows                      | What it means for you                                                                  |
| --------------------------- | -------------------------------------- | ---------------------------------------------------------------------------------------- |
| `requires_payment_method` | **Incomplete**                       | Needs a payment method. **Also where a declined payment lands** — the user can retry. |
| `requires_confirmation`   | **Incomplete**                       | Has a payment method, needs confirming. Most integrations skip this.                   |
| `requires_action`         | **Incomplete**                       | Needs extra auth (3D Secure). Stripe.js handles this automatically.                    |
| `processing`              | **Pending**                          | Submitted, awaiting delayed confirmation (bank debits). Can take *days*.               |
| `requires_capture`        | **Uncaptured** / **Partial capture** | Authorized, not yet captured. Only if you use separate capture.                        |
| `succeeded`               | **Succeeded**                        | Money moved. Fulfill.                                                                  |
| `canceled`                | **Canceled**                         | You or Stripe killed it. Terminal.                                                     |

Two facts from that table that surprise everyone:

**1. A failed payment does not get a terminal status.** When a card is
declined, the PaymentIntent goes *back* to `requires_payment_method`. There is
no `failed` status on a PaymentIntent. The failure detail lives in
`last_payment_error`, and the individual attempt is a `Charge` with
`status: "failed"`.

This is why the roadmap's webhook plan (`payment_intent.payment_failed` →
"leave PENDING, user can retry") is coherent: there's nothing to transition
*from*, because no state changed.

**2. Refunds don't change the PaymentIntent status.** A succeeded PaymentIntent
stays `succeeded` after a refund. The refund is recorded on the **Charge**.
From Stripe's docs:

> *"Subsequent refunds, disputes, and outcomes are reflected on the Charge.
> These might change what you see in the Dashboard, even though the
> PaymentIntent remains succeeded."*

`[Gotcha]` This is a real design trap for us. If you model "refund" as
`CONFIRMED → CANCELLED` in our database, but Stripe keeps the PaymentIntent
`succeeded`, then a naive reconciliation job that compares the two will see a
permanent disagreement. Your database's status and Stripe's status are
**separate state machines**, and it's your job to keep them consistent by hand.

## 7. Creating a PaymentIntent

The canonical call:

```bash
curl https://api.stripe.com/v1/payment_intents \
  -u "sk_test_...:" \
  -d amount=1099 \
  -d currency=usd
```

In Go, using the verified v82 API surface:

```go
params := &stripe.PaymentIntentCreateParams{
    Amount:   stripe.Int64(1099),
    Currency: stripe.String(string(stripe.CurrencyUSD)),
    AutomaticPaymentMethods: &stripe.PaymentIntentAutomaticPaymentMethodsParams{
        Enabled: stripe.Bool(true),
    },
    Metadata: map[string]string{
        "reservation_id": reservationID,
    },
}
params.SetIdempotencyKey("res:" + reservationID)

pi, err := client.V1PaymentIntents.Create(ctx, params)
if err != nil {
    return nil, err
}
// pi.ClientSecret → hand to the frontend
```

`[Locked]` **`automatic_payment_methods.enabled = true`.** This is the modern
default and it means Stripe dynamically shows the payment methods available for
that currency and country (cards, wallets, bank debits) without you
enumerating them. Setting `payment_method_types` explicitly is legacy.

## 8. The client_secret — what it is and what it isn't

The `client_secret` is the handoff to your frontend. It's a string like:

```
pi_3ABCdef..._secret_xyz123...
```

The frontend passes it to `stripe.confirmPayment()` to complete the charge
without ever seeing your secret key.

What it **does** allow:

- Confirming this PaymentIntent
- Attaching a PaymentMethod to it
- Reading non-sensitive fields (status, amount, currency)

What it **cannot** do:

- Read your customers or balance
- Issue refunds
- Affect any other PaymentIntent

The format encodes the intent ID (`pi_...`) plus a secret suffix. Stripe's docs
call it *"a unique key... lets the client access important fields from the
PaymentIntent (status, amount, currency) while hiding sensitive ones (metadata,
customer)."*

`[Gotcha]` **Don't treat `client_secret` as a bearer token for your own API.**
It's for Stripe.js. Your API has JWT auth. Don't conflate the two, and don't
accept a `client_secret` as proof of who the user is — it proves nothing about
*your* identity. The user's JWT still governs access to `/api/reservations/{id}`.

## 9. Amounts, currencies, and the zero-decimal trap

`[Gotcha]` **Amounts are always integers in the currency's smallest unit.**

Stripe's own wording: *"A positive integer representing how much to charge in
the smallest currency unit (e.g., 100 cents to charge $1.00 or 100 to charge
¥100, a zero-decimal currency)."*

So:

- USD, EUR, GBP, THB → **2 decimals**. `1099` = $10.99.
- JPY, KRW, VND → **0 decimals**. `1000` = ¥1,000.

Hardcoding `amount * 100` will produce a 100× error for JPY. For this repo,
`usd` and `thb` are both 2-decimal, so `* 100` is correct — but put it in a
named helper with a comment, not inline arithmetic, so the day someone adds JPY
support it fails loudly instead of silently.

Also from the field docs:

> *"The minimum amount is $0.50 US or equivalent in charge currency. The amount
> value supports up to eight digits."*

So the valid range for USD is `50` to `99999999`. A `0` amount is rejected —
you cannot create a zero-value PaymentIntent. Relevant to us: the seeded event
has 100 seats but no price column yet, so someone has to pick a price before
`/pay` can work.

## 10. Metadata — your reconciliation hook

`metadata` is a string→string map you can attach to any Stripe object. It is
not shown to customers and does not affect fraud decisions.

The canonical use is exactly ours — put your own ID on the Stripe object:

```bash
-d "metadata[order_id]=6735"
```

> *"Through metadata, you can associate information that is meaningful to you
> with Stripe activity... This allows you to easily reconcile payments in
> Stripe to orders in your system."*

`[Locked]` **Store `reservation_id` in the PaymentIntent's metadata.** This
gives us two independent paths from a webhook back to our row:

1. Look up `payment_intents` by `stripe_payment_intent_id` (primary, indexed)
2. Read `pi.Metadata["reservation_id"]` (fallback, human-debuggable)

Path 2 matters operationally: an ops person staring at a Stripe dashboard can
see your reservation ID without querying our database at all.

`[Gotcha]` **Metadata is limited to 50 key-value pairs, keys ≤ 40 chars, values
≤ 500 chars**, and values must be strings. Don't try to serialize a whole
object in there.

`[Gotcha]` **Metadata propagates one way only.** A PaymentIntent copies its
metadata to the Charge it creates. Updating the PaymentIntent's metadata later
does *not* update Charges already created from it.

---

# Part 3 — Idempotency (Stripe's side)

## 11. Stripe's own Idempotency-Key

Stripe has its own idempotency mechanism, separate from ours. You pass
`Idempotency-Key` as a **request header** on any POST:

```bash
curl https://api.stripe.com/v1/payment_intents \
  -u "sk_test_...:" \
  -H "Idempotency-Key: res-8f2a-..." \
  -d amount=1099 -d currency=usd
```

If Stripe has seen that key, it replays the **original response** rather than
creating a second object. Keys are retained for **24 hours**.

This is why Stripe lists *"No idempotency key issues"* as an advantage of
Payment Intents — they've solved it at the API layer.

## 12. How it differs from the Idempotency-Key you built in Layer 1

You already built an idempotency store in `internal/redis/idempotency.go`
(Layer 1 item 5). It has a different shape and a different job. Don't confuse
them.


|                           | Ours (`internal/redis/idempotency.go`) | Stripe's                            |
| --------------------------- | ---------------------------------------- | ------------------------------------- |
| Layer                     | Our API surface                        | Stripe's API                        |
| Scope                     | Caches *our* HTTP response             | Prevents duplicate *Stripe* objects |
| Storage                   | Redis, 24h TTL + 5s in-flight lock     | Stripe's own, 24h                   |
| Detects body mismatch     | Yes (`body_hash`)                      | No                                  |
| Prevents double seat-hold | **Yes**                                | No — it has no idea what a seat is |
| Set by                    | The client, via header                 | **Us**, on the server call          |

`[Locked]` **We send both.** They're not redundant:

- **Ours** protects `POST /api/reservations/{id}/pay` from a client retrying
  the whole HTTP call (no second PaymentIntent, no second Redis write, and the
  user gets the *same* `client_secret` back).
- **Stripe's** protects the server→Stripe hop from a network-level retry where
  our handler already ran but the response was lost.

The key for Stripe's should be **derived from our reservation ID**, not from the
client's header. That's deliberate: it means even a client that omits or
changes our idempotency key can't cause a duplicate PaymentIntent at Stripe.

```go
// Ours (defense in depth) + Stripe's (defense in depth)
if key := r.Header.Get("Idempotency-Key"); key != "" {
    // ... our Redis cache lookup
}
params.SetIdempotencyKey("res:" + reservationID)  // always set
```

`[Gotcha]` Stripe returns `409 idempotency_key_mismatch` if you reuse a key with
a *different* request body. Since our key is `res:<uuid>` and the body is
derived from the reservation, a mismatch means a genuine bug — the amount
changed between attempts. Worth alerting on.

---

# Part 4 — Webhooks

## 13. Why webhooks, not polling

Polling Stripe for "did this payment succeed?" is a documented anti-pattern:

> *"You can use polling instead of webhooks... but it's much less reliable and
> might trigger rate limits."*

And the reason you *must* use them:

> *"Don't attempt to handle order fulfillment on the client side because
> customers can leave the page after payment is complete but before the
> fulfillment process initiates. Instead, use webhooks to monitor the
> `payment_intent.succeeded` event."*

This is the crux for our system. The user closes their browser tab right after
paying. There is no client left to tell us. The webhook is the *only* signal
that `PENDING_PAYMENT → CONFIRMED` should happen.

That's also why `CONFIRMED` is currently unreachable in our state machine —
it's defined in `validTransitions` with no handler. Layer 2 item 7 is what
wires it up.

## 14. Snapshot vs thin events — the modern split

`[Gotcha]` **This is new and most tutorials are stale.** Stripe now has two
event payload formats, and picking the wrong one makes your handler subtly
broken.


|                   | **Snapshot**                                             | **Thin**                                        |
| ------------------- | ---------------------------------------------------------- | ------------------------------------------------- |
| Contains          | The **full API object** at event time                    | Just IDs + a fetch hint                         |
| You read          | `event.Data.Object` directly                             | `event_notification.fetch_related_object()`     |
| Use when          | You need the historical object, or `previous_attributes` | **New integrations** — Stripe's recommendation |
| Cost              | Larger payloads                                          | Smaller; you fetch on demand                    |
| Event type naming | `payment_intent.succeeded`                               | `v1.billing.meter.error_report_triggered`       |

From Stripe's current docs:

> *"For most integrations, use the thin event handler for new integrations. Use
> the snapshot event handler if a third-party tool requires the complete Event
> object payload or you need `previous_attributes` without an additional API
> call."*

`[Locked]` **We use snapshot events.** Rationale:

1. Our handler must be able to confirm the reservation **even if the Stripe API
   is degraded**. If we have to fetch the PaymentIntent to do that, a Stripe
   outage stops us confirming paid reservations. A snapshot payload means we
   can decide with zero dependency on Stripe being reachable.
2. `stripe-go/v82`'s `webhook.ConstructEvent` returns `*stripe.Event` with
   `Data.Raw` / `Data.Object` populated — thin events need a different,
   newer API path that's awkward to trace-correlate.
3. Our per-event work is a single indexed Postgres lookup plus a CAS
   transition. It's fast. There's no size problem to solve.

This is a deviation from Stripe's default recommendation and it's deliberate.
`[Open]` Worth revisiting if payload size ever matters — at our volume it
doesn't.

## 15. Which events we actually need

Stripe's own advice: *"Only listen to event types your integration requires.
Listening for extra events (or all events) puts undue strain on your server and
we don't recommend it."*

So — exactly three, plus `metadata`:


| Event                           | Our reaction                                                                                                                                               |
| --------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `payment_intent.succeeded`      | `PENDING_PAYMENT → CONFIRMED`. Emit metric.                                                                                                               |
| `payment_intent.payment_failed` | **No transition.** Log `last_payment_error`, increment a failure metric. The status is already back at `requires_payment_method` — the user just retries. |
| `charge.refunded`               | `CONFIRMED → CANCELLED`. Release the seat. Emit metric.                                                                                                   |

Deliberately *not* subscribed, and why:


| Event                            | Why we skip it                                                                                                               |
| ---------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `payment_intent.created`         | We create it; we already know.                                                                                               |
| `payment_intent.processing`      | Only relevant for delayed-confirmation methods (bank debits). We're card-only in v1.                                         |
| `payment_intent.canceled`        | We never cancel PaymentIntents. `POST /cancel` cancels the *reservation*; the PaymentIntent is left alone (see Part 7 §33). |
| `payment_intent.requires_action` | Stripe.js handles 3DS transparently. The browser reports the outcome; the webhook for the final result follows.              |
| `charge.dispute.created`         | Real, but a Layer 3 concern. Noted here so it's not a surprise.                                                              |

`[Gotcha]` **`payment_intent.processing` will matter if you ever add Thai
promptpay or bank transfer.** Those are asynchronous — `processing` can last
days. Our 10-minute hold TTL would expire long before the money moves. If
Layer 3 adds those methods, the hold TTL and the expiry watcher both need to
become method-aware.

## 16. The three things webhooks are bad at

Stripe documents all three. Each one is a design constraint on our handler.

### 1. Duplicate delivery

> *"Webhook endpoints might occasionally receive the same event more than
> once... In some cases, two separate Event objects are generated and sent."*

> *"Track event IDs to identify duplicate deliveries instead."*

`[Locked]` **Log the event ID and skip if seen.** The roadmap's audit table is
the natural home for this — but a dedicated dedup guard is better than
scanning an audit table, because the dedup check has to be atomic with the
state transition.

Note the subtlety in Stripe's wording: sometimes you get the *same* `evt_` ID
twice, and sometimes *two different* `evt_` IDs for the same logical event. The
second case is only detectable by looking at `data.object.id` + `type`.

### 2. No ordering guarantee

> *"Stripe doesn't guarantee the delivery of events in the order that they are
> generated... Make sure that your event destination isn't dependent on
> receiving events in a specific order."*

> *"Don't use `created` to determine event order or whether you've already
> processed an event."*

This has a direct consequence for us. Imagine this ordering:

```
payment_intent.succeeded    →  we mark CONFIRMED
charge.refunded             →  we mark CANCELLED + release seat
```

vs. the reverse order. Both are legitimate Stripe deliveries. Our handler
**must be order-independent**, which means every transition goes through
`db.TransitionStatus` and we accept `(false, nil)` as "someone else won the
race" rather than treating it as an error.

This is the same CAS discipline as `POST /cancel`, and it means the webhook
handler gets no special treatment — it just calls the same helper.

### 3. Slow handlers time out

> *"(Timed out) ERR — The destination server took too long to respond... Fix:
> defer complex logic and return a successful response immediately."*

Part 7 §31 covers this.

## 17. Returning 2xx fast

> *"Your endpoint must quickly return a successful code (2xx) before any
> complex logic that might cause a timeout. For example, you must return a 200
> response before updating a customer's invoice as paid in your accounting
> system."*

`[Locked]` **Verify signature → return 200 → process.** Two acceptable shapes:


| Shape                            | How                                     | Trade-off                              |
| ---------------------------------- | ----------------------------------------- | ---------------------------------------- |
| **Sync** (do the work, then 200) | Only if the work is fast and can't fail | Simple; a failure means Stripe retries |
| **Async** (enqueue, then 200)    | Publish to SQS, return immediately      | Matches this repo's existing pattern   |

`[Open]` Sync vs async. I've recommended async in Part 7 §31 given the
expiration-watcher's race, but it's genuinely close — and async is the more
impressive architecture because it reuses the SQS plumbing you already have.
Your call.

## 18. Automatic retries and the 3-day window

> *"Stripe attempts to deliver events to your destination for up to three days
> with an exponential back off in live mode."*

> *"We retry event deliveries created in a sandbox three times over the course
> of a few hours."*

So a 500 doesn't mean lost — it means Stripe tries again for up to 3 days. But
you cannot rely on that for correctness:

- Test mode gives you **3 retries over a few hours**. That's the environment we
  develop in, so a flaky handler looks fine locally and fails in prod.
- Retries mean **the same event arrives many times**, which is exactly the
  duplicate case from §16.
- Stripe only retries on non-2xx (and connection failures). A handler that
  returns 200 while silently swallowing an error will **never** be retried —
  that payment is just gone.

Manual retries exist too:

- Dashboard **Resend** — up to 15 days
- `stripe events resend <event_id> --webhook-endpoint=<endpoint_id>` — up to 30 days

`[Locked]` **Anything that makes an event undeliverable permanently must
out-of-band alert.** A `payment_intent.succeeded` that can't be applied is a
customer who paid and has no ticket. That's a metric that pages someone, not
a log line.

---

# Part 5 — Signature Verification

## 19. Why verification is mandatory

Without it, anyone who learns your webhook URL can POST arbitrary JSON to it:

```json
{"type":"payment_intent.succeeded","data":{"object":{"id":"pi_ATTACKER_CHOSEN"}}}
```

and your handler would confirm a reservation they never paid for. Worse, they
could confirm a *specific* reservation they'd been waiting to steal.

Stripe's words: *"Without verification, an attacker could send fake webhook
events to your endpoint to trigger actions like fulfilling orders, granting
account access, or modifying records. Always verify that webhook events
originate from Stripe before acting on them."*

They give two protections:

1. **IP allowlisting** — Stripe publishes its IP ranges. Worth doing, but it's
   coarse and can change.
2. **Signature verification** — cryptographic, per-event, secret-keyed.

`[Locked]` **Signature verification is mandatory. IP allowlisting is
belt-and-braces** — note that it's for *arriving* connections, so it only helps
if you're the edge proxy, not if you're the Go process behind someone else's
ingress.

## 20. The signature scheme, by hand

Understanding this makes the SDK errors make sense instead of being magic.

The header looks like this (real headers are one line; this is wrapped for
readability):

```
Stripe-Signature:
t=1492774577,
v1=5257a869e7ecebeda32affa62cdca3fa51cad7e77a0e56ff536d0ce8e108d8bd,
v0=6ffbb59b2300aae63f272406069a9788598b792a944a07aba816edb039989a39
```

The construction:

**Step 1 — extract.** Split on `,`, then on `=`. You get `t` (timestamp) and one
or more `v1` signatures. Discard everything else.

**Step 2 — build the signed payload.** This is the part people get wrong:

```
signed_payload = <timestamp> + "." + <raw JSON request body>
```

So literally `1492774577.{"id":"evt_...","type":"payment_intent.succeeded",...}`.

**Step 3 — HMAC.**

```
expected = HMAC-SHA256(key = whsec_..., message = signed_payload)
```

**Step 4 — compare in constant time**, and check `|now - t| < tolerance`.

Two important details, both straight from Stripe:

> *"Schemes start with v, followed by an integer. Currently, the only valid live
> signature scheme is v1. To aid with testing, Stripe sends an additional
> signature with a fake v0 scheme, for test events."*

So `v0` is a **decoy for tests**, and — this is the subtle bit —

> *"To prevent from downgrade attacks, ignore all schemes that aren't v1."*

**Only ever compare `v1` signatures.** If you naively accepted any scheme, an
attacker could downgrade you to `v0`, which is computed differently.

And on multiple `v1` signatures:

> *"You can have multiple signatures with the same scheme-secret pair when you
> roll an endpoint's secret, and keep the previous secret active for up to 24
> hours. During this time, your endpoint has multiple active secrets and Stripe
> generates a signature for each secret."*

That's secret rotation, and it's why "compare against *all* `v1` values, pass if
*any* matches" is the correct loop.

**Replay protection** comes free from the timestamp being inside the signed
payload — an attacker can't change `t` without invalidating the signature:

> *"Because this timestamp is part of the signed payload, it's also verified by
> the signature, so an attacker can't change the timestamp without invalidating
> the signature. If the signature is valid but the timestamp is too old, you
> can have your application reject the payload."*

> *"Don't use a tolerance value of 0. Using a tolerance value of 0 disables the
> recency check entirely."*

`[Locked]` Use the SDK default (5 min), or set it explicitly to `5 * time.Minute`
so it's visible in the code. Don't pass `0`, and don't pass `IgnoreTolerance: true` — that last one is only for tests.

## 21. The SDK's ConstructEvent, and its four error cases

`stripe-go/v82`'s `webhook` package wraps all of the above. The verified
signature is:

```go
func ConstructEvent(payload []byte, header string, secret string) (Event, error)
func ConstructEventWithOptions(payload []byte, header string, secret string,
    options ConstructEventOptions) (Event, error)
```

`ConstructEventOptions` (verified in v82.5.1) has exactly three fields:

```go
type ConstructEventOptions struct {
    Tolerance time.Duration  // custom tolerance window
    IgnoreTolerance bool      // skip the timestamp check entirely (tests only)
    IgnoreAPIVersionMismatch bool // skip API version validation
}
```

The four sentinel errors — **verified from `webhook/client.go` in v82.5.1** —
are the ones you'll branch on:


| Error                         | Message                                         | Meaning                                    | Correct response                   |
| ------------------------------- | ------------------------------------------------- | -------------------------------------------- | ------------------------------------ |
| `webhook.ErrNotSigned`        | `"webhook has no Stripe-Signature header"`      | Header absent                              | **400** — it's not a Stripe event |
| `webhook.ErrInvalidHeader`    | `"webhook has invalid Stripe-Signature header"` | Header malformed, or only non-`v1` schemes | **400**                            |
| `webhook.ErrNoValidSignature` | `"webhook had no valid signature"`              | HMAC mismatch                              | **400** — forged or corrupted     |
| `webhook.ErrTooOld`           | `"timestamp wasn't within tolerance"`           | Valid signature, stale timestamp           | **400** — possible replay         |

`DefaultTolerance` is `300 * time.Second`, i.e. **5 minutes**.

Handling it properly:

```go
event, err := webhook.ConstructEventWithOptions(payload, sigHeader, secret,
    webhook.ConstructEventOptions{
        Tolerance: 5 * time.Minute, // explicit for the reader
    })
if err != nil {
    switch {
    case errors.Is(err, webhook.ErrNotSigned),
         errors.Is(err, webhook.ErrInvalidHeader),
         errors.Is(err, webhook.ErrNoValidSignature),
         errors.Is(err, webhook.ErrTooOld):
        // All four are 400: we cannot attribute this event to Stripe.
        // Do NOT retry, do NOT process.
        apiutil.WriteJSON(w, http.StatusBadRequest,
            map[string]string{"error": "invalid_signature"})
        return
    default:
        // Unexpected — our own bug or a library change.
        apiutil.WriteJSON(w, http.StatusInternalServerError,
            map[string]string{"error": "verification_error"})
        return
    }
}
```

`[Gotcha]` **Do not return 401 or 403 here.** 400 is right — the request is
malformed, not unauthorized. Stripe treats any non-2xx as a failure and will
retry, so the status choice mostly matters for your logs and for any
signature-failure alerting (a spike in `ErrNoValidSignature` means either an
attack or a rotated secret you didn't deploy).

## 22. The raw-body rule — the #1 cause of "invalid signature"

Stripe is emphatic and repeats it in three separate places in their docs:

> **"Don't manipulate the raw body of the request to perform signature
> verification. If you're using a framework, make sure it doesn't manipulate
> the raw body. Any manipulation to the raw body of the request causes the
> verification to fail."*

The signature covers `<timestamp> + "." + <raw bytes>`. Re-serializing JSON
changes the bytes (key order, whitespace, unicode escaping) and the HMAC won't
match.

**In Go + chi, this is straightforward** — `io.ReadAll(r.Body)` gives you the
exact bytes. But there are three ways to get it wrong:

`[Gotcha]` **1. Reading the body twice.** If anything before your handler has
already consumed `r.Body`, you get an empty buffer. Our middleware stack
(`slogmw`, `middleware.Recoverer`, `metricsMiddleware`, `otelhttp`) doesn't
read bodies, but verify that if you add anything that does.

`[Gotcha]` **2. Using `http.MaxBytesReader` or decompression middleware.**
gzip/br middleware transparently decompresses and replaces `r.Body` — which
means the raw bytes you sign are no longer the raw bytes Stripe sent. Stripe
doesn't send compressed webhook bodies, so simply don't add body-transforming
middleware to this route.

`[Gotcha]` **3. Unmarshalling then re-marshalling.** This one looks innocent:

```go
// WRONG — verification on re-serialized bytes
var e map[string]any
json.Unmarshal(body, &e)
verify(remarshal(e))  // ← will fail ~always
```

Correct order, always: **verify the raw bytes first, then unmarshal.**

```go
// RIGHT
payload, err := io.ReadAll(r.Body)          // exact bytes
if err != nil { /* 400 */ }
event, err := webhook.ConstructEventWithOptions(payload, ...)  // verify raw
// only now is it safe to look at event.Data
```

Also: **set a size limit.** A webhook endpoint that reads an unbounded body is
a memory-exhaustion target. Stripe payloads are small (tens of KB), so cap at
something generous but finite.

```go
r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
```

## 23. Testing locally with the Stripe CLI

This is the tool that makes the whole thing tractable. It sits between Stripe
and your localhost, forwarding real signed events.

```bash
# Install (macOS)
brew install stripe/stripe-cli/stripe

# One-time: browser login
stripe login

# Forward events to your local API and print a signing secret
stripe listen --forward-to localhost:8080/webhooks/stripe
```

Output:

```
Ready! Your webhook signing secret is 'whsec_...' (^C to quit)
```

**That `whsec_...` is your `STRIPE_WEBHOOK_SECRET`.** It's ephemeral — it
changes every time you run `stripe listen`. This is the single most common
source of "why did verification work yesterday and not today."

Two things to know:

`[Gotcha]` **Filter events while developing**, or you'll get drowned in account
activity:

```bash
stripe listen --forward-to localhost:8080/webhooks/stripe \
  --events payment_intent.succeeded,payment_intent.payment_failed,charge.refunded
```

`[Gotcha]` **`stripe listen` does NOT need a publicly reachable URL** — that's
its entire purpose. No ngrok, no public DNS, no TLS cert. This is why it beats
the ngrok workflow for local dev. (ngrok is still the answer for testing from
a real Stripe account's live-mode webhooks, which CLI forwarding can't do.)

Trigger events without touching the UI:

```bash
stripe trigger payment_intent.succeeded
stripe trigger payment_intent.payment_failed
stripe trigger charge.refunded
```

`[Open]` Should `stripe` CLI be added to the Makefile as a target
(e.g. `make stripe-listen`)? Nice-to-have; the Makefile convention says
services get targets, and this is a foreground listener, so probably yes.

---

# Part 6 — The Go SDK

## 24. Client construction and the V1Services shape

`[Gotcha]` **`stripe.NewClient` returns a struct of services, not a flat
namespace.** This changed in v79+ and a lot of Go tutorials show the old API.

Verified from `stripe_client.go` in v82.5.1:

```go
sc := stripe.NewClient("sk_test_...")

// PaymentIntents  → /v1/payment_intents
pi, err := sc.V1PaymentIntents.Create(ctx, params)

// Refunds        → /v1/refunds
ref, err := sc.V1Refunds.Create(ctx, params)

// Disputes       → /v1/disputes
```

So it's `sc.V1PaymentIntents`, not `sc.PaymentIntents`. The service types are
unexported (`v1PaymentIntentService`), which means you never name them — you
always go through the `*Client`.

**Reuse one client for the process lifetime.** It holds an `*http.Client` and a
connection pool. Constructing a `stripe.Client` per request defeats HTTP
keep-alive and will show up as latency.

`[Gotcha]` **This is a strong fit for our existing patterns.** The Layer 2
roadmap's `Ping(ctx) error` trick (from `ReservationPublisher`) applies directly
— define a `payment.Service` interface in `internal/api/publisher.go`'s style,
with a real Stripe-backed impl and a fake for tests. Tests must not hit Stripe
(AGENTS.md convention #3: *"No external services in unit tests"*).

Wiring the HTTP client for timeouts:

```go
sc := stripe.NewClient(key, stripe.BackendConfig{
    HTTPClient: &http.Client{Timeout: 10 * time.Second},
    MaxNetworkRetries: stripe.Int64(2),
    LeveledLogger: ...,
})
```

`[Locked]` **Set an explicit timeout.** Stripe's default HTTP client has no
timeout, and your `/pay` handler inherits whatever the request context gives
it. A hung Stripe call would otherwise hold a Postgres connection open.

## 25. The PaymentIntent service surface

Verified method set on `v1PaymentIntentService` in v82.5.1 — these are the
methods available:


| Method                                            | Purpose                      | Do we need it?                                        |
| --------------------------------------------------- | ------------------------------ | ------------------------------------------------------- |
| `Create(ctx, *PaymentIntentCreateParams)`         | New PaymentIntent            | **Yes** — `/pay`                                     |
| `Retrieve(ctx, id, *PaymentIntentRetrieveParams)` | Fetch one                    | **Yes** — reconcile, retry, `/pay` idempotent replay |
| `Update(ctx, id, *PaymentIntentUpdateParams)`     | Change amount/metadata       | Yes — cart changed before pay                        |
| `Confirm(ctx, id, *PaymentIntentConfirmParams)`   | Server-side confirm          | No — browser confirms                                |
| `Capture(ctx, id, *PaymentIntentCaptureParams)`   | Capture an authorized amount | No — we don't use separate capture                   |
| `Cancel(ctx, id, *PaymentIntentCancelParams)`     | Cancel the intent            | **No** — see Part 7 §33                             |
| `ApplyCustomerBalance(...)`                       | Pay from customer balance    | No                                                    |
| `IncrementAuthorization(...)`                     | Increase an auth             | No                                                    |
| `VerifyMicrodeposits(...)`                        | Microdeposit flows           | No                                                    |
| `List(ctx, *PaymentIntentListParams)`             | Iterate                      | No                                                    |
| `Search(ctx, *PaymentIntentSearchParams)`         | Search                       | **Maybe** — ops lookup                               |

`[Gotcha]` **`List` and `Search` return `Seq2[*PaymentIntent, error]`, not a
slice.** That's Go 1.23+ iterator style:

```go
for pi, err := sc.V1PaymentIntents.List(ctx, params) {
    if err != nil { return err }
    _ = pi
}
```

If you write `pis, err := ...List(...)` expecting a slice, it won't compile.
Budget a little extra time here — the iterator idiom is unfamiliar and the
compile error is unhelpful.

**We need `Retrieve` even though we have webhooks**, for one specific case:
`POST /pay` called twice. On the second call, instead of creating a second
PaymentIntent we return the existing `client_secret`. `Retrieve` is how we get
it back.

## 26. Params and the `stripe.String` / `stripe.Int64` helpers

Params structs use **pointer fields**, so the helpers matter:

```go
Amount:   stripe.Int64(1099),
Currency: stripe.String(string(stripe.CurrencyUSD)),
Enabled:  stripe.Bool(true),
```

`[Gotcha]` **A bare `nil` field is omitted from the request; a non-nil pointer
is always sent, even if zero-valued.** So this sends `amount: 0`:

```go
amount := int64(0)
params := &stripe.PaymentIntentCreateParams{Amount: &amount}  // sends 0 → 400 from Stripe
```

vs. this omits it entirely (also a 400, but for a different reason — you get a
confusing message). Always use `stripe.Int64(1099)` with a real literal.

`[Locked]` **`Currency` is a `stripe.Currency` type, not a bare string.** You
can assign the raw string (`stripe.Currency("usd")`) — Stripe validates it —
but using the constants means the compiler catches typos.

`[Gotcha]` **`AutomaticPaymentMethods` is a nested pointer struct**, not a bool:

```go
// WRONG — won't compile
AutomaticPaymentMethods: stripe.Bool(true),

// RIGHT
AutomaticPaymentMethods: &stripe.PaymentIntentAutomaticPaymentMethodsParams{
    Enabled: stripe.Bool(true),
},
```

Similarly `Metadata` is a plain `map[string]string`, *not* a pointer struct.

**The idempotency setter** (verified in `params.go`) hangs off the embedded
`Params`:

```go
params.SetIdempotencyKey("res:" + reservationID)
```

## 27. Error handling — stripe.Error and its subtypes

Every Stripe API error is a `*stripe.Error`. Verified field set in v82.5.1:

```go
type Error struct {
    APIResource
    ChargeID    string
    Code        ErrorCode
    DeclineCode DeclineCode
    DocURL      string
    Err         error
    HTTPStatusCode    int
    Msg               string
    DeveloperMsg      string
    Param             string
    PaymentIntent     *PaymentIntent
    PaymentMethod     *PaymentMethod
    PaymentMethodType PaymentMethodType
    RequestID         string
    RequestLogURL     string
    SetupIntent       *SetupIntent
    Source            *PaymentSource
    Type              ErrorType
}
```

`[Locked]` **`RequestID` and `RequestLogURL` go in every error log.** They're
Stripe's support ticket handle — without `RequestID` Stripe can't trace a
failed call, and you can't debug it either. This is a genuinely valuable
detail that most integrations omit and every reviewer should notice is missing.

Branch on `Type`. Verified constants:


| `Type`                                                | Meaning                       | HTTP to return        |
| ------------------------------------------------------- | ------------------------------- | ----------------------- |
| `ErrorTypeCard` (`"card_error"`)                      | The *card* was declined       | `402`                 |
| `ErrorTypeInvalidRequest` (`"invalid_request_error"`) | Your request was malformed    | `400`                 |
| `ErrorTypeAuthentication` (`"authentication_error"`)  | Bad secret key                | `500` — it's our bug |
| `ErrorTypePermission` (`"permission_error"`)          | Key lacks the permission      | `500` — our bug      |
| `ErrorTypeIdempotency` (`"idempotency_error"`)        | Key reuse with different body | `409`                 |
| `ErrorTypeAPI` (`"api_error"`)                        | Stripe-side problem           | `503`                 |
| `ErrorTypeRateLimit` (`"rate_limit_error"`)           | Too many requests             | `429`                 |

`[Gotcha]` **`card_error` on `PaymentIntents.Create` is a client-visible,
expected outcome** — a bad test card number. Return 402 with the decline code,
not 500. Distinguishing "the user's card was declined" from "our Stripe
integration is broken" is exactly what this branch is for.

```go
var se *stripe.Error
if errors.As(err, &se) {
    slog.ErrorContext(ctx, "stripe api error",
        "type", string(se.Type),
        "code", string(se.Code),
        "decline_code", string(se.DeclineCode),
        "http_status", se.HTTPStatusCode,
        "param", se.Param,
        "request_id", se.RequestID,        // ← Stripe support needs this
        "request_log_url", se.RequestLogURL,
    )
    switch se.Type {
    case stripe.ErrorTypeCard:
        // 402 + decline code
    case stripe.ErrorTypeIdempotency:
        // 409 — a real bug on our side, alert
    case stripe.ErrorTypeAuthentication, stripe.ErrorTypePermission:
        // 500 — our config is wrong, page someone
    default:
        // 503
    }
}
```

`[Gotcha]` **`Error` has an `Err error` field that shadows the outer `error`
conceptually.** `se.Err` holds a more specific cause (e.g. a `CardError`).
Use `errors.As` on the outer error first; reach for `se.Err` only when you
need the fine-grained reason.

---

# Part 7 — The plan for this repo

Everything above is general Stripe knowledge. This part is about *our* system.

## 28. The migration

`[Open]` Table name — the roadmap says `payment_intents`, which shadows the
Stripe concept and could confuse. Lean toward `reservation_payments` or keep
`payment_intents` for roadmap fidelity. **Your call.**

Sketch (not committed):

```sql
-- migrations/005_stripe_payments.sql
CREATE TABLE payment_intents (
    payment_id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reservation_id        UUID NOT NULL UNIQUE
                              REFERENCES reservations(reservation_id),
    stripe_payment_intent_id TEXT NOT NULL UNIQUE,
    stripe_charge_id      TEXT,
    amount_cents          BIGINT NOT NULL,
    currency              TEXT NOT NULL,
    status                TEXT NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Webhook dedup. Separate from the audit log because the dedup check
-- must be atomic with the state transition, and because "have we seen
-- this event" is a different question from "what happened".
CREATE TABLE stripe_events (
    event_id      TEXT PRIMARY KEY,   -- evt_...
    event_type    TEXT NOT NULL,
    livemode      BOOLEAN NOT NULL,
    reservation_id UUID,
    processed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Reconciliation: "did this Stripe object end up where we think it is?"
CREATE INDEX idx_payment_intents_stripe_id
    ON payment_intents (stripe_payment_intent_id);
```

`[Locked]` **`reservation_id` is `UNIQUE`.** One reservation, one PaymentIntent,
enforced by the database — not by application discipline. This is what makes
`/pay` safely replayable and is the DB-level guarantee that Stripe's own
idempotency key can't provide.

`[Locked]` **Seed a price.** The `events` table has no price column, and Stripe
rejects amounts below $0.50. Either add `price_cents BIGINT` to `events` or
hardcode a demo price in the seed. Hardcoding is fine for a portfolio project;
the column is more honest.

## 29. The config

Following the existing `config.Load()` pattern — validated at startup,
fail-fast:

```go
// internal/config/config.go
type Config struct {
    // ... existing fields ...
    Stripe StripeConfig
}

type StripeConfig struct {
    // SecretKey is sk_test_... or sk_live_.... Never logged.
    SecretKey string
    // WebhookSecret is the whsec_... for THIS endpoint+mode.
    WebhookSecret string
    // Currency is the charge currency. Two decimals for usd/thb.
    Currency string
    // PriceCents is the per-seat price used until events have a price column.
    PriceCents int64
}
```

And in `Load()`:

```go
if c.Stripe.SecretKey == "" {
    return nil, errors.New("STRIPE_SECRET_KEY is required")
}
if c.Stripe.WebhookSecret == "" {
    return nil, errors.New("STRIPE_WEBHOOK_SECRET is required")
}
```

`[Locked]` **Fail fast on missing keys**, consistent with the package doc:
*"All values are validated at Load time so the caller can fail fast at startup,
before any goroutines, pools, or sockets are open."*

`[Locked]` **`STRIPE_SECRET_KEY` and `STRIPE_WEBHOOK_SECRET` never get logged,
never get returned from a handler, and never enter `/readyz`'s response body.**
The `/readyz` pattern of `"ok"` / `"error: <msg>"` / `"not_configured"` works
perfectly here:

```go
Stripe: func(ctx context.Context) error {
    if st == nil {
        return errNotConfigured  // → "not_configured", counts as healthy in dev
    }
    _, err := st.Retrieve(ctx, piID, nil)  // cheap authenticated call
    return err
},
```

`[Gotcha]` **A `/readyz` Stripe check is a liveness check for your *config*, not
for Stripe.** If Stripe has a bad afternoon, do you want every pod pulled from
the LB? Arguably no — that turns a Stripe degradation into a total outage.
Arguably yes — you don't want to accept `/pay` requests you can't fulfil.
`[Open]` I'd lean **not** adding Stripe to `/readyz` (a failing dependency
shouldn't remove you from rotation when you already have in-flight holds), but
add a dedicated metric + alert. Your call.

`[Locked]` **Add both keys to `.env.docker.example` and `.env.example`, and to
`scripts/setup-docker-env.sh`.** Never to a committed `.env*`. `.gitignore`
already covers `.env` — verify it covers `.env.docker`.

## 30. The routes

Two new routes, per the roadmap:

```
POST /api/reservations/:id/pay       JWT   → { client_secret, payment_intent_id }
POST /webhooks/stripe                 none  → 200 (signature-verified)
```

Route wiring, following the existing shape in `cmd/api/main.go`:

```go
r.Route("/api", func(r chi.Router) {
    // ... existing reservation routes ...

    r.With(auth.Middleware(cfg.JWTSecret)).Post("/reservations/{id}/pay",
        api.PayReservationHandler(pool, payments))
})

// No auth middleware — Stripe can't present a JWT.
// Signature verification IS the auth.
r.Post("/webhooks/stripe", api.StripeWebhookHandler(pool, rdb, payments))
```

`[Locked]` **`/webhooks/stripe` sits outside `/api` and outside
`auth.Middleware`.** It's authenticated by signature, not JWT. Two failure
modes to avoid: putting it under `/api` and accidentally inheriting the JWT
middleware (which would 401 every Stripe delivery), and putting it inside the
`/api` group at all (the URL shouldn't imply it's a user API).

`[Locked]` **`/webhooks/stripe` gets no `Idempotency-Key` handling.** That
mechanism exists to dedup *client* retries. Stripe's retries are deduped by the
event-ID guard instead (§31), which is a different key space.

**`/pay` handler flow:**

1. JWT → verify `row.UserID == claims.Subject` (same as every other handler)
2. `row.Status != PENDING_PAYMENT` → `409 already_confirmed` / `409 already_expired` / `409 already_cancelled`
3. Check the hold is still alive in Redis — **a hold may have expired even if the DB row is still `PENDING_PAYMENT`.** The expiration-watcher's keyspace sweep writes `EXPIRED`, but there's a window. If the hold key is gone, `409 hold_expired`.
4. `INSERT INTO payment_intents ... ON CONFLICT (reservation_id) DO NOTHING RETURNING stripe_payment_intent_id`
   - If the insert returns nothing → a PaymentIntent already exists → `SELECT` it and return the existing `client_secret`. **This is the idempotent path.**
   - If it inserted → call `V1PaymentIntents.Create` with our idempotency key → `UPDATE` the row with the returned ID
5. Return `client_secret` + `payment_intent_id`

`[Gotcha]` **Step 4 has a hole: we insert before we call Stripe.** If the
process dies between the insert and the `Create`, we've left a row with no
Stripe ID and the retry hits the "already exists" branch with nothing to
return.

Two fixes, in increasing order of complexity:

- Store `stripe_payment_intent_id` as NULLable, and on the "already exists"
  branch, if the ID is NULL, call `Create` then `UPDATE`. (Idempotent because
  Stripe's key is `res:<uuid>` — the retry gets the same PaymentIntent back.)
- Or derive the PaymentIntent ID client-side. Stripe doesn't offer that, so
  this isn't available.

`[Open]` Nullable-column retry is the pragmatic fix; I lean that way, but it
does mean a nullable column in a table where everything else is `NOT NULL`.
Worth deciding deliberately.

`[Gotcha]` **A failed `Create` must not leave the reservation un-payable.** If
Stripe returns 500 and we've already inserted a row, the next `/pay` call takes
the "already exists" path. With the nullable retry above that's recoverable;
without it, the reservation is bricked. Another argument for nullable.

## 31. The webhook handler, end to end

This is the heart of Layer 2. Recommended shape:

```go
func StripeWebhookHandler(pool *db.Pool, rdb *redis.Client, svc payment.Service) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // ── 1. bound the body ───────────────────────────────────
        r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

        // ── 2. read RAW bytes (never re-serialize) ─────────────
        payload, err := io.ReadAll(r.Body)
        if err != nil {
            apiutil.WriteJSON(w, http.StatusBadRequest,
                map[string]string{"error": "unreadable_body"})
            return
        }

        // ── 3. verify signature ─────────────────────────────────
        sig := r.Header.Get("Stripe-Signature")
        event, err := webhook.ConstructEventWithOptions(payload, sig, secret,
            webhook.ConstructEventOptions{Tolerance: 5 * time.Minute})
        if err != nil {
            // All four sentinels → 400. See §21.
            slog.WarnContext(ctx, "stripe signature verification failed",
                "error", err.Error(), "has_header", sig != "")
            apiutil.WriteJSON(w, http.StatusBadRequest,
                map[string]string{"error": "invalid_signature"})
            return
        }

        // ── 4. FAST PATH: accept ────────────────────────────────
        // Everything above is pure validation. Everything below is
        // "real work" and is allowed to be slow.

        // [Open] sync: process here, then return 200
        // [Locked] async: publish to SQS here, then return 200

        // ── 5. dedup by event ID ────────────────────────────────
        // INSERT ... ON CONFLICT DO NOTHING — if 0 rows, we've seen it.
        // [Locked] MUST be idempotent, not "SELECT then INSERT".

        // ── 6. switch on event type ─────────────────────────────
        switch event.Type {
        case stripe.EventTypePaymentIntentSucceeded:
            // → PENDING_PAYMENT → CONFIRMED via db.TransitionStatus
        case stripe.EventTypePaymentIntentPaymentFailed:
            // → NO transition. Log + metric only.
        case stripe.EventTypeChargeRefunded:
            // → CONFIRMED → CANCELLED + release seat
        default:
            // Unsubscribe in the dashboard; §15 says don't over-subscribe.
        }
    }
}
```

**Why dedup must be `INSERT ... ON CONFLICT DO NOTHING` and return early:**

Two concurrent deliveries of the same event both run `SELECT`. Both see
nothing. Both `INSERT`. One gets a unique-violation. If you're using
`SELECT`-then-`INSERT` naively you have a race; `ON CONFLICT DO NOTHING`
returning 0 rows is atomic and correct. This is the same principle as
`db.TransitionStatus`'s CAS — a database-enforced atomicity primitive rather
than application-level hope.

**Why `payment_intent.payment_failed` does no transition** (§6): the
PaymentIntent is already back at `requires_payment_method`. Our reservation
correctly stays `PENDING_PAYMENT`, the hold is still alive, and the user retries.
**Do not** transition to a failure state — we don't have one, and adding one
would break the retry-until-hold-expires UX.

**Why `charge.refunded` → `CONFIRMED → CANCELLED` also releases the seat:**
symmetry with `POST /cancel`. Otherwise a refunded customer keeps their seat.

`[Locked]` **`livemode` is checked before any state change.**

```go
if event.Livemode != expectedLivemode {
    slog.ErrorContext(ctx, "stripe event mode mismatch",
        "livemode", event.Livemode, "event_type", string(event.Type))
    apiutil.WriteJSON(w, http.StatusBadRequest,
        map[string]string{"error": "mode_mismatch"})
    return
}
```

Without this, a test-mode event replayed at your production endpoint would
confirm real reservations. It's one line and it closes a genuine attack /
operator-error surface. **The `whsec_` secrets differ per mode, so this is
defence in depth** — but secrets leak, config gets copied between environments,
and this is the check that makes that failure loud instead of silent.

## 32. Tracing the webhook

`[Locked]` **A webhook arrives with no parent span.** `otelhttp.NewHandler`
creates a root span for the request, so at minimum it won't be orphaned. But
the roadmap wants better:

> *"webhooks come in without an active span; create a root span on receipt
> using the trace_id from the body's `_traceparent` (we already have this
> pattern in SQS)"*

**Important correction to that assumption:** Stripe does **not** send a
`traceparent` header, and there's no `_traceparent` in the body. So there is no
upstream trace to link to — a webhook is genuinely a new root.

What we *can* do is make the root span rich, so it's findable:

```
Span: "stripe.webhook"
  stripe.event_type      = "payment_intent.succeeded"
  stripe.event_id        = "evt_1ABC..."
  stripe.livemode        = false
  stripe.reservation_id  = "6880e7fa-..."   (from metadata)
  stripe.delivery_attempt= 2
```

`[Locked]` **Log the `stripe.event_id` on every line.** It's the correlation key
for this whole subsystem — it's what Stripe's dashboard shows, what
`stripe events resend` takes, and what `stripe_events.event_id` stores. An ops
person who can give you an `evt_` ID can find every log line and span for that
delivery.

`[Locked]` **Tag `stripe.reservation_id` as a span attribute when known** (from
`payment_intent.Metadata["reservation_id"]`, or from the DB lookup). That makes
"show me the trace for reservation X" work in Jaeger, which pairs with the
trace→logs workflow already built in Grafana.

## 33. Refunds

`[Locked]` **`CONFIRMED → CANCELLED` is a refund, not a cancel.** `POST /cancel`
already returns `409 already_paid` for a confirmed reservation. That error is
pointing at this endpoint.

`[Gotcha]` **We should NOT cancel the PaymentIntent on refund.** Stripe's
`PaymentIntent.Cancel` only works on intents that haven't succeeded — calling it
on a `succeeded` intent returns an error. A refund is a new object:

```go
params := &stripe.RefundCreateParams{
    Charge: stripe.String(stripeChargeID),
    // Amount omitted = refund the full charge.
    Metadata: map[string]string{
        "reservation_id": reservationID,
    },
}
params.SetIdempotencyKey("refund:" + reservationID)

ref, err := svc.Refund(ctx, params)
```

Verified: `RefundCreateParams` has `Charge *string`, `Amount *int64`,
`Currency *string`, `Metadata map[string]string`, `InstructionsEmail *string`,
`Expand []*string`. Full refund = omit `Amount`.

`[Gotcha]` **A `Charge` can be refunded multiple times, partially.** So
"is this reservation refunded?" is *not* a boolean. It's
`sum(refunds for charge) >= charge.amount`. If we want partial refunds later
we need a `refunds` table, not just a status flip. For v1, full-refund-only
keeps it a status transition — document that assumption in the code.

`[Gotcha]` **Refund-as-cancel loses the distinction between "user cancelled"
and "we refunded".** The roadmap's `REFUND_INITIATED` audit event type exists
precisely for this — that's why item 6 (audit log) is sequenced before item 7
(Stripe). Follow that order.

`[Locked]` **`stripe_charge_id` must be persisted at refund time.** The
`payment_intent.succeeded` event gives you the PaymentIntent; the Charge is at
`pi.LatestCharge` (verified field on `PaymentIntent`). Store it, because a
refund needs a Charge ID and by then the webhook may be days old.

## 34. The state machine's new edges

Verified current table in `internal/db/transition.go`:

```go
var validTransitions = map[string][]string{
    StatusPendingPayment: {StatusConfirmed, StatusExpired, StatusCancelled},
    StatusConfirmed:      {StatusCancelled},
    // EXPIRED and CANCELLED are terminal — absent as keys
}
```

Layer 2 needs to confirm that this covers Stripe's reality:


| From              | To          | Trigger                    | Edge exists?            |
| ------------------- | ------------- | ---------------------------- | ------------------------- |
| `PENDING_PAYMENT` | `CONFIRMED` | `payment_intent.succeeded` | ✅ already in the table |
| `PENDING_PAYMENT` | `EXPIRED`   | hold TTL sweep             | ✅ already in the table |
| `PENDING_PAYMENT` | `CANCELLED` | `POST /cancel`             | ✅ already in the table |
| `CONFIRMED`       | `CANCELLED` | `charge.refunded`          | ✅ already in the table |

**No new edges are needed.** The table is already correct for this integration —
which is a nice validation of Layer 1 item 1's design. It was specified with
`CONFIRMED → CANCELLED` present before any payment code existed.

`[Gotcha]` **The two race conditions to think about:**

**Race 1: webhook vs expiration-watcher.** The hold expires at the same moment
the payment succeeds. Both want to move the row out of `PENDING_PAYMENT`.
`TransitionStatus`'s CAS resolves it: exactly one gets `(true, nil)`.

The *correct* outcome depends on whether the money actually moved. If the
webhook won, the reservation is `CONFIRMED` and the watcher's write is rejected —
and the watcher must **not** release the seat, because the customer paid. This
means the watcher's `Compensate` needs to re-read after a lost CAS and only
release if the status is genuinely `EXPIRED`. Worth an explicit comment, because
the naive reading ("my key expired, so I release the seat") is wrong.

**Race 2: webhook vs `POST /cancel`.** Same shape. If cancel wins, the
reservation is `CANCELLED` and the webhook's `→ CONFIRMED` is rejected — but the
money may already have moved. `payment_intent.succeeded` then arrives for a
cancelled reservation.

**This is the case that needs a decision.** Options:

1. Log + alert. Someone refunds manually. Simple, safe, manual.
2. Automatically refund. Aggressive — correct, but only if we're confident the
   webhook is right.
3. Transition `CANCELLED → CONFIRMED` (resurrect). **Wrong** — CANCELLED is
   terminal by design, and the seat is already back in inventory.

`[Open]` Option 1 for v1. It's honest about the edge case, and Layer 3's
`REFUND_INITIATED` audit event gives us somewhere to record it. Worth writing
this down before implementing, because the "obvious" fix under time pressure is
option 3, which is a bug.

## 35. The failure matrix

Every failure mode and the correct response:


| Failure                                | Detect                       | Response                      | Notes                                                      |
| ---------------------------------------- | ------------------------------ | ------------------------------- | ------------------------------------------------------------ |
| Bad/missing signature                  | `webhook.Err*`               | `400`, no processing          | Don't retry                                                |
| Duplicate event                        | `ON CONFLICT` returns 0 rows | `200`, skip                   | Idempotent                                                 |
| Unknown event type                     | `default:` case              | `200`, log                    | Don't 4xx — Stripe would retry forever                    |
| `livemode` mismatch                    | field compare                | `400`, alert                  | Defence in depth                                           |
| Reservation not found                  | DB lookup misses             | `200`, log WARN               | Not our error — don't make Stripe retry                   |
| CAS lost the race                      | `(false, nil)`               | `200`                         | Expected, normal                                           |
| CAS invalid edge                       | `ErrInvalidTransition`       | `200`, log ERROR              | Someone needs to look                                      |
| Postgres down                          | pgx error                    | **`500`**                     | Stripe retries for 3 days                                  |
| Redis down (seat release)              | redis error                  | `200`, log WARN               | Convention #11 — phantom seat, countable                  |
| Stripe API 5xx on `/pay`               | `stripe.Error`               | `503`                         | Client retries                                             |
| Stripe `card_error` on `/pay`          | `se.Type`                    | `402` + decline code          | Expected client outcome                                    |
| Stripe auth error                      | `se.Type`                    | `500`, page someone           | Our config is broken                                       |
| Hold expired before `/pay`             | Redis key missing            | `409 hold_expired`            | DB row may still say PENDING                               |
| Webhook lands after 3-day retry window | Event absent from Stripe     | **Alert, reconcile manually** | The `stripe events resend` escape hatch exists for 30 days |

`[Locked]` **Redis failure never fails a webhook.** AGENTS.md convention #11 is
explicit:

> *"A partial Redis failure after a successful DB write is not a 5xx... Same
> policy for a failed idempotency cache Store: at-most-once beats
> double-booking."*

`[Gotcha]` **Don't return 5xx for "I don't recognize this event."** Stripe
retries non-2xx for 3 days. A handler that 400s unknown types would retry every
unsubscribed event it receives forever. `200` + log.

---

# Part 8 — Open questions

Consolidated so these get decided rather than discovered:

1. **Sync or async webhook processing?** (§17, §31) — Async reuses the SQS
   plumbing and keeps the handler trivially fast. Sync is simpler and avoids a
   new failure mode (event accepted but SQS publish failed).
2. **Table name.** `payment_intents` (roadmap fidelity) vs
   `reservation_payments` (avoids shadowing the Stripe concept).
3. **Nullable `stripe_payment_intent_id`** to survive a crash between INSERT
   and Stripe `Create`. (§30)
4. **Stripe in `/readyz` or not?** (§29) — leaning no; metric + alert instead.
5. **Add a `stripe-listen` Makefile target?** (§23)
6. **Auto-refund on the cancel-vs-webhook race, or alert for manual handling?**
   (§34 race 2) — leaning alert, because auto-refund on a race condition is
   its own class of bug.
7. **Price source** — hardcode in seed, or add `price_cents` to `events`? (§28)
8. **Sync webhook or SQS — and if SQS, a new queue or the existing one?**
   (§17) The roadmap reserves `reservations-notifications` for item 8, so a
   third queue `stripe-events` may be warranted.
9. **`EventType` constants vs string literals** — v82 has typed constants
   (`stripe.EventTypePaymentIntentSucceeded`), verified. Use them, but note the
   thin-event type names (`v1.billing...`) have no constants, so a mixed setup
   would be inconsistent. We chose snapshot in §14, so this is fine.

---

# Appendix A — Setup checklist

```bash
# 1. Install the CLI (the local-dev linchpin)
brew install stripe/stripe-cli/stripe
stripe login

# 2. Get test keys from the dashboard
#    https://dashboard.stripe.com/test/apikeys
#    - Secret key      → sk_test_...
#    - Publishable key → pk_test_...   (frontend only, never in Go)

# 3. Get a webhook secret
stripe listen --forward-to localhost:8080/webhooks/stripe
#    → whsec_...   (changes every run — this is normal)

# 4. Add to .env (host mode) / .env.docker (docker mode)
#    STRIPE_SECRET_KEY=sk_test_...
#    STRIPE_WEBHOOK_SECRET=whsec_...
#    STRIPE_CURRENCY=usd
#    STRIPE_PRICE_CENTS=2500

# 5. Verify wiring
curl -s localhost:8080/readyz | jq
#    expect a "stripe" key

# 6. Fire a test event
stripe trigger payment_intent.succeeded
```

**Dashboard URLs:**

- Test keys: [https://dashboard.stripe.com/test/apikeys](https://dashboard.stripe.com/test/apikeys)
- Webhook endpoints: [https://dashboard.stripe.com/webhooks](https://dashboard.stripe.com/webhooks)
- Test payments: [https://dashboard.stripe.com/test/payments](https://dashboard.stripe.com/test/payments)
- Test logs: [https://dashboard.stripe.com/test/logs](https://dashboard.stripe.com/test/logs)

---

# Appendix B — Test card numbers

Stripe's test cards. **No money moves; no card number is real.**


| Card                  | Outcome                                                                                    |
| ----------------------- | -------------------------------------------------------------------------------------------- |
| `4242 4242 4242 4242` | ✅ Succeeds                                                                                |
| `4000 0000 0000 9995` | ❌ Declined (insufficient funds)                                                           |
| `4000 0000 0000 0002` | ❌ Declined (generic card declined)                                                        |
| `4000 0000 0000 0069` | ❌ Declined (expired card)                                                                 |
| `4000 0000 0000 0119` | ⚠️ Processing error                                                                      |
| `4000 0025 0000 3155` | ✅ Requires 3D Secure (SCA) — great for testing `requires_action`                         |
| `4000 0000 0000 0259` | ✅ Succeeds but as a **fraudulent** charge — triggers `charge.dispute` if you then refund |
| `4000 0000 0000 0341` | ✅ Attaching it to a Customer succeeds                                                     |

Any future expiry / any CVC / any postal code.

`[Gotcha]` **`4000 0000 0000 3155` is the one people forget.** It's the only
way to exercise the `requires_action` / 3D Secure path, which is where a lot of
real payment bugs live. Worth a test case.

---

# Appendix C — Local testing recipes

```bash
# ── Forward events, filtered to the three we care about ──
stripe listen --forward-to localhost:8080/webhooks/stripe \
  --events payment_intent.succeeded,payment_intent.payment_failed,charge.refunded

# ── Trigger each event ──
stripe trigger payment_intent.succeeded
stripe trigger payment_intent.payment_failed
stripe trigger charge.refunded

# ── Resend a real event (up to 30 days) ──
stripe events resend evt_ABC123 --webhook-endpoint=we_123456

# ── List what Stripe thinks happened ──
stripe events list --limit 20
stripe payment_intents retrieve pi_ABC123

# ── See live API calls + responses, with curl detail ──
stripe listen --verbose

# ── Confirm the raw signature header is arriving ──
#    (in the handler, temporarily)
slog.InfoContext(ctx, "stripe event",
    "sig_header_present", r.Header.Get("Stripe-Signature") != "",
    "body_len", len(payload))
```

**Testing the failure paths deliberately** — this is where most of the value
is:


| Test              | How                                                                                               |
| ------------------- | --------------------------------------------------------------------------------------------------- |
| Bad signature     | `curl -X POST localhost:8080/webhooks/stripe -d '{}'` (no header) → expect `400`                 |
| Wrong secret      | Set a bogus `STRIPE_WEBHOOK_SECRET` → expect `400` on every event                                |
| Duplicate         | Trigger the same event twice, or `stripe events resend` the same `evt_` → second must be a no-op |
| Out-of-order      | Confirm, then refund, then confirm again → must not resurrect `CANCELLED`                        |
| livemode mismatch | Send a test event at a prod-configured endpoint → expect `400`                                   |
| Hold expired      | `POST /pay` after the hold TTL → expect `409 hold_expired`                                       |
| Card declined     | `4000 0000 0000 0002` → expect `402` + decline code                                              |
| 3D Secure         | `4000 0000 0000 3155` → expect `requires_action`, then `succeeded` on completion                 |

The out-of-order test is the most valuable one, because §16 tells you Stripe
makes no ordering promise and that's the property most likely to be
under-tested.

---

# See also

**Project docs**

- [product-roadmap.md](./product-roadmap.md) — Layer 2, item 7 is this spec
- [architecture.md](./architecture.md) — the four Go services and how they talk
- [AGENTS.md](../AGENTS.md) — conventions this plan follows (#9 transitions,
  #11 partial failure, #3 no external services in tests)
- [comprehensive-go.md](./comprehensive-go.md) — Go language reference
- [comprehensive-redis.md](./comprehensive-redis.md) — Redis and the hot path

**Stripe**

- [Payment Intents guide](https://docs.stripe.com/payments/payment-intents)
- [Payment status updates](https://docs.stripe.com/payments/payment-intents/verifying-status)
- [Webhooks](https://docs.stripe.com/webhooks)
- [Payment Intents API reference](https://docs.stripe.com/api/payment_intents)
- [Idempotent requests](https://docs.stripe.com/api/idempotent_requests)
- [Stripe CLI](https://docs.stripe.com/stripe-cli)
- [pkg.go.dev/stripe-go/v82](https://pkg.go.dev/github.com/stripe/stripe-go/v82)
