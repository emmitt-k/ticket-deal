# AGENTS.md

> **This file is for AI assistants (Navi, future agents).** It is the canonical
> project context document. After any significant change, update this file first.
> Update README.md only when the change is user-facing (new feature, new command,
> changed behavior that a human visitor to the repo should know).

---

## Project Identity

| Field | Value |
|---|---|
| **Name** | Mini-Ticketmaster |
| **Type** | High-concurrency ticket booking / reservation system |
| **Go module** | `github.com/emmitt-k/ticket-deal` |
| **Repo** | https://github.com/emmitt-k/ticket-deal |
| **Owner** | Emmitt K. (`emmitt.karn@gmail.com`) |
| **Portfolio** | This IS the portfolio — all commits show `Emmitt K. <emmitt.karn@gmail.com>` |

---

## Tech Stack

| Layer | Technology |
|---|---|
| Language | Go 1.27.1 |
| HTTP API | `net/http` + chi v5 router |
| Auth | JWT HS256 (`golang-jwt/jwt/v5`, shared `JWT_SECRET`) |
| Cache / Rate-limit | Redis 7 via `redis/go-redis/v9` (Lua scripts) |
| Primary DB | Postgres 16 via `jackc/pgx/v5` (pgxpool) |
| Queue | ElasticMQ (SQS-compatible, local) |
| Metrics | Prometheus 2.55 + Grafana 11.3 |
| Tracing | OpenTelemetry SDK 1.47 → Jaeger 1.76 (all-in-one) |
| Logging | `log/slog` (stdlib, Go 1.21+) + custom OTel trace correlation handler |
| Log search | Grafana Loki 3.3 + Promtail 3.3 |
| Load testing | k6 (Go + JavaScript) |
| Service scripts | Bash + curl |
| Container runtime | Docker Compose (13 services: 8 infra + 5 Go) |
| CI | GitHub Actions |
| Dependabot | Go modules + GitHub Actions (weekly, Mondays 09:00 BKK) |

**Product surface status:** Layer 1 of [`docs/product-roadmap.md`](docs/product-roadmap.md)
is shipped 5/5 and merged. Layer 2 item 6 (the append-only audit log) is merged into
`main`. Layer 2 item 7 (the Stripe integration) is on `feature/stripe-integration`;
items 8-10 (notifications, admin API, anti-bot) are unstarted. Check the roadmap before
assuming an endpoint exists — it is the source of truth for what is built vs planned.

---

## Architecture (high-level)

```
Client → API (:8080) → Redis (rate-limit + waiting-room queue)
                   → Postgres (reservations, inventory)
                   → ElasticMQ (async reservation publishing)
                            → Worker (:8081) consumes SQS → writes to Postgres
                            → Expiration-Watcher (keyspace notifications) → auto-expires holds

Everything (infra + the 4 long-running Go services) runs as Docker
containers from `docker-compose.yml`. One command brings up the full
stack:

  docker compose --env-file .env.docker up -d   # see "Run (Docker mode)" below

Observability:
  All services expose /metrics → Prometheus (:9090) → Grafana (:3000)
  All services emit structured logs → ./logs/*.log → Promtail → Loki → Grafana
  All services trace via OTLP/gRPC → Jaeger (:16686)
```

**API surface** (full route table in "Key Files & Directories" below):

| Route | Auth | Purpose |
|---|---|---|
| `GET /healthz` | none | Process-alive. Always 200 once listening. Never checks dependencies |
| `GET /readyz` | none | Readiness. 3 parallel dependency checks (Redis / Postgres / SQS), 500ms each. 200 if all green, 503 otherwise |
| `GET /metrics` | none | Prometheus exposition (api) |
| `POST /api/tickets/enter` | none | Waiting-room admission (issues JWT on admit) |
| `GET /api/tickets/queue` | none | SSE stream of queue position |
| `POST /api/tickets/reserve` | JWT | **Seat reservation.** Honours `Idempotency-Key` |
| `GET /api/reservations/{id}` | JWT | Fetch one reservation. 403 if it belongs to another user |
| `GET /api/me/reservations` | JWT | List the caller's own reservations. `?status=`, `?limit=`, `?offset=` |
| `POST /api/reservations/{id}/cancel` | JWT | Cancel a `PENDING_PAYMENT` reservation; releases the seat |
| `POST /api/reservations/{id}/pay` | JWT | Create the Stripe PaymentIntent; returns the `client_secret` |
| `POST /webhooks/stripe` | Stripe signature | Stripe's callback. Verifies, snapshots, queues — no state change here |

**Key correctness invariants:**
- 100 tickets per event (hardcoded seed in `migrations/002_seed.sql`)
- 200ms HTTP timeout on /reserve
- Redis atomic Lua scripts for seat locking (no oversell)
- Waiting room: VUs queued in Redis, released in FIFO order
- Postgres: idempotent writes via reservation UUID
- **Every status change goes through `db.TransitionStatus`** — see Important Conventions #9
- **`/healthz` and `/readyz` are deliberately separate.** A failed Postgres should pull
  the container from load-balancer rotation *without* triggering a restart loop. `/readyz`
  answers that; `/healthz` must stay 200 so the process isn't killed. Do not collapse them.

---

## All Makefile Targets

Run `make` or `make help` to see all targets with descriptions.

### Development

**Docker mode (recommended):** all 4 long-running Go services run as containers
under `docker-compose.yml`. The full stack comes up with one command:

```bash
scripts/setup-docker-env.sh          # one-time per clone: create .env.docker
docker compose --env-file .env.docker up -d --build   # bring everything up
docker compose --env-file .env.docker down            # bring everything down
docker compose --env-file .env.docker logs -f api worker  # tail service logs
docker compose --env-file .env.docker restart api        # rebuild + restart one
```

**Host mode (legacy, still works):** Go services run on the host via `make`,
infra stays in Docker. Useful for fast `go run`-style iteration; uses the same
`.env` so JWTs minted by host-side `mintjwt` verify either way.

```bash
make all-services     # Start API + worker + expiration-watcher + dashboard-server in background
make api              # Run API in foreground (ctrl-c to stop)
make api-bg           # Start API in background (logs/logs/api.log)
make worker-bg        # Start queue worker in background
make watcher-bg       # Start expiration-watcher in background
make dashboard-bg     # Start live dashboard (http://localhost:8082/)
make down             # Stop all docker compose services
make status           # Show running processes, container state, DB/Redis counts
make clean-logs       # Delete all log files (logs/*.log + logs/burst/* + logs/ramp/*)
make clean-logs-truncate  # Truncate (don't delete) all log files; safe in-flight
```

### Load Testing

```bash
make loadtest-state        # Reset Redis + Postgres to clean state (only if you want to reset WITHOUT running a test)
make loadtest-jwts        # Mint 1000 JWTs (only if you want to mint WITHOUT running a test)
make loadtest-burst        # 1000 VUs × 1s burst — self-bootstraps (resets state + mints JWTs automatically)
make loadtest-ramp         # 7-stage ramp 0→50→200→1000 VUs over 90s — self-bootstraps
make stop-loadtest         # Kill any running k6 process (burst or ramp)
```

**Both `loadtest-burst` and `loadtest-ramp` auto-reset state and mint JWTs before launching k6.** No need to call `loadtest-state` / `loadtest-jwts` first. Override the JWT count with `VUS=500` (Makefile) or as a positional arg to the wrapper script.

**Load test logs** (never overwritten, timestamped):
- `logs/burst/TS-burst.log`
- `logs/ramp/TS-ramp.log`

### Observability

#### Metrics (Prometheus + Grafana)

```bash
# Brought up automatically by `docker compose --env-file .env.docker up -d`.
# To verify observability services are up:
docker compose --env-file .env.docker ps | grep -E 'prometheus|grafana'

#   Prometheus: http://localhost:9090
#   Grafana:    http://localhost:3000  (admin / admin123, anonymous Viewer enabled)
#   Dashboard:  "Ticket Deal" folder → "Ticket Deal — Overview"
```

All Go services expose `/metrics` (Prometheus format) on their own ports:
- API: `:8080/metrics` (chi router)
- Worker: `:8081/metrics` (dedicated tiny HTTP server, set `METRICS_ADDR` to change)
- Expiration-watcher: `:8083/metrics` (set `METRICS_ADDR` to change)

**Most important metric:** `reservations_oversold_total` — should ALWAYS be 0. Alert if > 0.

All these services run inside Docker; scrape targets are service names (e.g. `api:8080`, `worker:8081`), not `host.docker.internal`. The `host.docker.internal` entries in compose are kept for parity but unused now that nothing runs on the host.

#### Tracing (OpenTelemetry + Jaeger)

```bash
# Brought up automatically by `docker compose --env-file .env.docker up -d`.
# The Go services export OTLP/gRPC to http://jaeger:4317 (the compose
# service name, not localhost — the network is the same Docker user-defined
# network for every container in the compose file).

#   Jaeger UI:  http://localhost:16686
#   Search by service (api / worker / expiration-watcher) or trace_id
#   OTLP/gRPC:  localhost:4317 (services export to this)
#   OTLP/HTTP:  localhost:4318 (alternative)
```

Every Go service runs `tracing.Init(serviceName)` at startup. This sets up the
global OTel TracerProvider + W3C TraceContext propagator + OTLP/gRPC exporter
(default endpoint `http://localhost:4317`).

Auto-instrumentation:
- `otelhttp.NewHandler(router, "api")` — every HTTP route becomes a span
- `redisotel.InstrumentTracing(rdb)` — every Redis call (SET, GET, EVALSHA) becomes a span
- `otelpgx.NewTracer()` on `pgxpool.ConnConfig` — every pgx query becomes a span

Manual spans in the reservation flow:
- `api.reserve.handle` — parent of all reservation work (event_id, user_id, seats_requested, reservation.id attributes)
- `api.redis.acquire-hold` — wraps the Lua reserve script
- `api.sqs.publish` — wraps the SQS SendMessage (messaging.* semconv)
- `worker.worker.handleMessage` (kind=Consumer) — wraps the SQS handler
- `expiration-watcher.expire.compensate` — wraps the hold-key expiry compensation (root span, no upstream)

SQS context propagation:
- Publisher writes `traceparent` into `MessageAttributes` AND into the JSON body as `_traceparent`
- Consumer tries attributes first, falls back to body
- The body fallback exists because **ElasticMQ in local dev strips MessageAttributes**; real AWS SQS preserves them
- Both paths are tested in `internal/queue/trace_test.go`

Sample waterfall (one reservation):
```
POST (api)                         [35ms]
└─ reserve.handle (api)            [35ms]  parent=POST
   ├─ redis.acquire-hold (api)     [3ms]   parent=reserve.handle
   │  └─ evalsha (api)             [1ms]   parent=redis.acquire-hold
   └─ sqs.publish (api)            [22ms]  parent=reserve.handle
      └─ worker.handleMessage      [1ms]   parent=sqs.publish ← context jumps SQS
         └─ pool.acquire (worker)  [0ms]   parent=worker.handleMessage
         └─ INSERT (worker)        [1ms]   parent=worker.handleMessage
```

Env vars:
- `OTEL_EXPORTER_OTLP_ENDPOINT` (default `http://localhost:4317`)
- `OTEL_TRACES_SAMPLER_ARG` (default `1.0` = trace everything; lower in prod)

#### Logging (`log/slog` + OTel correlation)

Every long-running service initializes structured logging at the top of `main()`:

```go
logging.Init(logging.Config{
    Level:   logging.LevelFromEnv(),   // LOG_LEVEL: debug/info/warn/error
    Format:  logging.FormatFromEnv(),  // LOG_FORMAT: text (default) or json
    Service: "api",                    // hardcoded per binary
    Version: os.Getenv("SERVICE_VERSION"),
})
```

After Init, anywhere in the binary call:

- `slog.Info("msg", "key", val, ...)` — basic structured log
- `slog.InfoContext(ctx, "msg", ...)` — preferred when a `ctx` is in scope; auto-injects `trace_id` and `span_id` from the active OTel span via `logging.ContextHandler`
- `slog.WarnContext(ctx, ...)`, `slog.ErrorContext(ctx, ...)` — same with level

Key files:
- `internal/logging/logging.go` — `Init`, `LevelFromString`, env helpers
- `internal/logging/context_handler.go` — 30-LOC `slog.Handler` wrapper that pulls `trace.SpanContextFromContext(ctx)` and adds `trace_id` + `span_id` attrs
- `internal/apiutil/middleware/slog_logger.go` — chi access-log middleware (replaces `chi/middleware.Logger`); the access log line carries the request's `trace_id` because the OTel span is already active on the request ctx

Conventions:
- Functions with a `ctx` in scope use `*Context` variants so trace correlation works
- `service=` and `service_version=` are prepended to every record by Init — drop the `"api: "` / `"worker: "` prefix from message text
- `log.Fatalf` is replaced with `slog.Error + os.Exit(1)` so deferred shutdown funcs (tracing flush, DB pool close) still run
- One-shot CLIs (`cmd/seed-inventory`, `cmd/mintjwt`) keep stdlib `log.Printf` — they run once and exit, so structured logs add no value

Env vars:
- `LOG_LEVEL` (default `info`) — debug/info/warn/error
- `LOG_FILE` (default empty) — if set, the logger writes to this path
  instead of stdout. Used by the dockerised Go services so they can
  write directly into the bind-mounted `./logs/<svc>.log` (Promtail then
  ships those files to Loki). On host mode, leave unset — `start-bg.sh`
  redirects stdout to `logs/<svc>.log` instead.
- `LOG_FORMAT` (default `text`) — text (dev) / json (prod, Loki-ready)
- `SERVICE_VERSION` (default `dev`) — set by build via `-ldflags` in CI

Sample line (text format, with active span):
```
time=2026-10-09T15:00:00.000-07:00 level=INFO msg="reservation inserted" service=worker service_version=dev reservation_id=6880e7fa-... user=k6user-31 event=1 seats=1 trace_id=fac1f09d... span_id=71bebc50...
```

Same line in JSON format (`LOG_FORMAT=json`):
```json
{"time":"2026-10-09T15:00:00.000-07:00","level":"INFO","msg":"reservation inserted","service":"worker","service_version":"dev","reservation_id":"6880e7fa-...","user":"k6user-31","event":1,"seats":1,"trace_id":"fac1f09d...","span_id":"71bebc50..."}
```

#### Logs (Grafana Loki + Promtail)

```bash
# After `docker compose --env-file .env.docker up -d` (loki + promtail included):
#   Loki HTTP API:  http://localhost:3100
#   Promtail:       http://localhost:9080  (its own /metrics, rarely used directly)
#   Grafana:        http://localhost:3000  → "Ticket Deal" folder → "Ticket Deal — Logs"
```

**Architecture:**
```
Go services (in docker) ──./logs:/app/logs bind mount──> logs/*.log (host)
   ▲
   └──./logs:/var/log/ticket-deal:ro bind mount──> Promtail (in docker) ──> Loki (in docker) ──> Grafana
```

Even with the Go services in Docker, log shipping uses the same `./logs/`
bind mount pattern as before — `./logs` is bind-mounted RW into each Go
service (so each service writes to `logs/<service>.log`) and bind-mounted RO
into Promtail (so it tails the same files). Promtail parses `service`
(from filename) + `trace_id` (from line body, both text and json formats) as
Loki labels, and pushes them as streams. Loki stores them on disk
(`loki-data` volume).

**Config files:**
- `monitoring/loki/loki-config.yml` — filesystem backend, single replica, 7-day retention
- `monitoring/promtail/promtail-config.yml` — scrape job + regex pipeline stages

**Useful Loki queries (LogQL, used in Grafana → Explore):**
```logql
# Everything from the API
{service="api"}

# Errors only, across all services
{service=~".+"} | level="ERROR"

# All log lines for one specific request (paste trace_id from Jaeger)
{service=~"api|worker"} |~ "fac1f09d12ab34cd56ef78ab90cd1234"

# Sold-out events from the last 5m
{service="api"} |~ "sold_out" [5m]
```

**Trace → Logs workflow (the killer combo):**
1. Spot a slow trace in Jaeger → grab the `trace_id` (32-hex)
2. Open Grafana → "Ticket Deal — Logs" dashboard → paste it into the `trace_id` field
3. See every log line for that request across api/worker/watcher, all timestamped and ordered

The reverse (log → trace) is also wired up: the Loki datasource in
`monitoring/grafana/provisioning/datasources/datasource.yml` has a
`derivedField` that makes `trace_id` values in log lines clickable, jumping
to the matching Jaeger trace.

**No Go code changes were needed to add Loki** — it's a pure infra add. The
slog `ContextHandler` already injects `trace_id` into every log line, and
Promtail's regex stages extract it as a label.

### Code Quality

```bash
make build        # Build all 6 binaries into bin/
make vet          # go vet ./...
make test         # go test ./...
make clean        # Remove bin/
make purge        # clean + docker compose down (keeps volumes)
```

---

## Layer 1 — The Product Surface

Five items, each on its own branch. Before changing any of this, read
[`docs/product-roadmap.md`](docs/product-roadmap.md) — the specs there are authoritative
and this section is only a summary.

### Item 1 — `internal/db/transition.go` (the status-change gateway)

`TransitionStatus(ctx, pool, id, from, to) (changed bool, err error)`. Compare-and-set:
`UPDATE reservations SET status=$1, updated_at=NOW() WHERE reservation_id=$2 AND status=$3`.

Two callers racing the same row cannot both win — first gets `(true, nil)`, second gets
`(false, nil)`. `validTransitions` is a `map[string][]string` encoding the whole directed
graph as data; an edge that isn't in the table returns `(false, ErrInvalidTransition)`.
`EXPIRED` and `CANCELLED` are terminal — absent from the map *keys*, so "nothing leaves a
terminal state" is visible at a glance. See Convention #9.

### Item 2 — Read endpoints

- `GET /api/reservations/{id}` → `404` missing, `403 not_your_reservation` if
  `row.UserID != claims.Subject`, else `200`. **403 not 404 on purpose** — UUIDs are
  unguessable, so 403 leaks nothing, and the frontend can distinguish "wrong account"
  from "doesn't exist". The code comment says to switch to 404 if this ever opens to
  less-trusted clients.
- `GET /api/me/reservations?status=&limit=&offset=` → always scoped to the JWT subject;
  clients cannot request someone else's list. `status` omitted → all four statuses;
  unrecognised `status` → `400 invalid_status` (not a silently-empty result, which would
  mask typos). Empty result → `200 []`, never `404`. `limit` clamps to [1,100], default 20.
- Backs onto `internal/db/list_reservations.go` + the `(user_id, created_at DESC)`
  composite index from `migrations/004_user_lookup_index.sql` — the index ordering
  matches the query's filter *and* sort, so there's no sort node in the plan.

### Item 3 — `POST /api/reservations/{id}/cancel`

Takes both `*db.Pool` and `*redis.Client` — both mandatory. Five steps: JWT ownership
check → status pre-check → CAS transition → Redis INCRBY → best-effort hold-key DEL.

- Already `CANCELLED` → **idempotent 200**, and crucially does *not* INCRBY twice
- `CONFIRMED` → `409 already_paid` (refund, not cancel — that's Layer 2)
- `EXPIRED` → `409 already_expired`
- CAS returns `(false, nil)` → re-read to see who won: `EXPIRED`→409, `CANCELLED`→200
  (a concurrent duplicate), else generic `409 cancel_race`

Steps 4–5 log WARN on failure and **still return 200** — see Convention #11.

### Item 4 — `GET /readyz`

`ReadinessChecks` struct of three `func(ctx) error` fields, each run in its own goroutine
with a 500ms timeout, fanning in through a buffered channel. **Parallel by construction** —
a slow Postgres can't delay the Redis result.

```json
{"redis":"ok","postgres":"ok","sqs":"timeout after 500ms","ready":false}
```

Four per-check states from one `formatStatus` helper: `"ok"`, `"not_configured"`,
`"timeout after 500ms"` (via `errors.Is(err, context.DeadlineExceeded)`), `"error: <msg>"`.
A `nil` SQS check means "not configured" (dev-mode `LogPublisher`) and counts as healthy,
so a local process without a queue reports ready rather than a permanent 503. `safeCheck`
wraps each check in `recover()`.

`ReservationPublisher` was extended with `Ping(ctx) error` for this — deliberately the
*same* interface rather than a new one, so the readiness check can't drift from the
publisher the handler actually uses. `LogPublisher.Ping` → nil; `SQSPublisher.Ping` →
`GetQueueAttributes` on the bound queue URL.

### Item 5 — `Idempotency-Key` on `POST /reserve`

`internal/redis/idempotency.go`. Lenient key format (`[A-Za-z0-9-]`, max 255 chars). Two
keys per client key: `idempotency:<k>` (cached response, 24h TTL) and
`idempotency:<k>:inflight` (SETNX lock, 5s TTL).

Stored value is one JSON blob: `{body_hash, status, response}`. Co-storing `body_hash`
makes mismatch detection built in — `Lookup` returns `ErrIdempotencyBodyMismatch` and the
handler doesn't compare hashes itself. Co-storing `status` preserves the original status
on replay.

| Situation | Result |
|---|---|
| Cache hit, same body | Original status + body replayed verbatim, `Idempotency-Replayed: true`. No second seat, no second SQS publish |
| Cache hit, different body | `409 idempotency_key_mismatch` (Stripe-style — catches a client retrying with a changed body without rotating the key) |
| Lock held by another in-flight request | `409 in_flight` — the loser does **not** reserve |
| Cache entry corrupt | `ErrIdempotencyCacheCorrupt` → logged WARN, treated as a miss. Self-heals via TTL; never a user-facing 5xx |
| Non-2xx response | Lock released but **not** cached — a retry gets a fresh attempt rather than being pinned to a stale failure |

`Idempotency-Replayed` and `idempotency_body_mismatch_total` (rising trend = client bug)
are the operational signals.

---

## Layer 2 — The Product Surface (continued)

> **Layer 2, item 6** landed first because Layer 2 item 7 (Stripe) depends on it — a refund
> has to be distinguishable from a cancellation in the audit trail.

### Item 6 — The append-only audit log

`internal/db/audit.go` + `migrations/005_audit_log.sql`. The reservations row holds the
**current** status; the history of how it got there was gone the moment a second transition
overwrote it. `reservation_events` is the append-only trail.

| Column | Meaning |
|---|---|
| `event_id` | `BIGSERIAL` — insertion order, so it *is* the chronological order |
| `reservation_id` | FK → `reservations` |
| `user_id` | denormalised — Layer 3's GDPR sweep needs to find a user's rows by `user_id` alone |
| `event_type` | `CREATED` · `CONFIRMED` · `EXPIRED` · `CANCELLED` · `REFUND_INITIATED` |
| `actor_type` | `user` · `system` · `admin` · `stripe_webhook` |
| `actor_id` | the user id, the component name (`expiration-watcher`), the Stripe event id, … |
| `old_status` / `new_status` | NULL for `CREATED` (there was no previous status) |
| `trace_id` | the join key into Grafana / Loki / Jaeger |
| `metadata` | JSONB — seat counts, Stripe charge ids, … |

**The event type is derived from the EDGE, not the destination status.** `CONFIRMED →
REFUNDED` is a `REFUND_INITIATED` (money moved back); `PENDING_PAYMENT → CANCELLED` is a
`CANCELLED` (nothing was paid). `auditEventTypeForEdge` is the single source of truth for
that mapping, and its default case turns a forgotten edge into a loud error rather than a
silently mislabeled row.

**An EVENT and a STATUS are different concepts, and keeping them apart is load-bearing.**
The event records the *action* (`REFUND_INITIATED` — money went back); the status records the
*outcome* (`REFUNDED` — this reservation is now refunded). That is what lets a **partial**
refund be recorded as an event while the status stays `CONFIRMED` — only a full refund
becomes a state. A Charge can be refunded several times and partially, so "is this refunded?"
is not a boolean (see `docs/comprehensive-stripe.md` §33).

**`CANCELLED` and `REFUNDED` are separate statuses** (`migrations/006_refunded_status.sql`).
`CANCELLED` means "cancelled before payment"; `REFUNDED` means "paid, then reversed — the
money came back and the seat went back". They used to be one status ("refund = cancel in our
model"), which made "we lost the sale" and "we refunded the customer" the same query — in
the DB, in `?status=CANCELLED`, and in every report built on it. The state machine's edge
changed with it: `CONFIRMED → CANCELLED` is **not** an edge; `CONFIRMED → REFUNDED` is.

**Who writes what:**

| Path | event_type | actor_type / actor_id |
|---|---|---|
| Worker INSERT (`db.InsertIfAbsent`) | `CREATED` | `user` / the user id |
| Cancel handler | `CANCELLED` | `user` / the JWT subject |
| Expire watcher (`expire.Compensate`) | `EXPIRED` | `system` / `expiration-watcher` |
| Expire sweep (`expire.RunSweep`) | `EXPIRED` | `system` / `expiration-sweep` |
| *Stripe webhook (item 7, not yet written)* | `CONFIRMED`, `REFUND_INITIATED` | `stripe_webhook` / `evt_...` |

**Append-only is enforced by the database,** not by discipline: a `BEFORE UPDATE OR DELETE`
trigger refuses `DELETE` outright and refuses any `UPDATE` except rewriting `user_id` (the
GDPR anonymisation path, Layer 3 item 14). The one escape hatch is a session GUC,
`SET LOCAL ticket_deal.audit_purge = 'on'` — needed by test fixtures and future retention
policies. `SET LOCAL` scopes it to one transaction, so it can't leak across pooled
connections.

**`db.TransitionStatus` takes a mandatory `db.Audit`** and writes the audit row in the
*same transaction* as the status change:

```go
db.TransitionStatus(ctx, pool, id, db.StatusPendingPayment, db.StatusCancelled,
    db.Audit{Actor: db.UserActor(claims.Subject)})
```

The actor is required — an empty actor type is rejected with `db.ErrInvalidAudit` before
any SQL runs — so "a transition happened but nobody recorded it" is not a state the code can
reach. The signature change was deliberate: a sibling function without an actor would leave
a way to transition a row without recording it.

The expire paths are the deliberate exception (see Convention #9): they fold the audit
`INSERT` into the *same statement* as the status change, via a data-modifying CTE.
Postgres guarantees a data-modifying CTE runs exactly once and to completion even when the
outer query never reads its output — so the audit row and the status change commit or roll
back together, with no extra round-trip.

`db.AuditInsertSelect` is the shared piece that lets the expire paths build that CTE
insert without duplicating the column list, the trace extraction, or the metadata
marshalling.

### Item 7 — The Stripe integration

`internal/stripe/` + `internal/payment/` + `migrations/008_stripe_payments.sql`.

The design is **verify → queue → apply**, and the order is the whole point:

```
POST /webhooks/stripe
  → verify Stripe's signature over the RAW body   → 400 on failure
  → snapshot the event into our own struct
  → publish the snapshot to the `stripe-events` queue → 200
                                        │
                                        ▼
                             cmd/stripe-worker consumes it
                               → internal/payment applies it
```

**The webhook handler touches no database and changes no status.** The state machine
lives in `internal/payment`, behind a queue — the same shape as the reservation path
(`reserve → SQS → worker`). That keeps the webhook fast (Stripe has a short timeout and
retries anything that isn't a 2xx) and keeps every status change in one place.

**The failure mode this has to get right is "event accepted but the publish failed".**
The fix is ordering: **publish FIRST, then return 200**. If the publish fails we return
500 and Stripe retries the delivery. The window between "we said 200" and "the event is
durable" therefore does not exist.

| Event | What happens |
|---|---|
| `payment_intent.succeeded` | `PENDING_PAYMENT → CONFIRMED` (the only path to CONFIRMED) |
| `payment_intent.payment_failed` | Nothing changes. The seat is still held; the customer can retry. A failed payment is a **signal**, not a state — so it writes no audit row |
| `charge.refunded` (full) | `CONFIRMED → REFUNDED`, and the seat goes back on sale |
| `charge.refunded` (partial) | **An event, not a state.** The status stays `CONFIRMED`, the seat is NOT released, and the refund is recorded in the audit trail with `old_status == new_status` |

**`POST /api/reservations/{id}/pay`** creates the PaymentIntent and returns the
`client_secret`. It does NOT confirm the payment — the client confirms with the
client_secret (Stripe Elements), and the server learns the outcome from the webhook,
because a client that confirms and then never reports back is a case the server must not
depend on.

**The idempotency key is the `reservation_id`.** That is what makes the crash window
between "we wrote the payment row" and "Stripe answered" a *retry* rather than a
*double-charge*: a retried `Create` returns the SAME PaymentIntent. A random key would
make every retry a new charge.

**The amount is the server's number** — `unit_price_cents × seats` from the reservation's
price snapshot. The client never names an amount (Convention #16).

**The race.** The customer sat on the payment page for longer than the hold TTL; the
hold expired, the seat went back on sale, and then the payment succeeded. We do **not**
confirm (the seat may be sold) and we do **not** auto-refund (a refund on a race is its
own class of bug — it moves money on a path that did not reason about partial refunds or
about who owns the seat). We alert: `stripe_payment_after_expiry_total` (should always be
0), an ERROR log line, and the payment row saying `SUCCEEDED` while the reservation says
`EXPIRED` — which is exactly the mismatch to look for.

---

## Key Files & Directories

```
/
├── cmd/
│   ├── api/                    # HTTP API server (:8080)
│   ├── worker/                 # SQS consumer → Postgres writer
│   ├── stripe-worker/          # SQS consumer → the Stripe payment state machine
│   ├── expiration-watcher/     # Redis keyspace notifications → expire holds
│   ├── dashboard-server/       # Live k6 dashboard web server (:8082)
│   ├── mintjwt/                # JWT minting tool (used by load tests)
│   └── seed-inventory/         # Seeds Postgres with event + 100 seats
├── internal/
│   ├── api/                    # HTTP handlers: reserve, get, list, cancel, readyz, pay, stripe webhook, publisher seam
│   ├── auth/                   # JWT middleware, JWT minting
│   ├── config/                 # Env var config (incl. PostgresConfig/DSN composition)
│   ├── db/                     # pgx pool, reservation writes, TransitionStatus, list queries, audit log
│   ├── expire/                 # Expiration watcher, sweep, compensate
│   ├── iplimit/                # Redis IP rate-limiter (token bucket)
│   ├── logging/                # slog Init + ContextHandler (trace_id/span_id injection)
│   ├── metrics/                # Prometheus catalog (all counters/gauges/histograms)
│   ├── payment/                # The Stripe payment state machine (the consumer side)
│   ├── queue/                  # SQS publisher + consumer (aws-sdk-go-v2)
│   ├── e2e/                   # the end-to-end test (build tag `e2e`): the whole stack, one reservation
│   ├── redis/                  # Redis client, Lua scripts, seat locking, Idempotency-Key store
│   ├── stripe/                 # The Stripe client: create a PaymentIntent, verify a webhook, snapshot an event
│   ├── tracing/                # OpenTelemetry Init + shutdown flush
│   ├── waitingroom/            # Waiting room queue + SSE stream
│   └── apiutil/                # HTTP response helpers + slog access-log middleware
├── loadtest/
│   ├── burst.js               # 1000-VU burst test script
│   ├── ramp.js                # 7-stage ramp test script
│   ├── dashboard.html         # Live dashboard (open in browser)
│   ├── reset-state.sh         # Reset Redis + Postgres to clean state
│   ├── mint-jwts.sh           # Mint N JWTs to /tmp/k6_jwts.txt
│   ├── run-burst.sh           # Run burst — auto-resets state + mints JWTs; timestamped log → logs/burst/
│   └── run-ramp.sh            # Run ramp — auto-resets state + mints JWTs; timestamped log → logs/ramp/
├── scripts/
│   ├── start-bg.sh            # Generic background process starter (pid + log) — host mode only
│   ├── stop-bg.sh             # Generic background process stopper (pid-based) — host mode only
│   ├── setup-docker-env.sh    # One-time setup: create .env.docker from example + host .env JWT_SECRET
│   ├── clear-logs.sh          # Delete (or truncate) logs/*.log + logs/burst/* + logs/ramp/*; safe in-flight
│   └── mintjwt.go             # Untracked smoke-test scratch helper
├── docs/
│   ├── architecture.md         # Full architecture deep-dive (design + per-file reference)
│   ├── product-roadmap.md      # WHAT'S NEXT — numbered layers, per-item specs, gap list. Source of truth
│   ├── tracing-crashcourse.md  # Learn OpenTelemetry from scratch (background reading)
│   ├── comprehensive-go.md     # Go language tour
│   └── comprehensive-redis.md  # Redis deep-dive
├── logs/                      # Runtime logs + k6 output (bind-mounted into containers)
│   ├── api.log
│   ├── worker.log
│   ├── burst/                 # Timestamped burst logs
│   └── ramp/                  # Timestamped ramp logs
├── monitoring/                # All observability configs (bind-mounted into containers)
│   ├── prometheus.yml         # scrape config — service names (api:8080, worker:8081, watcher:8083)
│   ├── loki/
│   │   └── loki-config.yml    # filesystem backend, single replica, 7-day retention
│   ├── promtail/
│   │   └── promtail-config.yml  # scrape job + regex stages extracting service + trace_id labels
│   └── grafana/
│       ├── provisioning/      # auto-provisioned datasources + dashboards
│       │   ├── datasources/datasource.yml   # + derivedField: trace_id → click → Jaeger
│       │   └── dashboards/dashboards.yml
│       └── dashboards/
│           ├── ticket-deal-overview.json    # "Ticket Deal — Overview" (metrics)
│           └── ticket-deal-logs.json        # "Ticket Deal — Logs" (Loki)
├── migrations/
│   ├── 001_init.sql           # events + reservations schema, status CHECK, expiry partial index
│   ├── 002_seed.sql           # Seed data (event id=1, inventory 100)
│   ├── 003_add_seats.sql      # Adds seats column (Phase 7 INCRBY sizing)
│   ├── 004_user_lookup_index.sql  # Composite (user_id, created_at DESC) for GET /api/me/reservations
│   ├── 005_audit_log.sql         # Layer 2 item 6: append-only reservation_events + append-only trigger
│   ├── 006_refunded_status.sql   # Adds the REFUNDED status (a refund is not a cancel)
│   ├── 007_event_price.sql       # The price of a seat: events.price_cents + the reservation's snapshot
│   └── 008_stripe_payments.sql   # The payment_intents table (one reservation, one PaymentIntent)
├── docker/
│   └── elasticmq.conf         # Pre-creates the reservations queue on boot
├── docker-compose.yml         # 13 services: 8 infra (redis, postgres, elasticmq, prometheus,
│                              #   grafana, jaeger, loki, promtail) + 5 Go (api, worker,
│                              #   stripe-worker, expiration-watcher, dashboard-server)
├── Dockerfile                 # Multi-stage, distroless/static; SERVICE build arg → /app/server
├── .dockerignore              # Keeps build context tight (.git, logs, bin, monitoring, …)
├── .env.example               # Template for .env        (host mode — NEVER commit .env)
├── .env.docker.example        # Template for .env.docker (docker mode — NEVER commit .env.docker)
├── .github/                   # workflows/ci.yml + dependabot.yml
├── go.mod / go.sum
├── Makefile                   # All make targets
├── AGENTS.md                  # This file — canonical AI-assistant context
└── README.md                  # Human-facing project overview
```

---

## GitHub Setup (for this machine)

**SSH key:** `~/.ssh/id_ed25519_github` (comment: `emmitt.karn@gmail.com`)
- Added to GitHub: https://github.com/settings/keys
- Routed via `~/.ssh/config` with `IdentitiesOnly yes` (cannot accidentally use for other hosts)

**SSH config (`~/.ssh/config`):**
```
Host github.com
    IdentityFile ~/.ssh/id_ed25519_github
    IdentitiesOnly yes
```

**Git identity for this repo (local, not global):**
```
user.name  = Emmitt K.
user.email = emmitt.karn@gmail.com
```
Global identity (for other repos): `Emmitt Kaewkarn <emmitt.kaewkarn@krungsri.com>`

---

## CI/CD

### GitHub Actions (`.github/workflows/ci.yml`)

Runs on: push to `main` + all PRs. **Three** jobs, all on GitHub-hosted runners.

**`Test`** — the gate. It runs against a **real** Postgres, a **real** Redis and a
**real** SQS broker:

```
services:
  postgres:  postgres:16-alpine        → localhost:5432  (the DSN the tests already default to)
  redis:     redis:7-alpine            → localhost:6379
  elasticmq: softwaremill/elasticmq    → localhost:9324  (the SQS-compatible broker)
```

The queues (`reservations`, `stripe-events`) are created in a workflow step, because a
bare ElasticMQ starts with none. `internal/queue`'s publisher and consumer tests need a
real broker to be worth anything: the behaviours worth checking — a published message
arrives, a handled message is deleted, a failed handler's message comes back — are
**broker** behaviours, and no fake can reproduce them.

| Step | What it does |
|---|---|
| `go vet` | the static checks |
| **Apply migrations** | `psql -v ON_ERROR_STOP=1 -f migrations/0*.sql`, in order, on the fresh database — the same order `make migrate` uses. `ON_ERROR_STOP` matters: without it `psql` exits 0 on a failed statement and a broken migration looks like it worked |
| `go test -race -coverprofile` | the whole suite, with the race detector |
| **Enforce the coverage floor** | the total must be **≥ 45%**. A coverage number that is printed but never enforced is a number that quietly goes down |
| `go build` | all five binaries compile |
| Upload coverage artifact | 14-day retention |

**Why the service containers are not optional.** The state machine's correctness — the
compare-and-set, the audit row in the same transaction, the seat release on a refund —
is only provable against a real database. Without these two containers, **146 of the
236 tests skip themselves** (`t.Skipf("Postgres unavailable: …")`) and the job reports
**15.4%** coverage while looking green. With them: **236 tests, 48.5%**. The ports are
mapped to the runner's `localhost`, which is exactly where the tests look, so no
test-side change was needed.

**`Lint`** — `gofmt -l` (fails on any drift) → `go mod tidy` (fails if `go.mod`/`go.sum`
are not tidy) → `govulncheck ./...` (known CVEs in the stdlib and the dependencies).

**`E2E`** — the job that proves the SYSTEM works, not the packages. It boots the real
Docker Compose stack (api + worker + stripe-worker and their dependencies) and drives one
reservation all the way through:

```
reserve ──► SQS ──► worker ──► Postgres (PENDING_PAYMENT)
signed webhook ──► SQS ──► stripe-worker ──► Postgres (CONFIRMED)
```

then checks the audit trail recorded both transitions with the right actors. The test
lives in `internal/e2e/` behind the `e2e` build tag, so `go test ./...` does not run it
and the coverage number is unaffected. It skips (rather than fails) when the stack is not
there, so `go test -tags=e2e ./...` is safe to run anywhere.

The webhook is signed with the Stripe SDK's own `webhook.ComputeSignature`, so the API's
verification is exercised for real — not a hand-rolled approximation of the format.

> `govulncheck` earned its place the day it was added: it found **14** vulnerabilities —
> 12 in the Go standard library (fixed in go1.27.2/1.27.3) and one in
> `golang.org/x/net` (fixed in v0.60.0). The fix was a toolchain bump in `go.mod` plus
> `go get golang.org/x/net@v0.60.0`. It is now **clean**, and it stays clean because the
> step is a gate.

A `concurrency` group cancels the in-flight run for a branch when a new push lands, so a
rapid series of pushes does not queue up N runners.

### Dependabot (`.github/dependabot.yml`)
- Go modules: weekly Monday 09:00 BKK
- GitHub Actions: weekly
- Labels: `dependencies`, `go`, `ci`

### Branch Strategy
- `main`: stable, **protected** — the `Test` check is a required status check, so a PR
  cannot merge on a red build
- Feature work: branch from main → PR → merge
- **Major changes get their own branch.** New features and significant refactors: `git checkout -b feature/<short-name>` first, off `main` (or off the previous feature branch — see below). Small edits, bug fixes, doc tweaks and one-off tasks can be done on the current branch.
- **Always push to the remote when done.** After every commit, push to the remote feature branch immediately — `git push origin feature/<name>`. Never leave commits unpushed: an unpushed branch is work that exists on exactly one machine, and it is invisible to CI, to a reviewer, and to `git fetch`.
- **Stacked branches are normal here.** When one milestone builds on the previous, branch from the *previous feature branch*, not from `main`. Layer 1 shipped as a linear chain:
  `feature/dockerize-go-services` → `feature/get-reservations` → `feature/cancel-reservation` → `feature/readyz` → `feature/idempotency-key`
- **Reverse-order merge.** When the stack is finally merged, go tip-first so each PR's base exists before its child: `idempotency-key` → `readyz` → `cancel-reservation` → `get-reservations` → `dockerize-go-services` → `main`.
- Because the stack is linear, `git diff <base>..<tip>` shows the whole milestone at once — useful for review. But a single-branch diff on any *middle* commit shows only that one item, not the milestone.

### Current Merge State

**Layer 1 is fully merged into `main`** (PRs #11-#15, merged tip-first: dockerize →
get-reservations → cancel-reservation → readyz → idempotency-key), and so are the
four Dependabot PRs (#17-#20).

**Layer 2 item 6 (the audit log) is merged** (PRs #21 and #22) — the append-only audit
trail, the `REFUNDED` status split, the seat price, and three bugs the trail exposed
along the way:

- `fix(db)`: `ListReservationsByUser` returned a nil slice instead of an empty one.
  This left **`main` red** for anyone with a local Postgres — CI only stayed green
  because the `internal/db` tests `t.Skip` when Postgres is unreachable, and the CI
  job has no Postgres service.
- `test(api)`: the handler tests never mounted on a chi router, so 12 tests asserted against
  a `400 missing_id` instead of the real status. They had never run in CI (they `t.Skip`
  without Postgres/Redis).
- `fix(expire)`: a **double-INCRBY oversell race** in the watcher's expire path — see
  Convention #13

**Layer 2 item 7 (the Stripe integration) is on `feature/stripe-integration`** — the
payment table, the Stripe client, `POST /pay`, `POST /webhooks/stripe`, the
`stripe-events` queue, and the payment state machine behind `cmd/stripe-worker`.

Before starting new feature work, check whether that has landed. If it has, branch from
`main`; if not, branch from its tip and extend the stack.

---

## Important Conventions

1. **Load test before committing any reservation/Redis logic** — use `make loadtest-burst` to smoke-test correctness
2. **Reset state between load test runs** — `make loadtest-state && make loadtest-jwts`
3. **No external services in unit tests** — all tests use in-memory fakes; `go test ./...` passes without any docker services running. Tests that *want* a live dependency (`internal/db`, live-Redis tests) `t.Skip` when it's unreachable, so the suite is green either way
4. **JWT TTL** — default 600s (10 min); set via `JWT_TTL_SECONDS` env var in `loadtest/mint-jwts.sh`
5. **Ports** — *services*: API :8080, Worker :8081, Dashboard :8082, Expiration-watcher :8083, Stripe-worker :8084 · *infra*: Redis :6379, Postgres :5432, ElasticMQ **:9324** (its native SQS REST port — **not** 4566) + :9325 (web UI) · *observability*: Prometheus :9090, Grafana :3000, Jaeger :16686 (UI) + :4317 (OTLP gRPC) + :4318 (OTLP HTTP) + :14268 (thrift), Loki :3100, Promtail :9080
6. **Corporate proxy** — `http.proxy` is set globally in `~/.gitconfig`; SSH bypasses it, HTTPS does not. SSH is used for github.com pushes.
7. **Commit style** — Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`, `ci:`)
8. **Always use `make build` (or `go build -o bin/<name> ./cmd/<name>`)** — never raw `go build ./cmd/<name>/...`. Without `-o`, Go writes the binary to the CWD named after the cmd dir (`./api`, `./worker`, etc.), polluting the repo root. The strays are .gitignored so they won't be committed, but they clutter `ls` and confuse tooling. `make build` always uses `-o $(BIN_DIR)/<name>`, so it can't make this mistake.
9. **All status changes go through `db.TransitionStatus`** — never hand-roll `UPDATE reservations SET status = ...`. The helper encodes the legal edges as a `validTransitions` map and does a compare-and-set, so a caller cannot invent an impossible transition or race another writer into a lost update. Distinguish the two failure returns: `(false, nil)` = **lost the CAS race** (re-read the row to see who won; this is normal and expected), `(false, ErrInvalidTransition)` = **programmer error** (the edge doesn't exist). `internal/expire.Compensate` and `FindAndExpireSweptRows` are the deliberate exceptions — their CTEs already return `seats` in the same statement, so routing them through the helper would cost a second round-trip for no correctness gain.
10. **New Redis keys must use the existing key constructors** — `redis.InventoryKey`, `redis.HoldKey`, `redis.CacheKey`, `redis.LockKey`. No stringly-typed key building in handlers; tests import the same constructors so they can't drift from production.
11. **A partial Redis failure after a successful DB write is not a 5xx.** `POST /cancel` transitions in Postgres, then INCRBYs Redis; if the INCRBY fails it logs WARN and still returns 200. Returning 5xx would make the client retry into `409 already_cancelled` — strictly worse UX than a countable phantom seat an operator reconciles. Same policy for a failed idempotency cache `Store`: at-most-once beats double-booking.
12. **Check `docs/product-roadmap.md` before assuming a capability exists.** It is the source of truth for built-vs-planned. Layer 1 is shipped 5/5; Layer 2 item 6 (audit log) is shipped on `feature/audit-log`; Layer 2 items 7-10 (Stripe, notifications, admin API, anti-bot) are unstarted — `PENDING_PAYMENT → CONFIRMED` has no code path yet.
13. **The compare-and-set guard must live in the UPDATE's own WHERE clause, not only in a CTE.** This is the expire path's bug (fixed in `fix(expire): close a double-INCRBY oversell race`), and it is worth understanding because it is invisible in a single-threaded test. A CTE like `target` is evaluated against the statement's **snapshot** and is *not* re-read when a concurrent transaction commits. So `WHERE reservation_id IN (SELECT reservation_id FROM target)` — where `target` filtered on `status='PENDING_PAYMENT'` — lets a second concurrent transaction re-flip the row after the first commits: its post-lock re-check finds the id still in `target` (the id never changed), so it updates the row **again** and INCRBYs the seat a second time. That is a silent **oversell**. The fix is to repeat the guard in the UPDATE: `WHERE reservation_id IN (...) AND status = 'PENDING_PAYMENT'`. After the lock is released, the re-check runs against the *new* row version, sees `status='EXPIRED'`, and skips it. This is the same shape as `db.TransitionStatus` (`WHERE reservation_id = $2 AND status = $3`). **Any new CAS-shaped UPDATE must carry its guard in the UPDATE, not in a helper CTE.**
14. **Every status transition writes an audit row, in the same transaction.** `db.TransitionStatus` takes a mandatory `db.Audit` (actor + metadata) and writes `reservation_events` atomically with the status change. The actor is required — an empty actor type is rejected with `db.ErrInvalidAudit` before any SQL runs. The expire paths are the deliberate exception (their audit `INSERT` is a data-modifying CTE in the same statement — see the Layer 2 section above). **Never write a status change without its audit row, and never write an audit row without the status change.**
15. **`CANCELLED` and `REFUNDED` are different facts — never merge them.** `CANCELLED` = cancelled before payment; `REFUNDED` = paid, then reversed. The only edge out of `CONFIRMED` is `CONFIRMED → REFUNDED`. If you find yourself wanting to write `status = 'CANCELLED'` on a paid reservation, you want `REFUNDED`. The `?status=` filter, the DB CHECK constraint, and `db.StatusRefunded` all exist for this.
16. **The price of a seat is a server-side SNAPSHOT, and the client never names it.** `events.price_cents` (plus `events.currency`) is the single source of truth for what a seat costs; `reservations.unit_price_cents` + `reservations.currency` are a frozen copy of it, taken in the *same statement* that creates the reservation row (`db.InsertIfAbsent` reads them with scalar subqueries). The total owed is `unit_price_cents × seats` — computed in SQL from the two frozen columns, so it can never disagree with them. Why a snapshot and not a live lookup: an organizer changing the price must never retroactively alter a seat that is already held or already paid. No endpoint accepts a price field — a client that could send one could ask for a free ticket. The price is also recorded in the CREATED audit row's metadata, so the money is in the audit trail.
17. **The webhook handler verifies, snapshots and queues — it never changes a status.** Every status change happens in `internal/payment`, behind the `stripe-events` queue. The webhook endpoint must stay fast (Stripe has a short timeout) and it must publish BEFORE returning 200 — otherwise "the event was accepted but the publish failed" loses a payment confirmation permanently. A 500 makes Stripe retry; the state machine is idempotent, so a retry is safe.
18. **The idempotency key for a PaymentIntent is the `reservation_id`.** Never a random value and never a timestamp: a random key turns every retry into a new charge. The `payment_intents.reservation_id` UNIQUE constraint makes a second `/pay` a no-op at the database level, and the idempotency key makes it a no-op at Stripe's level. Together they close the crash window between "we wrote the row" and "Stripe answered".
19. **Stripe is OPTIONAL, but HALF-configured is a startup error.** Both secret keys absent → the payment routes answer 503 and the rest of the system works unchanged. Exactly one present → `config.Load()` fails fast, because an API that can create PaymentIntents but cannot verify webhooks is exactly the state where payments get silently stuck. A MALFORMED key is also a startup error; an ABSENT key is not — the reservation core does not need Stripe.
20. **A concurrency test must CONTROL the schedule, not hope for it.** A test that fires two goroutines and asserts a particular interleaving is asserting a timing window, not a contract — and it will pass on your machine and fail on a busy CI runner. The fix is to park one participant at a known point (a hook inside the code under test) until the test says go. `internal/api`'s `testPublisher.beforePublish` hook is the worked example: it parks the winner inside `Publish`, where the Idempotency-Key in-flight lock is provably still held, so the loser is *guaranteed* to meet the lock. And assert the **error code**, not just the status: with the lock broken, the loser falls through to the Lua script and gets `409 already_holding` — the same status, a different fact. A test that cannot fail is not a test.
21. **`reservation_events` is append-only, and the database enforces it.** A `BEFORE UPDATE OR DELETE` trigger refuses `DELETE` and refuses every `UPDATE` except rewriting `user_id` (the GDPR anonymisation path). The only way past it is `SET LOCAL ticket_deal.audit_purge = 'on'` — test fixtures and future retention policies use it; production code must not.

---

## Common Task Sequences

### Full local dev loop (Docker mode — recommended)
```bash
scripts/setup-docker-env.sh                                # one-time per clone
docker compose --env-file .env.docker up -d --build        # bring full stack up
make loadtest-state && make loadtest-jwts                  # Reset + seed JWTs
make loadtest-burst                                        # Smoke test
make loadtest-ramp                                         # Full stress test
# ...iterate code...
docker compose --env-file .env.docker restart api worker   # Rebuild + restart Go services
```

### Full local dev loop (host mode — legacy)
```bash
make all-services   # Start everything (Go on host, infra in Docker)
make loadtest-state && make loadtest-jwts   # Reset + seed JWTs
make loadtest-burst  # Smoke test
make loadtest-ramp   # Full stress test
# ...iterate code...
make stop-loadtest && make all-services  # Restart services
```

### After pulling new code
```bash
go mod download
make build
make test
```

### Quick correctness check (1 second)
```bash
make loadtest-state && make loadtest-jwts && make loadtest-burst
```

### Clean log files (start fresh for a new debugging session or load test)
```bash
# Wipe everything: service logs + k6 burst/ramp results
make clean-logs

# Targeted: just one service (e.g. keep worker.log, clear api.log)
scripts/clear-logs.sh api

# Targeted: just the k6 burst results (keep service logs)
scripts/clear-logs.sh burst

# Truncate instead of delete (keep files, reset content, safe in-flight)
make clean-logs-truncate
```

`scripts/clear-logs.sh` warns if any service PIDs are alive before clearing. **`.pid` files are never touched** — they're managed by `start-bg.sh` / `stop-bg.sh`. The `burst/` and `ramp/` directories themselves are kept; only their contents are cleared, so `make loadtest-burst` doesn't fail on next run.
