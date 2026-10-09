# Structured Logging Plan

> **Goal:** Replace stdlib `log.Printf` across all long-running services with
> structured `log/slog` (stdlib, Go 1.21+), and add **trace correlation** so
> every log line carries its `trace_id` and `span_id`. Bridges the missing
> third pillar so that metrics, traces, and logs all share the same identity.

> **Status:** ✅ Implemented on `feature/logging`. 5 atomic commits pushed;
> 19 tests pass without docker.

---

## Why now

We just shipped OpenTelemetry distributed tracing on `feature/tracing`. The
remaining gap:

| | Metrics | Traces | **Logs** |
|---|---|---|---|
| Format | PromQL, time series | W3C trace context, spans | unstructured `key=value` strings |
| Correlation | labels | parent / child span links | **none** — log lines have no `trace_id` |
| Storage | Prometheus | Jaeger (BadgerDB) | local `logs/<svc>.log` files |
| Query | PromQL | Jaeger UI by trace_id | `grep` on flat files |
| Pillar ✅? | yes | yes | **no** |

The current `log.Printf` output is fine for `tail -f` while developing, but:

1. **No correlation with traces** — when a reservation blows up, you can see
   the trace in Jaeger but you cannot find the related log lines without
   copying the `trace_id` and grepping every service's log file by hand.
2. **Unstructured text** — `"(addr=:8080, jwt_secret=64 bytes, redis=...)"`
   cannot be parsed by anything except a human.
3. **No aggregation** — local files are useless once you have more than one
   host.

`log/slog` (stdlib since Go 1.21) gives us structured key-value logging
without a new dependency, and a 30-line `slog.Handler` middleware gives us
free trace correlation. Grafana Loki (optional, follow-up) gives us log
search in the same UI as metrics and traces.

---

## Stack

| Component | Library | Version |
|---|---|---|
| Structured logger | `log/slog` (stdlib) | bundled with Go 1.27.1 |
| Trace context | `go.opentelemetry.io/otel/trace` (already present) | v1.47.0 |
| Trace↔log correlation | custom `slog.Handler` wrapper (~30 LOC) | n/a |
| Log aggregation (follow-up) | Grafana Loki + Promtail | latest stable |

No new dependencies are required for the core migration. Loki (if/when added)
is a Docker container, not a Go dep.

---

## Implementation phases

### Phase 1 — `internal/logging` package

**New file: `internal/logging/logging.go`**

- `Config` struct:
  - `Level slog.Level` — `Debug` / `Info` / `Warn` / `Error`
  - `Format string` — `"text"` or `"json"`
  - `Service string` — prepended to every record as `service=<name>`
  - `Version string` — prepended as `service_version=<semver>`
- `Init(cfg Config) *slog.Logger`:
  - Builds a `slog.Handler` from cfg.Format
  - Wraps it in our `ContextHandler` (see Phase 2)
  - Calls `slog.SetDefault(logger)` so all `slog.Info(...)` calls anywhere
    in the binary inherit it
  - Returns the logger for callers that want a per-instance handle
- `LevelFromString(s string) (slog.Level, error)` — parses env values like
  `"debug"`, `"info"`, `"warn"`, `"error"`
- `FormatFromString(s string) (slog.Handler, error)` — returns either
  `slog.NewJSONHandler` or `slog.NewTextHandler`

**New file: `internal/logging/context_handler.go`**

- `ContextHandler` — a `slog.Handler` wrapper that injects the current OTel
  `trace_id` and `span_id` into every record
- `Handle(ctx, record)`:
  1. Calls `trace.SpanContextFromContext(ctx)`
  2. If `ctx.IsValid()` and `sc.IsSampled()` (or always — see decision #1):
     appends `trace_id=<hex32>` and `span_id=<hex16>` attrs
  3. Delegates to the wrapped handler
- Implements the full `slog.Handler` interface: `Enabled`, `Handle`,
  `WithAttrs`, `WithGroup` (each delegates to the wrapped handler so
  composition works)
- ~30 LOC; mirrors the standard pattern used by OTel community examples

### Phase 2 — Migrate the 4 service binaries

| File | Pattern to replace |
|---|---|
| `cmd/api/main.go` | `log.Printf` / `log.Fatalf` |
| `cmd/worker/main.go` | `log.Printf` / `log.Fatalf` |
| `cmd/expiration-watcher/main.go` | `log.Printf` / `log.Fatalf` |
| `cmd/dashboard-server/main.go` | `log.Printf` / `log.Fatalf` |

For each:
- At the top of `main()`, call `logging.Init(logging.Config{...})` reading
  `LOG_LEVEL`, `LOG_FORMAT`, `SERVICE_NAME`, `SERVICE_VERSION` from env
- Replace `log.Printf("api: msg %s", x)` with `slog.Info("msg", "key", x)`
- Replace `log.Fatalf("api: %v", err)` with the
  `slog.Error("startup failed", "error", err); os.Exit(1)` pattern
- For functions that already have a `ctx` in scope, prefer
  `slog.InfoContext(ctx, ...)` so trace correlation kicks in

One-shot CLI binaries (`cmd/seed-inventory`, `cmd/mintjwt`) keep using
`log.Printf` — they run once and exit, so structured logs add no value. (This
is documented as a deliberate non-goal.)

### Phase 3 — Migrate internal packages

| File | Notes |
|---|---|
| `internal/api/reserve_handler.go` | switch to `InfoContext(ctx, ...)` for full correlation |
| `internal/api/publisher.go` | `LogPublisher` already prints JSON-ish; switch to `slog.Info` with proper attrs |
| `internal/queue/consumer.go` | use `slog.WarnContext(ctx, ...)` for malformed messages |
| `internal/db/reservations.go` | `slog.ErrorContext` on insert failures |
| `internal/expire/watcher.go` | use `InfoContext` |
| `internal/expire/compensate.go` | use `InfoContext` + `ErrorContext` on Postgres errors |
| `internal/expire/sweep.go` | start/stop log lines |
| `internal/waitingroom/drainer.go` | use `InfoContext` per admit |
| `internal/waitingroom/handler.go` | admission/rejection lines |
| `internal/apiutil/response.go` | error log lines |
| `internal/tracing/tracing.go` | init failure line (no ctx available — use `slog.Warn`) |

All replacements preserve the existing log data. The only format change is
`"api: msg key=val"` → structured JSON / key=value text. No semantic
information is lost.

### Phase 4 — Replace `chi/middleware.Logger` (HTTP access log)

`chi/middleware.Logger` writes a single line per request to its own
configured `Logger`. We replace it with a custom `middleware.RequestLogger`
that uses `slog.InfoContext` so each access log carries the request's
`trace_id` / `span_id` (since the upstream `otelhttp` middleware has already
started a span by then).

```go
// New file: internal/apiutil/middleware/slog_logger.go
func NewSlogRequestLogger() func(next http.Handler) http.Handler {
    return middleware.RequestLogger(&slogFormatter{logger: slog.Default()})
}
```

The formatter records: `http.method`, `http.path`, `http.status`,
`http.duration_ms`, `client.ip`, `user_agent`, `bytes.written`. Because
`otelhttp` runs the request inside an active span, our `ContextHandler`
auto-injects the trace context into every access log line.

### Phase 5 — Tests

`internal/logging/context_handler_test.go`:

- `TestContextHandler_NoSpan` — ctx without a span produces no `trace_id` attr
- `TestContextHandler_WithValidSpan` — in-memory `tracetest.SpanRecorder`
  started span → handler adds correct `trace_id` and `span_id`
- `TestContextHandler_InvalidSpan` — non-recording span → no attrs
- `TestContextHandler_WithAttrs` — `WithAttrs` returns a working handler
- `TestContextHandler_WithGroup` — group delegation works
- `TestInit_TextFormat` — text handler output is human-readable
- `TestInit_JSONFormat` — JSON handler output is valid JSON
- `TestLevelFromString` — parses `"debug"`, `"info"`, `"warn"`, `"error"`,
  uppercase, mixed-case
- `TestLevelFromString_Invalid` — unknown string returns error

All tests pass without docker. Style mirrors
`internal/queue/trace_test.go`.

### Phase 6 — Documentation

- `AGENTS.md`: add a "## Logging" section alongside the existing "## Metrics"
  and "## Tracing" sections. Cover: where to import from, env vars, the
  `*Context` family, the `chi/middleware.RequestLogger` swap.
- `README.md`: add a "### Logging" subsection under `## Observability` (the
  existing two subsections are Metrics and Tracing). Show a sample JSON log
  line with `trace_id` / `span_id` highlighted.
- Roadmap item "Structured logging" goes from `[ ]` to `[x]`.

---

## End-to-end verification (after implementation)

1. `make all-services` with `LOG_FORMAT=json LOG_LEVEL=info`
2. `make loadtest-burst` (50-100 VUs)
3. `tail -f logs/api.log` — see one JSON object per line:
   ```json
   {"time":"2026-10-09T15:00:00Z","level":"INFO","msg":"reservation inserted",
    "service":"worker","service_version":"dev",
    "reservation_id":"6880e7fa-1170-440f-883c-c5bca36002b0",
    "user":"k6user-31","event":1,"seats":1,
    "trace_id":"fac1f09d3a4b5c6d7e8f9a0b1c2d3e4f",
    "span_id":"71bebc50a1b2c3d4"}
   ```
4. Copy `trace_id` → Jaeger search → confirm same trace. (The whole point.)
5. Set `LOG_FORMAT=text` → restart → see human-readable colored output.
6. `grep '"level":"ERROR"' logs/*.log | jq` → instant structured error query.

---

## Environment variables

| Var | Default | Purpose |
|---|---|---|
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `LOG_FORMAT` | `text` | `text` (dev) / `json` (prod, Loki) |
| `SERVICE_NAME` | binary name | prepended as `service=` attr |
| `SERVICE_VERSION` | `dev` | prepended as `service_version=` attr |

(`OTEL_*` env vars already documented in `docs/tracing-plan.md` continue
to work and are read by `internal/tracing`.)

---

## Key design decisions

1. **Why `log/slog` and not zerolog / zap / logrus?**
   - `slog` is in the stdlib (Go 1.21+); zero new dependencies.
   - `logrus` is in maintenance mode and recommends `slog` as the successor.
   - `zap` and `zerolog` are faster on the hot path, but our log volume is
     well below the threshold where that matters (~1k lines/sec). If we ever
     push to 100k+ lines/sec, swap to `slog` + a custom fast handler.
   - The stdlib design (`Handler` interface, `*Context` methods) makes our
     `ContextHandler` straightforward.

2. **Why a custom `slog.Handler` and not `otelzslog` / `otelslog` bridges?**
   - The OTel bridges (`go.opentelemetry.io/contrib/bridges/*slog`) convert
     *into* an OTel logger or *from* zap; none of them are "wrap slog and
     inject trace context." Our handler is 30 lines and is the
     community-recommended pattern.
   - Keeps the core slog API standard; no `slog.Logger` → `otel.Logger`
     conversion needed at every callsite.

3. **Why `trace_id` injection even for unsampled spans?**
   - OTel `IsSampled()` defaults to `true` (we sample 100% in dev). In
     production, when a parent decides to drop the trace, we still want the
     log line searchable by the parent's trace_id so an operator can find
     it post-hoc.
   - If the span is the zero value (`SpanContext{}`), we skip injection
     entirely (no noise in non-OTel contexts like `cmd/seed-inventory`).

4. **Why migrate one-shot CLIs separately, not at all?**
   - `cmd/seed-inventory` and `cmd/mintjwt` print to stdout once and exit.
     Their output is meant for a human reading the terminal during a dev
     workflow, not for log aggregation. Migrating them adds noise to PR
     diffs without buying anything.
   - The 4 long-running services (`api`, `worker`, `expiration-watcher`,
     `dashboard-server`) are the targets. They run in production-shaped
     setups and benefit from correlation.

5. **Why swap `chi/middleware.Logger` in Phase 4 instead of leaving it?**
   - `chi/middleware.Logger` uses its own log format and doesn't carry the
     request's trace_id. So the very lines that would be most useful for
     debugging ("POST /api/tickets/reserve 200 40ms") are exactly the ones
     that don't have trace correlation. Replacing it with a slog-based
     formatter closes that gap.

6. **Why not jump straight to Grafana Loki?**
   - Keep this PR small. Slog + trace correlation alone is the biggest
     win. Loki is "nice to have" for cross-host search, but local files
     + `grep` + `jq` cover the dev workflow just fine. Add Loki as a
     follow-up PR when there's an actual multi-host need.

7. **Why drop the `"api:"` / `"worker:"` prefix from log lines?**
   - It's now the `service=api` attribute, redundant. The text handler
     still shows it (e.g. `service=api`), so the only thing that changes
     is fewer characters. If we ever want a human-friendly prefix back,
     the text handler can be configured to render `service` as
     `[api]`.

---

## Files changed

```
NEW     internal/logging/logging.go              + Init, Config, parsers
NEW     internal/logging/context_handler.go      + 30-LOC trace-id injector
NEW     internal/logging/context_handler_test.go + tests
NEW     internal/apiutil/middleware/slog_logger.go + chi access log → slog
MOD     cmd/api/main.go                          + logging.Init + slog migration
MOD     cmd/worker/main.go                       + logging.Init + slog migration
MOD     cmd/expiration-watcher/main.go           + logging.Init + slog migration
MOD     cmd/dashboard-server/main.go             + logging.Init + slog migration
MOD     internal/api/reserve_handler.go          + slog.InfoContext
MOD     internal/api/publisher.go                + LogPublisher → slog
MOD     internal/queue/consumer.go               + slog.WarnContext
MOD     internal/db/reservations.go              + slog.ErrorContext
MOD     internal/expire/watcher.go               + slog.InfoContext
MOD     internal/expire/compensate.go            + slog.ErrorContext
MOD     internal/expire/sweep.go                 + slog.Info
MOD     internal/waitingroom/drainer.go          + slog.InfoContext
MOD     internal/waitingroom/handler.go          + slog.InfoContext
MOD     internal/apiutil/response.go             + slog.ErrorContext
MOD     internal/tracing/tracing.go              + slog.Warn (no ctx)
MOD     docs/logging-plan.md                     + this file
MOD     AGENTS.md                                + Logging section
MOD     README.md                                + Observability → Logging subsection
```

**Estimated diff size:** ~400 lines changed across 19 files. Half the
changes are mechanical (`log.Printf` → `slog.Info` with the same args).

---

## Future work (not in this PR)

- **Grafana Loki + Promtail** — JSON logs make this trivial to add as a
  follow-up. Single `docker-compose.yml` block, Promtail tails
  `logs/*.log` and ships to Loki. Grafana gets a third data source in the
  same UI as Prometheus and Jaeger.
- **Log-based alerts in Grafana** — `count_over_time({service="api"}
  |~ "level=ERROR" [5m])` → alert on error-rate spikes.
- **OTel-native logs** — OTel has its own logs signal. Switching to it
  would mean exporting logs via the OTel Collector and dropping `slog`.
  Higher coordination cost; only worth it for very large deployments.
- **Log sampling** — at 100k+ lines/sec, downsample noisy debug logs
  before they hit disk. `slog` makes this a custom `Handler` away.
- **PII redaction layer** — a `slog.Handler` that redacts known sensitive
  fields (e.g. `password`, `token`) before they reach the output. Same
  pattern as `ContextHandler`.
- **Per-request logger** — pattern where the `http.Handler` middleware
  injects a `slog.Logger` into the request context with request-scoped
  attrs (`request_id`, `user_id`, etc.), so handlers don't have to repeat
  them. Not needed yet; revisit when we add more middleware.
