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
| HTTP API | `net/http` + chi router |
| Auth | JWT (RS256, keys on disk) |
| Cache / Rate-limit | Redis 7 (Lua scripts) |
| Primary DB | Postgres 16 |
| Queue | ElasticMQ (SQS-compatible, local) |
| Metrics | Prometheus 2.55 + Grafana 11.3 |
| Tracing | OpenTelemetry SDK 1.47 → Jaeger 1.76 (all-in-one) |
| Logging | `log/slog` (stdlib, Go 1.21+) + custom OTel trace correlation handler |
| Load testing | k6 (Go + JavaScript) |
| Service scripts | Bash + curl |
| Container runtime | Docker Compose |
| CI | GitHub Actions |
| Dependabot | Go modules + GitHub Actions (weekly, Mondays 09:00 BKK) |

---

## Architecture (high-level)

```
Client → API (:8080) → Redis (rate-limit + waiting-room queue)
                   → Postgres (reservations, inventory)
                   → ElasticMQ (async reservation publishing)
                            → Worker (:8081) consumes SQS → writes to Postgres
                            → Expiration-Watcher (keyspace notifications) → auto-expires holds

Observability:
  All services expose /metrics → Prometheus (:9090) → Grafana (:3000)
```

**Key correctness invariants:**
- 100 tickets per event (hardcoded seed in `scripts/start-bg.sh`)
- 200ms HTTP timeout on /reserve
- Redis atomic Lua scripts for seat locking (no oversell)
- Waiting room: VUs queued in Redis, released in FIFO order
- Postgres: idempotent writes via reservation UUID

---

## All Makefile Targets

Run `make` or `make help` to see all targets with descriptions.

### Development

```bash
make all-services     # Start API + worker + expiration-watcher + dashboard-server in background
make api             # Run API in foreground (ctrl-c to stop)
make api-bg          # Start API in background (logs/logs/api.log)
make worker-bg       # Start queue worker in background
make watcher-bg      # Start expiration-watcher in background
make dashboard-bg    # Start live dashboard (http://localhost:8082/)
make down            # Stop all docker compose services
make status          # Show running processes, container state, DB/Redis counts
make clean-logs      # Delete all log files (logs/*.log + logs/burst/* + logs/ramp/*)
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
# After `make all-services` + `docker compose up -d prometheus grafana`:
#   Prometheus: http://localhost:9090
#   Grafana:    http://localhost:3000  (admin / admin123, anonymous Viewer enabled)
#   Dashboard:  "Ticket Deal" folder → "Ticket Deal — Overview"
```

All Go services expose `/metrics` (Prometheus format) on their own ports:
- API: `:8080/metrics` (chi router)
- Worker: `:8081/metrics` (dedicated tiny HTTP server, set `METRICS_ADDR` to change)
- Expiration-watcher: `:8083/metrics` (set `METRICS_ADDR` to change)

Prometheus scrapes via `host.docker.internal` — the Go services run on the host, not in Docker.

**Most important metric:** `reservations_oversold_total` — should ALWAYS be 0. Alert if > 0.

#### Tracing (OpenTelemetry + Jaeger)

```bash
# After `docker compose up -d jaeger` + `make all-services`:
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
- One-shot CLIs (`cmd/seed-inventory`, `cmd/mintjwt`) keep stdlib `log.Printf` — they run once and exit, so structured logs add no value (documented in `docs/logging-plan.md` §Decision 4)

Env vars:
- `LOG_LEVEL` (default `info`) — debug/info/warn/error
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

### Code Quality

```bash
make build        # Build all 6 binaries into bin/
make vet          # go vet ./...
make test         # go test ./...
make clean        # Remove bin/
make purge        # clean + docker compose down (keeps volumes)
```

---

## Key Files & Directories

```
/
├── cmd/
│   ├── api/                    # HTTP API server (:8080)
│   ├── worker/                 # SQS consumer → Postgres writer
│   ├── expiration-watcher/     # Redis keyspace notifications → expire holds
│   ├── dashboard-server/       # Live k6 dashboard web server (:8082)
│   ├── mintjwt/                # JWT minting tool (used by load tests)
│   └── seed-inventory/         # Seeds Postgres with event + 100 seats
├── internal/
│   ├── api/                    # HTTP handlers, reserve_handler
│   ├── auth/                   # JWT middleware, JWT minting
│   ├── config/                 # Env var config
│   ├── db/                     # Postgres pool, reservation writes
│   ├── expire/                 # Expiration watcher, sweep, compensate
│   ├── iplimit/                # Redis IP rate-limiter (token bucket)
│   ├── queue/                  # SQS publisher + consumer
│   ├── redis/                  # Redis client, Lua scripts, seat locking
│   ├── waitingroom/            # Waiting room queue + SSE stream
│   └── apiutil/                # HTTP response helpers
├── loadtest/
│   ├── burst.js               # 1000-VU burst test script
│   ├── ramp.js                # 7-stage ramp test script
│   ├── dashboard.html         # Live dashboard (open in browser)
│   ├── reset-state.sh         # Reset Redis + Postgres to clean state
│   ├── mint-jwts.sh           # Mint N JWTs to /tmp/k6_jwts.txt
│   ├── run-burst.sh           # Run burst — auto-resets state + mints JWTs; timestamped log → logs/burst/
│   └── run-ramp.sh            # Run ramp — auto-resets state + mints JWTs; timestamped log → logs/ramp/
├── scripts/
│   ├── start-bg.sh            # Generic background process starter (pid + log)
│   ├── stop-bg.sh             # Generic background process stopper (pid-based)
│   ├── start-all.sh           # Start all services (used by make all-services)
│   ├── stop-all.sh            # Stop all services
│   └── clear-logs.sh          # Delete (or truncate) logs/*.log + logs/burst/* + logs/ramp/*; safe in-flight
├── docs/
│   ├── architecture.md        # Full architecture deep-dive
│   ├── comprehensive-go.md     # Go language tour
│   └── comprehensive-redis.md # Redis deep-dive
├── logs/                      # Runtime logs + k6 output
│   ├── api.log
│   ├── worker.log
│   ├── burst/                 # Timestamped burst logs
│   └── ramp/                  # Timestamped ramp logs
├── monitoring/                # Prometheus + Grafana configs (mounted into Docker)
│   ├── prometheus.yml         # scrape config (api, worker, watcher targets)
│   └── grafana/
│       ├── provisioning/      # auto-provisioned datasource + dashboard config
│       │   ├── datasources/datasource.yml
│       │   └── dashboards/dashboards.yml
│       └── dashboards/
│           └── ticket-deal-overview.json    # "Ticket Deal — Overview" dashboard
├── migrations/
│   ├── 001_init.sql           # Reservations + seats schema
│   ├── 002_seed.sql           # Seed data
│   └── 003_add_seats.sql      # Adds seats column
├── docker-compose.yml         # Postgres + Redis + ElasticMQ
├── go.mod / go.sum
├── Makefile                   # All make targets
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
- Runs on: push to `main` + all PRs
- Job: `go vet` → `go test -race -coverprofile` → `go build`
- Artifact: coverage report (14-day retention)

### Dependabot (`.github/dependabot.yml`)
- Go modules: weekly Monday 09:00 BKK
- GitHub Actions: weekly
- Labels: `dependencies`, `go`, `ci`

### Branch Strategy
- `main`: stable, protected
- Feature work: branch from main → PR → merge

---

## Important Conventions

1. **Load test before committing any reservation/Redis logic** — use `make loadtest-burst` to smoke-test correctness
2. **Reset state between load test runs** — `make loadtest-state && make loadtest-jwts`
3. **No external services in unit tests** — all tests use in-memory fakes; `go test ./...` passes without any docker services running
4. **JWT TTL** — default 600s (10 min); set via `JWT_TTL_SECONDS` env var in `loadtest/mint-jwts.sh`
5. **Ports**: API :8080, Dashboard :8082, Redis :6379, Postgres :5432, ElasticMQ :4566
6. **Corporate proxy** — `http.proxy` is set globally in `~/.gitconfig`; SSH bypasses it, HTTPS does not. SSH is used for github.com pushes.
7. **Commit style** — Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`, `ci:`)
8. **Always use `make build` (or `go build -o bin/<name> ./cmd/<name>`)** — never raw `go build ./cmd/<name>/...`. Without `-o`, Go writes the binary to the CWD named after the cmd dir (`./api`, `./worker`, etc.), polluting the repo root. The strays are .gitignored so they won't be committed, but they clutter `ls` and confuse tooling. `make build` always uses `-o $(BIN_DIR)/<name>`, so it can't make this mistake.

---

## Common Task Sequences

### Full local dev loop
```bash
make all-services   # Start everything
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
