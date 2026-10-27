// Command stripe-worker is the SQS consumer that applies Stripe
// webhook events to reservations.
//
// It is a separate process from the API for the same reasons
// cmd/worker is:
//
//   - the webhook endpoint stays fast (it only verifies and queues)
//   - a crash here does not take the API down
//   - it scales horizontally (replicas share the queue)
//
// The flow:
//
//	POST /webhooks/stripe (API)
//	  → verify the Stripe signature
//	  → snapshot the event
//	  → publish to the stripe-events queue
//	  → 200 to Stripe
//
//	SQS message (this binary)
//	  ↳ queue.Poll receives via long-poll
//	  ↳ payment.Handler applies the event:
//	      payment_intent.succeeded   → PENDING_PAYMENT → CONFIRMED
//	      payment_intent.payment_failed → stays PENDING_PAYMENT
//	      charge.refunded            → CONFIRMED → REFUNDED (+ the seat
//	                                   goes back on sale; a PARTIAL
//	                                   refund only records the event)
//	  ↳ DeleteMessage on success
//
// At-least-once delivery + an idempotent state machine = effectively
// once. Every transition is a compare-and-set, and every duplicate
// delivery lands on the "already done" branch.
package main

import (
	"context"
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
	"github.com/emmitt-k/ticket-deal/internal/logging"
	"github.com/emmitt-k/ticket-deal/internal/metrics"
	"github.com/emmitt-k/ticket-deal/internal/payment"
	"github.com/emmitt-k/ticket-deal/internal/queue"
	"github.com/emmitt-k/ticket-deal/internal/redis"
	"github.com/emmitt-k/ticket-deal/internal/tracing"
)

const stripeWorkerTracerName = "github.com/emmitt-k/ticket-deal/cmd/stripe-worker"

var stripeWorkerTracer = otel.Tracer(stripeWorkerTracerName)

func main() {
	logging.Init(logging.Config{
		Level:   logging.LevelFromEnv(),
		Format:  logging.FormatFromEnv(),
		Service: "stripe-worker",
		Version: os.Getenv("SERVICE_VERSION"),
	})
	if err := run(); err != nil {
		slog.Error("stripe-worker startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	slog.Info("stripe-worker starting",
		"queue", cfg.SQSQueueURL,
		"db", redactDSN(cfg.DatabaseURL),
		"poll_seconds", cfg.WorkerLongPollSeconds,
		"visibility_seconds", cfg.VisibilityTimeoutSeconds,
	)

	// ── OpenTelemetry ──────────────────────────────────────────
	tracingShutdown, err := tracing.Init(context.Background(), "stripe-worker")
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

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── Postgres ───────────────────────────────────────────────
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

	// ── Redis ──────────────────────────────────────────────────
	//
	// Needed for one thing: putting a REFUNDED seat back on sale.
	rdb := redis.NewClient(redis.Config{Addr: cfg.RedisAddr})
	defer rdb.Close()
	if err := redis.Ping(ctx, rdb); err != nil {
		return fmt.Errorf("ping Redis: %w", err)
	}
	slog.Info("Redis ping OK", "addr", cfg.RedisAddr)

	// ── SQS ────────────────────────────────────────────────────
	sqsClient, err := queue.NewSQSClient(ctx, queue.AWSConfig{
		Region:      cfg.SQSRegion,
		EndpointURL: cfg.SQSEndpointURL,
		QueueURL:    cfg.SQSQueueURL,
	})
	if err != nil {
		return fmt.Errorf("build SQS client: %w", err)
	}

	// ── Metrics server ─────────────────────────────────────────
	//
	// Same shape as cmd/worker's: a tiny dedicated HTTP server for
	// the Prometheus scrape target.
	metricsAddr := getEnv("METRICS_ADDR", ":8084")
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

	// ── Worker loop ────────────────────────────────────────────
	handler := queue.MessageHandlerFunc(func(ctx context.Context, body []byte) error {
		return handleMessage(ctx, pool, rdb, body)
	})

	err = queue.Poll(ctx, queue.ConsumerConfig{
		Client:                   sqsClient,
		QueueURL:                 cfg.SQSQueueURL,
		MaxMessages:              10,
		WaitTimeSeconds:          int32(cfg.WorkerLongPollSeconds),
		VisibilityTimeoutSeconds: int32(cfg.VisibilityTimeoutSeconds),
	}, handler)

	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	slog.Info("clean shutdown")
	return nil
}

// handleMessage is the per-message callback wired into queue.Poll.
//
// Returns nil → the message is deleted. Returns err → it is
// redelivered after the visibility timeout.
//
// The ctx arriving here already carries the API's trace_id (the
// consumer extracted it from the SQS message attributes), so the span
// below is a child of the API's webhook span — the end-to-end
// waterfall works across the queue.
func handleMessage(ctx context.Context, pool *db.Pool, rdb *redis.Client, body []byte) error {
	ctx, span := stripeWorkerTracer.Start(ctx, "stripe_worker.handleMessage",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "aws.sqs"),
			attribute.Int("messaging.message.body.size", len(body)),
		),
	)
	defer span.End()

	// The handler carries the dependencies; constructing it per
	// message is cheap (two struct fields) and keeps the wiring
	// local and obvious.
	handler := payment.NewHandler(pool, rdb)

	if err := handler.HandleMessage(ctx, body); err != nil {
		span.SetStatus(codes.Error, "event handling failed")
		span.RecordError(err)
		// Return the error so SQS redelivers. The state machine is
		// idempotent, so a redelivery of a partially-applied event
		// is safe.
		slog.ErrorContext(ctx, "stripe event handling failed (will redeliver)", "error", err)
		return err
	}
	return nil
}

// ── Config loading ─────────────────────────────────────────────────────

// config mirrors the env vars read here. Like cmd/worker, this binary
// keeps its own config rather than importing internal/config, so it
// stays runnable in isolation.
type config struct {
	DatabaseURL              string
	SQSQueueURL              string
	SQSRegion                string
	SQSEndpointURL           string
	VisibilityTimeoutSeconds int
	WorkerLongPollSeconds    int
	RedisAddr                string
}

func loadConfig() (*config, error) {
	// Dev ergonomics: load .env so ./bin/stripe-worker works without
	// `set -a; source .env; set +a` in the shell.
	_ = godotenv.Load()

	c := &config{
		DatabaseURL:              getEnv("DATABASE_URL", ""),
		SQSQueueURL:              getEnv("STRIPE_EVENTS_QUEUE_URL", ""),
		SQSRegion:                getEnv("AWS_REGION", "us-east-1"),
		SQSEndpointURL:           getEnv("SQS_ENDPOINT_URL", ""),
		VisibilityTimeoutSeconds: atoiOr(getEnv("SQS_VISIBILITY_TIMEOUT_SECONDS", "30"), 30),
		WorkerLongPollSeconds:    atoiOr(getEnv("WORKER_LONG_POLL_SECONDS", "20"), 20),
		RedisAddr:                getEnv("REDIS_ADDR", "localhost:6379"),
	}
	if c.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	if c.SQSQueueURL == "" {
		return nil, errors.New("STRIPE_EVENTS_QUEUE_URL is required")
	}
	return c, nil
}

// ── Tiny env helpers (binary-local; not shared with internal/config) ────

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
