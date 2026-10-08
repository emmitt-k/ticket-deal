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
make all-services    # start api + worker + watcher in background (logs → logs/)
make dashboard-bg    # start the k6 live dashboard on http://localhost:8082/
make status          # see what is running + DB/Redis counts
make logs            # tail all background logs
```

`make stop` cleans up `api/worker/watcher/dashboard`; `make stop-loadtest` cleans up any backgrounded k6 burst/ramp.

### Smoke test (full happy path)

```bash
# 1. Start the three processes in the background
make all-services                # api + worker + watcher; logs → logs/

# 2. Mint a JWT (godotenv picks up .env)
JWT=$(USER_ID=smoke-user EVENT_ID=1 make -s mintjwt)

# 3. POST /enter to be admitted (or hit the queue if event is full)
curl -sS -X POST http://localhost:8080/api/tickets/enter \
  -H "Content-Type: application/json" -d '{"user_id":"smoke-user","event_id":1}'

# 4. POST /reserve with the token → 200 + reservation_id
curl -sS -X POST http://localhost:8080/api/tickets/reserve \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $JWT" -d '{"seats_requested":1}'
# → {"reservation_id":"...","seats":1,"expires_in":600}

# 5. (cleanup) make stop
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

> **Phase 8 done.** All Phases 0–8 complete — see [Load testing](#load-testing-k6--live-dashboard) below for the proof of correctness under load.

### Load testing (k6 + live dashboard)

`/loadtest` ships two k6 scripts and a live web dashboard. Both prove the same core invariant — **with `initial_inventory=100` and 1000 unique users each requesting 1 seat, exactly 100 succeed and the rest get 409** — but under different load shapes.

| Script | Shape | Best for | Duration |
|---|---|---|---|
| `loadtest/burst.js` | 1000 VUs, 1 iter each — pure shock | Correctness check (no oversell, no data loss) | <1 s |
| `loadtest/ramp.js`  | 7 stages: 0 → 50 → 200 → 1000 VUs | Watching the system respond to *increasing* load (screencast-friendly) | ~90 s |

The shared dashboard (`loadtest/dashboard.html` served by `cmd/dashboard-server`) auto-polls k6's REST API every 1s and shows: active VUs (the headline number during a ramp), request rate, latency p50/p90/p95/p99, status-code breakdown (200 vs 409), and a progress bar. Without `--linger`, k6 exits when the test finishes; with it, the REST API stays up so you can keep watching the numbers (Ctrl-C to quit).

#### Run the burst (1 second of shock)

```bash
# Pre-reqs: services + dashboard running
make all-services dashboard-bg    # api/worker/watcher + http://localhost:8082/
open http://localhost:8082/        # open the live UI in your browser FIRST

# Then the actual test (auto-resets state + mints JWTs + runs k6 in background)
make loadtest-burst

# To re-run: make stop-loadtest && make loadtest-burst
```

Expected output (also written to `logs/k6-burst.log`):

```
─────────────── LOAD TEST RESULTS ───────────────
  Total requests    : 1000
  Reserved (200)    :  100  ✅
  Sold out (409)    :  900  ✅
  Other status      :    0  ✅
  Oversell check    : ✅ no oversell
──────────────────────────────────────────────────
```

#### Run the ramp (90 seconds of waves — like the YouTube videos)

```bash
make all-services dashboard-bg
open http://localhost:8082/
make loadtest-ramp
# k6 runs in the background for ~90s; the dashboard tracks the VU curve
# in real time. To free the REST port: make stop-loadtest
```

Tune the VU count: `make loadtest-burst VUS=500`, `make loadtest-ramp VUS=2000`. To target a different event: `EVENT_ID=2 make loadtest-burst`.

The dashboard will show VU count climbing 0 → 50 → 200 → 1000 in waves (the "█" bars in the terminal output below are the same number the dashboard graph is drawing). A real run looks like this — 1.8M requests over 90 s, exactly 100 winners, zero errors:

```
t=  5s  VUs=  49  ██            rate=18433/s  200=100  409= 190466
t= 10s  VUs=  50  ██            rate=19292/s  200=100  409= 297711
t= 25s  VUs= 122  ██████        rate=19906/s  200=100  409= 615953
t= 30s  VUs= 200  ██████████    rate=19958/s  200=100  409= 718963
t= 50s  VUs= 512  ████████...   rate=20005/s  200=100  409=1135223
t= 60s  VUs=1000  ██████████... rate=19999/s  200=100  409=1340616
t= 75s  VUs=1000  ██████████... rate=20084/s  200=100  409=1659254
t= 85s  VUs=   0                rate=20129/s  200=100  409=1811531
```

`reserved_ok` stays pinned at 100 for the entire 90 seconds — because inv=100 is decided in the first few hundred milliseconds and the ramp can never change that. The interesting number to watch is `rate` (does throughput scale linearly with VU count?) and `p99` (does latency degrade as the system absorbs more concurrent load?). On an M5 we held 20,000 req/s for a full minute with p99=62 ms.

The real correctness assertions (the four ✅ lines) live in `handleSummary()` at the bottom of each k6 script — k6's `http_req_failed` threshold is intentionally relaxed because we *expect* ~90% of the requests to be 409s; the `handleSummary` check distinguishes "expected 409" from "unexpected 5xx" via the dedicated `reserved_ok` / `sold_out` / `other_status` Counters.

Tune the burst size with `VUS=N ./loadtest/mint-jwts.sh N && VUS=N k6 run ...`. To target a different event, pass `EVENT_ID=N` to `reset-state.sh` and `mint-jwts.sh`.

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
