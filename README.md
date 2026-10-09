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
| 9   | Polish                      | ✅ Done      | `Makefile` (30+ targets, `make help`), `scripts/start-bg.sh` + `stop-bg.sh` (idempotent PID-tracked bg), `make status` shows live container + service + DB/Redis state; all commands now one-liners |

> Full phased plan with file layout, code snippets, verification steps, and the lite-auth design → [`/docs/implementation-plan.md`](docs/implementation-plan.md)

---

## Tech Stack

| Layer          | Technology                                                                                          | Why                                                                                                                       |
| -------------- | --------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------- |
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
| Containers     | **Docker Compose**                                                                                  | Whole stack with one command                                                                                              |

> Why these choices (and which we explicitly rejected) → [`/docs/architecture.md`](docs/architecture.md)

---

## Project Layout

```
.
├── cmd/
│   ├── api/                  # Go API server (chi router, SSE handler) [✅ Phase 3]
│   ├── worker/               # Go SQS consumer → Postgres writer + 60s expiry sweep goroutine [✅ Phase 6,7]
│   └── expiration-watcher/   # Redis keyspace listener → marks expirations in Postgres [✅ Phase 7]
├── internal/
│   ├── api/                  # chi router, handlers [✅ Phase 5]
│   ├── auth/                 # JWT issue + verify, HS256, fail-fast secret [✅ Phase 3]
│   ├── config/               # env loading (godotenv) + fail-fast validation
│   ├── apiutil/              # JSON helpers + ErrorBody shape (uniform error envelope)
│   ├── iplimit/              # Per-IP token bucket (rate limit at /enter) [✅ Phase 4]
│   ├── waitingroom/          # Per-event token bucket + ZSET queue + SSE handler + drainer [✅ Phase 4]
│   ├── redis/                # Lua scripts + Go wrappers [✅ Phase 2]
│   ├── queue/                # SQS publisher + consumer (aws-sdk-go-v2) [✅ Phase 6]
│   ├── db/                   # pgx pool + InsertIfAbsent (idempotent) [✅ Phase 6]
│   └── expire/               # Compensate (DB+INCRBY) shared by watcher + sweep [✅ Phase 7]
├── migrations/
│   ├── 001_init.sql          # events, reservations, idempotency PK, partial index
│   ├── 002_seed.sql          # Dev event id=1 with 100 inventory
│   └── 003_add_seats.sql     # seats column on reservations (Phase 7 INCRBY sizing)
├── docker/
│   └── elasticmq.conf        # Pre-creates 'reservations' queue on boot (2-RTT cheaper than manual aws-cli)
├── loadtest/                 # k6 scripts (Phase 8)
├── docs/
│   ├── architecture.md       # System design + diagrams + failure modes
│   └── implementation-plan.md # Phased build plan
├── docker-compose.yml        # Redis 7 + Postgres 16 + ElasticMQ, all with healthchecks
├── .env.example              # Template — copy to .env (NEVER commit .env)
├── go.mod                    # module github.com/emmitt-k/ticket-deal
└── README.md
```

---

## Quick Start

### Prerequisites

```bash
brew install go libpq redis pgcli k6
brew install --cask docker   # if you don't have Docker Desktop yet
```

(`libpq` is keg-only on Apple Silicon — if `psql` isn't on PATH, run `brew link --force libpq`.)

### Run it

The whole project is driven by `make`. Run `make help` for the full list; the common first-time flow:

```bash
make doctor          # check prereqs (go, docker, k6, curl)
make up              # start Redis + Postgres + ElasticMQ via docker compose
make migrate         # apply all 3 SQL migrations in order
make env             # copy .env.example → .env (edit JWT_SECRET if you want)
make seed            # copy events.initial_inventory from Postgres → Redis
make build           # build all 6 binaries into bin/
make test-race       # prove 1k concurrent reservations → exactly N winners
```

Then for the daily dev loop:

```bash
# Run the system
make all-services    # start api + worker + watcher in background (logs → logs/)
make dashboard-bg    # start the k6 live dashboard on http://localhost:8082/
open http://localhost:8082/      # watch load tests in real time
make status          # see what is running + DB/Redis counts
make logs            # tail all background logs

# Test it
make all-services
JWT=$(USER_ID=smoke-user EVENT_ID=1 make -s mintjwt)
curl -sS -X POST http://localhost:8080/api/tickets/enter \
  -H "Content-Type: application/json" -d '{"user_id":"smoke-user","event_id":1}'
curl -sS -X POST http://localhost:8080/api/tickets/reserve \
  -H "Content-Type: application/json" -H "Authorization: Bearer $JWT" \
  -d '{"seats_requested":1}'
# → {"reservation_id":"...","seats":1,"expires_in":600}

# Load test it (auto-resets state + mints JWTs + runs k6 in background)
make loadtest-burst  # 1000 VUs, ~1s — pure correctness shock
make loadtest-ramp   # 0→50→200→1000 VUs, ~90s — wave ramp (dashboard tracks VU curve)
make status          # reserved=100, sold_out=1.8M, p(99)=62ms after the ramp

# Stop everything
make stop            # api/worker/watcher/dashboard
make stop-loadtest   # any backgrounded k6 (burst or ramp)
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

> **All Phases 0–9 complete.** Run `make loadtest-burst` or `make loadtest-ramp` (see [Quick Start](#quick-start) above) to prove the core invariant under load.

### Load testing (k6 + live dashboard)

`/loadtest` ships two k6 scripts. Both prove the same invariant — **with `initial_inventory=100` and 1000 unique users, exactly 100 succeed and 900 get 409** — under different shapes:

| Script | Shape | Duration | Make target |
|---|---|---|---|
| `loadtest/burst.js` | 1000 VUs, 1 iter each — pure shock | <1 s | `make loadtest-burst` |
| `loadtest/ramp.js`  | 7 stages: 0 → 50 → 200 → 1000 VUs | ~90 s | `make loadtest-ramp` |

The shared live dashboard (`loadtest/dashboard.html` served by `cmd/dashboard-server`) polls k6's REST API every 1s — VU count, request rate, latency p50/p90/p95/p99, 200 vs 409 split, progress bar. Both tests run k6 in the background with `--linger` so the dashboard stays populated after the test finishes.

**How to run** (commands are also in [Quick Start](#quick-start) above):

```bash
make all-services dashboard-bg     # 1. start the system
open http://localhost:8082/         # 2. open the live UI in your browser
make loadtest-burst                # 3a. correctness shock — ~1s
# or:
make loadtest-ramp                 # 3b. wave ramp — ~90s, VU count climbs
# 4. (optional) re-run:
make stop-loadtest && make loadtest-burst
# 5. (cleanup)
make stop
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
# Bring up the stack (one-time)
docker compose up -d prometheus grafana

# Run your services (as usual)
make all-services

# Open dashboards
#   Prometheus:  http://localhost:9090
#   Grafana:     http://localhost:3000   (admin / admin123)
#   Dashboard:   "Ticket Deal" folder → "Ticket Deal — Overview"
```

The dashboard shows request rate, p99 latency, reservation outcomes (held vs sold_out vs oversold), worker throughput, DB write latency, and expiration activity. The headline metric is **`reservations_oversold_total`** — should always be `0`. Alert if it ticks.

For the full metrics inventory and how to add a new one → [`/docs/observability-plan.md`](docs/observability-plan.md).

### Tracing (OpenTelemetry + Jaeger)

End-to-end distributed traces, OTLP/gRPC → Jaeger:

```bash
docker compose up -d jaeger
make all-services

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
rationale → [`/docs/tracing-plan.md`](docs/tracing-plan.md).

### Logging (`log/slog` + OTel correlation)

Every long-running service emits **structured** logs, with the active OTel `trace_id` and `span_id` injected on every line. No new dependencies — `log/slog` is stdlib since Go 1.21.

```bash
# Default: human-readable text (great for `tail -f` while developing)
LOG_FORMAT=text make all-services

# For Loki / Datadog / `jq`-fu, switch to JSON
LOG_FORMAT=json LOG_LEVEL=info make all-services
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

For the design rationale, env var matrix, and what we deliberately kept as `log.Printf` (one-shot CLIs) → [`/docs/logging-plan.md`](docs/logging-plan.md).

---

## Project Roadmap

Done items are crossed off; remaining items are aspirational roadmap (not phase progression).

- [x] **Idempotent worker writes** — `INSERT ... ON CONFLICT DO NOTHING` *(schema ready since Phase 1; writer logic lands in Phase 5)*
- [ ] **Redis Cluster** — shard seats across nodes for >100k concurrent users
- [x] **Observability** — Prometheus + Grafana (request rate, latency, reservation outcomes, worker throughput, expiration activity; `reservations_oversold_total` is the headline alert)
- [x] **Distributed tracing** — OpenTelemetry → Jaeger, end-to-end waterfall across API → SQS → worker (with body-fallback for ElasticMQ in dev)
- [x] **Structured logging** — `log/slog` across all 4 long-running services, with OTel `trace_id` / `span_id` auto-injected on every line (and HTTP access log via custom chi middleware)
- [ ] **Payment gateway** — Stripe webhook handler that confirms reservations and releases the hold
- [ ] **Seat-level granularity** — Redis Hash per event instead of a single counter
- [ ] **Anti-bot** *(intentionally skipped in lite-auth; revisit if slash-traction emerges)* — CAPTCHA + device fingerprinting before JWT issuance
- [ ] **Replay testing** — deterministic k6 replay against a seeded Redis state

---

## License

MIT — built for learning, hack away.
