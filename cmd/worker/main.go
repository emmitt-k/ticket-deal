// Command worker is the SQS consumer that writes reservations to
// Postgres. It runs as a separate process from the API so that:
//
//   - API latency is independent of Postgres latency (Phase 5's
//     /reserve returns ~2 ms even if the DB is having a bad day)
//   - Worker crash doesn't take the API down with it
//   - Workers can scale horizontally (multiple replicas of cmd/worker
//     share the SQS queue)
//
// The hot path:
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
// At-least-once delivery + idempotent INSERT = effectively-once
// durable state.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/queue"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := run(); err != nil {
		log.Fatalf("worker: %v", err)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	log.Printf("worker: starting (queue=%s, db=%s, poll=%ds, visibility=%ds)",
		cfg.SQSQueueURL, redactDSN(cfg.DatabaseURL),
		cfg.WorkerLongPollSeconds, cfg.VisibilityTimeoutSeconds)

	// ── DB pool (fail fast if Postgres unreachable) ────────────
	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, db.Config{
		DSN:     cfg.DatabaseURL,
		MaxConns: 5,
		MinConns: 1,
	})
	if err != nil {
		return fmt.Errorf("open DB pool: %w", err)
	}
	defer pool.Close()
	log.Printf("worker: DB pool open (max=5, min=1)")

	// ── SQS client (pointed at ElasticMQ locally, AWS in prod) ──
	sqsClient, err := queue.NewSQSClient(ctx, queue.AWSConfig{
		Region:      cfg.SQSRegion,
		EndpointURL: cfg.SQSEndpointURL,
		QueueURL:    cfg.SQSQueueURL,
	})
	if err != nil {
		return fmt.Errorf("build SQS client: %w", err)
	}

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
	log.Printf("worker: clean shutdown")
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
	var r queue.Reservation
	if err := json.Unmarshal(raw, &r); err != nil {
		// Bad JSON can't be fixed by retrying. Log loud and bail
		// (returning nil so the message gets deleted — otherwise we
		// loop forever on the same broken message).
		log.Printf("worker: malformed message (deleting) body=%q: %v", truncate(raw, 256), err)
		return nil
	}

	if err := db.InsertIfAbsent(ctx, pool, r); err != nil {
		// Transient DB errors (connection lost, deadlock, etc.) are
		// retried via SQS redelivery. We just log and return the error.
		return fmt.Errorf("insert reservation %s: %w", r.ReservationID, err)
	}

	log.Printf("worker: inserted reservation_id=%s user=%s event=%d seats=%d",
		r.ReservationID, r.UserID, r.EventID, r.Seats)
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
}

func loadConfig() (*config, error) {
	c := &config{
		DatabaseURL:             getEnv("DATABASE_URL", ""),
		SQSQueueURL:             getEnv("SQS_QUEUE_URL", ""),
		SQSRegion:               getEnv("AWS_REGION", "us-east-1"),
		SQSEndpointURL:          getEnv("SQS_ENDPOINT_URL", ""),
		VisibilityTimeoutSeconds: atoiOr(getEnv("SQS_VISIBILITY_TIMEOUT_SECONDS", "30"), 30),
		WorkerLongPollSeconds:   atoiOr(getEnv("WORKER_LONG_POLL_SECONDS", "20"), 20),
	}
	if c.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	if c.SQSQueueURL == "" {
		return nil, errors.New("SQS_QUEUE_URL is required")
	}
	return c, nil
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
	_ = log.Printf
	_ = time.Now
	_ = aws.Config{}
)