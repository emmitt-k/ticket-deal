// Package metrics defines the Prometheus metrics exposed by all services
// in ticket-deal. Centralized here so every service uses the same metric
// names, labels, and bucket boundaries — which is essential for correct
// PromQL queries across services.
//
// Usage:
//
//	// In a service's main.go:
//	r.Get("/metrics", metrics.Handler())
//
//	// Incrementing from a handler:
//	metrics.ReservationsHeld.Inc()
//
//	// Setting a gauge (e.g. available seats):
//	metrics.RedisSeatsAvailable.Set(100)
//
// Prometheus scrapes each service's /metrics endpoint on its own port.
// Metrics are defined at package level so they register with the
// default registry on import (via promauto).
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// HTTP request metrics. Exposed by every service that has an HTTP
// server (api, worker, expiration-watcher, dashboard-server).
var (
	HTTPRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests served, by path/method/status.",
		},
		[]string{"service", "path", "method", "status"},
	)

	HTTPRequestDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency in seconds, by path.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		},
		[]string{"service", "path"},
	)
)

// Reservation outcomes. Incremented by cmd/api's reserve handler.
var (
	ReservationsHeld = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "reservations_held_total",
			Help: "Total seats placed on hold in Redis.",
		},
	)

	ReservationsCompleted = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "reservations_completed_total",
			Help: "Total reservations that successfully published to SQS.",
		},
	)

	ReservationsOversold = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "reservations_oversold_total",
			Help: "Total oversell attempts. Should always be 0; alert on >0.",
		},
	)

	ReservationsSoldOut = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "reservations_sold_out_total",
			Help: "Total /reserve calls rejected with 409 sold_out.",
		},
	)

	ReservationsAlreadyHolding = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "reservations_already_holding_total",
			Help: "Total /reserve calls rejected because the user already holds a seat.",
		},
	)
)

// Worker metrics. Exposed by cmd/worker.
var (
	WorkerMessagesProcessed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "worker_messages_processed_total",
			Help: "SQS messages processed by the worker, by status (ok|error|malformed).",
		},
		[]string{"status"},
	)

	WorkerDBWrites = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "worker_db_writes_total",
			Help: "Postgres writes by the worker, by status (ok|error).",
		},
		[]string{"status"},
	)

	WorkerDBWriteDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "worker_db_write_duration_seconds",
			Help:    "Postgres write latency for reservations.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		},
	)
)

// Expiration metrics. Exposed by cmd/expiration-watcher (and
// cmd/worker's sweep goroutine, with a service label).
var (
	ExpirationSweepCycles = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "expiration_sweep_cycles_total",
			Help: "Sweep cycles run, by service (watcher|worker).",
		},
		[]string{"service"},
	)

	ExpirationSeatsExpired = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "expiration_seats_expired_total",
			Help: "Seats expired and returned to inventory, by service.",
		},
		[]string{"service"},
	)
)

// State gauges. Updated periodically by the API service.
var (
	RedisSeatsAvailable = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "redis_seats_available",
			Help: "Current available seat count for the active event.",
		},
	)

	WaitingRoomQueueLength = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "waiting_room_queue_length",
			Help: "Current number of VUs/users in the waiting room queue.",
		},
	)
)

// Handler returns the HTTP handler that exposes the /metrics endpoint.
// Mount it at /metrics in your service's router.
func Handler() http.Handler {
	return promhttp.Handler()
}
