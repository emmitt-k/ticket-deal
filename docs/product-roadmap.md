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
| `/metrics` | GET | none (host-only) | Prometheus scrape (per service) |
| `/api/tickets/enter` | POST | none | Returns JWT or queues (Phase 1 waiting room) |
| `/api/tickets/queue` | GET | queue token | SSE position stream |
| `/api/tickets/reserve` | POST | JWT | Atomic seat lock → SQS publish |

That's the **complete public surface**. Five endpoints. Behind the scenes there
are 8 docker services, 14 internal packages, 5 migrations, and a 4-pillar
observability stack — but **none of that is exposed to a user**.

### What's there but unreachable

The `reservations.status` column has a `CHECK` constraint allowing four values:

```sql
CHECK (status IN ('PENDING_PAYMENT', 'CONFIRMED', 'EXPIRED', 'CANCELLED'))
```

But **only `PENDING_PAYMENT` and `EXPIRED` are actually reachable in code today**:

| Status | Reached via | Source code |
|---|---|---|
| `PENDING_PAYMENT` | Worker `db.InsertIfAbsent` | `internal/db/reservations.go` |
| `EXPIRED` | Watcher + sweep job | `internal/expire/compensate.go` |
| `CONFIRMED` | **nothing** — defined but no path | ❌ no handler (Layer 2, item 7 — Stripe webhook) |
| `CANCELLED` | POST /api/reservations/{id}/cancel | `internal/api/cancel_reservation_handler.go` ✅ |

This is the single biggest "code-defined-but-unwired" gap. Adding the missing
transitions is Layer 2, item 7.

> **Note:** `CONFIRMED → CANCELLED` *is* wired (as the refund edge — it was
> specified into the state machine in Layer 1 and is now recorded as a
> `REFUND_INITIATED` audit event), but nothing can reach `CONFIRMED` yet
> because there's no payment integration. That's item 7.

### What's not there at all (not even a stub)

- No payment integration — `CONFIRMED` is unreachable (see above)
- No webhook receiver — systems can't be told things changed
- No notifications service — second consumer on the queue pattern
- No admin/ops API — no way for support to look up / cancel / refund
- No `/readyz` — only bare `/healthz`, no deep dep checks *(Layer 1 item 4 — done; see branch `feature/readyz`)*
- No anti-bot/CAPTCHA — only IP rate-limit via waiting room
- No idempotency keys on POST /reserve — network retries could double-charge. *(Layer 1 item 5 — done; see branch `feature/idempotency-key`)*
- ~~No audit log — `who did what when` is unanswerable~~ *(Layer 2 item 6 — done; see branch `feature/audit-log`)*

---

## Layer 1 — "Make it a usable product"

> **Goal:** the system looks like a real product on the outside. Small,
> focused, one weekend's worth of work. Builds the spine that Layer 2 hangs off.

### 1. Reservation state machine (with typed transitions) ✅ Done

**Effort:** ~half a day. **Blocks:** items 2, 3, 8 (admin), 6 (Stripe).

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

> **Shipped** on `feature/audit-log`. The table, the append-only trigger, and the
> atomic write on every transition. Notes on what changed vs the sketch below:
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
  `CONFIRMED → CANCELLED` is a `REFUND_INITIATED` (money moved back);
  `PENDING_PAYMENT → CANCELLED` is a `CANCELLED` (nothing was paid). Same
  destination status, completely different meaning to an operator.
- **Append-only is enforced by the database.** A `BEFORE UPDATE OR DELETE`
  trigger refuses `DELETE` and refuses every `UPDATE` except rewriting
  `user_id` (the GDPR anonymisation path this very section anticipates).

### 7. Stripe test-mode integration (THE headline feature)

**Effort:** ~3-4 days. **Why it's #2 here:** audit log lets you safely debug webhooks.

Adds:
- New migration: `payment_intents` table (maps reservation_id ↔ stripe payment_intent_id)
- New env vars: `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_PUBLISHABLE_KEY`
- New endpoint: `POST /api/reservations/:id/pay` — creates a Stripe PaymentIntent in
  test mode, returns `client_secret` so a test front-end could confirm it
- New endpoint: `POST /webhooks/stripe` — verifies `Stripe-Signature` header,
  switches on event type:
  - `payment_intent.succeeded` → transition PENDING_PAYMENT → CONFIRMED, idempotent
  - `payment_intent.payment_failed` → leave PENDING (user can retry); emit a metric
  - `charge.refunded` → transition CONFIRMED → CANCELLED (refund = cancel in our model)
- Trace everything: webhooks come in without an active span; create a root span
  on receipt using the trace_id from the body's `_traceparent` (we already have
  this pattern in SQS)

This is **the single most commercially-impressive feature**. It exercises:
signature verification, webhook idempotency, race conditions (webhook vs.
expiry-watcher), payment state, refund-vs-cancel semantics. Recruiters know it.

### 8. Notifications service (email via MailHog, SMS via Twilio test creds)

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

### 9. Admin / ops API (gated by HMAC admin key)

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

### 10. Anti-bot hardening

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

### 11. Graceful degradation (circuit breakers)

**Effort:** ~1 day. **Why:** when Redis is degraded, `/reserve` should return
503 after 100ms, not hang for 30s.

Use `github.com/sony/gobreaker` (or roll your own in 50 LOC). Wrap:
- Redis: if open → return 503 with `Retry-After`
- Postgres: if open → return 503 (worker drains queue normally)
- SQS: publisher should retry with exponential backoff; if open, log and
  continue (hold-key orphan scenario, already known — see §9 in
  `architecture.md`)

Each circuit breaker emits metrics: `circuit_breaker_state{name}`, `circuit_breaker_trips_total{name}`.

### 12. Multi-event (bulk load + listing)

**Effort:** ~1 day. Schema is ready, data is hardcoded.

- New migration: `004_events_seed.sql` with 10 realistic events (concert, sports,
  theater, conference)
- New endpoint: `GET /api/events` (no auth, public list)
- A `cmd/seed-events` tool that reads a YAML and bulk-inserts into Postgres + Redis

### 13. Redis Cluster / sharding

**Effort:** ~2-3 days (mostly design + testing).

Document the sharding key decision (`event_id % N` vs `hash(all_seats) % N`),
what breaks (Lua scripts cross-slot, keyspace notifications are per-node),
and what the migration plan looks like. You don't have to deploy it — the
**design document** is the deliverable that proves you think this far ahead.

### 14. GDPR endpoints

**Effort:** ~half a day.

```
GET  /api/me/data-export    → 200 application/zip with all my PII
DELETE /api/me              → 410 with confirmation; purges PII, anonymizes audit log
```

The audit log can't be deleted (regulators need the trail), so anonymize instead:
`UPDATE reservation_events SET user_id='redacted:<hash>', metadata=metadata - 'pii'
WHERE user_id = $1`.

### 15. Feature flags (env-keyed, no SaaS)

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

**If you have one weekend:** Layer 1 in order — 1 → 2 → 3 → 4 → 5. Small,
focused, every commit ships a working feature. End-state: the system *feels*
complete.

**If you have two weekends:** add 6 → 7 (audit + Stripe). Now it has payment,
which is the headline. Recruiters will see it. *(Item 6 is now done — only 7 remains.)*

**If you have a month:** full Layer 1 + Layer 2. Notifications + admin + anti-bot
+ Stripe. This is what a senior backend engineer at Stripe or Ticketmaster
actually shipped during their first year. The portfolio goes from "I built a
ticket app" to "I built a payment-grade distributed system with admin tooling."

## The strategic question

The biggest impact-portfolio item, ranked:

| Rank | Feature | Why it's the differentiator |
|---|---|---|
| 1 | **Stripe test mode** (#7) | Forces you to solve every pattern real engineers solve: signature verification, webhook idempotency, payment state, race-vs-expiry. Recruiters recognize it. |
| 2 | **State machine + cancel** (#1, #3) | The connective tissue. Makes Layer 2 possible. |
| 3 | **Audit log** (#6) | "Who did this when?" is the SOC2 question. ✅ Shipped |
| 4 | **Anti-bot** (#10) | Proves you think like an attacker. |
| 5 | **Notifications** (#8) | Tests the design's flexibility claim. |
| 6 | **Admin API** (#9) | Every real product has this; usually glossed over. |

Stripe is the headline. State machine is the prerequisite. Build #1 → #2 → #3
→ #6 → #7 in that order. The rest fill in around them.

---

## See also

- [`architecture.md`](architecture.md) — the engineering manual (what every
  file does, why)
- [`tracing-crashcourse.md`](tracing-crashcourse.md) — learn OpenTelemetry from
  scratch (background for the observability work already done)
- [`comprehensive-redis.md`](comprehensive-redis.md) — Lua atomicity, keyspace
  notifications
- [`comprehensive-go.md`](comprehensive-go.md) — Go patterns used here
