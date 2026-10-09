# Observability Plan

> **Goal:** Add Prometheus + Grafana to the local dev stack so we can watch live metrics (RPS, latency, reservation rates, queue depth) during load tests.

---

## Stack

```
Services (localhost:8080, 8081, 8083)
    → /metrics endpoints (Prometheus text format)
        → Prometheus (:9090, via host.docker.internal)
            → Grafana (:3000, dashboards)
```

| Service | Port | Metrics |
|---|---|---|
| API | `:8080` | `GET /metrics` |
| Worker | `:8081` | `GET /metrics` |
| Expiration-Watcher | `:8083` | `GET /metrics` |
| Dashboard-server | `:8082` | optional |

---

## Implementation Order

### Phase 1 — Instrument Go services (must be first)

1. **`go.mod` / `go.sum`**: add `github.com/prometheus/client_golang v1.20`
2. **`cmd/api/main.go`**: add `/metrics` handler via `github.com/prometheus/client_golang/prometheus/promhttp`
3. **`cmd/worker/main.go`**: add `/metrics` handler
4. **`cmd/expiration-watcher/main.go`**: add `/metrics` handler on `:8083`
5. **`cmd/dashboard-server/main.go`**: add `/metrics` handler

**Metrics to expose per service:**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `http_requests_total` | Counter | `path`, `method`, `status` | Total HTTP requests |
| `http_request_duration_seconds` | Histogram | `path` | Request latency |
| `reservations_held_total` | Counter | — | Seats placed on hold |
| `reservations_completed_total` | Counter | — | Seats confirmed |
| `reservations_oversold_total` | Counter | — | Oversell attempts (should be 0) |
| `worker_messages_processed_total` | Counter | `status` | SQS messages processed |
| `worker_db_writes_total` | Counter | `status` | DB writes by worker |
| `expiration_sweep_cycles_total` | Counter | — | Sweep cycles run |
| `expiration_seats_expired_total` | Counter | — | Seats expired per sweep |
| `redis_seats_available` | Gauge | — | Current available seats |
| `waiting_room_queue_length` | Gauge | — | VUs waiting |

### Phase 2 — Docker Compose additions

1. **`docker-compose.yml`**: add `prometheus` + `grafana` services
2. **`monitoring/prometheus.yml`**: scrape config targeting `host.docker.internal:8080/8081/8083`
3. **Auth**: Grafana `admin / admin123` (local dev only, no TLS)

### Phase 3 — Grafana provisioning (auto-configure on startup)

1. **`monitoring/grafana/provisioning/datasources/datasource.yml`**: auto-add Prometheus as datasource
2. **`monitoring/grafana/provisioning/dashboards/dashboard.yml`**: auto-load dashboard JSONs
3. **`monitoring/grafana/dashboards/ticket-deal.json`**: custom dashboard

### Phase 4 — Verify

```bash
docker compose up -d
open http://localhost:3000   # admin / admin123
# Dashboard: Home → Ticket Deal Overview
make all-services
make loadtest-state && make loadtest-jwts
make loadtest-burst   # watch charts move in Grafana
```

---

## Files to Create / Modify

```
# New files
monitoring/
├── prometheus.yml                          # Prometheus scrape config
├── grafana/
│   ├── provisioning/
│   │   ├── datasources/
│   │   │   └── datasource.yml              # auto-provision Prometheus datasource
│   │   └── dashboards/
│   │       └── dashboard.yml               # auto-provision dashboards
│   └── dashboards/
│       └── ticket-deal-overview.json      # Grafana dashboard JSON

# Modified files
docker-compose.yml                          # add prometheus + grafana services
go.mod / go.sum                            # add prometheus/client_golang
cmd/api/main.go                            # add /metrics handler + custom metrics
cmd/worker/main.go                         # add /metrics handler + custom metrics
cmd/expiration-watcher/main.go             # add /metrics handler on :8083 + custom metrics
cmd/dashboard-server/main.go               # add /metrics handler (optional)
```

---

## Grafana Dashboard Panels

**Ticket Deal Overview** (single page):

1. **Request Rate** — `rate(http_requests_total[10s])` by `path`
2. **Error Rate** — `rate(http_requests_total{status=~"5.."}[10s])`
3. **p99 Latency** — `histogram_quantile(0.99, rate(http_request_duration_seconds_bucket[10s]))` by `path`
4. **Reservation Outcomes** — `rate(reservations_held_total[10s])` vs `rate(reservations_completed_total[10s])` vs `rate(reservations_oversold_total[10s])`
5. **Worker Throughput** — `rate(worker_messages_processed_total[10s])`
6. **Expiration Sweeps** — `rate(expiration_sweep_cycles_total[10s])` + `rate(expiration_seats_expired_total[10s])`
7. **Seat Availability** — `redis_seats_available` (gauge)
8. **Waiting Room Depth** — `waiting_room_queue_length` (gauge)

---

## Ports

| Service | Port | Note |
|---|---|---|
| Prometheus | `:9090` | Web UI |
| Grafana | `:3000` | Dashboards |
| Prometheus won't need a port mapping | runs in Docker | — |
