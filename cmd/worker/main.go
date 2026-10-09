// Command worker is the SQS consumer that writes reservations to
// Postgres. It runs as a separate process from the API so that:
//
//   - API latency is independent of Postgres latency (Phase 5's
//     /reserve returns ~2 ms even if the DB is having a bad day)
//   - Worker crash doesn't take the API down with it
//   - Workers can scale horizontally (multiple replicas of cmd/worker
//     share the SQS queue)
//
// Phase 7 added a second responsibility: a goroutine that runs the
// expiry sweep every 60 s, transitioning PENDING_PAYMENT rows whose
// expires_at has passed to EXPIRED and INCRBYing the inventory counter.
// This is the "belt-and-suspenders" safety net — the watcher (cmd/expiration-watcher)
// handles 99% of expiries via Redis keyspace notifications, but keyspace
// notifications have no delivery guarantee, so we re-derive the truth
// from Postgres every minute.
//
// Hot path:
//
//   POST /reserve (API)
//     → Redis Lua reserve (hold key, decrement inventory)
//     → SQSPublisher.SendReservation (Phase 6)
//     → 200 to client
//
//   SQS message (this binary)
//     ↳ queue.Poll receives via long-poll
//     ↳ db.InsertIfAbsent writes Postgres row (ON CONFLICT DO NOTHING)
//     ↳ DeleteMessage on success
//
//   hold:event:<id>:user:<user>  TTL expires (Phase 7)
//     ↳ SIDE: RunSweep goroutine picks up the row on a ticker
//     ↳ expire.Compensate: row flipped + inventory incremented
//
// At-least-once delivery + idempotent INSERT = effectively-once
// durable state.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/joho/godotenv"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/expire"
	"github.com/emmitt-k/ticket-deal/internal/logging"
	"github.com/emmitt-k/ticket-deal/internal/metrics"
	"github.com/emmitt-k/ticket-deal/internal/queue"
	"github.com/emmitt-k/ticket-deal/internal/redis"
	"github.com/emmitt-k/ticket-deal/internal/tracing"
)

const workerTracerName = "github.com/emmitt-k/ticket-deal/cmd/worker"

var workerTracer = otel.Tracer(workerTracerName)

func main() {
	logging.Init(logging.Config{
		Level:   logging.LevelFromEnv(),
		Format:  logging.FormatFromEnv(),
		Service: "worker",
		Version: os.Getenv("SERVICE_VERSION"),
	})
	if err := run(); err != nil {
		slog.Error("worker startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	slog.Info("worker starting",
		"queue", cfg.SQSQueueURL,
		"db", redactDSN(cfg.DatabaseURL),
		"poll_seconds", cfg.WorkerLongPollSeconds,
		"visibility_seconds", cfg.VisibilityTimeoutSeconds,
		"sweep", cfg.SweepInterval,
	)

	// ── OpenTelemetry tracing ─────────────────────────────────
	//
	// Same pattern as the API: init the global TracerProvider, then
	// defer the flush. If this fails (e.g. Jaeger isn't up), log and
	// keep going — the worker can still process SQS messages, we just
	// won't see the spans in the UI.
	tracingShutdown, err := tracing.Init(context.Background(), "worker")
	if err != nil {
		slog.Warn("tracing init failed (continuing without traces)", "error", err)
	}
	defer func() {
		if tracingShutdown != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = tracingShutdown(ctx)
			cancel()
		}
	}()

	// ── DB pool (fail fast if Postgres unreachable) ────────────
	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	// otelpgx.Tracer adds a span for every pgx query (auto-instrumentation).
	// Using the default tracer (no args) means it picks up our
	// global TracerProvider from tracing.Init above, so spans
	// show up in Jaeger automatically. Each INSERT ... ON CONFLICT
	// becomes a child span of the worker's handler span.
	pool, err := db.NewPool(ctx, db.Config{
		DSN:      cfg.DatabaseURL,
		MaxConns: 5,
		MinConns: 1,
		Tracer:   otelpgx.NewTracer(),
	})
	if err != nil {
		return fmt.Errorf("open DB pool: %w", err)
	}
	defer pool.Close()
	slog.Info("DB pool open", "max", 5, "min", 1)

	// ── Redis client (Phase 7: needed by the sweep goroutine) ──
	rdb := redis.NewClient(redis.Config{Addr: cfg.RedisAddr})
	defer rdb.Close()
	if err := redis.Ping(ctx, rdb); err != nil {
		return fmt.Errorf("ping Redis: %w", err)
	}
	slog.Info("Redis ping OK", "addr", cfg.RedisAddr)

	// ── SQS client (pointed at ElasticMQ locally, AWS in prod) ──
	sqsClient, err := queue.NewSQSClient(ctx, queue.AWSConfig{
		Region:      cfg.SQSRegion,
		EndpointURL: cfg.SQSEndpointURL,
		QueueURL:    cfg.SQSQueueURL,
	})
	if err != nil {
		return fmt.Errorf("build SQS client: %w", err)
	}

	// ── Expiry sweep (Phase 7 safety net) ─────────────────────
	//
	// Runs as a goroutine inside the worker process. Returns ctx.Err()
	// on graceful shutdown. We log non-canceled errors but don't
	// propagate — losing the sweep doesn't kill SQS consumption,
	// and the watcher (separate binary) covers the 99% case.
	go func() {
		if err := expire.RunSweep(ctx, pool, rdb, "worker", cfg.SweepInterval); err != nil &&
			!errors.Is(err, context.Canceled) {
			slog.Error("sweep exited with error", "error", err)
		}
	}()

	// ── Metrics HTTP server (Prometheus scrape target) ────────────
	//
	// The worker is primarily an SQS consumer (no inbound HTTP), but
	// we still need /metrics for Prometheus. Spin up a tiny dedicated
	// server on cfg.MetricsAddr (default :8081) just for that.
	metricsAddr := getEnv("METRICS_ADDR", ":8081")
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", metrics.Handler())
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	metricsSrv := &http.Server{
		Addr:              metricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		slog.Info("metrics server listening", "addr", metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server error", "error", err)
		}
	}()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutCtx)
	}()

	// ── Worker loop ─────────────────────────────────────────────
	handler := queue.MessageHandlerFunc(func(ctx context.Context, body []byte) error {
		return handleMessage(ctx, pool, body)
	})

	err = queue.Poll(ctx, queue.ConsumerConfig{
		Client:                  sqsClient,
		QueueURL:                cfg.SQSQueueURL,
		MaxMessages:             10,
		WaitTimeSeconds:         int32(cfg.WorkerLongPollSeconds),
		VisibilityTimeoutSeconds: int32(cfg.VisibilityTimeoutSeconds),
	}, handler)

	// queue.Poll returns ctx.Err() on graceful shutdown — that's expected.
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	slog.Info("clean shutdown")
	return nil
}

// handleMessage is the per-message callback wired into queue.Poll.
// Returns nil → message deleted. Returns err → message redelivered
// after visibility timeout.
//
// We deliberately keep this function small: parse, write, done. Any
// complex business logic (status transitions, retries, etc.) belongs
// in a dedicated method on a service type — not here.
func handleMessage(ctx context.Context, pool *db.Pool, raw []byte) error {
	// Manual span around the whole handler. The ctx arriving here
	// already carries the API's trace_id (the consumer extracted it
	// from the SQS message attributes), so this span is a child of
	// the API's reserve.handle — not a new root. That's what makes
	// the end-to-end waterfall possible.
	ctx, span := workerTracer.Start(ctx, "worker.handleMessage",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "aws.sqs"),
			attribute.Int("messaging.message.body.size", len(raw)),
		),
	)
	defer span.End()

	var r queue.Reservation
	if err := json.Unmarshal(raw, &r); err != nil {
		// Bad JSON can't be fixed by retrying. Log loud and bail
		// (returning nil so the message gets deleted — otherwise we
		// loop forever on the same broken message).
		span.SetStatus(codes.Error, "malformed JSON")
		span.RecordError(err)
		metrics.WorkerMessagesProcessed.WithLabelValues("malformed").Inc()
		slog.WarnContext(ctx, "malformed message (deleting)", "body", truncate(raw, 256), "error", err)
		return nil
	}
	span.SetAttributes(
		attribute.String("reservation.id", r.ReservationID),
		attribute.Int64("reservation.event_id", int64(r.EventID)),
		attribute.Int("reservation.seats", r.Seats),
	)

	dbStart := time.Now()
	if err := db.InsertIfAbsent(ctx, pool, r); err != nil {
		// Transient DB errors (connection lost, deadlock, etc.) are
		// retried via SQS redelivery. We just log and return the error.
		span.SetStatus(codes.Error, "insert failed")
		span.RecordError(err)
		metrics.WorkerMessagesProcessed.WithLabelValues("error").Inc()
		metrics.WorkerDBWrites.WithLabelValues("error").Inc()
		return fmt.Errorf("insert reservation %s: %w", r.ReservationID, err)
	}
	metrics.WorkerDBWriteDuration.Observe(time.Since(dbStart).Seconds())
	metrics.WorkerDBWrites.WithLabelValues("ok").Inc()
	metrics.WorkerMessagesProcessed.WithLabelValues("ok").Inc()

	slog.InfoContext(ctx, "reservation inserted",
		"reservation_id", r.ReservationID,
		"user", r.UserID,
		"event", r.EventID,
		"seats", r.Seats,
	)
	return nil
}

// ── Config loading ─────────────────────────────────────────────────────────

// config mirrors the env vars read here. Worker is intentionally its own
// config (does not import internal/config) so it stays runnable in
// isolation — e.g. for a manual `go run ./cmd/worker` smoke test in
// any environment where DATABASE_URL and SQS_QUEUE_URL are set.
type config struct {
	DatabaseURL            string
	SQSQueueURL            string
	SQSRegion              string
	SQSEndpointURL         string // local path
	VisibilityTimeoutSeconds int
	WorkerLongPollSeconds  int
	RedisAddr              string        // Phase 7: needed by the sweep goroutine
	SweepInterval          time.Duration // Phase 7: how often to scan for expired rows
}

func loadConfig() (*config, error) {
	// Dev ergonomics: load .env so `./bin/worker` works without
	// `set -a; source .env; set +a` in the shell. The error is
	// ignored because production runs inject real env vars
	// (Kubernetes, ECS, systemd) and don't have a .env file.
	// Matches internal/config/config.go:95 for the API.
	_ = godotenv.Load()

	c := &config{
		DatabaseURL:             getEnv("DATABASE_URL", ""),
		SQSQueueURL:             getEnv("SQS_QUEUE_URL", ""),
		SQSRegion:               getEnv("AWS_REGION", "us-east-1"),
		SQSEndpointURL:          getEnv("SQS_ENDPOINT_URL", ""),
		VisibilityTimeoutSeconds: atoiOr(getEnv("SQS_VISIBILITY_TIMEOUT_SECONDS", "30"), 30),
		WorkerLongPollSeconds:   atoiOr(getEnv("WORKER_LONG_POLL_SECONDS", "20"), 20),
		RedisAddr:               getEnv("REDIS_ADDR", "localhost:6379"),
		SweepInterval:           sweepIntervalOr(getEnv("SWEEP_INTERVAL_SECONDS", "60"), 60*time.Second),
	}
	if c.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	if c.SQSQueueURL == "" {
		return nil, errors.New("SQS_QUEUE_URL is required")
	}
	return c, nil
}

// sweepIntervalOr parses a seconds-string into time.Duration. Falls back
// to def on error. Kept separate from atoiOr so callers don't have to
// remember the seconds→Duration conversion.
func sweepIntervalOr(s string, def time.Duration) time.Duration {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return time.Duration(n) * time.Second
}

// ── Tiny env helpers (worker-local; not shared with internal/config) ───────

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

// redactDSN hides the password in a Postgres URL for log lines.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "(unparseable)"
	}
	if u.User != nil {
		u.User = url.UserPassword(u.User.Username(), "REDACTED")
	}
	return u.String()
}

// truncate caps a byte slice for log output. UTF-8 aware truncation
// would be nicer but adds a dependency; ASCII is fine for log lines.
func truncate(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

// silence unused-import warnings for symbols kept for future use.
var (
	_ = time.Now
)