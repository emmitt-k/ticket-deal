# Distributed Tracing — Crash Course

> **Audience:** new to OpenTelemetry / Jaeger / distributed tracing. Goal: understand
> what we're about to build in `ticket-deal` and *why* — fast.

---

## TL;DR

A **trace** is a record of *one* request's full journey through the system. A **span**
is one step in that journey (e.g. "redis acquire hold"). All spans for a request share a
`trace_id`, so you can rebuild the whole story as a **waterfall** in the Jaeger UI.

Prometheus already shows you *aggregated* behavior ("1000 req/s, p99 = 200ms").
Tracing shows you *one specific* request ("VU-42 took 312ms, 80% of which was waiting
for the SQS publish"). Different tool, different question.

---

## 1. The mental model (60 seconds)

One customer hits `POST /reserve`. Behind the scenes, **5 things happen across 3 services
in ~200ms**:

```
[ VU / curl ]                    ┌── API (:8080) ────────────────────────────┐
   │  POST /reserve              │  span A: "POST /reserve"          (45ms)  │
   │                             │    A.1: "redis.SET hold"          ( 3ms)  │
   │                             │    A.2: "sqs.Publish"             ( 8ms)  │
   │                             │    A.3: "pg.Query seat"           (12ms)  │
   ▼                             └──────────────────────────────────────────┘
   ...time passes... (60s hold TTL)
                                  ┌── Expiration-Watcher (:8083) ─────────────┐
                                  │  span B: "expire.seat"           ( 8ms)   │
                                  │    B.1: "pg.UPDATE seat"         ( 5ms)   │
                                  └──────────────────────────────────────────┘
                                  ┌── Worker (:8081) ──────────────────────────┐
                                  │  span C: "worker.handleMessage"   (35ms)  │
                                  │    C.1: "pg.INSERT reservation"   (28ms)  │
                                  └──────────────────────────────────────────┘
```

In the Jaeger UI, this renders as a **waterfall**:

- Bar **width** = how long that step took
- Bar **nesting** = "I called this from inside that"
- Bar **color** = which service emitted it
- Each bar has a `span_id`; the whole tree shares a `trace_id`

Click a bar → see its attributes (e.g. `event_id=1`, `status=201`, `hold_key=seat:42`),
its logs (errors), and the parent span that called it.

---

## 2. The three things you need to know

### 2.1 The `context.Context` is everything

In Go you pass `ctx context.Context` everywhere. The OTel SDK tucks a `trace_id` and
`span_id` into `ctx` automatically. When a child function calls `tracer.Start(ctx, ...)`,
the new span knows its parent — that's how the tree gets built.

> **If you forget to pass `ctx`, the trace breaks.** This is the #1 thing OTel gets right
> for you when you use the auto-instrumentation libraries.

### 2.2 Two ways to add tracing

**Auto-instrumentation** (easy, less code):
- One line: `otelhttp.NewHandler(router, "api")` — wraps every HTTP route
- One line: `otelpgx.NewTracer(...)` — auto-traces every Postgres query
- One line: `redisotel.InstrumentTracing(rdb)` — auto-traces every Redis call
- One line: AWS SDK middleware for SQS

Covers ~80% of common cases with **zero manual code**.

**Manual spans** (for the interesting business logic):

```go
func reserveSeat(ctx context.Context, eventID int) error {
    ctx, span := tracer.Start(ctx, "redis.acquire-hold",
        trace.WithAttributes(attribute.Int("event_id", eventID)),
    )
    defer span.End()

    ok, err := acquireHold(ctx, eventID)
    if err != nil {
        span.SetStatus(codes.Error, "hold failed")
        span.RecordError(err)
        return err
    }
    if !ok {
        span.SetStatus(codes.Error, "sold out")
    }
    return nil
}
```

You'd add manual spans for things the auto-instrumentation can't see — the *meaning* of
a request, not just its I/O. E.g. `reserve.handle`, `redis.acquire-hold`, `sqs.publish`,
`expire.seat`.

### 2.3 The collector

Spans need somewhere to live. We add **Jaeger** to docker-compose. Services export spans
via **OTLP/gRPC** (port 4317) to Jaeger, which stores them and serves the UI on
port 16686.

```
api ─────┐
worker ──┼── OTLP/gRPC ──► jaeger:4317 ──► Jaeger UI :16686
watcher ─┘
```

---

## 3. The five new concepts (cheat sheet)

| Concept | What it is | Example |
|---|---|---|
| **trace** | One full request lifecycle | `trace_id=7f3a2b1c...` covers POST /reserve all the way to PG write |
| **span** | One unit of work inside a trace | `span_id=0a12...` for `redis.acquire-hold` |
| **context propagation** | Passing `trace_id` across boundaries | via `ctx` across functions, `traceparent` HTTP header, SQS message body, Redis key (rare) |
| **OTLP** | OpenTelemetry Protocol — the wire format | gRPC on :4317 or HTTP on :4318 |
| **sampling** | Don't trace 100% of 1000-VU traffic | Head-based: sample X% at start. Tail-based: keep only "interesting" ones. Default: 100% in dev, lower in prod. |

---

## 4. What tracing gets you in *this* codebase

| Pain today | What tracing shows you |
|---|---|
| "Why is p99 200ms when Redis is 3ms?" | The exact span breakdown: 3ms redis + 8ms SQS + 12ms PG = where the time really goes |
| "Did my worker actually process message X?" | Search by `reservation_id` attribute, see the worker span that handled it |
| "Are holds expiring or completing?" | Compare trace counts of `expire.seat` vs `worker.handleMessage` |
| "Is the waiting room the bottleneck?" | Spans from the waiting-room release + `time_in_queue_ms` attribute |
| "README needs a hero screenshot" | A Jaeger waterfall of one reservation through the entire system 🔥 |

---

## 5. What tracing is NOT

| You have | Tracing is **different** from |
|---|---|
| **Prometheus metrics** | Metrics = aggregated counters (req/s, p99). Tracing = *one specific* request's journey. |
| **Application logs** | Logs = timestamped text. Tracing = structured tree of timed operations. You can attach log lines to spans via `span.AddEvent("log message")`. |

The three **complement** each other:

- **Metrics** = "the system's behavior in aggregate" (1000 req/s, p99 = 200ms)
- **Logs** = "the things the system said" ("[ERROR] pg connection refused")
- **Traces** = "what one specific request did" (VU-42: 3ms redis → 8ms SQS → 12ms PG → ok)

When a user reports a bug, only tracing shows *their* journey. When a SLO breaches, only
metrics show the trend. When you need forensic detail, only logs have the message text.

---

## 6. The plan for this codebase

### New files

```
internal/tracing/
└── tracing.go                OTel SDK init, OTLP exporter, sampler, shutdown

docs/tracing-plan.md          Design doc (like observability-plan.md)
```

### Modified files

```
go.mod / go.sum               + go.opentelemetry.io/otel, /sdk, /exporters/otlp/grpc
                              + go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp
                              + go.opentelemetry.io/contrib/instrumentation/github.com/redis/go-redis/extra/redisotel/v9
                              + github.com/exaring/otelpgx  (Postgres auto-instrumentation)

cmd/api/main.go               + otelhttp.Middleware (wraps every route)
                              + manual span "reserve.handle" in reserve_handler
                              + manual span "redis.acquire-hold"
                              + manual span "sqs.publish"
                              + redisotel.InstrumentTracing(rdb)

cmd/worker/main.go            + extract traceparent from SQS message body
                              + manual span "worker.handleMessage"
                              + otelpgx.Tracer on pgxpool

cmd/expiration-watcher/main.go
                              + manual span "expire.seat" on keyspace notification
                              + manual span "pg.compensate"
                              + otelpgx.Tracer on pgxpool

docker-compose.yml            + jaeger service (all-in-one image, :16686 UI, :4317 OTLP)
                              + OTEL_EXPORTER_OTLP_ENDPOINT env var on all Go services

AGENTS.md                     + tracing section
README.md                     + tracing row in tech stack, hero screenshot
```

### Key env vars

| Var | Default | Purpose |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://jaeger:4317` | Where to send spans |
| `OTEL_SERVICE_NAME` | per-binary (`api`, `worker`, `expiration-watcher`) | Labels traces in Jaeger |
| `OTEL_TRACES_SAMPLER_ARG` | `1.0` | Sample 100% in dev; lower in prod |

---

## 7. Suggested build order

1. **Add Jaeger to docker-compose** — see the collector running (5 min)
2. **Build `internal/tracing` package** — OTel SDK init, OTLP exporter, sampler, graceful shutdown (30 min)
3. **Wire `cmd/api`** — middleware + 3 manual spans around the reservation flow (30 min)
4. **Verify waterfall** — fire 1 request, see it in Jaeger UI (10 min)
5. **Wire `cmd/worker` + `cmd/expiration-watcher`** — context propagation through SQS, root span on each (30 min)
6. **Run k6 burst, take a screenshot** — hero shot for README (15 min)
7. **Tune sampling** — don't melt Jaeger at 1000 VUs (15 min)
8. **Docs + commit** (20 min)

**Total:** ~2.5 hours. The whole point of the exercise is one beautiful Jaeger waterfall
shot for the README — that's your "this person really gets distributed systems" flex.

---

## 8. Glossary

- **OTel / OpenTelemetry** — the vendor-neutral standard SDK + protocol. Successor to
  OpenTracing and OpenCensus. What you import.
- **OTLP** — OpenTelemetry Protocol, the wire format. Two flavors: gRPC (default,
  port 4317) and HTTP (port 4318).
- **Jaeger** — the storage + UI. Open-source, made by Uber. Spans in, waterfall out.
  Alternatives: Tempo, Zipkin, Honeycomb (SaaS).
- **Traceparent** — the W3C standard header (`traceparent: 00-<trace_id>-<span_id>-01`)
  that carries context across HTTP calls. Set automatically by otelhttp.Middleware.
- **Resource** — metadata about the thing emitting spans (`service.name=api`,
  `service.version=1.0.0`, `host.name=...`). Attached to every span.
- **Span attributes** — key/value tags on a span (`event_id=1`, `hold_key=seat:42`).
  Indexable, searchable, show up in the Jaeger UI.
- **Span events** — timestamped log lines attached to a span (different from
  application logs — these live inside the trace).
- **Head-based sampling** — decide at span *start* whether to keep it.
- **Tail-based sampling** — decide at span *end* based on the whole trace (e.g.
  "keep all traces that contain an error span").
