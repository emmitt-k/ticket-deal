# Product Roadmap — What ticket-deal needs to become a real product

> **Audience:** the builder (you). Goal: one-pager of where we are, what we're
> missing, and what to build next — ordered by impact, sized by effort.

---

## TL;DR

You built an **infrastructure-grade reservation kernel** (atomic Lua seat
locking, SQS-backed async fulfillment, two-path expiration, full 4-pillar
observability). What it lacks is the **product surface** — anything that makes
it usable as a real product, not just an inner loop.

Three layers, in order:

| Layer | Theme | Time | Outcome |
|---|---|---|---|
| **Layer 1** | Make it a usable product | 1–2 weekends | Looks like a real product on the outside |
| **Layer 2** | Make it a *real* product | 2–4 weekends | Looks like a real product on the inside too |
| **Layer 3** | Production-grade hardening | as needed | Stands up under real-world failure |

You don't have to do them in order — but the earlier items in each layer
unblock the later ones (state machine → cancel → refund → audit → Stripe webhooks).

---

## Where we are right now

### What's exposed today

| Route | Method | Auth | Purpose |
|---|---|---|---|
| `/healthz` | GET | none | Bare liveness |
| `/readyz` | GET | none | Readiness — 3 parallel dep checks (Redis / Postgres / SQS) |
| `/metrics` | GET | none (host-only) | Prometheus scrape (per service) |
| `/api/tickets/enter` | POST | none | Returns JWT or queues (Phase 1 waiting room) |
| `/api/tickets/queue` | GET | queue token | SSE position stream |
| `/api/tickets/reserve` | POST | JWT | Atomic seat lock → SQS publish |
| `/api/reservations/{id}` | GET | JWT | One reservation. `403` if it isn't yours |
| `/api/me/reservations` | GET | JWT | The caller's own list — `?status=`, `?limit=`, `?offset=` |
| `/api/reservations/{id}/cancel` | POST | JWT | Cancel a `PENDING_PAYMENT` reservation; releases the seat |
| `/api/reservations/{id}/pay` | POST | JWT | Create the Stripe PaymentIntent; returns the `client_secret` |
| `/webhooks/stripe` | POST | Stripe signature | Stripe's callback. Verifies, snapshots, queues — no state change here |

That's the **complete public surface**. Eleven endpoints — five from the original
kernel, four added by Layer 1, two added by Layer 2 item 7. Behind the scenes there
are 13 docker services, 15 internal packages, 8 migrations, and a 4-pillar
observability stack.

The read endpoints are what make the seat price reachable by a client:
`GET /api/reservations/{id}` and `GET /api/me/reservations` return
`unit_price_cents`, `currency` and the computed `total_price_cents` — the
**server's** number, never a client-supplied one.

### What's there but unreachable

The `reservations.status` column has a `CHECK` constraint allowing five values:

```sql
CHECK (status IN ('PENDING_PAYMENT', 'CONFIRMED', 'EXPIRED', 'CANCELLED', 'REFUNDED'))
```

**All five are reachable in code today**:

| Status | Reached via | Source code |
|---|---|---|
| `PENDING_PAYMENT` | Worker `db.InsertIfAbsent` | `internal/db/reservations.go` |
| `EXPIRED` | Watcher + sweep job | `internal/expire/compensate.go` |
| `CONFIRMED` | Stripe `payment_intent.succeeded` webhook | `internal/payment/handler.go` ✅ |
| `CANCELLED` | POST /api/reservations/{id}/cancel | `internal/api/cancel_reservation_handler.go` ✅ |
| `REFUNDED` | Stripe `charge.refunded` webhook (full refund only) | `internal/payment/handler.go` ✅ |

Both of those gaps are now closed: Layer 2 item 6 wired the transitions and item 7
gave them a way to be reached.

> **Note:** `REFUNDED` was split out of `CANCELLED` in
> `migrations/006_refunded_status.sql`: "the customer never paid" and "the
> customer paid and we gave the money back" are different facts and were sharing
> one status.
>
> The refund edge (`CONFIRMED → REFUNDED`) is wired, and the audit log records it
> as a `REFUND_INITIATED` event — the **event** says what was done (money came
> back), the **status** says where the reservation ended up. That separation is
> what lets a *partial* refund be an event while the status stays `CONFIRMED`.
> Only a full refund becomes a state.

### What's not there at all (not even a stub)

- ~~No payment integration — `CONFIRMED` is unreachable~~ *(Layer 2 item 7 — done; see branch `feature/stripe-integration`)*
- ~~No webhook receiver — systems can't be told things changed~~ *(Layer 2 item 7 — done)*
- No notifications service — second consumer on the queue pattern
- No admin/ops API — no way for support to look up / cancel / refund
- ~~No `/readyz` — only bare `/healthz`, no deep dep checks~~ *(Layer 1 item 4 — done; see branch `feature/readyz`)*
- No anti-bot/CAPTCHA — only IP rate-limit via waiting room
- ~~No idempotency keys on POST /reserve — network retries could double-book~~ *(Layer 1 item 5 — done; see branch `feature/idempotency-key`)*
- ~~No audit log — `who did what when` is unanswerable~~ *(Layer 2 item 6 — done; see branch `feature/audit-log`)*

---

## Layer 1 — "Make it a usable product"

> **Goal:** the system looks like a real product on the outside. Small,
> focused, one weekend's worth of work. Builds the spine that Layer 2 hangs off.

### 1. Reservation state machine (with typed transitions) ✅ Done

**Effort:** ~half a day. **Blocks:** items 2, 3, 9 (admin), 6 (Stripe).

Today, status transitions are implicit (worker INSERT sets PENDING, watcher
overwrites to EXPIRED). Make them explicit:

```go
// internal/db/reservations.go (add this)
type Status string
const (
    StatusPendingPayment Status = "PENDING_PAYMENT"
    StatusConfirmed      Status = "CONFIRMED"
    StatusExpired        Status = "EXPIRED"
    StatusCancelled      Status = "CANCELLED"
)

// Allowed transitions table (compile-time enforced)
var validTransitions = map[Status][]Status{
    StatusPendingPayment: {StatusConfirmed, StatusExpired, StatusCancelled},
    StatusConfirmed:      {StatusCancelled},  // refund-as-cancel for simplicity
}
```

Add `internal/db/transition.go` with `TransitionStatus(ctx, pool, id, from, to)` —
it runs the CAS UPDATE with a `WHERE status = $from` clause so concurrent
cancellations can't double-flip. Returns `ErrInvalidTransition` if the row is in
the wrong state.

> **Shipped**, with three differences from the sketch above. The constants are
> plain `string` consts rather than a named `Status` type, so `validTransitions`
> is `map[string][]string` — the edge table is *runtime*-enforced, with typed
> constants catching typos at the call site. `TransitionStatus` takes a
> **mandatory** `db.Audit` argument, because since Layer 2 item 6 every
> transition writes an audit row in the same transaction and an empty actor is
> rejected before any SQL runs. And the refund edge is now
> `CONFIRMED → REFUNDED`, not `CONFIRMED → CANCELLED` (see item 6).

### 2. `GET /api/reservations/:id` and `GET /api/me/reservations` ✅ Done

**Effort:** ~2 hours. **Unblocks:** the dashboard-the-user-sees use case.

`db.GetReservation(ctx, pool, id)` already exists. Wrap it:

```
GET /api/reservations/:id       → 200 ReservationRow | 404 | 403 (not yours)
GET /api/me/reservations?status=PENDING_PAYMENT&limit=20  → 200 []{...}
```

Both require JWT auth. The second one is paginated (cursor or offset+limit; offset
is fine for now). The `ReservationRow` JSON shape is the same thing the worker
writes — define it in `internal/db/types.go` once and reuse.

### 3. `POST /api/reservations/:id/cancel` ✅ Done

**Effort:** ~half a day. **Unlocks:** the most common user action in any ticketing
system.

The handler:

1. Verify the JWT matches the reservation's `user_id`. (Rejects cross-user access
   attempts.)
2. Verify status is `PENDING_PAYMENT` (can't cancel after payment — that's a
   refund, which is Layer 2).
3. Call `db.TransitionStatus(... PENDING_PAYMENT → CANCELLED)`. This is a CAS —
   if the watcher got there first with EXPIRED, transition fails and we return
   409 `already_expired`.
4. Atomically release the seat back to Redis: `INCRBY inventory:event:<id> <seats>`.
5. Best-effort: delete the hold key so a new `POST /reserve` from the same user
   doesn't 409 (`already_holding`).

Item 1's transition function makes this safe by construction.

### 4. `/readyz` endpoint with deep dep checks ✅ Done

**Effort:** ~1 hour. **Why:** k8s rolling deploys, ALB health checks, monitoring.

```
GET /healthz  → 200 always (process is alive)
GET /readyz   → 200 if {redis ping OK, pg SELECT 1 OK, sqs ListQueues OK} else 503
```

The `/readyz` checks should each have a short timeout (~500ms) and run
**in parallel** so a slow Postgres doesn't block the whole check. Return a JSON
body showing which check failed for ops debugging:

```json
{"redis":"ok","postgres":"ok","sqs":"timeout after 500ms","ready":false}
```

### 5. Idempotency-Key on POST /reserve ✅ Done

**Effort:** ~half a day. **Why:** network retries shouldn't double-charge the user.

Add `Idempotency-Key: <uuid>` header support:

1. If header present, check Redis: `GET idempotency:<key>` → returns cached
   response if hit.
2. If miss, run the normal reserve flow. On success, `SETEX idempotency:<key>
   86400 <json-response>` (24h TTL — long enough for retries, short enough to
   self-clean).
3. Return the response (whether new or cached).

Subtle: the cached response must include the `reservation_id` from the *original*
request, not a new one. This prevents the user's retry from looking like a fresh
reservation.

Stripe-grade. Real systems live and die by this.

### Layer 1 deliverable

After this layer, the system has:
- ✅ A real state machine users can drive  *(Layer 1 item 1 — done)*
- ✅ Look-up their own tickets  *(Layer 1 item 2 — done)*
- ✅ Cancel them  *(Layer 1 item 3 — done; branch `feature/cancel-reservation`)*
- ✅ Tell ops "I'm healthy and all my deps are reachable"  *(Layer 1 item 4 — done; branch `feature/readyz`)*
- ✅ Network-retry safely  *(Layer 1 item 5 — done; branch `feature/idempotency-key`)*

About 1 weekend of focused work. Small commits. Each item is independent.

---

## Layer 2 — "Make it a *real* product"

> **Goal:** the inside matches the outside. This is where you go from "good
> backend" to "scary engineer." Audit log first because it makes everything else
> safely debuggable.

### 6. Audit log (event-sourced append-only) ✅ Done

**Effort:** ~1 day. **Why first:** refunds without audit = SOC2 fail. Admin
actions without audit = unanswerable "who did this?"

> **Shipped** — merged into `main` (PRs #21 and #22). The table, the append-only
> trigger, and the atomic write on every transition. Notes on what changed vs the sketch below:
> the migration is `005_audit_log.sql` (not `004` — that number was taken), and
> `user_id` was added to the table because Layer 3's GDPR sweep needs to find a
> user's rows by `user_id` alone.

```sql
-- migrations/005_audit_log.sql
CREATE TABLE reservation_events (
    event_id        BIGSERIAL PRIMARY KEY,
    reservation_id  UUID NOT NULL REFERENCES reservations(reservation_id),
    user_id         TEXT NOT NULL,   -- denormalised for the GDPR sweep
    event_type      TEXT NOT NULL CHECK (event_type IN (
                        'CREATED','CONFIRMED','EXPIRED','CANCELLED','REFUND_INITIATED'
                    )),
    actor_type      TEXT NOT NULL,   -- 'user' | 'system' | 'admin' | 'stripe_webhook'
    actor_id        TEXT,            -- user_id, 'watcher', admin key id, stripe event id
    old_status      TEXT,
    new_status      TEXT,
    trace_id        TEXT,            -- from ctx; lets you join with Jaeger
    metadata        JSONB,           -- event-specific (e.g. stripe charge id)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

Every status transition goes through one wrapper that does:

```sql
BEGIN;
  UPDATE reservations SET status = $to WHERE reservation_id = $id AND status = $from;
  INSERT INTO reservation_events (...) VALUES (...);
COMMIT;
```

The audit row has the OTel `trace_id` so an ops person can paste it into Grafana
→ "Ticket Deal — Logs" and see every log line for the transition.

**Two design decisions worth keeping:**

- **The event type is derived from the EDGE, not the destination status.**
  `CONFIRMED → REFUNDED` is a `REFUND_INITIATED` (money moved back);
  `PENDING_PAYMENT → CANCELLED` is a `CANCELLED` (nothing was paid). Two
  different facts, completely different meaning to an operator.
- **An EVENT and a STATUS are different concepts.** The event records the
  *action* (`REFUND_INITIATED` — the money went back); the status records the
  *outcome* (`REFUNDED`). That is what lets a **partial** refund be recorded as
  an event while the status stays `CONFIRMED` — only a full refund becomes a
  state.
- **Append-only is enforced by the database.** A `BEFORE UPDATE OR DELETE`
  trigger refuses `DELETE` and refuses every `UPDATE` except rewriting
  `user_id` (the GDPR anonymisation path this very section anticipates).

### 7. Stripe test-mode integration (THE headline feature) ✅ Done

**Effort:** ~3-4 days. **Why it's #2 here:** audit log lets you safely debug webhooks.

> **Shipped** on `feature/stripe-integration`. Notes on what changed vs the sketch
> below:
>
> - The migration is `008_stripe_payments.sql` (005–007 were taken) and the table is
>   `payment_intents`, with a **nullable** `stripe_payment_intent_id`.
> - **The webhook is async**, on a third SQS queue `stripe-events`, consumed by a new
>   `cmd/stripe-worker` binary. The handler verifies the signature, snapshots the event
>   and queues it — it never touches the database. The publish happens BEFORE the 200,
>   so "the event was accepted but the publish failed" cannot lose a payment.
> - **The idempotency key is the `reservation_id`.** That is what makes the crash
>   window between "we wrote the payment row" and "Stripe answered" a retry rather than
>   a double-charge.
> - **Stripe is OPTIONAL.** Both keys absent → the payment routes answer 503 and the
>   rest of the system is unchanged. Exactly one present → a startup error.
> - A **partial** refund records the event and leaves the status at `CONFIRMED` — the
>   payoff of keeping the event and the status as separate concepts.
>
> **The decisions are made** — Part 8 of
> [`comprehensive-stripe.md`](./comprehensive-stripe.md) is now a decision log
> rather than a list of open questions:
>
> - **Async**, on a third SQS queue `stripe-events` — the webhook verifies,
>   publishes, then returns 200. Consistent with the architecture the project
>   already uses, and the trace propagation comes free.
> - **`payment_intents`**, one row per reservation, `reservation_id UNIQUE`,
>   `stripe_payment_intent_id` **nullable** — and `reservation_id` is Stripe's
>   own idempotency key, so a crash between our INSERT and Stripe's `Create`
>   converges on a retry instead of double-charging.
> - **Stripe is not a `/readyz` dependency** (it would take the API out of
>   rotation for a third-party outage) — a metric and an alert instead. But
>   **yes** to a `stripe-listen` Makefile target for local testing.
> - **The cancel-vs-webhook race alerts for manual handling** — it does not
>   auto-refund. Money should not move on a path that didn't reason about
>   idempotency and partial refunds.
> - **Typed `EventType` constants**, and the price comes from the reservation's
>   snapshot (already shipped).
>
> **Migration numbering:** the Stripe doc sketches `005_stripe_payments.sql`.
> That number is taken — the audit log is 005, and `006_refunded_status.sql` +
> `007_event_price.sql` follow it. The payment table's migration is
> **`008_stripe_payments.sql`**.

Adds:
- New migration: `payment_intents` table (maps reservation_id ↔ stripe payment_intent_id)
- The PaymentIntent amount comes from the reservation's **price snapshot**
  (`unit_price_cents × seats`) — already implemented in `007_event_price.sql`,
  so no price lookup is needed here. The client never names the amount.
- New env vars: `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_PUBLISHABLE_KEY`
- New endpoint: `POST /api/reservations/:id/pay` — creates a Stripe PaymentIntent in
  test mode, returns `client_secret` so a test front-end could confirm it
- New endpoint: `POST /webhooks/stripe` — verifies `Stripe-Signature` header,
  switches on event type:
  - `payment_intent.succeeded` → transition PENDING_PAYMENT → CONFIRMED, idempotent
  - `payment_intent.payment_failed` → leave PENDING (user can retry); emit a metric
  - `charge.refunded` → transition CONFIRMED → REFUNDED (a refund is not a cancel)
- Trace everything: webhooks come in without an active span; create a root span
  on receipt using the trace_id from the body's `_traceparent` (we already have
  this pattern in SQS)

This is **the single most commercially-impressive feature**. It exercises:
signature verification, webhook idempotency, race conditions (webhook vs.
expiry-watcher), payment state, refund-vs-cancel semantics. Recruiters know it.

### 8. Notifications service (email via MailHog, SMS via Twilio test creds) ⬜ Not started

**Effort:** ~1-2 days. **Why:** tests the design's claim that adding a 2nd
consumer to the queue is trivial.

Adds a second SQS queue (`reservations-notifications`):

```
Worker (existing) ──reservations DB write──> reservations table
NotificationWorker (new)  ──reservations DB write + SQS fanout──> email/SMS
```

MailHog is added to `docker-compose.yml` (smtp on :1025, web UI on :8025). For
local dev, all "emails" land in MailHog's inbox — perfect for testing. Production
swap is `STRIPE_SECRET_KEY`-style: an env var pointing at real SES/SendGrid.

Events to send:
- `PENDING_PAYMENT → CONFIRMED` → "Your ticket is confirmed" (with QR or ticket
  text — markdown to HTML)
- `PENDING_PAYMENT → EXPIRED` → "Your hold expired, the seats are back"
- `PENDING_PAYMENT → CANCELLED` (user-initiated) → "You cancelled your reservation"

### 9. Admin / ops API (gated by HMAC admin key) ⬜ Not started

**Effort:** ~1 day. **Why:** every real product has "support override."

```
POST /admin/reservations/:id/cancel        (refund-as-cancel)
POST /admin/reservations/:id/release-seat  (manually release a stuck seat)
GET  /admin/reservations/lookup?user_id=x  (support lookup)
```

All gated by `X-Admin-Key: <hmac>` header. The HMAC is `HMAC-SHA256(admin_secret,
method + path + body + timestamp)` with a 5-minute window. Replay protection by
rejecting timestamps outside the window.

Every admin call writes to the audit log (`actor_type='admin'`, `actor_id=<key
fingerprint>`). This is why item 6 (audit log) was sequenced first.

### 10. Anti-bot hardening ⬜ Not started

**Effort:** ~1-2 days. **Why:** ticket scalpers are a real threat to any
high-demand drop.

Two layers:
1. **CAPTCHA** — invisible hCaptcha on `/reserve` (or Cloudflare Turnstile).
   Test keys work in dev. Front-end sends the token via `X-Captcha-Token`
   header; server validates it before Lua reserve.
2. **Lua-side velocity check** — extend `reserve.lua` to track per-IP
   reservation attempts in a sliding window:
   ```lua
   -- Pseudo: if INCR rate:ip:1.2.3.4 > 3 in last 60s, reject
   ```
   This catches scripted bots even if they pass CAPTCHA (a single human can
   solve 3/min, but a bot will hit 100/min immediately).

Item 1-10 of Layer 1 + Layer 2 = the system looks like a real product.

---

## Layer 3 — Production-grade hardening

> **Goal:** stands up under real-world failure. Most of these don't show in a
> demo but absolutely show in a 3am page.

### 11. Graceful degradation (circuit breakers) ⬜ Not started

**Effort:** ~1 day. **Why:** when Redis is degraded, `/reserve` should return
503 after 100ms, not hang for 30s.

Use `github.com/sony/gobreaker` (or roll your own in 50 LOC). Wrap:
- Redis: if open → return 503 with `Retry-After`
- Postgres: if open → return 503 (worker drains queue normally)
- SQS: publisher should retry with exponential backoff; if open, log and
  continue (hold-key orphan scenario, already known — see §9 in
  `architecture.md`)

Each circuit breaker emits metrics: `circuit_breaker_state{name}`, `circuit_breaker_trips_total{name}`.

### 12. Multi-event (bulk load + listing) ⬜ Not started

**Effort:** ~1 day. Schema is ready, data is hardcoded.

- New migration: `004_events_seed.sql` with 10 realistic events (concert, sports,
  theater, conference)
- New endpoint: `GET /api/events` (no auth, public list)
- A `cmd/seed-events` tool that reads a YAML and bulk-inserts into Postgres + Redis

### 13. Redis Cluster / sharding ⬜ Not started

**Effort:** ~2-3 days (mostly design + testing).

Document the sharding key decision (`event_id % N` vs `hash(all_seats) % N`),
what breaks (Lua scripts cross-slot, keyspace notifications are per-node),
and what the migration plan looks like. You don't have to deploy it — the
**design document** is the deliverable that proves you think this far ahead.

### 14. GDPR endpoints ⬜ Not started

**Effort:** ~half a day.

```
GET  /api/me/data-export    → 200 application/zip with all my PII
DELETE /api/me              → 410 with confirmation; purges PII, anonymizes audit log
```

The audit log can't be deleted (regulators need the trail), so anonymize instead:
`UPDATE reservation_events SET user_id='redacted:<hash>', metadata=metadata - 'pii'
WHERE user_id = $1`.

### 15. Feature flags (env-keyed, no SaaS) ⬜ Not started

**Effort:** ~half a day.

```go
// internal/feature/flag.go
var EnablePayment = os.Getenv("FEATURE_PAYMENT") == "true"
var EnableWaitlist = os.Getenv("FEATURE_WAITLIST") == "true"
```

Lets you toggle `enable_payment`, `enable_waitlist`, `enable_anti_bot` without
redeploying. Two lines per flag at the call site. Beats a real feature-flag
service for our scale and removes a future "I forgot to clean up" problem.

---

## What I'd build first — and why

**Done:** Layer 1 (1 → 2 → 3 → 4 → 5) and Layer 2 items 6 and 7. The system
now has the whole user-facing spine, an audit trail behind it, and it takes
money. Stripe was the only item that turned *"a reservation kernel"* into
*"a product that takes money"*, and it exercised every pattern a payments
engineer actually solves: signature verification, webhook idempotency,
out-of-order delivery, and the race between the payment webhook and the expiry
watcher.

**Next: item 9 (the admin/ops API)** — it became interesting the moment there
was money to refund. The audit log already has `actor_type = 'admin'` waiting
for it, so this is the item that makes the audit trail earn its keep rather
than just exist. **Then item 10 (anti-bot)**, the cheapest proof that you think
like an attacker. Item 8 (notifications) is the best demonstration that the
queue design scales to a second consumer, but it is the least differentiating —
so it goes third, unless the demo needs the MailHog inbox to look busy.

**Before any of them, two chores** that are cheap and make the repo honest:
CI did not run the `internal/db` tests (they skip without a Postgres, and the CI
job had no Postgres service), and ~24 untouched files carried gofmt drift. A
reviewer spots both in five seconds.

> **Both are done** (branch `feature/ci-hardening`): the CI job now has a real
> Postgres, a real Redis and a real SQS broker, so all the tests run instead of
> 146 skipping — and the coverage it reports went from **15.4%** to **49.3%**. The
> gofmt drift is gone, and a `gofmt` step now keeps it gone. The same change added a
> coverage floor and a `govulncheck` gate — which found 14 real CVEs on the way in
> (12 in the Go standard library, one in `golang.org/x/net`) and is now clean.
>
> **And the safety net grew to match the system.** Three more things came out of
> the same pass:
>
> - `internal/config` went from **0% to 100%** — it is where the both-or-neither
>   Stripe rule lives, and a bug there is a *production crash loop*. It had only
>   ever been verified by running the binary by hand.
> - `internal/queue` went from **27% to 90%** — the layer both critical paths
>   (reserve and payment) depend on. The SQS publisher and consumer now have real
>   tests against a real broker: a published message arrives, a handled message is
>   deleted, a failed handler's message comes back.
> - A third CI job, **E2E**, boots the whole Docker Compose stack and drives one
>   reservation all the way through — reserve → SQS → worker → Postgres, then a
>   signed Stripe webhook → SQS → stripe-worker → CONFIRMED — and checks the audit
>   trail. It is the only thing that can catch "every package passes and the
>   system does not work".
>
> **And it paid for itself on the first run.** The new CI caught a flaky test
> immediately: `TestReserveHandler_Idempotency_ParallelRetries` asserted that the
> loser of the Idempotency-Key race gets `409 in_flight`, but the handler holds the
> in-flight lock across publish → cache → respond → release — so if the winner
> finishes first, the loser correctly gets a 200 with the *cached response* instead.
> The test was asserting a timing window, not a contract. It had never been executed
> by anything but a developer sitting in front of the machine, so it stayed invisible
> for the life of the project. It is now deterministic (the winner is parked inside
> `Publish` until the loser has reported) and asserts the error *code*, so it fails
> when the lock is broken.

## The strategic question

The biggest impact-portfolio item, ranked:

| Rank | Feature | Why it's the differentiator | Status |
|---|---|---|---|
| 1 | **Stripe test mode** (#7) | Forces you to solve every pattern real engineers solve: signature verification, webhook idempotency, payment state, race-vs-expiry. Recruiters recognize it. | ✅ Shipped |
| 2 | **State machine + cancel** (#1, #3) | The connective tissue. Makes Layer 2 possible. | ✅ Shipped |
| 3 | **Audit log** (#6) | "Who did this when?" is the SOC2 question. | ✅ Shipped |
| 4 | **Anti-bot** (#10) | Proves you think like an attacker. | ⬜ Not started — **← next** |
| 5 | **Notifications** (#8) | Tests the design's flexibility claim. | ⬜ Not started |
| 6 | **Admin API** (#9) | Every real product has this; usually glossed over. | ⬜ Not started |

The three headline items are shipped, and they were the hard ones — the
prerequisites (the state machine, the audit log) and the money (Stripe). What
is left is the layer of polish and support tooling that makes a project look
like it was *operated*, not just written: the admin override, the bot defence,
the notifications. That is where the remaining differentiator is, and it is a
different kind of differentiator — less "impressive endpoint" and more "someone
ran this in production and thought about what goes wrong".

---

## See also

- [`architecture.md`](architecture.md) — the engineering manual (what every
  file does, why)
- [`tracing-crashcourse.md`](tracing-crashcourse.md) — learn OpenTelemetry from
  scratch (background for the observability work already done)
- [`comprehensive-redis.md`](comprehensive-redis.md) — Lua atomicity, keyspace
  notifications
- [`comprehensive-go.md`](comprehensive-go.md) — Go patterns used here
