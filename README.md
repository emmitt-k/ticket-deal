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
                   │  Signed JWT
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

> Architecture deep-dive (TOCTOU, Lua internals, SQS Standard vs FIFO, expiration handling) → `/docs/` (coming soon)

---

## Tech Stack

| Layer | Technology | Why |
|---|---|---|
| API server | **Go** + [`chi`](https://github.com/go-chi/chi) | Goroutines handle 10k+ connections per process; single static binary |
| Hot-path | **Redis 7** | Single-threaded execution makes Lua atomic; microsecond reads; native TTL |
| Queue | **AWS SQS Standard** | Unlimited throughput (Standard, not FIFO). At-least-once + idempotent workers = reliable enough |
| Local queue | **ElasticMQ** | SQS-compatible, Apache 2.0, runs in Docker — same `aws-sdk-go-v2` code talks to it locally and to real SQS in prod. (LocalStack went paid in 2026, this stays free forever.) |
| Worker | **Go** | Same language, same tooling, type-safe payment handling |
| DB | **PostgreSQL 16** | Source of truth for confirmed orders; never on the hot path |
| Queue UI | **Server-Sent Events** | One-way position push over `text/event-stream` — way simpler than WebSockets for this use case |
| Load testing | **k6** | Scriptable VUs, threshold-based assertions, great reporting |
| Containers | **Docker Compose** | Whole stack with one command |

> Why these choices? → `/docs/tooling.md` (coming soon)

---

## Project Layout

```
.
├── cmd/
│   ├── api/              # Go API server (chi router, SSE handler)
│   └── worker/           # Go SQS consumer → Postgres writer
├── internal/
│   ├── redis/            # Lua scripts + Redis client wrappers
│   ├── queue/            # SQS publisher + consumer (aws-sdk-go-v2)
│   ├── auth/             # JWT issue + verify
│   └── waitingroom/      # Token bucket + ZSET queue logic
├── migrations/           # Postgres schema
├── loadtest/             # k6 scripts
├── docker-compose.yml    # Redis + Postgres + ElasticMQ
└── README.md
```

---

## Quick Start

### Prerequisites

- Docker & Docker Compose v2
- Go 1.22+
- k6 (`brew install k6`)
- AWS CLI (for creating the SQS queue locally)

### Run it

```bash
# 1. Spin up Redis, Postgres, and ElasticMQ (SQS-compatible)
docker compose up -d redis postgres elasticmq

# 2. Create the SQS queue (Standard, not FIFO — unlimited throughput)
aws --endpoint-url=http://localhost:9324 sqs create-queue \
  --queue-name reservations

# 3. Run schema migrations
psql -h localhost -U tickets -d tickets -f migrations/001_init.sql

# 4. Seed an event with 10 seats
psql -h localhost -U tickets -d tickets \
  -c "INSERT INTO events (id, name, initial_inventory) VALUES (42, 'Test Event', 10);"
redis-cli SET inventory:event:42 10

# 5. Start the Go API
go run ./cmd/api

# 6. Start the Go worker
go run ./cmd/worker

# 7. Run the load test
k6 run loadtest/burst.js

# 8. Verify no overselling
psql -h localhost -U tickets -d tickets \
  -c "SELECT COUNT(*) FROM reservations WHERE event_id = 42;"
```

> **Switching to real AWS for production?** Just remove `--endpoint-url` from the `aws` CLI command and set `AWS_REGION` + real credentials. Same Go code, no changes.

---

## Project Roadmap

- [ ] **Redis Cluster** — shard seats across nodes for >100k concurrent users
- [ ] **Observability** — Prometheus + Grafana (Lua script duration, SQS depth, hold-TTL distribution)
- [ ] **Distributed tracing** — OpenTelemetry across API → SQS → worker
- [ ] **Idempotent worker writes** — `INSERT ... ON CONFLICT DO NOTHING` (now **critical**: SQS Standard has no built-in dedup)
- [ ] **Payment gateway** — Stripe webhook handler that confirms reservations and releases the hold
- [ ] **Seat-level granularity** — Redis Hash per event instead of a single counter
- [ ] **Anti-bot** — CAPTCHA + device fingerprinting before JWT issuance
- [ ] **Replay testing** — deterministic k6 replay against a seeded Redis state

---

## License

MIT — built for learning, hack away.
