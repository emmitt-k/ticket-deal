# Architecture Deep-Dive

> The full story behind Mini-Ticketmaster — what each piece does, why it exists, and what failure it prevents.

This document complements the [README](../README.md). The README is the project landing page; this is the engineering manual. **§1–§14 explain the design**, **§15 is a per-file reference** for when you need to find or modify a specific file.

**Code state reflected:** through Phase 13 (4-pillar observability). Phases 0–9 (the core system) are landed and unchanged; Phases 10–13 layered on a complete observability stack split across 4 stacked feature branches:

| Branch | Phase | What |
|---|---|---|
| `feature/observability` | 10 | Prometheus scrape + Grafana "Ticket Deal — Overview" dashboard |
| `feature/tracing` | 11 | OTel SDK + Jaeger UI + SQS traceparent propagation (ElasticMQ body-fallback) |
| `feature/logging` | 12 | `log/slog` migration + custom `ContextHandler` for trace_id/span_id correlation + clear-logs script |
| `feature/loki` | 13 | Loki + Promtail + Grafana "Ticket Deal — Logs" dashboard (trace_id in label-derived log line → click → Jaeger) |

The headline test (`loadtest/burst.js`, 1000 VUs, event_id=1, inv=100) still produces exactly 100 × 200 + 900 × 409 — and now every one of those requests shows up in all three observability sinks, joined by `trace_id`. See §14 for the story.

---

## Table of Contents

1. [System Overview](#1-system-overview)
2. [The 3-Phase Gatekeeper](#2-the-3-phase-gatekeeper)
3. [Phase 1 — Virtual Waiting Room](#3-phase-1--virtual-waiting-room)
4. [Phase 2 — Atomic Seat Locking](#4-phase-2--atomic-seat-locking)
5. [Phase 3 — Asynchronous Fulfillment](#5-phase-3--asynchronous-fulfillment)
6. [TOCTOU and Why Lua Fixes It](#6-toctou-and-why-lua-fixes-it)
7. [The Two-State Model](#7-the-two-state-model)
8. [Reservation Lifecycle](#8-reservation-lifecycle)
9. [Handling Expirations](#9-handling-expirations)
10. [Why SQS Standard (Not FIFO)](#10-why-sqs-standard-not-fifo)
11. [Failure Modes & Mitigations](#11-failure-modes--mitigations)
12. [Known Development Sharp Edges](#12-known-development-sharp-edges)
13. [The Observability Pillar](#13-the-observability-pillar)
14. [File-by-File Reference](#14-file-by-file-reference)

---

## 1. System Overview

```mermaid
graph TB
    subgraph Client["Client"]
        User["User Browser<br/>(or k6 VU)"]
    end

    subgraph HotPath["Hot Path — request-time, target < 50 ms"]
        API["Go API<br/>(chi router)"]
        RR["Redis: Waiting Room<br/>IP + event token bucket<br/>+ ZSET queue"]
        RI["Redis: Inventory + Hold keys<br/>(Lua atomic ops)"]
        SSE["SSE Stream<br/>Queue position"]
        Watcher["expiration-watcher<br/>(Redis keyspace subscriber)"]
    end

    subgraph AsyncPath["Async Path — post-reservation"]
        SQS["AWS SQS Standard<br/>(ElasticMQ locally)"]
        Worker["Go Worker<br/>(SQS consumer + 60s sweep)"]
    end

    subgraph ColdPath["Cold Path — durable, no SLA"]
        PG["PostgreSQL 16<br/>Source of truth"]
    end

    subgraph Obs["Observability Pillar — see §14"]
        Prom["Prometheus<br/>:9090 scrape"]
        Grafana["Grafana<br/>:3000 dashboards"]
        Jaeger["Jaeger<br/>:16686 traces"]
        Loki["Loki<br/>:3100 log search"]
        Promtail["Promtail<br/>tail + ship"]
        Logs["logs/*.log<br/>(host)"]
    end

    User -->|"HTTP"| API
    API <-->|"Lua: token / ZADD / ZRANK"| RR
    API <-->|"text/event-stream"| SSE
    SSE -->|"position / admitted"| User
    API <-->|"EVAL reserve.lua"| RI
    API -->|"SendMessage<br/>(+ traceparent)"| SQS
    SQS -->|"ReceiveMessage<br/>(propagate ctx)"| Worker
    Worker -->|"INSERT ... ON CONFLICT<br/>RETURNING seats"| PG
    RI -.->|"hold key TTL expires"| Watcher
    Watcher -.->|"Compensate:<br/>UPDATE EXPIRED + INCRBY"| PG
    Watcher -.->|"INCR"| RI
    Worker -.->|"60s sweep:<br/>UPDATE EXPIRED + INCRBY"| PG
    Worker -.->|"INCR"| RI

    API -.->|"/metrics"| Prom
    Worker -.->|"/metrics"| Prom
    Watcher -.->|"/metrics"| Prom
    Prom --> Grafana

    API -.->|"OTLP gRPC :4317"| Jaeger
    Worker -.->|"OTLP gRPC :4317"| Jaeger
    Watcher -.->|"OTLP gRPC :4317"| Jaeger

    API -.->|"slog (stdout → file)"| Logs
    Worker -.->|"slog (stdout → file)"| Logs
    Watcher -.->|"slog (stdout → file)"| Logs
    Logs -.->|"tail"| Promtail
    Promtail -->|"push"| Loki
    Loki --> Grafana
    Grafana -.->|"trace_id click → Jaeger"| Jaeger
```

**Four latency tiers, each with its own SLA:**

| Tier      | Components                 | Target          | Why                                                    |
| ----------- | ---------------------------- | ----------------- | -------------------------------------------------------- |
| **Hot**   | API ↔ Redis               | < 50 ms         | User is waiting for a seat; every ms matters           |
| **Async** | API → SQS                 | < 10 ms publish | Only needs to enqueue; the user already has their seat |
| **Watch** | watcher ↔ Redis keyspace  | ~ real-time     | Catch expiries as they fire                            |
| **Cold**  | Worker → Postgres (sweep) | every 60 s      | Safety net for missed keyspace events                  |

The cardinal rule: **Postgres never touches the hot path.** This is the design constraint that shapes every decision below.

The observability layer is **a side-channel, not part of the request path.** The hot path is unchanged — every observability wire on the right is sampled/buffered locally and shipped to a side system. A failure in Promtail, Loki, or even the whole Jaeger OTLP exporter does not affect a single user request. (See §14.6 for the "trace_id is the join key" story that makes all three sinks queryable as one.)

---

## 2. The 3-Phase Gatekeeper

```mermaid
sequenceDiagram
    autonumber
    actor User
    participant API as Go API
    participant IP as Redis<br/>(IP bucket)
    participant RR as Redis<br/>(event bucket + ZSET)
    participant RI as Redis<br/>(inventory + hold key)
    participant SQS as SQS Standard
    participant W as Go Worker
    participant KS as Watcher
    participant DB as PostgreSQL

    rect rgb(240, 248, 255)
        Note over User,RR: Phase 1 — Virtual Waiting Room
        User->>API: POST /api/tickets/enter<br/>{user_id, event_id}
        API->>IP: EVAL ip_bucket.lua
        alt IP rate-limited
            IP-->>API: rejected
            API-->>User: 429 {ip_rate_limited}
        else IP admitted
            IP-->>API: ok
            API->>RR: EVAL token_bucket.lua
            alt event admitted
                    RR-->>API: admitted
                    API->>API: Issue JWT
                    API-->>User: 200 {token, expires_in: 120}
                else queued
                    RR-->>API: position N
                    API-->>User: 429 {queue_url, position, eta}
                    User->>API: GET /queue (SSE)
                    loop every 15 s until admitted
                        RR-->>API: ZRANK position
                        API-->>User: event: position {N, eta}
                    end
                    Note over RR: Drainer goroutine promotes<br/>as token refills → SSE emits<br/>"event: admitted {token}"
                end
        end
    end

    rect rgb(255, 250, 240)
        Note over User,RI: Phase 2 — Atomic Seat Lock
        User->>API: POST /api/tickets/reserve<br/>{seats_requested?} + JWT
        API->>API: auth.Verify(JWT)
        API->>RI: EVAL reserve.lua<br/>{event_id, user_id, ttl, seats}
        alt reserved
            RI-->>API: {1, "reserved"}
            API->>API: mint reservation_id (UUID)
            API->>SQS: SendMessage<br/>{reservation_id, user_id,<br/>event_id, seats, expires_at}
            API-->>User: 200 {reservation_id,<br/>seats, expires_in: 600}
        else sold_out
            RI-->>API: {0, "sold_out"}
            API-->>User: 409 sold_out
        else already_holding
            RI-->>API: {0, "already_holding"}
            API-->>User: 409 already_holding
        end
    end

    rect rgb(245, 255, 245)
        Note over W,DB: Phase 3 — Async Fulfillment
        SQS->>W: ReceiveMessage (long-poll 20s)
        W->>DB: INSERT reservations (...) <br/>ON CONFLICT (reservation_id) DO NOTHING
        W->>SQS: DeleteMessage
    end

    rect rgb(255, 245, 245)
        Note over KS,DB: Phase 7 — Expiration (two paths)
        RI-.->KS: hold key expires
        KS->>DB: Compensate: UPDATE ... EXPIRED<br/>RETURNING seats
        KS->>RI: INCRBY inventory
        W-->>DB: 60s sweep: UPDATE ... EXPIRED<br/>WHERE expires_at < NOW()
        W->>RI: INCRBY inventory (safety net)
    end
```

Each phase solves a different problem and they compose:

- **Phase 1** keeps Phase 2 from being overwhelmed (and blocks bulk bots via the IP tier)
- **Phase 2** keeps Phase 3's queue from being flooded with "almost-winners"
- **Phase 3** keeps the user-facing response time independent of Postgres latency
- **Phase 7** returns inventory when a hold expires, by either event-driven (fast) or DB-sweep (safe) path

---

## 3. Phase 1 — Virtual Waiting Room

**Goal:** shed load *before* it reaches the inventory layer. The unique wrinkle vs. naive rate limiting is the **two-tier gate**: a per-IP bucket kills bulk abuse; a per-event bucket handles legitimate burst.

### Two-tier admission

```mermaid
graph LR
    A["POST /enter<br/>{user_id, event_id}"] --> B{"Tier 1:<br/>IP bucket<br/>(Lua atomic)"}
    B -->|"< 1 token"| C["429 ip_rate_limited<br/>(hard reject, no queue)"]
    B -->|"≥ 1 token"| D{"Tier 2:<br/>event bucket<br/>(Lua atomic)"}
    D -->|"< 1 token"| E["ZADD queue:event:42<br/>429 + {queue_url}"]
    D -->|"≥ 1 token"| F["Issue JWT (HS256, 2 min TTL)<br/>200 {token}"]
    E --> G["SSE: position events<br/>every 15s"]
    G -->|"drainer goroutine<br/>promotes user"| F

    style B fill:#ffe6e6
    style D fill:#fff5e6
    style F fill:#e6ffe6
```

**Why two tiers?** A per-IP bucket is cheaper than a per-event bucket to evaluate, but it can't be the only defense — one attacker with 10,000 residential IPs would each get the full burst. The per-event bucket caps *total* admission regardless of attacker count.

### Tier 1: IP bucket (`internal/iplimit/scripts/ip_bucket.lua`)

Classic lazy-refill token bucket:

- State per IP: Redis Hash `{tokens, last_refill_ms}` (one key per IP, e.g. `ip:1.2.3.4`)
- On call: compute elapsed time → refill `tokens = min(capacity, tokens + elapsed * refill_rate)` → consume one if available
- 1-day TTL on the bucket state (safety net against stale entries)
- **Hard reject** on exhaustion — no queue, no fairness for bots

### Tier 2: Event bucket + ZSET queue (`internal/redis/scripts/token_bucket.lua`)

Same shape, but with an overflow queue:

- State per event: Redis Hash `{tokens, last_refill_ms}` (key: `bucket:event:42`)
- Queue: Redis ZSET (key: `queue:event:42`), scored by **nanosecond timestamp** for fairness
- On call: refill → consume if available; else `ZREMRANGEBYSCORE` to drop stale entries, `ZADD now user`, return 1-based position
- Queue TTL is refreshed on every insert (track current and stale)

### The drainer goroutine (`internal/waitingroom/drainer.go`)

A long-lived goroutine in the API process. Lazily spawns one sub-loop per event that currently has a queue; sub-loop exits after 5 minutes of empty queue.

Every tick (100 ms):

1. `ZCARD queue:event:42` — how many people waiting?
2. Compute `tokensToAdd = refill_rate * tick_interval` (capped at queue size, floor of 1)
3. For each token: `ZPOPMIN` (FIFO), `Issue` JWT, `Deliver` to the user's SSE channel

The drainer and the SSE goroutines communicate via an in-process `map[eventID:userID]chan string` (`internal/waitingroom/queue.go`). At Phase 4 scale (thousands, not millions) an in-process registry is simpler and faster than Redis pub/sub.

### SSE position updates

When queued, the user receives `429 {queue_url}` and opens an SSE stream:

```
GET /api/tickets/queue?event_id=42&user_id=alice HTTP/1.1
Accept: text/event-stream
```

```
HTTP/1.1 200 OK
Content-Type: text/event-stream

event: position
data: {"position": 137, "eta_seconds": 14}

event: position
data: {"position": 98, "eta_seconds": 10}

event: admitted
data: {"token": "eyJhbGciOiJIUzI1NiIs..."}
```

The handler runs a goroutine that listens on three things at once (`select{}`): the admission channel, a 15-second heartbeat ticker (re-reads `ZRANK` to keep the position fresh), and the request context (fires when the client disconnects).

**Why SSE, not WebSockets?** Data flows one way: server → client. SSE is plain `HTTP/1.1`, auto-reconnects on the client, works through every proxy, and needs zero framing/upgrade ceremony — WebSockets would add framing, masking, ping/pong, and a separate handshake for no benefit.

---

## 4. Phase 2 — Atomic Seat Locking

**Goal:** guarantee that exactly N seats are allocated for N requests, even with millisecond-level concurrency.

### The Lua reservation script (`internal/redis/scripts/reserve.lua`)

```lua
-- KEYS[1] = seat_inventory_key   e.g. "inventory:event:42"
-- KEYS[2] = user_hold_key        e.g. "hold:event:42:user:abc123"
-- ARGV[1] = hold_ttl_seconds     e.g. "600" (10 minutes)
-- ARGV[2] = seats_requested      e.g. "1" (max 10 — see API handler validation)

local available = tonumber(redis.call('GET', KEYS[1]))

if available == nil then
  return {-1, 'event_not_found'}        -- event was never seeded
end

local requested = tonumber(ARGV[2])
if available < requested then
  return {0, 'sold_out'}                -- not enough seats
end

-- Guard: same user can't double-book within their hold TTL
local existing_hold = redis.call('GET', KEYS[2])
if existing_hold then
  return {0, 'already_holding'}
end

redis.call('DECRBY', KEYS[1], requested)            -- commit the decrement
redis.call('SET', KEYS[2], ARGV[2], 'EX', ARGV[1])   -- hold key TTL + seats count

return {1, 'reserved'}
```

**Execution time:** ~50 µs on a single Redis node. Out of 1,000 requests racing for 10 seats, exactly 10 return `{1, "reserved"}`. The other 990 get `{0, "sold_out"}` in microseconds — **no DB call ever happens**.

### Why Redis Lua is atomic

```mermaid
graph LR
    A["Redis single-threaded<br/>command loop"] --> B["EVAL reserve.lua<br/>request A"]
    A --> C["EVAL reserve.lua<br/>request B"]
    B -->|"block until done"| D["GET inventory<br/>check<br/>DECRBY<br/>SET hold"]
    C -->|"queued behind A"| E["GET inventory<br/>check (now 0)<br/>return sold_out"]

    style A fill:#ffe6e6
    style D fill:#e6ffe6
    style E fill:#fff5e6
```

Redis runs all commands on a **single thread**. When you `EVAL` a Lua script, the server blocks on that thread until the script completes — no other command from any client can interleave. The script is a transaction by construction. You don't need `WATCH`/`MULTI`/`EXEC`, you don't need optimistic locking, you don't need to worry about retries.

**This is the single most important property of the system. Atomicity for free.**

### The hold key

A successful reservation creates a hold key (`hold:event:42:user:abc123`) with a **600-second TTL**. The *value* of the key is the seats count (so Phase 7's keyspace watcher can know how many to INCRBY without a DB round-trip — see §9).

The hold key serves **four** roles simultaneously:

1. **Anti-double-booking** — `GET` blocks the same user from a second reservation within 10 minutes
2. **Auto-release on abandonment** — TTL expires → Redis fires keyspace notification → watcher compensates
3. **No cron job for cleanup** — Redis itself enforces the TTL; no background sweeper required for the lock itself
4. **Seats payload for compensation** — the value is the seat count, readable when needed

### Seat limits

The API handler (`internal/api/reserve_handler.go`) enforces `1 ≤ seats_requested ≤ 10`. The DB schema (`migrations/003_add_seats.sql`) mirrors that with a `CHECK (seats >= 1 AND seats <= 10)` constraint.

---

## 5. Phase 3 — Asynchronous Fulfillment

**Goal:** persist confirmed holds to PostgreSQL *after* the user has secured their seat, never during the seat allocation race.

```mermaid
graph LR
    A["Lua returns 1"] --> B["SET hold key<br/>value=seats, EX=600"]
    B --> C["SQS SendMessage<br/>{reservation_id,<br/>user_id, event_id,<br/>seats, expires_at}"]
    C --> D["HTTP 200<br/>to user"]
    C -.->|"async"| E["Worker<br/>ReceiveMessage"]
    E --> F["INSERT reservations<br/>(..., seats)<br/>ON CONFLICT DO NOTHING"]
    F --> G["DeleteMessage"]

    style A fill:#e6ffe6
    style C fill:#fff5e6
    style E fill:#e6f3ff
    style F fill:#f0e6ff
```

### Publisher interface seam

The handler doesn't import SQS directly — it depends on `api.ReservationPublisher` (`internal/api/publisher.go`), a one-method interface:

```go
type ReservationPublisher interface {
    Publish(ctx context.Context, body []byte) error
}
```

Two implementations:

- **`LogPublisher`** — Phase 5 stub. Writes `[STUB SQS publish] {…}` to the log. Used in dev environments with `SQS_QUEUE_URL` unset.
- **`ReservationAPIPublisher`** — Phase 6. Unmarshals the body (round-trip acts as wire-format validation), then forwards to an inner `SQSPublisher`, which marshals back to JSON for `sqs.SendMessage`. The double marshal is a few microseconds — cheap insurance against silent drift between API and worker.

Wiring happens once in `cmd/api/main.go`'s `buildPublisher()`. **If `SQS_QUEUE_URL == ""`, the function falls back to `LogPublisher`** so the API stays runnable for handler-level work even without SQS configured.

### The worker's idempotency contract

SQS Standard guarantees **at-least-once** delivery. The worker may receive the same message twice (visibility timeout, network blip, crash before delete). That's fine, *as long as the SQL is idempotent*:

```sql
INSERT INTO reservations
    (reservation_id, user_id, event_id, status, expires_at, seats)
VALUES
    ($1, $2, $3, 'PENDING_PAYMENT', $4, $5)
ON CONFLICT (reservation_id) DO NOTHING;
```

The `reservation_id` is a UUID generated by the API when the hold is created. The `UUID PRIMARY KEY` constraint on the table means the second INSERT is a no-op. The worker can `DeleteMessage` either way — `tag.RowsAffected() == 0` is informational, not an error.

**This is why we chose SQS Standard over FIFO.** FIFO has built-in dedup — but it's capped at 300 msg/s, with extra cost. Standard has unlimited throughput; we get exactly-once *processing* by making the *database* the dedup layer. See §10 for the full reasoning.

### At-least-once in practice

`internal/queue/consumer.go::Poll` long-polls in 20-second waits (capped by SQS), receives up to 10 messages per call, dispatches each to a `MessageHandler`:

- `handler` returns `nil` → `DeleteMessage` (success path)
- `handler` returns `err` → leave visible (SQS will redeliver after visibility timeout)
- `Poll`'s `ReceiveMessage` errors → log, 1-second backoff, retry
- `Poll`'s `DeleteMessage` errors after success → log and move on (the DB row already exists; idempotency absorbs the redelivery)

The worker binary runs `Poll` in its main goroutine and watches `signal.NotifyContext(SIGINT, SIGTERM)` for graceful shutdown.

---

## 6. TOCTOU and Why Lua Fixes It

**TOCTOU** = Time-of-Check to Time-of-Use. The bug class that owns most overselling incidents.

### Without Lua: the race

```mermaid
sequenceDiagram
    title TOCTOU race with a relational DB
    participant TX-A
    participant TX-B
    participant DB as Postgres

    par TX-A
        TX-A->>DB: SELECT available WHERE id=1
        DB-->>TX-A: 1
        TX-A->>DB: UPDATE available=0 WHERE id=1
    and TX-B (interleaved)
        TX-B->>DB: SELECT available WHERE id=1
        DB-->>TX-B: 1   ← stale read!
        TX-B->>DB: UPDATE available=0 WHERE id=1
    end

    Note over TX-A,DB: Both transactions committed.<br/>OVERSOLD. The event now shows 0 seats but sold 2.
```

The window between `SELECT` and `UPDATE` is the TOCTOU window. With 1,000 concurrent transactions on the same row, this window opens thousands of times per second. `WHERE available > 0` does not save you — the predicate check happens at `UPDATE` time, but the *read* may have been from a stale snapshot.

You can fix this with `SELECT ... FOR UPDATE` or optimistic concurrency control, but every fix adds latency and complexity.

### With Redis Lua: no window

```mermaid
sequenceDiagram
    title Lua script — the check and the act are one operation
    participant R-A as Request A
    participant R-B as Request B
    participant Redis

    Note over Redis: Single-threaded command loop

    R-A->>Redis: EVAL reserve.lua
    activate Redis
    Redis->>Redis: GET inventory (1)
    Redis->>Redis: check available >= 1 ✓
    Redis->>Redis: DECRBY inventory 1
    Redis->>Redis: SET hold key EX 600
    Redis-->>R-A: {1, "reserved"}
    deactivate Redis

    R-B->>Redis: EVAL reserve.lua
    activate Redis
    Redis->>Redis: GET inventory (0)
    Redis->>Redis: check available >= 1 ✗
    Redis-->>R-B: {0, "sold_out"}
    deactivate Redis
```

No window. The check (`GET inventory`) and the act (`DECRBY`) happen in the **same atomic step**. By the time Request B's script runs, the inventory is already 0.

---

## 7. The Two-State Model

```mermaid
graph LR
    subgraph Active["Active state<br/>(truth of NOW)"]
        R1["Redis<br/>hold:event:42:user:abc<br/>EX 600, value=seats<br/>inventory:event:42 = N"]
    end

    subgraph Persistent["Persistent state<br/>(truth of FOREVER)"]
        R2["PostgreSQL<br/>reservations table<br/>status, expires_at, seats"]
    end

    R1 -->|"Phase 3:<br/>SQS → worker"| R2
    R2 -.->|"Phase 7 watcher:<br/>on hold key expiry"| R1
    R2 -.->|"Phase 7 sweep:<br/>every 60 s"| R1

    style Active fill:#ffe6e6
    style Persistent fill:#e6f3ff
```


| Concern                                | Lives in | Lifetime           |
| ---------------------------------------- | ---------- | -------------------- |
| "Who has what, right now"              | Redis    | Hold TTL (~10 min) |
| "What was ever sold, with what status" | Postgres | Forever            |

**The asymmetry is the point.** Redis is the hot, mutable, fast-fading truth of "this user's seat is reserved, expires in 9m 42s." Postgres is the slow, immutable, forever-true ledger of confirmed orders. They serve different access patterns.

You never read from Postgres in the hot path. You never trust Redis for durability.

Phase 7 closes the asymmetry: Postgres's truth can *correct* Redis's truth (sweep). For 99% of cases, Redis drives Postgres (worker INSERT). For the rare lost-notification case, Postgres drives Redis (sweep INCRBY).

---

## 8. Reservation Lifecycle

```mermaid
stateDiagram-v2
    [*] --> PENDING_PAYMENT: Lua reserves seat<br/>+ API publishes SQS message<br/>(worker INSERTs row)

    PENDING_PAYMENT --> CONFIRMED: Payment webhook<br/>(Stripe, future)
    PENDING_PAYMENT --> EXPIRED: Hold key TTL fires<br/>(watcher OR sweep,<br/>within 60s)
    PENDING_PAYMENT --> CANCELLED: User cancels<br/>(future feature)

    CONFIRMED --> [*]: Order complete
    EXPIRED --> [*]: Seat released<br/>via INCRBY inventory
    CANCELLED --> [*]: Seat released<br/>via INCRBY inventory
```

**Four terminal states** (`CONFIRMED`, `EXPIRED`, `CANCELLED`, plus the implicit "no row ever written" if the worker never gets the SQS message). `PENDING_PAYMENT` is the only transitional state.

The `status` column has a `CHECK` constraint enforcing this set (`migrations/001_init.sql`), and `internal/db/reservations.go` exposes the four values as typed constants (`StatusPendingPayment`, etc.) so a typo at a call site fails to compile.

Phase 7 owns `PENDING_PAYMENT → EXPIRED` (with two redundant paths — see §9).

---

## 9. Handling Expirations

The lifecycle's `PENDING_PAYMENT → EXPIRED` transition needs two things to happen atomically: the DB row flips to `EXPIRED` and the Redis inventory counter goes back up. Phase 7 ships **two redundant paths** so neither can lose a seat.

```mermaid
sequenceDiagram
    participant Hold as Redis<br/>(hold:event:42:user:abc)
    participant Watch as Watcher<br/>(cmd/expiration-watcher)
    participant DB as PostgreSQL<br/>(reservations)
    participant Inv as Redis<br/>(inventory:event:42)
    participant Sweep as Worker sweep<br/>(cmd/worker goroutine)

    Note over Hold,Watch: Path A — event-driven, fast
    Hold-->>Watch: PSUBSCRIBE __keyevent@0__:expired
    Watch->>Watch: ParseHoldKey<br/>(event=42, user=abc)
    Watch->>DB: Compensate():<br/>WITH target AS (...PENDING)<br/>UPDATE ... SET status='EXPIRED'<br/>RETURNING seats
    alt CAS winner (rows updated)
        DB-->>Watch: seats=2
        Watch->>Inv: INCRBY inventory:event:42 2
    else CAS loser (race with sweep, or no row)
        DB-->>Watch: seats=0
        Watch->>Watch: log warn, skip
    end

    Note over Sweep,DB: Path B — DB-driven, safety net
    loop every 60s
        Sweep->>DB: UPDATE reservations<br/>SET status='EXPIRED'<br/>WHERE status='PENDING_PAYMENT'<br/>  AND expires_at < NOW()<br/>RETURNING reservation_id, user_id, event_id, seats
        DB-->>Sweep: [row1, row2, ...]
        loop for each row
            Sweep->>Inv: INCRBY inventory:event:<id> <seats>
        end
    end
```

### The shared `Compensate` function

Both paths converge on `internal/expire/compensate.go::Compensate(ctx, pool, rdb, eventID, userID)`:

1. **CAS UPDATE** — single CTE that finds the matching `PENDING_PAYMENT` row for `(user_id, event_id)` and flips it to `EXPIRED`, returning the seat count. The `WHERE status='PENDING_PAYMENT'` filter is the compare-and-set: a second concurrent caller sees 0 rows and skips the INCRBY.
2. **INCRBY inventory** — `INCRBY inventory:event:<id> <seats>` using the row's `seats` value (persisted via migration `003_add_seats.sql`).

This function is the only thing both paths need to share. Putting it in one place means the watcher's event path and the worker's sweep path can never drift.

### Path A: Keyspace watcher (`cmd/expiration-watcher`)

A separate binary that runs `expire.RunWatcher(ctx, rdb, pool)`:

1. `PSUBSCRIBE __keyevent@0__:expired` (DB 0 is the only DB this app uses)
2. On each message, `expire.ParseHoldKey(key)` extracts `(eventID, userID)`. Keys that don't match `hold:event:N:user:U` are filtered out — the expired channel fires for **every** expired key, including ZSET members and bucket state.
3. Valid keys go to `Compensate(ctx, pool, rdb, eventID, userID)`.

**Reconnect strategy:** go-redis PubSub doesn't auto-reconnect on connection drop. `RunWatcher` wraps the subscribe loop with exponential backoff (1 s → 2 → 4 → … → 30 s cap). After a clean subscribe, backoff resets to 1 s.

**Configuration:** `DATABASE_URL` + `REDIS_ADDR` env vars. Defaults to `localhost:6379`.

### Path B: DB-driven sweep (`cmd/worker` goroutine)

The worker runs `expire.RunSweep(ctx, pool, rdb, interval)` as a goroutine alongside the SQS poll loop.

Every `interval` (default 60 s; env `SWEEP_INTERVAL_SECONDS`):

1. `FindAndExpireSweptRows` runs the atomic SQL:
   ```sql
   UPDATE reservations
   SET status = 'EXPIRED', updated_at = NOW()
   WHERE status = 'PENDING_PAYMENT' AND expires_at < NOW()
   RETURNING reservation_id, user_id, event_id, seats;
   ```
2. For each returned row, `INCRBY inventory:event:<id> <seats>`.

**Idempotent** — the `WHERE` clause limits to currently-`PENDING_PAYMENT` rows, so a second tick finds nothing to do.

### Race resolution

If both paths fire for the same `(user, event)`:


| Path                                  | What happens                                                                                                             |
| --------------------------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| Watcher CAS wins, sweep runs after    | Sweep's UPDATE finds 0 rows (status already EXPIRED). Sweep's INCRBY doesn't run.**Total INCRBY: 1.** ✓                 |
| Sweep runs first, watcher fires after | Watcher's CAS finds 0 rows (status already EXPIRED). Watcher's INCRBY doesn't run.**Total INCRBY: 1.** ✓                |
| Both run concurrently                 | Postgres row-locking serializes the UPDATEs. Whichever commits first wins; the other sees 0 rows.**Total INCRBY: 1.** ✓ |

The `WHERE status='PENDING_PAYMENT'` is the entire mechanism. No distributed locks, no coordination protocol.

### Configuration


| Env var                  | Default          | Used by                 |
| -------------------------- | ------------------ | ------------------------- |
| `SWEEP_INTERVAL_SECONDS` | `60`             | Worker sweep tick rate  |
| `REDIS_ADDR`             | `localhost:6379` | Both watcher and worker |

Redis must be started with `--notify-keyspace-events Ex` (`docker-compose.yml` already does this) for any of Path A to fire.

### Caveat: orphan hold keys

The Lua reserve script always publishes SQS *after* creating the hold key. If SQS publish fails between those two steps, the API logs the error but the Redis hold key remains. The Lua committed, but no DB row will ever be written.

When the hold key fires the keyspace notification, `Compensate` finds no DB row → seats=0 → no INCRBY. The seat is **stuck held forever** until a manual operator intervention.

This is the documented edge case from `internal/expire/compensate.go`. To close it would require a Redis-side companion key that survives the TTL (uglier than the current trade-off), or a recovery scan over all `hold:*` keys with no matching DB row (O(inventory) work per restart).

---

## 10. Why SQS Standard (Not FIFO)

The original design used SQS FIFO. We switched. Here's why.

```mermaid
graph TB
    subgraph Standard["SQS Standard"]
        S1["Throughput: unlimited"]
        S2["Ordering: best-effort"]
        S3["Dedup: none"]
        S4["Cost: lower"]
    end

    subgraph FIFO["SQS FIFO"]
        F1["Throughput: 300 msg/s<br/>(3,000 w/ batching)"]
        F2["Ordering: per message group"]
        F3["Dedup: 5-min window"]
        F4["Cost: higher"]
    end

    subgraph Ours["Our mitigation"]
        O1["Idempotent INSERT<br/>(ON CONFLICT DO NOTHING)<br/>on reservation_id"]
        O2["Single consumer per reservation<br/>(no partition needed<br/>because Lua already serialized)"]
    end

    FIFO -.->|"dedup moved to DB"| Ours
    Standard -.->|"ordering already free"| Ours

    style Standard fill:#e6ffe6
    style FIFO fill:#ffe6e6
    style Ours fill:#fff5e6
```


| Concern              | FIFO's answer   | Our answer                                                                          |
| ---------------------- | ----------------- | ------------------------------------------------------------------------------------- |
| Duplicate processing | Built-in dedup  | `ON CONFLICT DO NOTHING` on `reservation_id`                                        |
| Ordering             | Message groups  | Already free — each reservation only flows through the worker's single INSERT path |
| Throughput           | Capped at 300/s | Unlimited                                                                           |

**The math:** Taylor Swift drops 150,000 tickets in 60 seconds = 2,500/sec sustained. FIFO caps at 300/s. We'd need 9 FIFO queues with message-group sharding just to keep up. Standard gives us the headroom for free; we pay for it with idempotent worker writes (which we'd need anyway, because FIFO dedup isn't bulletproof either — it only deduplicates within a 5-minute window).

---

## 11. Failure Modes & Mitigations


| #  | Failure                              | Where it hurts                        | Mitigation                                                                                                                     |
| ---- | -------------------------------------- | --------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------- |
| 1  | Redis goes down                      | Hot path dies                         | Sentinel + replica promotion (sub-second); API returns 503 from health check                                                   |
| 2  | SQS unreachable                      | API can't publish                     | Log + continue (Redis hold is created; orphan hold key — see §9 caveat)                                                      |
| 3  | Worker dies mid-INSERT               | Message redelivered                   | `ON CONFLICT (reservation_id) DO NOTHING` makes the retry safe                                                                 |
| 4  | Postgres unreachable                 | Worker can't persist                  | Message stays in SQS (visibility timeout expires → redelivered); seat is safe in Redis                                        |
| 5  | Lua script crashes mid-execution     | Connection error to caller            | Redis aborts the script; inventory unchanged; caller retries                                                                   |
| 6  | Token bucket under-fills             | Legit users queued unfairly           | Calibrate`WAIT_ROOM_REFILL_RATE`; expose metric on `bucket:event:<id>`                                                         |
| 7  | Hold key expires before payment      | User loses seat                       | Acceptable — 10 min is industry standard; bump via`RESERVE_HOLD_TTL_SECONDS`                                                  |
| 8  | Keyspace notification dropped        | Stale inventory                       | Sweep job catches within 60 s (Path B in §9)                                                                                  |
| 9  | Watcher process down                 | Real-time compensation stops          | Sweep continues to catch missed expirations within 60 s; spawn multiple replicas for high availability                         |
| 10 | Clock skew between Redis and API     | Hold expiry miscalculated             | All TTLs are relative durations from Redis`SET ... EX` — not absolute timestamps                                              |
| 11 | DDoS at the waiting room             | Bucket exhausted forever              | Per-IP token bucket tier (`internal/iplimit/`) rejects before the event bucket is touched                                      |
| 12 | Orphan hold key (no DB row)          | Seat stuck held forever               | Logged by`Compensate`; operator must manually INCR inventory (see §9 caveat)                                                  |
| 13 | INCRBY fails after DB EXPIRED flip   | DB says released, Redis says sold     | `Compensate` logs `WARN`; sweep won't re-process (status is EXPIRED). Operator alerting required.                              |
| 14 | AWS SDK initialized with empty creds | SQS client fails on first SendMessage | `internal/queue/aws.go` injects dummy `local/local` creds when `SQS_ENDPOINT_URL` is non-empty. **Local dev only** — see §12 |
| 15 | Prometheus down | Dashboards stop refreshing; no alerts fire | Hot path is unaffected (Prometheus scrapes are pull-based, timeouts only). Once Prometheus is back, `/metrics` re-scrapes; counters resume from current values (Prometheus computes `rate()` correctly across the gap). |
| 16 | Jaeger / OTLP collector down | Spans buffered in-memory are dropped on shutdown | `tracing.Init` returns a shutdown func; main calls it on `SIGTERM`. If the OTLP endpoint is unreachable, spans are *not* retried — they're dropped (acceptable: next request will be traced). Lower `OTEL_TRACES_SAMPLER_ARG` in prod so a transient gap isn't a big loss. |
| 17 | Promtail / Loki down | Logs still write to `logs/<service>.log`; only Grafana search breaks | Local file buffer is durable. Run `make clear-logs` to inspect offline. Promtail resumes shipping from the last read offset when it restarts (it does NOT replay missed lines from earlier). |
| 18 | `trace_id` made a Loki label | Loki starts 429-ing after ~5k unique values | The label cardinality rule (§13.5): only `service_name` and `detected_level` are labels. `trace_id` is a body field, queried via LogQL `\|= "<id>"`. If you ever see `429` from Loki, suspect a new label. |
| 19 | `make build` not used; raw `go build ./cmd/...` | Stray binaries (`./api`, `./worker`, …) in repo root | Use `make build` (or `go build -o bin/<name> ./cmd/<name>`). The Makefile rule enforces `-o`; raw `go build` doesn't. See AGENTS.md §Important Conventions #8. |

---

## 12. Known Development Sharp Edges

*Gotchas that cost debugging time during Phase 0–7. Save future-us the same headache.*

### 1. AWS SDK requires non-empty credentials even with a custom endpoint

The `aws-sdk-go-v2` SQS client fails on first send with `NoCredentialProviders` if both `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` are empty — **even when** you've pointed it at `http://localhost:9324` via `SQS_ENDPOINT_URL`. ElasticMQ does not authenticate, but the SDK still requires credentials to construct the signer.

**The fix** — `internal/queue/aws.go::NewSQSClient` detects `EndpointURL != ""` and sets dummy `local/local` creds if unset:

```go
if cfg.EndpointURL != "" {
    if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
        _ = os.Setenv("AWS_ACCESS_KEY_ID", "local")
    }
    if os.Getenv("AWS_SECRET_ACCESS_KEY") == "" {
        _ = os.Setenv("AWS_SECRET_ACCESS_KEY", "local")
    }
}
```

In production, `EndpointURL` is empty so this branch doesn't run — the SDK uses the standard credential chain (IAM role, env vars, `~/.aws/credentials`).

### 2. ElasticMQ healthcheck: use `curl`, not `wget` or `/dev/tcp`

The image's shell is **alpine `sh` (busybox)**, and a bare `GET /` against ElasticMQ returns **HTTP 400**. That breaks two seemingly-obvious healthchecks:


| Approach                                  | Why it fails                                                                           |
| ------------------------------------------- | ---------------------------------------------------------------------------------------- |
| `wget -q --spider http://localhost:9324/` | Exits with code 8 on the 400 response → false negative                                |
| `exec 3<>/dev/tcp/127.0.0.1/9324`         | `/dev/tcp` is **bash-only**; alpine `sh` says *"cannot create: Directory nonexistent"* |

**The fix** — `curl` exits 0 on any HTTP response, so we succeed whenever TCP is up:

```yaml
test: ["CMD-SHELL", "curl -s -o /dev/null http://127.0.0.1:9324/"]
```

Use this shape for any future docker-compose SQS-compatible service.

### 3. Adding the `seats` column to an existing `reservations` table

When Phase 7 added `seats` to track how many seats a hold represents (so the watcher can INCRBY the right number on expiry), the migration had to preserve all existing rows.

**Wrong:** `ADD COLUMN seats INTEGER NOT NULL` — fails because existing rows have no default.

**Right:** `ADD COLUMN seats INTEGER NOT NULL DEFAULT 1 CHECK (seats >= 1 AND seats <= 10)` — the `DEFAULT 1` backfills existing rows, and the `CHECK` mirrors the API handler's `1 ≤ seats_requested ≤ 10` validation. See `migrations/003_add_seats.sql`.

### 4. Lua's `TIME` breaks determinism in tests

All four Lua scripts (`reserve.lua`, `token_bucket.lua`, `ip_bucket.lua`) take `now_ms` as an `ARGV` rather than calling `redis.call('TIME')` inside the script. **Why:** tests need to inject specific times to verify "bucket refilled by X over Y seconds" deterministically. If a script called `TIME`, every test would race the wall clock and produce flaky assertions.

The convention is enforced by the comment in each script:

```lua
-- ARGV[3] = now_ms (current time in ms; passed by caller for testability — never use TIME inside Lua)
```

### 5. The redis client `PoolSize = 100` is a hot-path trade-off

`internal/redis/client.go::NewClient` sets `PoolSize: 100`. The justification is in the comment: 1,000 concurrent Lua calls each grab a connection, run EVAL in ~50 µs, and return — so 100 conns × ~20k ops/sec per conn = 2M ops/sec ceiling. Below the 1000-concurrent test ceiling, no connection contention.

If you ever bump the per-process connection count above the Redis server's `maxclients` setting (default 10,000), the client will block waiting for a free connection. Watch for `i/o timeout` errors in `cmd/api` logs as the warning sign.

---

## 13. The Observability Pillar

> **Why this section exists:** Phases 10–13 added a complete 4-pillar observability stack on top of the core system. The hot path is unchanged — every observability wire below is **a side-channel** that samples/buffers locally and ships to a side system. A failure in any observability sink does not affect a single user request.

### 13.1 Why observability, why now

The core system (§§1–10) is correct on paper but **blind at runtime**. The burst test can show "100 reserved, 900 sold_out" — but a real production system needs to answer:

| Question | Pillar that answers it |
|---|---|
| "How many reservations/sec right now? Is p(99) climbing?" | **Metrics** |
| "Why was *this one* request slow? Where did the time go?" | **Traces** |
| "What code path did this trace hit? What was the error message?" | **Logs** |
| "Is this anomaly real, or a transient blip?" | **All three**, joined by `trace_id` |

A single pillar gives one of these answers. **All four, joined by `trace_id`, give you the others for free** (§13.6). This is the design.

### 13.2 The four pillars at a glance

```mermaid
graph LR
    subgraph P1["Pillar 1 — Metrics (what's the number?)"]
        M1["Prometheus<br/>numeric time series"]
        M2["Grafana<br/>'Ticket Deal — Overview'<br/>7 panels"]
    end
    subgraph P2["Pillar 2 — Traces (where did the time go?)"]
        T1["OTel SDK<br/>auto + manual spans"]
        T2["Jaeger UI<br/>waterfall view"]
    end
    subgraph P3["Pillar 3 — Logs (what happened, in detail?)"]
        L1["log/slog + ContextHandler<br/>trace_id auto-injected"]
        L2["Loki + Promtail<br/>label-indexed log search"]
        L3["Grafana<br/>'Ticket Deal — Logs'<br/>live stream + filters"]
    end
    subgraph Join["The join key"]
        K["trace_id<br/>(32-hex, in every line/span/metric)"]
    end
    P1 --> K
    P2 --> K
    P3 --> K
```

| Pillar | Sink | Format | Cardinality | Sample question |
|---|---|---|---|---|
| **Metrics** | Prometheus → Grafana | numeric time series | low (label is `service`, `path`, `status`) | "What's the p(99) of `/reserve` over the last 5m?" |
| **Traces** | Jaeger UI | tree of spans | medium (per-request `trace_id`, `span_id`) | "Show me the slowest 5 traces in the last 10m." |
| **Logs** | Loki → Grafana | text lines with key=value attrs | low (label is `service_name`, `detected_level`) | "Find every log line for trace `fac1f09d…`." |
| **(Join)** | `trace_id` | 32-hex W3C TraceContext | one per request | "Show me everything that happened in this one request." |

### 13.3 Metrics: Prometheus + Grafana

**What it answers:** "Is the system healthy? Is something degrading? Did we just oversell?"

**The wire.** Every Go service exposes `/metrics` on its own port (Prometheus text format):

| Service | Endpoint |
|---|---|
| API | `http://localhost:8080/metrics` (mounted on the chi router) |
| Worker | `http://localhost:8081/metrics` (dedicated tiny HTTP server, env `METRICS_ADDR`) |
| Expiration-watcher | `http://localhost:8083/metrics` (env `METRICS_ADDR`) |

The Go services run on the host (not in Docker); Prometheus runs in a container and scrapes them via `host.docker.internal`. See `monitoring/prometheus.yml` for the scrape config.

**The metric catalog** (`internal/metrics/metrics.go`): every metric name, label set, and histogram bucket is defined in **one file** so all services use identical semantics. Highlights:

| Metric | Type | Labels | Use |
|---|---|---|---|
| `reservations_held_total` | counter | — | Throughput: reservations accepted |
| `reservations_completed_total` | counter | — | Throughput: SQS publish succeeded |
| `reservations_oversold_total` | counter | — | **The alarm metric. MUST stay 0.** |
| `reservations_sold_out_total` | counter | — | Load shedding: 409s returned |
| `reservations_already_holding_total` | counter | — | Idempotency: same user retried |
| `redis_seats_available` | gauge | `event_id` | Live inventory (periodically sampled) |
| `waiting_room_queue_length` | gauge | `event_id` | Live queue depth |
| `http_requests_total` | counter | `service, path, method, status` | Per-route, per-status traffic |
| `http_request_duration_seconds` | histogram | `service, path` | Latency p50/p90/p99 |

The **defining invariant** of this project: `reservations_oversold_total` is always 0. If it goes positive, *something fundamental broke* and a human needs to look. The Grafana "Ticket Deal — Overview" dashboard has a single-stat panel pinned to this metric in the top-left so it's the first thing the eye lands on.

**The dashboard** (`monitoring/grafana/dashboards/ticket-deal-overview.json`): 7 panels, dark "control room" aesthetic. Top row: oversold counter (red, always 0), reservations held/sold_out rate, HTTP error rate. Middle: p50/p90/p99 latency by route (line graph over 5m window), live inventory gauge per event. Bottom: queue depth per event, status code split (200/409/other as stacked area).

Auto-provisioned on Grafana startup via `monitoring/grafana/provisioning/dashboards/dashboards.yml` — no manual `import JSON` step.

### 13.4 Traces: OpenTelemetry + Jaeger

**What it answers:** "Why was *this one* request slow? Where did the time go? Which call took 200ms?"

**The wire.** Every Go service calls `tracing.Init(ctx, "api"|"worker"|"expiration-watcher")` at startup. This sets up:

1. The **global `TracerProvider`** with a W3C TraceContext propagator.
2. An **OTLP/gRPC exporter** pointing at `http://localhost:4317` (Jaeger's all-in-one collector port). Env override: `OTEL_EXPORTER_OTLP_ENDPOINT`.
3. A **sampler** controlled by `OTEL_TRACES_SAMPLER_ARG` (default `1.0` = trace everything; lower in prod to sample).
4. A shutdown func that flushes pending spans — invoked during graceful shutdown so the last 5s of spans aren't lost.

**Auto-instrumentation** (no per-request code needed):

| Library | What it traces |
|---|---|
| `otelhttp.NewHandler(router, "api")` | Every HTTP request becomes a parent span (api only) |
| `redisotel.InstrumentTracing(rdb)` | Every Redis call (GET, SET, EVALSHA) becomes a child span |
| `otelpgx.NewTracer()` on `pgxpool.ConnConfig` | Every pgx query becomes a child span (worker + watcher) |

**Manual spans** in the reserve flow (`internal/api/reserve_handler.go`):

| Span | Kind | Attributes | Parent |
|---|---|---|---|
| `reserve.handle` | internal | `event_id`, `user_id`, `seats_requested`, `reservation.id` | HTTP root |
| `redis.acquire-hold` | client | `redis.op=EVALSHA`, `seats` | `reserve.handle` |
| `sqs.publish` | producer | `messaging.system=aws_sqs`, `messaging.destination=reservations` | `reserve.handle` |

**The hard part: SQS context propagation.** SQS has no native trace header propagation. The publisher (`internal/queue/trace.go`) writes the W3C `traceparent` header into **two** places:

1. `MessageAttributes` (AWS-standard) — preserved by real AWS SQS.
2. The JSON body as `_traceparent` — **required** for ElasticMQ, which strips `MessageAttributes` in local dev.

The consumer tries `MessageAttributes` first, falls back to the body. Both paths are tested in `internal/queue/trace_test.go`. End result: the worker's `worker.handleMessage` span is a **child of** the API's `sqs.publish` span, even though they run in different processes with a queue between them.

**The waterfall** for a single `POST /api/tickets/reserve` (typical happy path):

```
POST /api/tickets/reserve           [35ms]   ← HTTP root
└─ reserve.handle (api)             [35ms]   ← manual parent
   ├─ redis.acquire-hold (api)      [3ms]    ← Lua EVAL
   │  └─ EVALSHA (api)              [1ms]    ← auto-instrumented
   └─ sqs.publish (api)             [22ms]   ← SQS SendMessage
      └─ (queue latency)            [~ms]    ← invisible to trace
         └─ worker.handleMessage    [1ms]    ← consumer span, parent jumps
            ├─ pool.acquire (wkr)   [0ms]    ← auto-instrumented
            └─ INSERT (wkr)         [1ms]    ← pgx auto-instrumented
```

**The Jaeger UI** (http://localhost:16686) lets you filter by service (`api`, `worker`, `expiration-watcher`) or by `trace_id`. The "min duration" filter is the go-to for "show me the slow ones."

### 13.5 Logs: `log/slog` + Loki + Promtail

**What it answers:** "What code path did this trace hit? What was the exact error message? Who got rate-limited?"

**The wire.** Every long-running service (api, worker, watcher, dashboard-server) calls `logging.Init(logging.Config{...})` **first thing in `main()`** — before any other code, so even `tracing.Init` failures are captured in the right format. The config takes level, format (text/json), service name, and version; env vars are `LOG_LEVEL`, `LOG_FORMAT`, `SERVICE_VERSION`.

The init wraps `slog`'s default `Handler` in a **`ContextHandler`** (`internal/logging/context_handler.go`, ~30 LOC) that pulls the active OTel span off the request `ctx` and auto-injects `trace_id` + `span_id` into every log line. The convention is: **if you have a `ctx`, use `slog.InfoContext(ctx, ...)`; if not, use `slog.Info(...)`** (rare). The ContextHandler is a no-op when there's no active span (e.g. one-shot CLIs).

**The chi access log** (`internal/apiutil/middleware/slog_logger.go`) replaces `chi/middleware.Logger`. It runs *after* the OTel HTTP middleware, so the access log line carries the request's `trace_id` automatically. One swap, every request gets correlated.

**The shipping pipeline:**

```
Go service ──slog (stdout)──> nohup catches it ──> logs/<service>.log
                                                       │
                                                       ▼  (host bind mount)
                                                  /var/log/ticket-deal  ◀── Promtail (in container)
                                                       │
                                                       ▼  (parse + label)
                                                  Loki  ──>  Grafana "Ticket Deal — Logs"
```

Promtail (`monitoring/promtail/promtail-config.yml`) tails each `logs/<service>.log` file and ships to Loki (`monitoring/loki/loki-config.yml`, port 3100). Loki stores the lines in a filesystem backend (single-replica, 7-day retention) and serves them via its HTTP API.

**The label cardinality rule (CRITICAL).** Loki indexes only a small set of *labels* per log line. Every unique (label-set → stream) combination counts against Loki's **active-stream cap (~5,000 in default config)**. If you make `trace_id` a label, you blow past that cap in 5,000 requests. **Rule:** only **low-cardinality** dimensions become labels (`service_name`, `detected_level`); high-cardinality fields (`trace_id`, `user_id`, `reservation_id`) stay in the log **body** and are queried via LogQL line-filter `|= "..."` (text contains), not label match.

**The Promtail 3.x gotchas** (all earned in production debugging — see `monitoring/promtail/promtail-config.yml` for inline comments):

1. `__path__` is consumed during file discovery, so `relabel_configs` can't see the expanded path. **Workaround:** one `scrape_configs` block per file, each with an explicit `service_name` label, not a glob + regex.
2. `labels: level:` (empty value) silently sets the label to `""`, which Loki drops. The `{{.lvl}}` template syntax for capture groups is finicky. **Workaround:** drop the custom pipeline entirely; Promtail's built-in `detected_level` (lowercase) does the same job.

**The Loki 3.x query gotcha.** LogQL rejects queries where *every* matcher is empty-compatible. So `{service_name=~".*"} | detected_level=~".*"` is rejected with:
```
queries require at least one regexp or equality matcher that does not have
an empty-compatible value. For instance, app=~".*" does not meet this
requirement, but app=~".+" will
```
The "All" default for every variable on the logs dashboard is `.+` (positive match, requires ≥1 char), not `.*`.

**The Grafana "Ticket Deal — Logs" dashboard** (`monitoring/grafana/dashboards/ticket-deal-logs.json`, uid `ticket-deal-logs`): 4 panels + 4 variables.

- **Variables:** `service` (multi-select from `label_values(service_name)`), `level` (All / INFO+ / WARN+ / ERROR — uses `detected_level` regex), `trace_id` (text box), `search` (text box).
- **Panel 1 (text):** "How to use this dashboard" hint.
- **Panel 2 (timeseries):** Log volume by service, last 1m rate.
- **Panel 3 (timeseries):** Log mix by detected level, last 1m rate.
- **Panel 4 (logs):** Live log stream with all four variable filters applied.

### 13.6 The killer combo: `trace_id` is the join key

The whole reason for picking W3C TraceContext + auto-injecting `trace_id` into every log line is this workflow:

```mermaid
sequenceDiagram
    actor Dev
    participant J as Jaeger UI
    participant G as Grafana Logs

    Dev->>J: Filter api, min duration 200ms
    J-->>Dev: Slow trace abc123...
    Dev->>Dev: Copy trace_id (32-hex)
    Dev->>G: Paste into trace_id variable
    G-->>Dev: Every log line for this trace,<br/>across api → SQS → worker,<br/>timestamped and ordered
    Dev->>Dev: Find the WARN/ERROR line
    Note over Dev,G: Reverse direction (log → trace) is wired too:<br/>Grafana's Loki datasource has a `derivedField`<br/>that makes trace_id values in log lines clickable,<br/>jumping straight back to Jaeger
```

In practice, the workflow is:

1. **Spot a slow trace in Jaeger** → "This POST took 380ms, the SQS publish alone took 250ms." Grab the `trace_id` from the URL or the span.
2. **Open Grafana → "Ticket Deal — Logs"** → paste the `trace_id` into the `trace_id` variable. See every log line for that request, across services, in time order. The 250ms publish usually has a `WARN` line right next to it explaining why.
3. **Click the `trace_id` value in a log line** → Grafana jumps back to Jaeger showing the full waterfall. (Wired via `derivedField` in `monitoring/grafana/provisioning/datasources/datasource.yml`.)

**The reverse** (log → trace) is the one that gets used most in practice: you're reading logs, see a `WARN: publish failed`, click the `trace_id`, Jaeger opens with the full request context. You can do this in two clicks from the Grafana logs panel.

### 13.7 Why this design (the decisions behind the choices)

| Decision | Why |
|---|---|
| **3-pillar Prometheus+Jaeger+Loki, not a SaaS like Datadog/New Relic** | Self-hostable, no per-GB fees, every layer is replaceable. The 4th pillar (Grafana) ties it together with a single UI. |
| **OpenTelemetry SDK, not a vendor-specific tracer** | Vendor-neutral. The OTLP exporter sends to Jaeger today, to Tempo or Honeycomb tomorrow, with zero code change. |
| **`log/slog` from stdlib, not zap/zerolog/logrus** | `slog` is in stdlib since Go 1.21; no dep churn, no `init()` magic, structured attrs by default. The custom `ContextHandler` is 30 lines and replaces every other logger's equivalent. |
| **W3C TraceContext, not B3 or Jaeger's own** | The standard. Every observability vendor supports it. Cross-service propagation "just works" because it's the same header everywhere. |
| **Filesystem Loki backend, not S3** | Local dev doesn't need S3. The storage layer is pluggable; same `loki-config.yml` works with `aws: { s3: … }` block in prod. |
| **Label cardinality cap, not "index everything"** | Loki's stream cap is a hard limit (~5k by default). Knowing which dimensions are low-cardinality is the difference between a queryable system and a 429-spamming outage. |
| **OTLP/gRPC, not OTLP/HTTP** | Lower overhead, multiplexing, bidi streaming. Jaeger all-in-one accepts both, so `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317` works out of the box. |
| **Text logs to stdout, not journald/syslog** | One format, one tool (`tail`, `grep`, `jq`), no surprise container-log rotation. Promtail tails the file directly. |

## 14. File-by-File Reference

> **Reading guide:** each entry lists purpose, key types/functions, and what depends on it. When you forget how something fits, scan the matching entry. Files are grouped by directory.

### Top level

#### `README.md`

Project landing page. Current Status table (Phases 0–9), Tech Stack table, Project Layout tree, Quick Start (docker-compose + migrations + env), and the "Next:" pointer. Updated in lockstep with each phase's docs commit.

#### `.env.example`

Template for `.env` (gitignored). Every env var consumed by the project lives here: API_PORT, JWT_SECRET, REDIS_ADDR, IP/WAIT_ROOM limits, RESERVE_HOLD_TTL, DATABASE_URL, SQS_* (ElasticMQ URL + queue name + URL), SWEEP_INTERVAL_SECONDS, AWS_* (region + dummy creds for ElasticMQ). Comments explain each value.

#### `.gitignore`

Standard Go ignores (`*.test`, `coverage.out`), editor cruft (`.vscode/`, `.idea/`, `.DS_Store`), vendoring (`/vendor/`), secrets (`.env`, `.env.*`), runtime logs, k8 `loadtest/results/` and `summary.html`. Doesn't currently ignore `scripts/*.go` — that's the untracked smoke-test artifact.

#### `docker-compose.yml`

**Eight services**, all with healthchecks wired into `depends_on: condition: service_healthy` (where applicable):

| Service | Image | Port(s) | Purpose |
|---|---|---|---|
| `redis` | `redis:7-alpine` | 6379 | Hot path: token buckets, hold keys, Lua |
| `postgres` | `postgres:16-alpine` | 5432 | Cold path: source of truth |
| `elasticmq` | `softwaremill/elasticmq:latest` | 9324 | Async: SQS-compatible queue |
| `prometheus` | `prom/prometheus:v2.55.0` | 9090 | Metrics scrape |
| `grafana` | `grafana/grafana:11.3.0` | 3000 | Dashboards (admin/admin123) |
| `jaeger` | `jaegertracing/all-in-one:1.76.0` | 16686, 4317-4318, 14268 | OTLP collector + UI |
| `loki` | `grafana/loki:3.3.0` | 3100 | Log storage (HTTP API, no UI) |
| `promtail` | `grafana/promtail:3.3.0` | 9080 | Log shipper (binds host `./logs`) |

Volumes: `redis-data`, `postgres-data`, `prometheus-data`, `grafana-data`, `jaeger-data`, `loki-data`. Configs are bind-mounted from `monitoring/{prometheus,grafana,jaeger,loki,promtail}/`.

Notes:
- `redis` is started with `--notify-keyspace-events Ex` (Phase 7) + `--appendonly yes` (durability) + `--maxmemory 512mb` + `--maxmemory-policy allkeys-lru`.
- `postgres` env from `POSTGRES_USER/PASSWORD/DB` (default `tickets/tickets/tickets`).
- `elasticmq` mounts `./docker/elasticmq.conf` so the `reservations` queue is auto-created with 30 s visibility + 20 s long-poll.
- `prometheus` scrapes the Go services via `host.docker.internal` (services run on the host, not in Docker).
- `jaeger` accepts OTLP/gRPC on `:4317` and OTLP/HTTP on `:4318`.
- `promtail` reads the host's `./logs` directory and ships to `loki` on `:3100`.

#### `docker/elasticmq.conf`

Pre-creates the `reservations` queue on boot — saves the dev the `aws --endpoint-url=... sqs create-queue` step. Standard type (no max-receive-count → no DLQ).

---

### `monitoring/` — observability configs

All observability side-channel configs. Bind-mounted into the corresponding Docker service by `docker-compose.yml`. Self-contained: nothing in this directory is imported by the Go code; they're pure config for Prometheus / Grafana / Jaeger / Loki / Promtail.

#### `monitoring/prometheus.yml`

Scrape config. Three targets (`api:8080`, `worker:8081`, `expiration-watcher:8083` — the Go services run on the host and Prometheus reaches them via `host.docker.internal`). 10s scrape interval. Used as Prometheus's `--config.file` flag.

#### `monitoring/grafana/provisioning/`

Auto-provisioned datasource + dashboard configs so Grafana is "ready" the moment it starts (no manual `Add datasource` or `Import JSON` step).

- **`datasources/datasource.yml`** — Prometheus + Loki datasources. The Loki datasource has a `derivedField` that extracts `trace_id` (32-hex) from log lines and turns it into a clickable link to Jaeger (`http://localhost:16686/trace/${...}`). This is what makes the log → trace navigation "just work."
- **`dashboards/dashboards.yml`** — provider config that points Grafana at `monitoring/grafana/dashboards/` and auto-loads every JSON file it finds there on startup.

#### `monitoring/grafana/dashboards/ticket-deal-overview.json`

**"Ticket Deal — Overview"** dashboard (uid `ticket-deal-overview`). 7 panels, dark "control room" aesthetic, Prometheus-backed. The headline panel is `reservations_oversold_total` (top-left, single-stat, must always read 0). Other panels: held/completed rate, sold-out rate, HTTP error rate, p50/p90/p99 latency by route, live inventory gauge per event, queue depth per event, status code split.

Designed in `docs/dashboard-redesign-plan.md` and shipped on `feature/observability`.

#### `monitoring/grafana/dashboards/ticket-deal-logs.json`

**"Ticket Deal — Logs"** dashboard (uid `ticket-deal-logs`). 4 panels, Loki-backed. Variables: `service` (multi-select from `label_values(service_name)`), `level` (All/INFO+/WARN+/ERROR, uses Promtail's `detected_level` regex with `.+` for "All" — see §13.5), `trace_id` (text box), `search` (text box). Panel 1 is a "how to use" hint. Panel 2 is log volume by service (1m rate). Panel 3 is log mix by level (1m rate). Panel 4 is the live log stream with all four variable filters applied. The "killer combo" workflow (§13.6) starts here.

#### `monitoring/loki/loki-config.yml`

Loki 3.3.0 single-replica config. Filesystem backend (no S3 in dev — swap to `aws: { s3: … }` for prod). `auth_enabled: false` (local dev). `ingestion_rate_mb: 32` and `per_stream_rate_limit: 16MB` (both bumped from defaults to absorb burst load). 7-day retention via `compactor`. Schema v13 with tsdb. Bound to `:3100`; no UI (use Grafana as the frontend).

#### `monitoring/promtail/promtail-config.yml`

Promtail 3.3.0 config. **One `scrape_configs` block per log file** (api, worker, expiration-watcher, dashboard-server) — see §13.5 for the Promtail 3.x `__path__` gotcha. Each block has an explicit `service_name` label (NOT a glob + regex; that doesn't work in Promtail 3.x because `__path__` is consumed during file discovery). `pipeline_stages: []` — no custom level extraction; Promtail's built-in `detected_level` (lowercase) does the job. Bind-mounts the host's `./logs` to `/var/log/ticket-deal:ro` in `docker-compose.yml`.

#### `monitoring/jaeger/`

The Jaeger all-in-one container reads no config file by default — the `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317` in the Go services is the entire contract. (If we ever need a config, this is where it would go.)

#### `go.mod` / `go.sum`

Module `github.com/emmitt-k/ticket-deal`, Go 1.27.1. Direct deps:

- `github.com/go-chi/chi/v5` — router (Phase 3)
- `github.com/golang-jwt/jwt/v5` — HS256 only (Phase 3)
- `github.com/google/uuid` — reservation_id generation (Phase 5)
- `github.com/jackc/pgx/v5` + `pgxpool` — Postgres (Phase 6)
- `github.com/aws/aws-sdk-go-v2` + `awsconfig` + `service/sqs` — SQS (Phase 6)
- `github.com/redis/go-redis/v9` — Redis client + Lua EVAL (Phase 2, 4, 7)
- `github.com/redis/go-redis/extra/redisotel/v9` — Redis OTel auto-instrumentation (Phase 11)
- `github.com/joho/godotenv` — `.env` overlay (Phase 3)
- `github.com/stretchr/testify` — test assertions everywhere
- `github.com/prometheus/client_golang` — Prometheus metrics (Phase 10)
- `go.opentelemetry.io/otel` + `sdk` + `exporters/otlp/otlptrace/otlptracegrpc` + `instrumentation/net/http/otelhttp` + `instrumentation/github.com/redis` + `instrumentation/github.com/jackc/pgx` — OTel SDK + auto-instrumentation (Phase 11)
- `go.opentelemetry.io/contrib/instrumentation/runtime` — Go runtime metrics (goroutines, GC, heap) (Phase 10)

---

### `cmd/` — six binaries

#### `cmd/api/main.go`

The whole HTTP server bootstrap. **Order of init matters** (later depends on earlier):

1. `logging.Init(...)` — first, so every subsequent slog call uses the right handler (including any that might fire during `tracing.Init` failure).
2. `tracing.Init(ctx, "api")` — sets up the global OTel TracerProvider, OTLP/gRPC exporter → Jaeger, returns a shutdown func.
3. `redisotel.InstrumentTracing(rdb)` — wraps the Redis client so every `EVAL` becomes a child span.
4. `metrics.Handler()` is mounted on `/metrics` (chi router).
5. The chi router is wrapped in `otelhttp.NewHandler(router, "api")` — every HTTP request becomes a parent span automatically.
6. `slogmw.NewSlogAccessLog()` replaces `chi/middleware.Logger` — every request gets an access log line with the OTel `trace_id` auto-injected by the `ContextHandler`.
7. Mounts `/healthz` + `/api/tickets/{enter,queue,reserve}` (the reserve route is behind `auth.Middleware()`).
8. `http.Server` with graceful shutdown on `SIGINT`/`SIGTERM` (5-second grace). The shutdown sequence: HTTP server (stop accepting) → drainer goroutine (cancel) → tracing shutdown (flush pending spans, ~5s budget).

`buildPublisher()` is the key wiring function: if `SQS_QUEUE_URL` is set, constructs `ReservationAPIPublisher(SQSPublisher(sqsClient))`; otherwise falls back to `LogPublisher` so the API stays runnable for handler work without a worker. Returns a `config.Config` value (not pointer) — handlers read it once at handler-construction time.

#### `cmd/worker/main.go`

The SQS consumer + Phase 7 sweep goroutine. Loads worker env vars (DATABASE_URL, SQS_QUEUE_URL, REDIS_ADDR, SWEEP_INTERVAL_SECONDS) — including a `_ = godotenv.Load()` at the top of `loadConfig()` so the binary runs from a `.env` in dev with no shell exports — opens a Postgres pool, opens a Redis client, runs `expire.RunSweep` as a goroutine, then blocks on `queue.Poll` for SQS messages. The Poll handler unmarshals `queue.Reservation` and calls `db.InsertIfAbsent` (which now writes the `seats` column).

#### `cmd/expiration-watcher/main.go`

Phase 7's event-driven path. Loads `DATABASE_URL` + `REDIS_ADDR` (also via `godotenv.Load()` for dev ergonomics — same shim as the worker), opens both clients, then blocks on `expire.RunWatcher(ctx, rdb, pool)`. Logs `clean shutdown` on `SIGINT`/`SIGTERM`.

#### `cmd/seed-inventory/main.go`

Dev-tool: copies `events.initial_inventory` from Postgres into the canonical Redis key (`redis.InventoryKey(id)` = `inventory:event:<id>`) so the `POST /reserve` Lua script can read it. Idempotent (hard `SET`, not `INCR`), safe to re-run between dev sessions. Pairs with the `002_seed.sql` migration: that creates the Postgres row, this mirrors it into Redis. Required because the API's reserve path expects Redis to already be populated; the seeder is the missing bootstrap step. Run via `go run ./cmd/seed-inventory` (or `./bin/seed-inventory` after `go build`). Refuses to run without `DATABASE_URL`.

#### `cmd/mintjwt/main.go`

Dev-tool: mints a short-lived HS256 JWT for smoke-testing `POST /api/tickets/reserve` (or anything else that goes through `auth.Middleware`). Reads `JWT_SECRET` from env (godotenv in dev) — **never embeds the secret in source** — refuses to run if the secret is missing or shorter than 32 bytes. Same default `USER_ID`/`EVENT_ID`/`TTL_SECONDS` env vars as the old `scripts/mintjwt.go`, plus a `TTL_SECONDS` override. Calls the same `auth.Issue` the API uses, so a test token can never drift from a real one.

#### `cmd/dashboard-server/main.go`

Phase 8 dev-tool: serves the live k6 load-test dashboard on `http://localhost:8082/` and reverse-proxies `/v1/*` to the k6 REST API on `http://localhost:6565`. Solves the CORS problem of opening `loadtest/dashboard.html` from `file://` against k6's REST API (k6 has no CORS layer, so we go through a same-origin proxy). Reads the HTML once at startup via `os.ReadFile` so the user can tweak the UI without rebuilding. Flags: `-addr` (default `:8082`), `-k6` (default `http://localhost:6565`), `-html` (default `loadtest/dashboard.html`); all overridable via env. ~85 lines, no external deps.

---

### `internal/api/` — HTTP handler logic

#### `internal/api/publisher.go`

Defines the `ReservationPublisher` interface (`Publish(ctx, []byte) error`) — the seam between the handler and the message queue. Also defines `LogPublisher`, the Phase 5 stub that logs messages with a `[STUB SQS publish]` prefix. The handler depends on this small interface; the queue package supplies the Phase 6 implementation.

#### `internal/api/reserve_handler.go`

`POST /api/tickets/reserve` handler. Pulls verified claims out, parses optional `{seats_requested}` body (defaults to 1, caps at 10), calls `redis.ReserveSeat`, maps Lua outcomes to HTTP codes (200, 400, 401, 404, 409 sold_out, 409 already_holding, 500), on success mints a `reservation_id` (UUID), marshals `queue.Reservation` as JSON, calls `publisher.Publish` (logs failure but does not roll back — Redis hold is already created; orphan key scenario), responds 200.

#### `internal/api/publisher_test.go` / `internal/api/reserve_handler_test.go`

Tests for the handler and publisher interface. Handler tests use `httptest` and mock publishers. Critical coverage: each Lua outcome maps to the right HTTP code; seats_requested validation; publisher failure doesn't roll back.

---

### `internal/apiutil/`

#### `internal/apiutil/response.go`

Two helpers used by every handler: `WriteJSON(w, status, v)` sets `application/json` content-type, encodes, writes; `WriteError(w, status, code, message)` is the shortcut for the uniform error envelope `{"error":string, "message":string}`. Adding fields like `request_id` to every response means one edit here.

#### `internal/apiutil/middleware/slog_logger.go`

**Phase 12 replacement for `chi/middleware.Logger`.** A chi-compatible middleware that writes one structured `slog.InfoContext(ctx, "http request", ...)` line per request, with `method`, `path`, `status`, `bytes`, `duration_ms`, `remote_addr`, and `user_agent` as attrs. The `ctx` is the OTel-instrumented one (the chi router is wrapped in `otelhttp.NewHandler` upstream), so the `ContextHandler` auto-injects `trace_id`/`span_id` into every line — meaning the access log and the trace are queryable by the same key in Grafana.

`X-Forwarded-For` is honored (first hop), falling back to `RemoteAddr`. ~95 LOC, no external deps.

#### `internal/apiutil/middleware/slog_logger_test.go`

8 unit tests using `httptest`. Verifies: line is emitted with the right level, correct status code captured (via `statusRecorder` wrapper around `ResponseWriter`), trace_id present when ctx has a span, no attrs leaked.

---

### `internal/auth/` — JWT and middleware

#### `internal/auth/jwt.go`

HS256 JWT. `Issue(userID, eventID, secret, ttl)` signs a token with claims `{EventID, sub, iat, nbf, exp}`. `Verify(token, secret)` parses with `jwt.WithValidMethods([]string{"HS256"})` (the single most important line — rejects `alg=none` and `alg=RS256` swap attacks). Returns `*Claims` or one of six stable error sentinels (`ErrTokenMalformed`, `ErrTokenSignature`, `ErrTokenExpired`, `ErrTokenNotYetValid`, `ErrTokenAlgUnexpected`, `ErrClaimsMissing`).

`classifyError` translates jwt v5's internal error taxonomy into our stable sentinels so callers don't have to import `jwt/v5`.

#### `internal/auth/middleware.go`

Chi middleware that reads `Authorization: Bearer <token>`, calls `Verify`, injects `*Claims` into `context.Context` via the package-private `claimsKey`. On any failure, short-circuits with a 401 + a client-safe error code (`token_expired`, `invalid_signature`, `invalid_token`). Deliberately opaque: attackers don't get to know *why* their forged token failed.

`ClaimsFromContext(ctx)` is how `ReserveHandler` (and only that handler) reads the verified identity. Other handlers don't depend on claims.

---

### `internal/config/`

#### `internal/config/config.go`

Single `Load()` function reads env via `godotenv`, validates every value, returns `*Config` with fail-fast behavior. Required: `API_PORT` (1-65535), `JWT_SECRET` (≥32 bytes — HS256 keyspace). Optional with defaults: `REDIS_ADDR`, `REDIS_DB`, `IP_LIMIT_*`, `WAIT_ROOM_*`, `RESERVE_HOLD_TTL_SECONDS`, SQS triple (`AWS_REGION`, `SQS_ENDPOINT_URL`, `SQS_QUEUE_URL`).

---

### `internal/db/` — Postgres pool + reservation writes

#### `internal/db/pool.go`

`Pool` wraps `*pgxpool.Pool`. `NewPool(ctx, Config{DSN, MaxConns, MinConns, HealthCheckPeriod})` parses the DSN, applies defaults (MaxConns=10, MinConns=1, HealthCheck=1min), opens the pool, **pings with a 5-second budget** (fail-fast at startup — better than discovering the bad DSN on first message). Caller owns the pool; must `defer pool.Close()`.

#### `internal/db/reservations.go`

Status constants (`StatusPendingPayment`, `StatusConfirmed`, `StatusExpired`, `StatusCancelled`) — typed string consts that mirror the SQL `CHECK` constraint, so a typo at a call site fails to compile.

`InsertIfAbsent(ctx, pool, queue.Reservation)` parses the wire payload's timestamps, runs the idempotent INSERT:

```sql
INSERT INTO reservations
    (reservation_id, user_id, event_id, status, expires_at, seats)
VALUES
    ($1, $2, $3, 'PENDING_PAYMENT', $4, $5)
ON CONFLICT (reservation_id) DO NOTHING;
```

Returns nil whether or not a row was created. `tag.RowsAffected() == 0` is informational only.

`GetReservation(ctx, pool, id)` reads back a row by ID (used by tests + smoke-test verify scripts). Returns `pgx.ErrNoRows` for missing rows; `IsNoRows(err)` wraps that for callers that don't want to import pgx.

---

### `internal/expire/` — Phase 7 compensation logic

#### `internal/expire/compensate.go`

**The shared compensation function.** `Compensate(ctx, pool, rdb, eventID, userID)`:

1. CTE: `WITH target AS (...PENDING_PAYMENT FOR user,event ORDER BY created_at DESC LIMIT 1) UPDATE ... SET status='EXPIRED' RETURNING seats`
2. If 0 seats returned → no matching row (orphan hold key) or already non-PENDING → no work, return nil
3. Else `INCRBY inventory:event:<id> <seats>`. INCRBY failure is logged as `WARN` but doesn't fail the call (DB row is the durable state; surfacing an error would just trigger watcher retries that find 0 rows via the CAS).

This is the function both watcher and sweep call — the two paths can never drift apart.

#### `internal/expire/watcher.go`

`RunWatcher(ctx, rdb, pool)` long-running loop that:

1. `PSUBSCRIBE __keyevent@0__:expired`
2. For each message, `ParseHoldKey(msg.Payload)` filters out non-hold keys (the channel fires for ALL expired keys)
3. Valid keys go to `Compensate(ctx, pool, rdb, eventID, userID)`

Wraps the subscribe in reconnect-with-backoff (1s → 30s cap, exponential, resets on clean subscribe). `context.Canceled` and `context.DeadlineExceeded` both bail cleanly.

`ParseHoldKey(key string) (int64, string, error)` extracts event_id and user_id from `hold:event:<N>:user:<U>`. Returns an error for non-hold keys, malformed event_id, or empty user_id. Pure function — no Redis dependency, easy to test.

#### `internal/expire/sweep.go`

`RunSweep(ctx, pool, rdb, dur)` ticker fires, one sweep runs:

1. `FindAndExpireSweptRows` — atomic `UPDATE ... WHERE status='PENDING_PAYMENT' AND expires_at < NOW() RETURNING reservation_id, user_id, event_id, seats`
2. For each returned row, `INCRBY inventory:event:<id> <seats>`

Runs once immediately on startup, then on every tick. Returns `ctx.Err()` on graceful shutdown.

The query is atomic per-row (Postgres row-locking serializes concurrent UPDATEs); idempotent (a re-tick after the first sweep returns 0 rows because status is already EXPIRED).

#### `internal/expire/*_test.go`

20 tests across the three files. Highlights:

- `TestCompensate_Concurrent` — 50 goroutines race Compensate on the same `(user, event)`. CAS resolves to one INCRBY (verify inventory == 1, not 50).
- `TestCompensate_AlreadyNonPending` — pre-flip a row to CONFIRMED, call Compensate, verify inventory unchanged (the WHERE filters it).
- `TestFindAndExpireSweptRows_Idempotent` — run sweep twice, second call returns empty.
- `TestRunSweep_Integration` — seed expired PENDING + inventory=5, run sweep at 50ms interval, poll until inventory hits 7 (5+2 seats).
- Watcher tests focus on `ParseHoldKey` (unit) and ctx-cancel handling — full PSUBSCRIBE flow is verified in the live smoke test (miniredis doesn't fully emulate keyspace events).

---

### `internal/iplimit/` — Phase 4 IP bucket

#### `internal/iplimit/bucket.go`

The Phase 4 IP-tier gate. `Check(ctx, rdb, iplimit.Config{capacity, refillRate}, ip, nowMS) (admitted bool, err error)` runs `ip_bucket.lua` and returns `(true, nil)` if the IP consumed a token, `(false, nil)` if the IP is exhausted (caller 429s hard — no queue).

Used only by `waitingroom.EnterHandler`. Not imported by Phase 5/6/7.

#### `internal/iplimit/scripts/ip_bucket.lua`

49 lines. Lazy-refill per-IP bucket, no queue. State per IP: Redis Hash `{tokens, last_refill_ms}`. 1-day TTL on the hash (safety net).

---

### `internal/logging/` — Phase 12 `log/slog` wrapper

The structured-logging package. Every long-running service calls `logging.Init(...)` first thing in `main()`; one-shot CLIs (`cmd/seed-inventory`, `cmd/mintjwt`) keep stdlib `log.Printf` because they run once and exit (deliberate, documented in `docs/logging-plan.md`).

#### `internal/logging/logging.go`

`Init(Config{Level, Format, Service, Version})` installs a slog handler as `slog.SetDefault()`. Helpers: `LevelFromString` (parses `LOG_LEVEL` env: debug/info/warn/error), `FormatFromEnv` (parses `LOG_FORMAT` env: text/json), `Setup()` (the no-config version for one-shot CLIs). The `service=` and `service_version=` attributes are prepended to every record by `Init` — callers drop the `"api: "` / `"worker: "` text prefix from log messages.

#### `internal/logging/context_handler.go`

**~30 LOC, the keystone of the trace → log join.** A `slog.Handler` wrapper that, for every `Handle(ctx, record)`, calls `trace.SpanContextFromContext(ctx)` and adds `trace_id` and `span_id` attributes to the record if a span is active. The wrap is one level: `ContextHandler{inner: slog.Default().Handler()}.Handle(...)` — so any record emitted with a `ctx` (via `slog.InfoContext(ctx, ...)`) gets correlated. Records emitted without a ctx (e.g. during early startup, before `tracing.Init` runs) pass through with no extra attrs.

#### `internal/logging/*_test.go`

Unit tests for the handler and init. Verifies: a record emitted *with* an active span gets `trace_id`/`span_id`; a record emitted *without* a span (or before tracing is up) doesn't crash and emits no extra attrs; the Init function correctly parses env vars and panics on bad input. All tests pass without any external service.

---

### `internal/metrics/` — Phase 10 Prometheus catalog

#### `internal/metrics/metrics.go`

**The single source of truth for metric names, label sets, and bucket boundaries.** Every service imports this package; nothing else defines Prometheus metrics directly. All metrics register with the default registry via `promauto` on package import.

Exposed groups (full list in the file):

- **HTTP** — `http_requests_total` (counter, labels: `service, path, method, status`), `http_request_duration_seconds` (histogram, labels: `service, path`, 11-bucket: 1ms → 5s).
- **Reservations** — `reservations_held_total`, `reservations_completed_total`, `reservations_oversold_total` (the alarm — must always be 0), `reservations_sold_out_total`, `reservations_already_holding_total`.
- **Redis state** — `redis_seats_available` (gauge, label: `event_id`), `waiting_room_queue_length` (gauge, label: `event_id`). Set by the api binary's `seatsAdapter`/`queueAdapter` (small interface adapters that satisfy `metrics.SeatsAvailableGetter` / `metrics.QueueSizeGetter` without a circular import on `internal/redis`).
- **Go runtime** — wired via `runtime.NewGoCollector()` and `runtime.NewProcessCollector()` in the `Handler()` wrapper. Goroutines, GC pause time, heap, fd count, all free.

`Handler()` returns a `http.Handler` for the chi `/metrics` route. Each service runs this on its own port (api :8080, worker :8081, watcher :8083 — `METRICS_ADDR` env for the latter two).

---

### `internal/queue/` — SQS producer + consumer

#### `internal/queue/reservation.go`

**The wire format.** `queue.Reservation` is the JSON payload both the API publishes and the worker consumes. Defining it here (not in either caller) means producer/consumer drift breaks the build. `Parse()` converts `CreatedAt`/`ExpiresAt` strings to `time.Time` (called by `db.InsertIfAbsent`).

#### `internal/queue/publisher.go`

`Publisher` interface (`SendReservation(ctx, Reservation) error`). `SQSPublisher` implements it via `aws-sdk-go-v2/service/sqs`. `ReservationAPIPublisher` adapts `SQSPublisher` to the smaller `api.ReservationPublisher` interface (takes `[]byte`, unmarshals+remarshals as wire-format validation). The double marshal is ~µs — cheap insurance.

#### `internal/queue/consumer.go`

`MessageHandler` interface (`HandleMessage(ctx, body) error`). `MessageHandlerFunc` adapts a plain `func`. `Poll(ctx, ConsumerConfig{...}, handler)` is the long-poll loop: defaults to MaxMessages=10, WaitTimeSeconds=20, VisibilityTimeout=30; loops ReceiveMessage → dispatch → DeleteMessage on success; logs + 1-second backoff on ReceiveMessage errors; ctx-cancel/DeadlineExceeded bail cleanly.

#### `internal/queue/aws.go`

Shared `NewSQSClient(ctx, AWSConfig{Region, EndpointURL, QueueURL})`. Detects `EndpointURL != ""` and injects dummy `local/local` creds if unset — see §12 sharp edge #1. Sets `o.BaseEndpoint = cfg.EndpointURL` on the SDK client.

#### `internal/queue/trace.go`

**Phase 11's hard part: SQS context propagation.** SQS has no native trace header propagation. This file provides:

- `InjectOTelIntoReservation(ctx, body []byte) (attrs map[string]types.MessageAttributeValue, bodyWithTraceparent []byte, err error)` — called by the publisher. Pulls the active W3C TraceContext off the request ctx, serializes it as a `traceparent` MessageAttribute (real AWS SQS preserves this), AND embeds the same value into the JSON body as a top-level `_traceparent` field (ElasticMQ strips MessageAttributes, so the body fallback is the only path that works in local dev). Both writes are best-effort: an empty `traceparent` from a no-op ctx produces a body without the field, and the consumer treats it as a "no upstream trace" message.
- `ExtractOTelFromMessage(ctx, attrs, body []byte) (newCtx context.Context, ok bool)` — called by the consumer. Tries the MessageAttribute first, falls back to parsing the body. If both fail, returns the original ctx unchanged (the consumer still runs, just as a root span).

The result: a Jaeger trace shows the worker's `worker.handleMessage` span as a **child of** the API's `sqs.publish` span, even across the queue. The body fallback is the keystone — see `internal/queue/trace_test.go` which exercises both paths (with-attribute and body-only) to guarantee local dev matches prod.

#### `internal/queue/trace_test.go`

Tests both propagation paths. (1) Publisher injects the `traceparent` MessageAttribute + body field when ctx has a span. (2) Consumer extracts from MessageAttribute when present. (3) Consumer falls back to body when MessageAttribute is empty (the ElasticMQ case). (4) Both empty → consumer starts a root span. ~150 LOC, no external deps (uses an in-memory context only).

---

### `internal/redis/` — Lua scripts + Go wrappers

#### `internal/redis/client.go`

`Client = redisclient.Client` (type alias). `NewClient(Config{Addr, DB})` returns a pool-100, timeouts-set go-redis client. `Ping(ctx, c)` is the fail-fast startup check used by `cmd/api`.

#### `internal/redis/reserve.go`

`ReserveSeat(ctx, rdb, eventID, userID, holdTTLSec, seatsRequested) (Result, error)` runs `reserve.lua`. `Result{Status: StatusReserved | StatusRejected | StatusEventNotFound, Reason: ReasonSoldOut | ReasonAlreadyHolding}`. `InventoryKey(eventID)` and `HoldKey(eventID, userID)` are the canonical key formats; both the API handler and the expire package import these.

#### `internal/redis/waitingroom.go`

Wraps `token_bucket.lua`:

- `TryAdmit(ctx, rdb, WaitRoomConfig, userID, nowMS) (AdmissionResult, error)` — atomic admit-or-queue. Returns `{Admitted: true}` or `{Admitted: false, Position: 1-based}`.
- `GetPosition(ctx, rdb, eventID, userID) (int, error)` — `ZRANK + 1`, returns 0 for non-members. Used by SSE stream refreshes.
- `Enqueue(ctx, rdb, eventID, userID, nowMS, ttlSec) (int, error)` — explicit `ZADD` with stale-entry cleanup. Used by `EnterHandler` when TryAdmit returns "queued" (most callers don't need this).
- `ZPopMin(ctx, rdb, eventID) (string, error)` — FIFO pop for the drainer. Returns `ErrQueueEmpty` on empty.
- `QueueSize(ctx, rdb, eventID) (int, error)` — `ZCARD`. Used by drainer to decide whether to keep the per-event loop alive.
- `Promote(ctx, rdb, eventID, userID) error` — explicit `ZREM`. Currently unused (drainer uses ZPopMin); exposed for admin tools.

#### `internal/redis/scripts/reserve.lua`

35 lines. The single most important Lua in the project. See §4.

#### `internal/redis/scripts/token_bucket.lua`

63 lines. Lazy-refill per-event bucket with ZSET overflow queue. See §3.

#### `internal/redis/reserve_test.go` / `waitingroom_test.go`

Critical coverage: `TestReserve_NoOversell` is the headline — 1000 concurrent goroutines fight for 10 seats, exactly 10 win, 990 get `sold_out`. Without Lua atomicity, this fails catastrophically.

---

### `internal/tracing/` — Phase 11 OpenTelemetry init

#### `internal/tracing/tracing.go`

`Init(ctx, serviceName) (shutdown func(context.Context) error, err error)` — sets up the global `TracerProvider` + W3C `TraceContext` propagator + OTLP/gRPC exporter. Idempotent (safe to call from multiple binaries in the same process for tests, though we don't do that). The shutdown func flushes pending spans (~5s budget) — main calls it on `SIGTERM` so the last 5s of spans aren't lost.

Env vars:

- `OTEL_EXPORTER_OTLP_ENDPOINT` — default `http://localhost:4317` (Jaeger all-in-one).
- `OTEL_TRACES_SAMPLER_ARG` — default `1.0` (trace everything; lower in prod for cost).

The package also sets up the **W3C TraceContext propagator as global**, so any code that does `otel.GetTextMapPropagator().Inject(ctx, carrier)` (e.g. the SQS publisher writing into MessageAttributes) uses W3C format by default. The consumer uses `Extract` with the same propagator.

This is the single setup point — `cmd/{api,worker,expiration-watcher}/main.go` each call it once.

---

### `internal/waitingroom/` — Phase 4 HTTP + drainer wiring

#### `internal/waitingroom/handler.go`

`EnterHandler(cfg, rdb)` is `POST /api/tickets/enter`. Two-tier gate: `iplimit.Check` → `redis.TryAdmit` → if admitted, `auth.Issue` JWT and 200; if queued, `redis.Enqueue` and 429 with `queue_url`. `QueueSSEHandler(rdb)` is `GET /api/tickets/queue`. Registers an admission channel, sets SSE headers, spawns `streamSSE` goroutine, blocks on context. `streamSSE` selects between three things — ctx cancel (client disconnect), admission chans (drainer promoted), 15-second heartbeat (refresh `ZRANK`). `sendSSEEvent` writes one chunk + flushes. `clientIP` extracts from `X-Forwarded-For` or `RemoteAddr`. `etaSeconds` is a UX approximation (position / refill_rate).

#### `internal/waitingroom/drainer.go`

`StartDrainer(ctx, DrainerConfig) func()` — top-level goroutine. Lazily spawns one `drainLoop` per event that has queued users (registered via `RegisterEventCallback` from `queue.go`). Each `drainLoop` exits after 5 minutes of empty queue. Per tick (default 100 ms): `ZCARD`, compute `tokensToAdd = refill_rate * tick_interval`, loop `ZPOPMIN` + `Issue` JWT + `Deliver` to the user's SSE channel.

The drainer is its own self-contained goroutine tree — no central registry of "active events" needed; sub-loops come and go as queues form and drain.

#### `internal/waitingroom/queue.go`

In-process pub/sub between drainer and SSE handlers. `Register(eventID, userID) chan string` creates the channel (one per `(event, user)`). `Unregister` closes it on SSE disconnect. `Deliver` does a non-blocking send (channel already closed or full = user has already moved on). `RegisterEventCallback` + `NotifyEvent` is the drainer's hook for "this event now has a queue" — `EnterHandler` calls `NotifyEvent` after a successful enqueue.

#### `internal/waitingroom/types.go`

The `SSEEvent` struct: `{type: "position" | "admitted", position, eta_seconds, token}`. The full shape of every SSE event the stream emits.

#### `internal/waitingroom/*_test.go`

Coverage for handler paths, drainer token calculations, and the in-process channel registry.

---

### `migrations/`

#### `migrations/001_init.sql`

Creates the two tables every phase needs:

- `events` — `id BIGINT PRIMARY KEY`, `name TEXT`, `initial_inventory INTEGER CHECK (>= 0)`, `created_at TIMESTAMPTZ DEFAULT NOW()`
- `reservations` — `reservation_id UUID PRIMARY KEY` (idempotency key), `user_id TEXT`, `event_id BIGINT FK → events(id)`, `status TEXT CHECK IN ('PENDING_PAYMENT','CONFIRMED','EXPIRED','CANCELLED')`, `expires_at TIMESTAMPTZ`, `created_at`/`updated_at TIMESTAMPTZ DEFAULT NOW()`
- Indexes: `idx_reservations_event_status (event_id, status)` for "how many PENDING does this event have?" queries; partial `idx_reservations_pending_expiry (expires_at) WHERE status = 'PENDING_PAYMENT'` powers the sweep job's WHERE clause.

#### `migrations/002_seed.sql`

Idempotent (`ON CONFLICT DO NOTHING`). Inserts event id=1 with name "Dev Test Event" and 100 inventory. The default target for k6 loadtest in Phase 8.

#### `migrations/003_add_seats.sql`

Phase 7's schema change. Adds `seats INTEGER NOT NULL DEFAULT 1 CHECK (seats >= 1 AND seats <= 10)` to `reservations`. The `DEFAULT 1` backfills existing rows; the CHECK mirrors the API handler's `1 ≤ seats_requested ≤ 10` validation. Without this, the watcher/sweep wouldn't know how many to INCRBY on expiry.

---

### `docs/`

#### `docs/architecture.md`

**This file.** The engineering manual — design rationale + per-file reference. Updated with each phase's docs commit.

#### `docs/implementation-plan.md`

The phased checklist. Each phase has its goal, files created, wiring steps, and verification commands. The single source of truth for "what's next." Companion to this file.

#### `docs/observability-plan.md`

Phase 10 plan. The argument for Prometheus + Grafana over SaaS alternatives (Datadog, New Relic), the metric catalog (what to measure, why, what buckets), the "Ticket Deal — Overview" dashboard layout, and the "control-room" design rationale. ~4.9KB.

#### `docs/tracing-plan.md`

Phase 11 plan. Why OpenTelemetry (not vendor-specific), the W3C TraceContext choice, manual span strategy in the reserve flow, SQS context propagation strategy, OTLP/gRPC vs HTTP trade-offs, and the sample waterfall walkthrough. ~8.7KB.

#### `docs/tracing-crashcourse.md`

A 3-page "learn tracing fast" — covers spans, contexts, propagation, sampling, and the bag of vocabulary. Written for someone who has never used OTel before. Companion to `tracing-plan.md` (which is the project-specific application). ~11KB.

#### `docs/logging-plan.md`

Phase 12 plan. Why `log/slog` (stdlib, no dep churn) over zap/zerolog/logrus, the `ContextHandler` design (the 30-LOC wrapper that joins traces and logs), the chi access log migration, the `LOG_FORMAT=text|json` convention, the convention for `*Context` variants, the one-shot-CLI exception, and the Phase 13 addendum (Loki). ~16KB.

#### `docs/dashboard-redesign-plan.md`

The design rationale for the "control room" aesthetic of `ticket-deal-overview.json`: why dark theme, why the 7-panel layout, why `reservations_oversold_total` is top-left and red, why per-route latency is line-graphed instead of stat'd. ~7.8KB.

#### `docs/comprehensive-go.md`

Background reading on the Go patterns used here (Lua EVAL wrappers, pool management, context plumbing). Not load-bearing — read when you want depth.

#### `docs/comprehensive-redis.md`

Background reading on Redis primitives used (Lua atomicity, keyspace notifications, ZSET semantics, stream consumer patterns). Not load-bearing.

---### `scripts/` — Phase 9 bg-service helpers + Phase 12 log cleanup

#### `scripts/start-bg.sh`

Launch a binary in the background, save PID to `logs/<name>.pid`, log stdout+stderr to `logs/<name>.log`. Idempotent: if the PID file is alive, prints "already running" and exits 0; if the PID is stale, cleans up and starts fresh. Sleeps 0.3s after launch and checks the PID — if the process died during startup (bad import, port already bound, etc.), tails the log and exits 1. macOS doesn't ship `setsid`; the `nohup ... & disown` pattern is what we use.

#### `scripts/stop-bg.sh`

Reads `logs/<name>.pid`, sends SIGINT, waits 5s for graceful shutdown, escalates to SIGKILL only if still alive. Removes the PID file on success. No-op (with a friendly message) if the service isn't running. The 5-second grace matches the API's `http.Server` graceful-shutdown timeout.

#### `scripts/clear-logs.sh`

Phase 12. **Default: delete** all `logs/*.log`, `logs/burst/*`, and `logs/ramp/*` files. With `--truncate` flag: keep the files but `: >` them (safer in-flight). Warns if any service PIDs are alive before clearing. **Never** touches `*.pid` files (managed by start-bg/stop-bg). The `burst/` and `ramp/` directories themselves are kept; only their contents are cleared, so `make loadtest-burst` doesn't fail on next run. Exposed as `make clean-logs` and `make clean-logs-truncate`.

> Historical note: there used to be a `scripts/mintjwt.go` scratch helper, but it was promoted to `cmd/mintjwt/main.go` so the JWT secret could be env-driven and the binary could be committed. The `.go` file in this directory is the leftover untracked artifact; safe to delete.

### `loadtest/` — Phase 8 k6 rig + Phase 9 self-bootstrapping wrappers

#### `loadtest/burst.js`

The headline test. 1000 VUs, each runs exactly 1 iteration (so `__VU` is a unique 1-1000 index, used to pick a pre-minted JWT so every VU has a distinct user_id and therefore a distinct hold key in Redis). All 1000 VUs race the `POST /api/tickets/reserve` endpoint at once; with `initial_inventory=100` the Lua reserve path guarantees exactly 100 × 200 and 900 × 409 (Lua is atomic — no two reservations can both decrement past zero). Custom counters `reserved_ok` / `sold_out` / `other_status` track the split; the real assertions live in `handleSummary()` (the ✅/❌ block at the end of the k6 output) because k6's `http_req_failed` threshold can't distinguish "expected 409" from "unexpected 5xx". The `summaryTrendStats` option forces k6 to track p(50)/p(90)/p(95)/p(99) in the live REST API so the dashboard can graph them.

#### `loadtest/ramp.js`

The wave-shaped companion to `burst.js`. Uses the `ramping-vus` executor with 7 stages over ~90 s: 0 → 50 (warm-up) → hold 50 → 50 → 200 (ramp to medium) → hold 200 → 200 → 1000 (ramp to peak) → hold 1000 → 1000 → 0 (cooldown). Same 1000-JWT pool, but here each iteration picks `exec.scenario.iterationInTest % JWTS.length` so the same JWT cycles through the VUs over time (otherwise a single VU looping would always get 409 on the second iteration with "already holding"). The interesting numbers during a ramp are *throughput* (does it scale linearly with VU count?) and *p99 latency* (does it degrade as concurrent load rises?); the 100-winner correctness check is unchanged from the burst test because inv=100 is decided in the first few hundred ms. Verified on an M5: 1,811,631 requests over 90 s, exactly 100 winners, 0 errors, p(99)=62 ms while sustaining 1000 VUs at 20,000 req/s.

#### `loadtest/dashboard.html`

Single-file dark-themed dashboard. No build step, no external deps — vanilla HTML + inline CSS + ~70 lines of fetch/JSON polling. Auto-polls `/v1/metrics` every 1s; renders active VUs, request rate + 60-point sparkline, latency p50/p90/p95/p99 table, 200/409/other counters, iteration progress bar, and a connection-status dot. Opened via the `cmd/dashboard-server` proxy so it's same-origin with k6's REST API (no CORS extensions required).

#### `loadtest/run-burst.sh` / `loadtest/run-ramp.sh`

Phase 9 **self-bootstrapping wrappers**. Each script:

1. Verifies the API is up (`curl -sf -m 2 http://localhost:8080/healthz`).
2. Calls `reset-state.sh` to truncate the test event's reservations and re-seed inventory.
3. Calls `mint-jwts.sh` to pre-mint the JWT pool (default 1000, override with `VUS=N` env).
4. Invokes `k6 run burst.js` (or `ramp.js`) via `scripts/start-bg.sh`, saving output to a **timestamped** log file in `logs/burst/<TS>-burst.log` or `logs/ramp/<TS>-ramp.log` (never overwritten, accumulated across runs).
5. Saves the k6 PID to `logs/k6-burst.pid` (or `k6-ramp.pid`) so `make stop-loadtest` can find it.

Net effect: `make loadtest-burst` is now a one-liner that goes from "dirty dev state" → "burst test running with valid JWTs and clean state" in a single command. Override behavior with env vars: `EVENT_ID=2 ./loadtest/run-burst.sh`, `VUS=500 ./loadtest/run-burst.sh`, etc.

#### `loadtest/reset-state.sh`

Idempotent state reset for repeat runs. Truncates `reservations WHERE event_id=$EVENT_ID`, deletes all `hold:event:$EVENT_ID:user:*` keys (via Lua `KEYS` + `DEL`), re-runs `go run ./cmd/seed-inventory` to repopulate `inventory:event:$EVENT_ID` from Postgres, then prints a one-line verification (reservations count, inventory value, hold-key count). Reads `EVENT_ID`/`EXPECTED_INV` from env. Avoids the macOS-bash-3 UTF-8 `…` quirk by using ASCII `...` everywhere.

#### `loadtest/mint-jwts.sh`

Pre-mints N unique JWTs to a file (default `/tmp/k6_jwts.txt`, one JWT per line). Loops `USER_ID=k6user-$i` through `bin/mintjwt` with `2>/dev/null` and a trailing `echo` to produce a line-oriented file. Build `bin/mintjwt` if missing. Default TTL is 600 s (matches the reserve-hold TTL — long enough for the 90 s ramp test plus repeated runs without re-minting; override with `TTL_SECONDS=...`). ~5 sec for 1000 JWTs on an M5.

---

### `Makefile` — Phase 9 polish + Phase 12 log-cleanup targets

#### `Makefile`

The single entry point for the whole project. `make help` lists 40+ targets (every line is grepped for `## comment` so descriptions stay in sync). Targets are grouped:

- **Setup**: `up`, `migrate`, `seed`, `env`, `doctor`
- **Build**: `build`, `vet`, `test`, `test-race`
- **Run foreground**: `api`, `worker`, `watcher`
- **Run background**: `api-bg`, `worker-bg`, `watcher-bg`, `dashboard-bg`, `all-services`, `stop`
- **Tools**: `mintjwt`, `dashboard`
- **Load test**: `loadtest-burst`, `loadtest-ramp`, `loadtest-state`, `loadtest-jwts`, `stop-loadtest`
- **Cleanup**: `clean`, `clean-logs` (delete log files), `clean-logs-truncate` (truncate log files), `purge`
- **Status**: `status`, `logs`

Common overrides via `make <target> VUS=500 EVENT_ID=2 USER=alice`.

The two load-test targets pre-flight a `curl -sf -m 2 http://localhost:8080/healthz` and fail fast with a clear "API not running on :8080" message if the API isn't up — prevents the silent-failure mode where k6 fires 1000 requests into a dead socket and the dashboard shows 1000 `connection refused` as "other_status". They run k6 in the background (via `scripts/start-bg.sh k6-burst k6 ... --linger`) so the Make target returns immediately and the dashboard stays populated with the final state.

**The `build` rule is the only one that creates production binaries.** It always uses `-o bin/<name>`, so it can never accidentally leave stray `./api`, `./worker`, etc. in the repo root. See AGENTS.md §Important Conventions #8.

## See also

- [README](../README.md) — project landing page (status table, tech stack, quick start)
- [AGENTS.md](../AGENTS.md) — canonical AI-assistant context (project layout, conventions, env vars)
- [`/docs/implementation-plan.md`](implementation-plan.md) — phased checklist (what's next)
- [`/docs/observability-plan.md`](observability-plan.md) — Phase 10: metrics design
- [`/docs/tracing-plan.md`](tracing-plan.md) — Phase 11: tracing design + SQS propagation
- [`/docs/tracing-crashcourse.md`](tracing-crashcourse.md) — learn OTel from scratch (companion to `tracing-plan.md`)
- [`/docs/logging-plan.md`](logging-plan.md) — Phase 12/13: slog + Loki
- [`/docs/dashboard-redesign-plan.md`](dashboard-redesign-plan.md) — "Ticket Deal — Overview" dashboard design rationale
- [`/docs/comprehensive-redis.md`](comprehensive-redis.md) — Redis deep-dive (Lua atomicity, keyspace notifications)
- [`/docs/comprehensive-go.md`](comprehensive-go.md) — Go patterns used in this codebase
