# Mini-Ticketmaster

**A high-concurrency ticket booking system in Go.** 1,000 people click *Buy* in the same
millisecond — exactly 100 get a ticket, the other 900 get a clean `409`, and nobody ever
buys a seat that doesn't exist.

[![CI](https://github.com/emmitt-k/ticket-deal/actions/workflows/ci.yml/badge.svg)](https://github.com/emmitt-k/ticket-deal/actions/workflows/ci.yml)

> Design rationale → [`docs/architecture.md`](docs/architecture.md) · **The load-bearing
> decisions and why** → [`docs/design-decisions.md`](docs/design-decisions.md) · What's next
> → [`docs/product-roadmap.md`](docs/product-roadmap.md)

---

## Quick start

```bash
git clone https://github.com/emmitt-k/ticket-deal.git && cd ticket-deal
./scripts/setup-docker-env.sh                        # writes .env.docker + a generated JWT secret
docker compose --env-file .env.docker up -d --build  # 13 containers
```

That is the whole system: **Postgres, Redis, ElasticMQ, Jaeger, Prometheus, Grafana,
Loki, Promtail** plus **5 Go services** — the API, the reservation worker, the Stripe
worker, the expiry watcher and the k6 dashboard.

```bash
curl -s localhost:8080/readyz
# {"redis":"ok","postgres":"ok","sqs":"ok","ready":true}
```

Reserve a seat:

```bash
make env
JWT=$(USER=demo EVENT=1 make -s mintjwt)

curl -s -X POST localhost:8080/api/tickets/reserve \
  -H "Authorization: Bearer $JWT" -H "Content-Type: application/json" \
  -d '{"seats_requested":1}'
# {"reservation_id":"…","seats":1,"expires_in":600}
```

**You need:** Docker Desktop. Go 1.27 and k6 only if you plan to build or load-test.

---

## Architecture

### The 3-phase gatekeeper

```
       1,000 concurrent requests
                  │
                  ▼
      ┌─────────────────────────┐
      │ 1. Virtual waiting room │ ── token bucket + fair FIFO queue (Redis)
      └────────────┬────────────┘
                   │ signed JWT
                   ▼
      ┌─────────────────────────┐
      │ 2. Atomic seat locking  │ ── ONE Redis Lua script decides the winner
      └────────────┬────────────┘
                   │ 10-minute hold
                   ▼
      ┌─────────────────────────┐
      │ 3. Async fulfillment     │ ── SQS → worker → Postgres (durable, retriable)
      └─────────────────────────┘
```

The rule: **never put the database on the hot path of seat allocation.**

1. **Shed load.** A virtual waiting room — a Redis token bucket plus a fair queue. The
   API admits what it can serve right now and parks the rest with a position and an ETA.
2. **Lock atomically.** One Lua script decides who gets a seat. Redis executes it on a
   single thread, so it is atomic: 1,000 concurrent callers, 100 seats, 100 winners, and
   no loser who thinks they won. The same script carries a per-user hold, so one person
   cannot double-book.
3. **Fulfill asynchronously.** The reservation goes onto a queue and a worker writes the
   durable row. The customer is already holding their seat; the write is a formality that
   can be retried forever, because the row is keyed by a UUID and the insert is
   `ON CONFLICT DO NOTHING`.

### The state machine

Every status change in the system goes through one function:

```go
db.TransitionStatus(ctx, pool, id, from, to, db.Audit{Actor: …})
```

A compare-and-set `UPDATE` guarded by a table of legal edges — so two writers cannot
both win — with the **audit row written in the same transaction**. The audit actor is
mandatory: a transition that cannot be recorded cannot happen.

```
PENDING_PAYMENT ──▶ CONFIRMED ──▶ REFUNDED
      │  │
      │  └──────────▶ EXPIRED
      └─────────────▶ CANCELLED
```

`CANCELLED` and `REFUNDED` are deliberately different facts: *"the customer never paid"*
versus *"the customer paid and we gave the money back"*. A partial refund is recorded as
an **event** without changing the status — the event says what was done, the status says
where the reservation ended up.

### Payments: verify → queue → apply

```
POST /webhooks/stripe
  → verify Stripe's signature over the RAW bytes       → 400 on failure
  → snapshot the event into our own struct
  → publish the snapshot to the `stripe-events` queue   → 200
                                        │
                                        ▼
                             cmd/stripe-worker consumes it
                               → internal/payment applies it
```

The webhook handler **touches no database and changes no status** — the state machine
lives behind a queue, the same shape as the reservation path. The publish happens
**before** the 200, so a publish failure returns 500, Stripe retries, and the window
between "we said 200" and "the event is durable" does not exist.

Two details that make money safe:

- **The idempotency key is the `reservation_id`.** The payment row is written *before*
  Stripe is called, so a crash in between leaves a row with a null Stripe id — and the
  retry, carrying the same key, gets the **same** PaymentIntent back instead of charging
  twice.
- **The amount is the server's number.** It comes from the price snapshot taken when the
  seat was held. A client never names a price.

### The audit trail

`reservation_events` is append-only, enforced by a database trigger that refuses
`DELETE` and refuses history rewrites. Every row carries who did it, the old and new
status, the OTel `trace_id`, and event-specific metadata — so "who confirmed this
reservation, and when?" has an answer that does not depend on log retention.

---

## The numbers

| | |
|---|---|
| **0 oversell** | **417,563 requests → exactly 100 reserved, 417,463 sold out.** Never 101 |
| **4,639 req/s** | sustained through a 7-stage ramp to 1,000 concurrent VUs |
| **33 ms** | median reserve latency under that load (p99 525 ms at the 1,000-VU peak) |
| **298 tests** | 56.4% statement coverage, with a 45% floor enforced in CI |
| **3 CI jobs** | Test (a real Postgres, Redis and SQS broker), Lint, E2E — and the E2E job boots the whole stack and drives a reservation through it |
| **0 known CVEs** | `govulncheck` is a gate, not a report |
| **15 routes · 9 migrations · 17 packages · 13 containers** | 5 Go binaries, 8 infrastructure services |

The load test is the proof, and it is one command:

```bash
make loadtest-ramp    # 0 → 50 → 200 → 1,000 VUs, ~90 s
make loadtest-burst   # 1,000 VUs at once — the thundering herd
```

Both reset the state, mint the JWTs and run themselves. The live k6 dashboard is on
`:8082` while they run.

---

## API

| Method | Route | Auth | Purpose |
|---|---|---|---|
| `GET` | `/healthz` | — | Process-alive. Never checks dependencies |
| `GET` | `/readyz` | — | Readiness — Redis, Postgres and SQS in parallel. `503` if any is down |
| `GET` | `/metrics` | — | Prometheus exposition |
| `POST` | `/api/tickets/enter` | — | Waiting-room admission. A JWT if admitted, a queue position if not |
| `GET` | `/api/tickets/queue` | — | SSE stream of your queue position |
| `POST` | `/api/tickets/reserve` | JWT | **Reserve a seat.** Honours `Idempotency-Key` |
| `GET` | `/api/reservations/{id}` | JWT | Fetch one reservation |
| `GET` | `/api/me/reservations` | JWT | List yours — `?status=`, `?limit=`, `?offset=` |
| `POST` | `/api/reservations/{id}/cancel` | JWT | Cancel a `PENDING_PAYMENT` reservation |
| `POST` | `/api/reservations/{id}/pay` | JWT | Create the Stripe PaymentIntent; returns the `client_secret` |
| `POST` | `/webhooks/stripe` | Stripe signature | Stripe's callback — verifies, snapshots, queues |
| `GET` | `/admin/reservations/lookup?user_id=` | HMAC | Support lookup — a user's reservations |
| `GET` | `/admin/reservations/{id}` | HMAC | Support lookup — one reservation |
| `POST` | `/admin/reservations/{id}/cancel` | HMAC | Cancel a held reservation; releases the seat |
| `POST` | `/admin/reservations/{id}/release-seat` | HMAC | Put a seat back on sale |

**Retries are safe.** `POST /reserve` takes an `Idempotency-Key`: send it twice and you
get the **same reservation** back — no second seat, no second message. Two genuinely
parallel requests with one key: the loser gets `409 in_flight` and reserves nothing.

**The admin API is signed, not tokenised.** Each request carries an HMAC over the method,
the full URI, the body and a timestamp — so a captured signature is good for one
request, one body, five minutes. The audit trail records the *fingerprint* of the key
that acted, never the key itself. And its power is bounded by the same state machine: a
paid reservation is **refunded**, not cancelled.

---

## Testing

```bash
make test         # unit + integration — needs the stack up
make test-race    # the same, with the race detector
```

Everything runs against the real thing: a real Postgres, a real Redis, a real SQS
broker, and the Stripe SDK's own signer for the webhook fixtures. No mocks of the
interesting parts.

| CI job | What it does |
|---|---|
| **Test** | `go vet` → migrations → `go test -race` against real Postgres + Redis + ElasticMQ → a coverage floor → `go build` |
| **Lint** | `gofmt` · `go mod tidy` · `govulncheck` |
| **E2E** | Boots the real Compose stack and drives a reservation through reserve → SQS → worker → Postgres → signed webhook → SQS → Stripe worker → `CONFIRMED`, then checks the audit trail |

`main` is branch-protected; the `Test` check is required.

---

## Observability

| Pillar | Where | What it shows |
|---|---|---|
| **Metrics** | Prometheus `:9090` · Grafana `:3000` | request rate, p99 latency, reservation outcomes, worker throughput. `reservations_oversold_total` should always be `0` |
| **Traces** | Jaeger `:16686` | one trace across `api → redis → sqs → worker → postgres` — the context is propagated through the queue |
| **Logs** | Loki `:3100` | structured `slog` with the `trace_id` injected on every line; click a `trace_id` to jump to its trace |
| **Live dashboard** | `:8082` | the k6 view while a load test runs |

---

## Tech stack

| | |
|---|---|
| **Language** | Go 1.27 — goroutines for the hot path, one static binary per service |
| **HTTP** | `net/http` + `chi` — one process, 10k+ connections |
| **Hot path** | Redis 7 — single-threaded Lua execution is what makes the seat lock atomic |
| **Queue** | AWS SQS Standard (ElasticMQ locally) — at-least-once delivery plus idempotent workers is reliable enough |
| **Database** | PostgreSQL 16 — the source of truth, never on the hot path |
| **Auth** | JWT HS256 — `sub` + `event_id` + `exp`, and Stripe confirms the money |
| **Payments** | Stripe test mode — PaymentIntents, signature-verified webhooks, refunds |
| **Metrics / Traces / Logs** | Prometheus + Grafana · OpenTelemetry → Jaeger · `log/slog` → Loki |
| **Load testing** | k6 — 1,000 VUs, threshold assertions |
| **Containers** | Docker Compose — the whole stack with one command |

---

## Layout

```
cmd/                api · worker · stripe-worker · expiration-watcher · dashboard-server · mintjwt · seed-inventory
internal/
  api/              the HTTP handlers
  admin/            the HMAC-signed support API
  payment/          the Stripe state machine (the consumer side)
  stripe/           the Stripe client — create, verify, snapshot
  queue/            SQS publisher + consumer, and the trace propagation
  db/               pgx pool, the state machine, the audit log, payments
  redis/            the Lua scripts — seat locking, the waiting room, idempotency
  expire/           the expiry path, shared by the watcher and the sweep
  waitingroom/      the token bucket, the queue and the SSE stream
  e2e/              the end-to-end test (build tag `e2e`)
migrations/         9 files, applied in order
monitoring/         Prometheus, Grafana dashboards, Loki, Promtail
loadtest/           the k6 scripts and the live dashboard
docs/               the engineering manual and the roadmap
```

---

## Roadmap

- [x] Atomic seat locking · virtual waiting room · async fulfillment · expiry handling
- [x] The product surface — read, cancel, `/readyz`, `Idempotency-Key`
- [x] The append-only audit trail, written with every transition
- [x] Stripe test mode — payments, webhooks, refunds
- [x] The HMAC-signed admin / ops API
- [ ] Notifications — a second consumer on the same queue pattern
- [ ] Anti-bot — CAPTCHA plus a Lua-side velocity check
- [ ] Circuit breakers, GDPR endpoints, feature flags

---

## License

MIT.
