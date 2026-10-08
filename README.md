# Mini-Ticketmaster: A High-Concurrency Ticket Booking System

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
| 2   | Redis Lua layer             | ⏳ Next up   | Per-event token bucket, `reserve.lua` for atomic hold                                                    |
| 3   | HTTP API + JWT              | Pending      | `chi` router, `/enter`, `/reserve`; HS256 fail-fast secret                                               |
| 4   | Per-IP rate limit           | Pending      | Second token-bucket layer in front of per-event bucket                                                   |
| 5   | Worker                      | Pending      | SQS consumer, `INSERT ... ON CONFLICT DO NOTHING` into Postgres                                          |
| 6   | Hold keys                   | Pending      | 10-min Redis hold, release on payment confirm                                                            |
| 7   | Expirations                 | Pending      | Keyspace listener (`__keyevent@0__:expired`) + 60-s DB sweep                                             |
| 8   | Load test + invariants      | Pending      | k6 burst, zero-oversell + zero-lose assertions                                                           |

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
| Containers     | **Docker Compose**                                                                                  | Whole stack with one command                                                                                              |

> Why these choices (and which we explicitly rejected) → [`/docs/architecture.md`](docs/architecture.md)

---

## Project Layout

```
.
├── cmd/
│   ├── api/                  # Go API server (chi router, SSE handler)
│   ├── worker/               # Go SQS consumer → Postgres writer
│   └── expiration-watcher/   # Redis keyspace listener → marks expirations in Postgres
├── internal/
│   ├── api/                  # chi router, handlers
│   ├── auth/                 # JWT issue + verify, HS256, fail-fast secret
│   ├── iplimit/              # Per-IP token bucket (rate limit at /enter)
│   ├── waitingroom/          # Per-event token bucket + ZSET queue
│   ├── redis/                # Lua scripts + Redis client wrappers
│   ├── queue/                # SQS publisher + consumer (aws-sdk-go-v2)
│   └── postgres/             # DB connection + reservation queries
├── migrations/
│   ├── 001_init.sql          # events, reservations, idempotency PK, partial index
│   └── 002_seed.sql          # Dev event id=1 with 100 inventory
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

```bash
# 1. Spin up everything (Redis + Postgres + ElasticMQ; reservations queue auto-created)
docker compose up -d

# 2. Apply schema + seed an event
PGPASSWORD=tickets psql -h localhost -U tickets -d tickets -f migrations/001_init.sql
PGPASSWORD=tickets psql -h localhost -U tickets -d tickets -f migrations/002_seed.sql

# 3. Copy the env template and set a JWT secret
cp .env.example .env
openssl rand -hex 32 | pbcopy     # paste the result into JWT_SECRET= in .env

# 4. Verify
docker compose ps                 # all 3 services should be "healthy"
pgcli -h localhost -U tickets -d tickets -c "SELECT id, name, initial_inventory FROM events;"
redis-cli PING                    # expect PONG
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

> **Next:** move on to Phase 2 (Redis Lua layer) by following [`/docs/implementation-plan.md`](docs/implementation-plan.md).

---

## Project Roadmap

Done items are crossed off; remaining items are aspirational roadmap (not phase progression).

- [x] **Idempotent worker writes** — `INSERT ... ON CONFLICT DO NOTHING` *(schema ready since Phase 1; writer logic lands in Phase 5)*
- [ ] **Redis Cluster** — shard seats across nodes for >100k concurrent users
- [ ] **Observability** — Prometheus + Grafana (Lua script duration, SQS depth, hold-TTL distribution)
- [ ] **Distributed tracing** — OpenTelemetry across API → SQS → worker
- [ ] **Payment gateway** — Stripe webhook handler that confirms reservations and releases the hold
- [ ] **Seat-level granularity** — Redis Hash per event instead of a single counter
- [ ] **Anti-bot** *(intentionally skipped in lite-auth; revisit if slash-traction emerges)* — CAPTCHA + device fingerprinting before JWT issuance
- [ ] **Replay testing** — deterministic k6 replay against a seeded Redis state

---

## License

MIT — built for learning, hack away.
