# Mini-Ticketmaster: A High-Concurrency Ticket Booking System

[![CI](https://github.com/emmitt-k/ticket-deal/actions/workflows/ci.yml/badge.svg)](https://github.com/emmitt-k/ticket-deal/actions/workflows/ci.yml)

> An end-to-end showcase of how real-world ticketing platforms (Ticketmaster, AXS, See Tickets) survive the "1,000 users clicking **Buy** at the exact same millisecond" problem — built to learn, not to ship.

---

## The Problem

When a high-demand event opens, thousands of fans click **Buy** within the same second. A naive `SELECT` + `UPDATE` dies under load:

- **Connection pool exhaustion** — Postgres caps at ~100 connections; the rest queue and time out
- **Row-level lock contention** — `UPDATE ... WHERE available > 0` serializes on a single row
- **TOCTOU race condition** — check-then-act is not atomic; overselling happens
- **Cascading failures** — slow DB backs up HTTP workers, latency climbs, users retry, system melts

**Core principle:** *never let the database be on the hot path of seat allocation.*

---

## Architecture: The 3-Phase Gatekeeper

```
       1,000 Concurrent Requests
                  │
                  ▼
      ┌─────────────────────────┐
      │ 1. Virtual Waiting      │ ── Token bucket + ZSET queue (Redis)
      │    Room                 │
      └────────────┬────────────┘
                   │  Signed JWT (HS256, sub + event_id + exp, 2 min)
                   ▼
      ┌─────────────────────────┐
      │ 2. Atomic In-Memory     │ ── Lua script seat reservation (Redis)
      │    Seat Locking         │
      └────────────┬────────────┘
                   │  Lock Acquired (10-min hold)
                   ▼
      ┌─────────────────────────┐
      │ 3. Asynchronous         │ ── Go worker reads SQS, writes Postgres
      │    Fulfillment          │
      └─────────────────────────┘
```

The three phases, in one line each:

1. **Shed load** — admit only what the hot path can handle, queue the rest with a fair position
2. **Lock atomically** — a single Redis Lua script decides who gets a seat, no oversell possible
3. **Persist asynchronously** — the durable record is written out-of-band; the user is already holding their seat in memory

> Architecture deep-dive (TOCTOU, Lua internals, SQS Standard vs FIFO, expiration handling, failure modes, known sharp edges) → [`/docs/architecture.md`](docs/architecture.md)

---

## API Surface

Nine routes. Everything under `/api/reservations` and `/api/me` requires a JWT from
`/enter`; the waiting-room and health routes are open.

| Method | Route | Auth | Purpose |
|---|---|---|---|
| `GET` | `/healthz` | — | Process-alive. Never checks dependencies |
| `GET` | `/readyz` | — | Readiness. Checks Redis + Postgres + SQS in parallel. `503` if any is down |
| `GET` | `/metrics` | — | Prometheus exposition |
| `POST` | `/api/tickets/enter` | — | Waiting-room admission. Returns a JWT if admitted, a queue position if not |
| `GET` | `/api/tickets/queue` | — | SSE stream of your queue position |
| `POST` | `/api/tickets/reserve` | JWT | **Reserve a seat.** Honours `Idempotency-Key` |
| `GET` | `/api/reservations/{id}` | JWT | Fetch one reservation |
| `GET` | `/api/me/reservations` | JWT | List your reservations |
| `POST` | `/api/reservations/{id}/cancel` | JWT | Cancel a `PENDING_PAYMENT` reservation |

### Try the whole flow

```bash
make env                                              # host-side .env for mintjwt
JWT=$(USER_ID=demo EVENT_ID=1 make -s mintjwt)

# 1. Reserve a seat
RES=$(curl -sS -X POST http://localhost:8080/api/tickets/reserve \
  -H "Content-Type: application/json" -H "Authorization: Bearer $JWT" \
  -d '{"seats_requested":1}')
ID=$(echo "$RES" | jq -r .reservation_id)

# 2. Read it back
curl -sS http://localhost:8080/api/reservations/$ID -H "Authorization: Bearer $JWT"

# 3. List everything you own (filter by status)
curl -sS "http://localhost:8080/api/me/reservations?status=PENDING_PAYMENT" \
  -H "Authorization: Bearer $JWT"

# 4. Cancel it — idempotent, so calling twice is safe
curl -sS -X POST http://localhost:8080/api/reservations/$ID/cancel \
  -H "Authorization: Bearer $JWT"

# 5. Health + readiness
curl -sS http://localhost:8080/healthz
curl -sS http://localhost:8080/readyz
# → {"redis":"ok","postgres":"ok","sqs":"ok","ready":true}
```

### Safe retries

`POST /reserve` accepts an `Idempotency-Key`. Send the same key twice and you get the
**same reservation back** — no second seat consumed, no second queue message:

```bash
curl -sS -X POST http://localhost:8080/api/tickets/reserve \
  -H "Authorization: Bearer $JWT" -H "Idempotency-Key: my-unique-request-42" \
  -d '{"seats_requested":1}' -D- | grep -i 'replayed\|HTTP/'
# 2nd call → "Idempotency-Replayed: true"
```

If you rotate nothing and change the *body*, you get `409 idempotency_key_mismatch` —
that's the signal you reused a key for a different request. Two genuinely parallel
requests with one key: the second gets `409 in_flight` and does not reserve.

---

## Current Status

| #   | Phase                       | Status      | What landed                                                                                              |
| --- | --------------------------- | ----------- | -------------------------------------------------------------------------------------------------------- |
| 0   | Bootstrap                   | ✅ Done      | `docker-compose.yml` (Redis 7, Postgres 16, ElasticMQ) + auto-created `reservations` queue + healthchecks |
| 1   | Database schema             | ✅ Done      | `events`, `reservations`; `reservation_id UUID PK` for idempotency; partial index on `expires_at`        |
| 2   | Redis Lua layer             | ✅ Done      | `reserve.lua` (atomic seat lock) + `token_bucket.lua` (waiting-room admission); 9/9 tests, 1k-goroutine no-oversell |
| 3   | HTTP API + JWT              | ✅ Done      | `cmd/api/main.go` (chi, graceful shutdown); HS256 Issue/Verify/middleware; 14 tests; alg=none + replay rejected |
| 4   | Waiting room + SSE          | ✅ Done      | IP bucket (`ip_bucket.lua`) → event bucket (`token_bucket.lua`) → JWT or ZSET queue; SSE position stream; drainer goroutine issues JWTs as tokens refill; 15 tests |
| 5   | Reservation endpoint        | ✅ Done      | `internal/api/reserve_handler.go`: JWT claims → UUID reservation_id, Lua reserve → 200/409/404; `ReservationPublisher` interface + `LogPublisher` stub (Phase 6 swaps for SQS); 15 tests |
| 6   | SQS & Worker                | ✅ Done      | `internal/queue` (publisher + consumer + Reservation wire type), `internal/db` (pgx pool + InsertIfAbsent), `cmd/worker` consumer binary; `SQSPublisher` wired into API; at-least-once + idempotent INSERT; 16 tests |
| 7   | Expiration handling         | ✅ Done      | `internal/expire` (Compensate + RunWatcher + RunSweep), `cmd/expiration-watcher` (PSUBSCRIBE `__keyevent@0__:expired`), sweep goroutine inside `cmd/worker`; watcher fires real-time, sweep is the safety net; both converge on a single CAS UPDATE so only one INCRBY happens per (user, event); 20 tests (incl. 50-goroutine race, multi-seat, idempotent) |
| 8   | Load test + invariants      | ✅ Done      | `loadtest/burst.js` (1000 VUs, 1 iter — pure shock) + `loadtest/ramp.js` (7-stage 0→50→200→1000 VUs, ~90s); live dashboard on http://localhost:8082 (polls k6 REST); 100×200 + 1.8M×409 verified across 1.8M requests, 0 oversell, 0 data loss, p(99)=62ms under sustained 1000 VUs |
| 9   | Polish                      | ✅ Done      | `Makefile` (30+ targets, `make help`); all services (infra + the 4 long-running Go services) brought up by one `docker compose --env-file .env.docker up -d`; legacy host-mode `make all-services` kept for fast `go run` iteration; structured logging + Loki/Promtail log shipping; 100×200 + 1.8M×409 verified across 1.8M requests, 0 oversell, 0 data loss, p(99)=62ms under sustained 1000 VUs |

### Layer 1 — the product surface (all 5 shipped)

The core system above is a reservation *engine*. Layer 1 is what turns it into
something a user can actually operate — read their tickets, cancel one, see whether the
service is healthy, and retry safely.

| # | Item | Status | What landed |
|---|---|---|---|
| 1 | **State-machine gateway** | ✅ Done | `internal/db/transition.go` — `TransitionStatus` does a compare-and-set UPDATE guarded by a `validTransitions` table. Every status change in the system goes through it; an impossible transition is refused before the DB is touched |
| 2 | **Read endpoints** | ✅ Done | `GET /api/reservations/{id}` + `GET /api/me/reservations?status=&limit=&offset=` — JWT-scoped, paginated, filterable, backed by a composite index that satisfies filter and sort in one pass |
| 3 | **Cancel** | ✅ Done | `POST /api/reservations/{id}/cancel` — CAS-safe against the expiry watcher, releases the seat back to Redis, idempotent on a second cancel (and does not double-release) |
| 4 | **Readiness probe** | ✅ Done | `GET /readyz` — 3 parallel dependency checks (Redis / Postgres / SQS) with 500ms budgets. Deliberately separate from `/healthz` so a dead Postgres pulls the container from load-balancer rotation *without* triggering a restart loop |
| 5 | **Idempotent retries** | ✅ Done | `Idempotency-Key` on `POST /reserve` — 24h cached-response replay plus a short-TTL SETNX in-flight lock, so a retry storm can't double-book. Stripe-style `409` if the same key arrives with a different body |

### Layer 2 — the inside of the product

Layer 1 made the product usable from the outside. Layer 2 makes it honest from the
inside: an audit trail you can defend, and a payment flow that settles.

| # | Item | Status | What landed |
|---|---|---|---|
| 6 | **Audit log** | ✅ Done | `reservation_events` — an append-only trail of every transition (`CREATED` / `CONFIRMED` / `EXPIRED` / `CANCELLED` / `REFUND_INITIATED`), written in the **same transaction** as the status change, so a transition can't happen unrecorded. Records *who* did it (`user` / `system` / `admin` / `stripe_webhook`), carries the OTel `trace_id` for the trace↔logs join, and is sealed by a database trigger that refuses `DELETE` and refuses history rewrites |
| 7 | **Stripe test mode** | 🔜 Next | PaymentIntents + a signature-verified webhook receiver — the last unbuilt transition (`PENDING_PAYMENT → CONFIRMED`) |

> Layer-by-layer specs, and what's next → [`/docs/product-roadmap.md`](docs/product-roadmap.md) · Design rationale and per-file reference → [`/docs/architecture.md`](docs/architecture.md)

---

## Tech Stack

| Layer          | Technology                                                                                          | Why                                                                                                                       |
| -------------- | --------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------- |
| Language       | **Go 1.27.1**                                                                                       | Goroutines make the hot path cheap; single static binaries; one language across API, worker, watcher, dashboard               |
| API server     | **Go** + [`chi`](https://github.com/go-chi/chi)                                                     | Goroutines handle 10k+ connections per process; single static binary                                                      |
| Hot-path       | **Redis 7**                                                                                         | Single-threaded execution makes Lua atomic; microsecond reads; native TTL; keyspace notifications built in               |
| Queue          | **AWS SQS Standard**                                                                                | Unlimited throughput (Standard, not FIFO). At-least-once + idempotent workers = reliable enough                           |
| Local queue    | **ElasticMQ**                                                                                       | SQS-compatible, Apache 2.0, runs in Docker — same `aws-sdk-go-v2` code talks to it locally and to real SQS in prod         |
| Worker         | **Go**                                                                                              | Same language, same tooling; type-safe payment handling                                                                  |
| DB             | **PostgreSQL 16**                                                                                   | Source of truth for confirmed orders; never on the hot path                                                               |
| Auth (lite)    | **JWT HS256**, 2-min expiry                                                                         | `sub` + `event_id` + `exp` claims only; identity deferred to Stripe webhook. Add CAPTCHA/device-fingerprinting only at scale |
| Queue UI       | **Server-Sent Events**                                                                              | One-way position push over `text/event-stream` — way simpler than WebSockets for this use case                            |
| Load testing   | **k6**                                                                                              | Scriptable VUs, threshold-based assertions, great reporting                                                                |
| DB explorer    | **pgcli**                                                                                           | `psql` with autocomplete + syntax highlighting + pretty tables                                                            |
| Metrics        | **Prometheus** + **Grafana**                                                                        | `/metrics` on every service, scraped by Prometheus, visualized in a 9-panel "Ticket Deal — Overview" dashboard             |
| Tracing        | **OpenTelemetry** → **Jaeger**                                                                      | OTLP/gRPC export, end-to-end waterfall from `api → Redis → SQS → worker → Postgres` with rich attributes                |
| Logging        | `log/slog` (stdlib) + OTel trace correlation                                                        | Structured key-value logs with `trace_id` / `span_id` auto-injected on every line — paste a `trace_id` from Jaeger and find its log line in `jq` |
| Log search     | **Grafana Loki** + **Promtail**                                                                     | Tails `logs/*.log` from the host into Loki; "Ticket Deal — Logs" dashboard in Grafana with service + trace_id filters        |
| Containers     | **Docker Compose**                                                                                  | Whole stack with one command                                                                                              |

> Why these choices (and which we explicitly rejected) → [`/docs/architecture.md`](docs/architecture.md)

---

## Project Layout

```
.
├── cmd/                      # 6 binaries
│   ├── api/                  # HTTP server (chi router, SSE, reserve/get/list/cancel, readyz)
│   ├── worker/               # SQS consumer → Postgres writer + 60s expiry sweep goroutine
│   ├── expiration-watcher/   # Redis keyspace listener → marks expirations in Postgres
│   ├── dashboard-server/     # Serves the live k6 dashboard (:8082)
│   ├── mintjwt/              # Mint a JWT by hand (used by load tests + curl)
│   └── seed-inventory/       # Seeds Postgres with an event + 100 seats
├── internal/
│   ├── api/                  # HTTP handlers + the publisher seam
│   ├── auth/                 # JWT issue + verify, HS256, fail-fast secret
│   ├── config/               # env loading + fail-fast validation (incl. Postgres DSN)
│   ├── apiutil/              # JSON helpers + ErrorBody shape + slog access-log middleware
│   ├── iplimit/              # Per-IP token bucket (rate limit at /enter)
│   ├── waitingroom/          # Per-event token bucket + ZSET queue + SSE handler + drainer
│   ├── redis/                # Lua scripts, seat locking, Idempotency-Key store
│   ├── queue/                # SQS publisher + consumer (aws-sdk-go-v2)
│   ├── db/                   # pgx pool, idempotent writes, TransitionStatus, list queries
│   ├── expire/               # Compensate (DB+INCRBY) shared by watcher + sweep
│   ├── logging/              # slog init + OTel trace correlation handler
│   ├── tracing/              # OpenTelemetry init + shutdown flush
│   └── metrics/              # Prometheus catalog
├── migrations/
│   ├── 001_init.sql          # events, reservations, idempotency PK, partial index
│   ├── 002_seed.sql          # Dev event id=1 with 100 inventory
│   ├── 003_add_seats.sql     # seats column on reservations (Phase 7 INCRBY sizing)
│   └── 004_user_lookup_index.sql  # (user_id, created_at DESC) for GET /api/me/reservations
├── docker/
│   └── elasticmq.conf        # Pre-creates 'reservations' queue on boot (2-RTT cheaper than manual aws-cli)
├── loadtest/                 # k6 scripts (burst.js, ramp.js, live dashboard)
├── monitoring/               # Observability configs, bind-mounted into containers
│   ├── prometheus.yml        # scrape config (api:8080, worker:8081, watcher:8083)
│   ├── loki/loki-config.yml
│   ├── promtail/promtail-config.yml
│   └── grafana/              # provisioned datasources + 2 dashboards (Overview, Logs)
├── docs/
│   ├── architecture.md            # System design + diagrams + failure modes + per-file reference
│   ├── product-roadmap.md         # Where we are, what's next, build order
│   ├── tracing-crashcourse.md     # Learn OpenTelemetry from scratch (background)
│   ├── comprehensive-redis.md     # Lua atomicity, keyspace notifications
│   └── comprehensive-go.md        # Go patterns used in this codebase
├── scripts/
│   ├── setup-docker-env.sh   # One-time: creates .env.docker from .env.docker.example (reuses host .env JWT_SECRET)
│   ├── start-bg.sh / stop-bg.sh  # Host-mode background service control
│   └── clear-logs.sh         # Delete or truncate runtime + k6 logs (safe in-flight)
├── logs/                     # Runtime logs + timestamped k6 output (bind-mounted into containers)
├── .github/                  # CI workflow + Dependabot config
├── docker-compose.yml        # 12 services: 8 infra + the 4 Go services (api, worker, watcher, dashboard-server), all with healthchecks
├── Dockerfile                # Multi-stage, distroless/static-debian12:nonroot; one file, parameterised by SERVICE build arg
├── .dockerignore             # Keeps the build context tight (.git, logs, bin, monitoring, etc.)
├── .env.example              # Template — copy to .env (NEVER commit .env) — host-mode run config
├── .env.docker.example       # Template — copy to .env.docker (NEVER commit .env.docker) — dockerised run config
├── Makefile                  # Every task: build, test, load tests, service control, log cleanup
├── AGENTS.md                 # Canonical context for AI coding assistants
├── go.mod                    # module github.com/emmitt-k/ticket-deal
└── README.md
```

---

## Quick Start

### Prerequisites

```bash
brew install k6
brew install --cask docker   # Docker Desktop
```

That's it. The 4 long-running Go services are now Dockerised too — `docker compose up -d` brings up **everything** (infra + Go services + observability), no Go toolchain required for just running it.

If you want to develop the Go services themselves (edit source, rebuild, etc.) you'll also need:

```bash
brew install go
```

(`libpq` is keg-only on Apple Silicon — if `psql` isn't on PATH, run `brew link --force libpq`.)

### Run it

First-time setup (one-shot per clone):

```bash
git clone <repo> ticket-deal && cd ticket-deal
make doctor                            # check prereqs (docker, k6)
./scripts/setup-docker-env.sh          # create .env.docker from .env.docker.example + your host .env's JWT_SECRET
docker compose --env-file .env.docker up -d --build
```

That's every service running: redis, postgres, elasticmq, jaeger, prometheus, grafana, loki, promtail, **api, worker, expiration-watcher, dashboard-server**.

Daily dev loop:

```bash
# Run / inspect / restart / log
docker compose --env-file .env.docker up -d                 # bring everything up
docker compose --env-file .env.docker ps                    # what's up
docker compose --env-file .env.docker logs -f api worker    # tail logs across services
docker compose --env-file .env.docker restart api           # rebuild + restart one service
docker compose --env-file .env.docker down                  # stop everything

# Test it
make env                                                    # create host-side .env from .env.example (for mintjwt + ad-hoc CLI)
JWT=$(USER_ID=smoke-user EVENT_ID=1 make -s mintjwt)
curl -sS -X POST http://localhost:8080/api/tickets/enter \
  -H "Content-Type: application/json" -d '{"user_id":"smoke-user","event_id":1}'
curl -sS -X POST http://localhost:8080/api/tickets/reserve \
  -H "Content-Type: application/json" -H "Authorization: Bearer $JWT" \
  -d '{"seats_requested":1}'
# → {"reservation_id":"...","seats":1,"expires_in":600}
# Full walkthrough (read back → list → cancel → readiness) in "API Surface" above.

# Load test it (auto-resets state + mints JWTs + runs k6 in background)
make loadtest-burst  # 1000 VUs, ~1s — pure correctness shock
make loadtest-ramp   # 0→50→200→1000 VUs, ~90s — wave ramp (dashboard tracks VU curve)
make status          # reserved=100, sold_out=1.8M, p(99)=62ms after the ramp

# Stop everything
docker compose --env-file .env.docker down                  # infra + Go services
make stop-loadtest                                           # any backgrounded k6 (burst or ramp)
```

#### Legacy: host-mode services (still works, not recommended)

The old flow had Go services running on the host via `make all-services` and only infra in Docker. That still works if you want it (e.g. for rapid local iteration with `go run`), but the canonical flow is Docker compose. Both modes read the same `.env` — JWTs minted with `make mintjwt` verify against either the host or the dockerised API as long as the `JWT_SECRET` matches.

```bash
make up               # infra only (Redis + Postgres + ElasticMQ)
make migrate          # apply migrations + seed if first run
make build            # all 6 binaries into bin/
make all-services     # api + worker + watcher in background on host
make dashboard-bg     # dashboard-server in background on host
make stop             # stop all 4
```

### What you should see

```
+---------------+-----------------------------------------+-------------------+
| id            | name                                    | initial_inventory |
|---------------+-----------------------------------------+-------------------|
| 1             | Dev Test Event: Ticketmaster-style Drop | 100               |
+---------------+-----------------------------------------+-------------------+
SELECT 1

ticket-redis      6379   healthy
ticket-postgres   5432   healthy
ticket-elasticmq  9324   healthy   reservations queue pre-created
```

### Switching to real AWS for production

- Set `AWS_REGION` and real IAM credentials in `.env`. (`AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` must be non-empty *even when* pointing at a custom endpoint — sharp edge of `aws-sdk-go-v2`.)
- Remove the LocalStack-style endpoint override from the queue config.
- No Go code changes. The same worker binary talks to ElasticMQ locally and AWS SQS in prod.

> **All Phases 0–9 plus Layer 1 are complete.** Run `make loadtest-burst` or `make loadtest-ramp` (see [Quick Start](#quick-start) above) to prove the core invariant under load.

### Load testing (k6 + live dashboard)

`/loadtest` ships two k6 scripts. Both prove the same invariant — **with `initial_inventory=100` and 1000 unique users, exactly 100 succeed and 900 get 409** — under different shapes:

| Script | Shape | Duration | Make target |
|---|---|---|---|
| `loadtest/burst.js` | 1000 VUs, 1 iter each — pure shock | <1 s | `make loadtest-burst` |
| `loadtest/ramp.js`  | 7 stages: 0 → 50 → 200 → 1000 VUs | ~90 s | `make loadtest-ramp` |

The shared live dashboard (`loadtest/dashboard.html` served by `cmd/dashboard-server`) polls k6's REST API every 1s — VU count, request rate, latency p50/p90/p95/p99, 200 vs 409 split, progress bar. Both tests run k6 in the background with `--linger` so the dashboard stays populated after the test finishes.

**How to run** (commands are also in [Quick Start](#quick-start) above):

```bash
docker compose --env-file .env.docker up -d   # 1. start everything (system + observability)
open http://localhost:8082/                    # 2. open the live UI in your browser
make loadtest-burst                            # 3a. correctness shock — ~1s
# or:
make loadtest-ramp                             # 3b. wave ramp — ~90s, VU count climbs
# 4. (optional) re-run:
make stop-loadtest && make loadtest-burst
# 5. (cleanup)
docker compose --env-file .env.docker down
```

**Expected output** (printed by k6 and written to `logs/burst/TS-burst.log` or `logs/ramp/TS-ramp.log`):

```
─────────────── LOAD TEST RESULTS ───────────────
  Total requests    : 1000
  Reserved (200)    :  100  ✅
  Sold out (409)    :  900  ✅
  Other status      :    0  ✅
  Oversell check    : ✅ no oversell
──────────────────────────────────────────────────
```

Tune the load: `make loadtest-burst VUS=500`, `make loadtest-ramp VUS=2000`. Target a different event: `EVENT_ID=2 make loadtest-burst`. For the why behind the k6 script structure, the dashboard polling, and the 4 assertion categories → [`/docs/architecture.md`](docs/architecture.md) §13 (`loadtest/` section).

---

## Observability

### Metrics (Prometheus + Grafana)

Live metrics for every service, scrape-and-dashboard ready:

```bash
# Bring up everything (one-time, or for any dev session)
docker compose --env-file .env.docker up -d

# Open dashboards
#   Prometheus:  http://localhost:9090
#   Grafana:     http://localhost:3000   (admin / admin123)
#   Dashboard:   "Ticket Deal" folder → "Ticket Deal — Overview"
```

The dashboard shows request rate, p99 latency, reservation outcomes (held vs sold_out vs oversold), worker throughput, DB write latency, and expiration activity. The headline metric is **`reservations_oversold_total`** — should always be `0`. Alert if it ticks.

For the full metrics inventory and how to add a new one → [`/docs/architecture.md`](docs/architecture.md) §Observability.

### Tracing (OpenTelemetry + Jaeger)

End-to-end distributed traces, OTLP/gRPC → Jaeger:

```bash
# Bring up everything (one command)
docker compose --env-file .env.docker up -d

# Open Jaeger UI
#   http://localhost:16686
#   - Pick service: api / worker / expiration-watcher
#   - Click a trace to see the waterfall
```

A single reservation's trace covers the whole pipeline (auto + manual spans):

```
POST  (otelhttp)                      [35ms]
└─ reserve.handle (manual)            [35ms]
   ├─ redis.acquire-hold (manual)     [ 3ms]
   │  └─ evalsha (redisotel)          [ 1ms]
   └─ sqs.publish (manual)            [22ms]      ← context jumps via SQS
      └─ worker.handleMessage (worker) [ 1ms]
         └─ pool.acquire (otelpgx)    [ 0ms]
         └─ INSERT (otelpgx)          [ 1ms]
```

For the propagator details, manual span inventory, and SQS body-fallback
rationale → [`/docs/tracing-crashcourse.md`](docs/tracing-crashcourse.md).

### Logging (`log/slog` + OTel correlation)

Every long-running service emits **structured** logs, with the active OTel `trace_id` and `span_id` injected on every line. No new dependencies — `log/slog` is stdlib since Go 1.21.

```bash
# Default: human-readable text (great for `docker logs -f` while developing)
# LOG_FORMAT=text is set in .env.docker.example by default.
docker compose --env-file .env.docker logs -f api

# For Loki / Datadog / `jq`-fu, switch to JSON
LOG_FORMAT=json LOG_LEVEL=info docker compose --env-file .env.docker up -d
```

A reservation's worker log line in **text** format (with active OTel span):

```
time=2026-10-09T15:00:00.000-07:00 level=INFO msg="reservation inserted"
service=worker service_version=dev
reservation_id=6880e7fa-1170-440f-883c-c5bca36002b0
user=k6user-31 event=1 seats=1
trace_id=fac1f09d3a4b5c6d7e8f9a0b1c2d3e4f span_id=71bebc50a1b2c3d4
```

Same line in **JSON** (`LOG_FORMAT=json`):

```json
{"time":"2026-10-09T15:00:00.000-07:00","level":"INFO","msg":"reservation inserted",
 "service":"worker","service_version":"dev",
 "reservation_id":"6880e7fa-1170-440f-883c-c5bca36002b0",
 "user":"k6user-31","event":1,"seats":1,
 "trace_id":"fac1f09d3a4b5c6d7e8f9a0b1c2d3e4f","span_id":"71bebc50a1b2c3d4"}
```

The 30-LOC `logging.ContextHandler` is what makes this work — it pulls the OTel span from the request context and appends `trace_id` / `span_id` to every record. HTTP access logs (replacing `chi/middleware.Logger`) carry the same correlation.

For the design rationale, env var matrix, and what we deliberately kept as `log.Printf` (one-shot CLIs) → [`/docs/architecture.md`](docs/architecture.md) §Logging.

### Log search (Grafana Loki + Promtail)

```bash
# Brought up automatically by `docker compose up -d`. To see them explicitly:
docker compose --env-file .env.docker ps   # confirm loki + promtail are running

#   Loki HTTP API:  http://localhost:3100
#   Grafana:        http://localhost:3000  → "Ticket Deal" folder → "Ticket Deal — Logs"
```

**Architecture:**

```
Go services (in docker) ──bind mount ./logs──> logs/*.log (host) ──bind mount──> Promtail ──> Loki ──> Grafana
```

With everything in Docker, the Go services and Promtail share the host's `logs/` directory via two bind mounts (`./logs:/app/logs` on the Go services, `./logs:/var/log/ticket-deal:ro` on Promtail). Promtail parses `service` (from filename) + `trace_id` (from line body, both text and json formats) as Loki labels, and pushes them as streams. Loki stores them on disk (`loki-data` volume).

**The killer combo — Trace → Logs:**

1. Spot a slow trace in Jaeger → grab the `trace_id` (32-hex)
2. Open Grafana → "Ticket Deal — Logs" dashboard → paste it into the `trace_id` field
3. See every log line for that request across api/worker/watcher, all timestamped and ordered

The reverse (log → trace) is also wired up: the Loki datasource in `monitoring/grafana/provisioning/datasources/datasource.yml` has a `derivedField` that makes `trace_id` values in log lines clickable, jumping to the matching Jaeger trace.

**No Go code changes were needed to add Loki** — it's a pure infra add. The slog `ContextHandler` already injects `trace_id` into every log line, and Promtail's regex stages extract it as a label.

---

## Project Roadmap

Done items are crossed off; remaining items are aspirational roadmap (not phase progression).
Full layer-by-layer specs live in [`/docs/product-roadmap.md`](docs/product-roadmap.md) —
that file is the source of truth; this list is the short version.

- [x] **Idempotent worker writes** — `INSERT ... ON CONFLICT DO NOTHING` *(schema ready since Phase 1; writer logic lands in Phase 5)*
- [ ] **Redis Cluster** — shard seats across nodes for >100k concurrent users
- [x] **Observability** — Prometheus + Grafana (request rate, latency, reservation outcomes, worker throughput, expiration activity; `reservations_oversold_total` is the headline alert)
- [x] **Distributed tracing** — OpenTelemetry → Jaeger, end-to-end waterfall across API → SQS → worker (with body-fallback for ElasticMQ in dev)
- [x] **Structured logging** — `log/slog` across all 4 long-running services, with OTel `trace_id` / `span_id` auto-injected on every line (and HTTP access log via custom chi middleware)
- [x] **Log aggregation** — Grafana Loki + Promtail, "Ticket Deal — Logs" dashboard with service + trace_id filters, click-through from log line to Jaeger trace
- [x] **Dockerised** — all 4 long-running Go services run in containers; one `docker compose up -d` brings up everything
- [x] **Read + cancel + retry safety** — Layer 1: state-machine gateway, `GET` endpoints, `POST /cancel`, `/readyz`, `Idempotency-Key`
- [x] **Audit trail** — Layer 2 item 6: an append-only `reservation_events` log written atomically with every transition, recording who did what and joining to traces via `trace_id`
- [ ] **Payment gateway** — Stripe webhook handler that confirms reservations (`PENDING_PAYMENT → CONFIRMED`, the one transition with no code path yet) and releases the hold. **This is Layer 2 item 7 — the next milestone.**
- [ ] **Seat-level granularity** — Redis Hash per event instead of a single counter
- [ ] **Anti-bot** *(intentionally skipped in lite-auth; revisit if slash-traction emerges)* — CAPTCHA + device fingerprinting before JWT issuance
- [ ] **Replay testing** — deterministic k6 replay against a seeded Redis state

---

## License

MIT — built for learning, hack away.
