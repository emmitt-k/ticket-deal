# Distributed Tracing Plan

> **Goal:** Add OpenTelemetry distributed tracing to the entire reservation
> flow so a single `trace_id` covers `api → Redis → SQS → worker → Postgres`,
> with rich attributes at every step. Storage + UI: **Jaeger** (all-in-one
> in docker-compose).

> **Status:** ✅ Implemented on `feature/tracing`. See `docs/tracing-crashcourse.md`
> for the beginner intro and `docs/architecture.md` for the full architecture.

---

## Stack

```
Go services (host)                ┌── Jaeger (docker, all-in-one) ──┐
  api      :8080  ─┐ OTLP/gRPC ─► │  collector :4317                │ ─► UI
  worker   :8081  ─┤              │  query     :16686               │ ─► queries
  watcher  :8083  ─┘              │  storage   BadgerDB (volume)    │
                                  └────────────────────────────────┘
```

| Component | Library / Image | Version |
|---|---|---|
| OTel SDK | `go.opentelemetry.io/otel` | v1.47.0 |
| OTLP/gRPC exporter | `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` | v1.47.0 |
| HTTP middleware | `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | v0.72.0 |
| Redis auto-instrumentation | `github.com/redis/go-redis/extra/redisotel/v9` | v9.23.0 |
| Postgres auto-instrumentation | `github.com/exaring/otelpgx` | v0.13.0 |
| Semantic conventions | `go.opentelemetry.io/otel/semconv/v1.43.0` | matches SDK default |
| Collector / UI | `jaegertracing/all-in-one:1.76.0` | latest stable |

---

## Implementation Phases

### Phase 1 — `internal/tracing` package ✅

- `tracing.Init(ctx, serviceName)` wires up:
  - OTLP/gRPC exporter (env: `OTEL_EXPORTER_OTLP_ENDPOINT`, default `http://localhost:4317`)
  - W3C TraceContext + Baggage global propagator
  - `ParentBased(TraceIDRatioBased(sampler))` (default 1.0 = trace everything; tunable via `OTEL_TRACES_SAMPLER_ARG`)
  - Resource with `service.name` and `service.version` (semconv v1.43.0 to match SDK default — v1.26.0 caused a `conflicting Schema URL` at init)
- Returns a `ShutdownFunc` for graceful flush on SIGTERM

### Phase 2 — API instrumentation ✅

- `cmd/api/main.go`: `tracing.Init("api")` + `otelhttp.NewHandler(router, "api")` + `redisotel.InstrumentTracing(rdb)`
- `internal/api/reserve_handler.go`: 3 manual spans:
  - `reserve.handle` (parent of all reservation work; attributes: `reservation.event_id`, `reservation.user_id`, `reservation.seats_requested`, `reservation.id`)
  - `redis.acquire-hold` (attributes: `hold.event_id`, `hold.user_id_hash` (hashed, not PII), `hold.ttl_seconds`, `hold.status_code`, `hold.rejection_reason`)
  - `sqs.publish` (attributes: `messaging.system=aws.sqs`, `messaging.destination.name`, `messaging.message.body.size`)

### Phase 3 — Worker instrumentation ✅

- `cmd/worker/main.go`: `tracing.Init("worker")` + `otelpgx.NewTracer()` on the pgxpool + `redisotel.InstrumentTracing(rdb)`
- `internal/queue/trace.go`: new helpers
  - `InjectTraceContext` — write current ctx into SQS `MessageAttributes.traceparent` (W3C standard)
  - `ExtractTraceContext` — pull `traceparent` from a message + fall back to body `_traceparent` field (ElasticMQ in dev strips attributes; real AWS SQS preserves them)
  - `TraceContextFromContext` — debugging helper
- `internal/queue/publisher.go`: `SendReservation` injects ctx into both attributes AND message body
- `internal/queue/consumer.go`: `Poll` extracts ctx before dispatching to the handler
- `cmd/worker/main.go` `handleMessage`: new `worker.handleMessage` span (kind=Consumer) with `messaging.system`, `messaging.message.body.size`, `reservation.id`, `reservation.event_id`, `reservation.seats`

### Phase 4 — Watcher instrumentation ✅

- `cmd/expiration-watcher/main.go`: `tracing.Init("expiration-watcher")` + `otelpgx.NewTracer()` + `redisotel.InstrumentTracing(rdb)`
- `internal/expire/compensate.go`: new `expire.compensate` span (root) with `expire.event_id`, `expire.user_id_hash`, `expire.seats_released`

### Phase 5 — DB pool hook ✅

- `internal/db/pool.go`: new `Config.Tracer *otelpgx.Tracer` field; `NewPool` installs it on `pgxpool.ConnConfig.Tracer` so every pgx query becomes a span
- Default (nil) → `otelpgx.NewTracer()` which picks up the global OTel TracerProvider (no-op until `tracing.Init` runs — safe for tests)

### Phase 6 — Tests ✅

- `internal/queue/trace_test.go`:
  - `TestInjectExtractRoundtrip` — in-memory SDK, full inject → extract → assert trace_id preserved
  - `TestInjectEmpty` — no active span → no attributes set
  - `TestExtractNoAttributes` — message with no attrs → ctx unchanged
  - `TestExtractFromBodyFallback` — body `_traceparent` works when attributes stripped
- All 4 tests pass without docker

---

## End-to-end verification ✅

1000-VU burst produced multi-service waterfalls in Jaeger (api + worker in
same trace, 10 spans). Example:

```
[185.7ms] POST (api)
[185.5ms] └─ reserve.handle (api)
[ 61.8ms]    ├─ redis.acquire-hold (api)
[ 61.8ms]    │  └─ evalsha (api)
[ 21.5ms]    │     └─ hello (api)              ← drainer-injected span
[  5.2ms]    │     └─ redis.pipeline (api)
[123.7ms]    └─ sqs.publish (api)
[  0.5ms]       └─ worker.handleMessage (worker) ← context jumps via SQS body
[  0.0ms]          └─ pool.acquire (worker)
[  0.4ms]          └─ INSERT (worker)
```

---

## Environment variables

| Var | Default | Purpose |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://localhost:4317` | OTLP/gRPC collector URL |
| `OTEL_SERVICE_NAME` | per-binary | Already set by `tracing.Init(serviceName)` |
| `OTEL_TRACES_SAMPLER_ARG` | `1.0` | Fraction of root spans to sample; 0.1 = 10% |

---

## Key design decisions

1. **Why OTLP/gRPC over OTLP/HTTP?** Faster, multiplexed, fewer connections. Both are supported by Jaeger; we use gRPC on :4317.

2. **Why `ParentBased(TraceIDRatioBased)` sampler?** If an upstream service already decided to keep a trace, we keep all child spans. For root spans (no upstream), we sample at the configured ratio. Production: 0.1; dev: 1.0.

3. **Why embed `_traceparent` in the message body?** Belt-and-suspenders for local dev. Real AWS SQS preserves `MessageAttributes` end-to-end, so the attribute is the primary path. ElasticMQ (local) drops them, so the body is the fallback. Both paths are tested.

4. **Why hashed `user_id` as a span attribute?** Spans are searchable in Jaeger; if you want to find all of user X's traces, you need *some* stable identifier. Raw `user_id` may be PII depending on what the system uses it for. A stable 64-bit hash is a safe middle ground — searchable, but doesn't leak identity.

5. **Why not use `otelgin` or `otelchi`?** We use `chi`, but `otelhttp.NewHandler(router, "api")` is router-agnostic and works for any `http.Handler`. Skips a dependency for the same coverage.

---

## Files changed

```
NEW     docs/tracing-crashcourse.md
NEW     docs/tracing-plan.md
NEW     internal/tracing/tracing.go
MOD     cmd/api/main.go                          + tracing init + otelhttp
MOD     internal/api/reserve_handler.go          + 3 manual spans
NEW     internal/queue/trace.go                  + propagator helpers
MOD     internal/queue/publisher.go              + InjectTraceContext + body fallback
MOD     internal/queue/consumer.go               + ExtractTraceContext
NEW     internal/queue/trace_test.go             + 4 unit tests
MOD     internal/db/pool.go                      + Tracer config field
MOD     cmd/worker/main.go                       + tracing init + handleMessage span
MOD     cmd/expiration-watcher/main.go           + tracing init
MOD     internal/expire/compensate.go            + expire.compensate span
MOD     docker-compose.yml                       + jaeger service
MOD     go.mod / go.sum                          + OTel deps
```

---

## Future work (not in this PR)

- **Tail-based sampling** — keep only "interesting" traces (errors, slow requests). Need an OTel Collector with tail-sampling processor.
- **Metrics from spans** — derive RED metrics (Rate, Errors, Duration) from span data via the OTel Collector, not from Prometheus directly. Better correlation.
- **Log correlation** — add `trace_id` / `span_id` to every log line so you can jump from a log to its trace in Jaeger. Needs `slog` integration with OTel.
- **DB query attribute stripping** — the auto-spans include the full SQL in `db.statement`. If SQL ever contains PII, switch to `otelpgx.NewTracer(otelpgx.WithIncludeQueryParameters(false))`.
