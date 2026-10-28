// Package config loads and validates runtime configuration for the API
// server. All values are validated at Load time so the caller can fail
// fast at startup, before any goroutines, pools, or sockets are open.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds every runtime knob the API server needs.
type Config struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// JWTSecret is the HS256 signing key. Must be at least 32 bytes so
	// the key space matches the algorithm's stated strength.
	JWTSecret []byte

	// Redis is the Redis connection used by the hot path (waiting room
	// handlers and drainer). Postgres is NOT on the hot path — workers
	// manage their own pool separately.
	Redis RedisConfig

	// IPLimit gates /enter before the event bucket. Hard reject (no queue)
	// for IPs that exceed the per-IP rate.
	IPLimit IPLimitConfig

	// WaitRoom is the per-event token bucket that drives the virtual
	// waiting room. Capacity = burst size; RefillRate = steady-state
	// users admitted per second.
	WaitRoom WaitRoomConfig

	// ReserveHoldTTL is how long a successful /reserve hold lives in
	// Redis before the seat auto-releases. Mirrors the 10-minute
	// payment window in the README's flow diagram.
	ReserveHoldTTL int

	// SQS is the SQS client config. The API publishes reservation
	// events here; the worker (cmd/worker) consumes them. EndpointURL
	// points at local ElasticMQ in dev, "" for prod (real SQS).
	SQS SQSConfig

	// Postgres is the database connection used by the API for
	// reservation lookups (Layer 1, GET /api/reservations/:id +
	// GET /api/me/reservations). Hot path (/reserve) does NOT
	// touch Postgres — only Redis. The worker writes reservation
	// rows into Postgres after SQS so the API's reads are at most
	// a few hundred ms behind Redis (the system of record during
	// sell-time).
	Postgres PostgresConfig

	// Stripe is the payment integration (Layer 2, item 7). It is
	// OPTIONAL: with no keys set, the feature is off and the Stripe
	// routes answer 503. The reservation core does not need Stripe,
	// and making a ticket server refuse to boot without a payment
	// account is a strange dependency.
	Stripe StripeConfig
}

// StripeConfig groups the Stripe knobs.
//
// The two secret values are validated together, and their ABSENCE is
// allowed while their half-presence is not:
//
//   - neither set   → the feature is off (Enabled = false)
//   - both set      → the feature is on, and the values are validated
//   - exactly one   → a configuration error, and Load() fails fast
//
// A half-configured Stripe is a bug, not a choice: it would mean an
// API that can create PaymentIntents but cannot verify webhooks (or
// the reverse), which is exactly the state where payments get
// silently stuck.
type StripeConfig struct {
	// SecretKey is the Stripe API key (sk_test_… / sk_live_…).
	SecretKey string
	// WebhookSecret is the signing secret for THIS endpoint + mode
	// (whsec_…). Never logged, never returned to a client.
	WebhookSecret string
	// PublishableKey is the client-side key (pk_test_… / pk_live_…).
	// It is safe to expose to a browser — the client needs it to
	// confirm a payment with Stripe Elements.
	PublishableKey string
	// EventsQueueURL is the SQS queue that carries Stripe webhook
	// events from the API to the stripe-worker. Separate from the
	// reservations queue: three queues, three concerns.
	EventsQueueURL string
	// Enabled is true when both secret values are present. When
	// false, the Stripe routes answer 503 rather than pretending to
	// work.
	Enabled bool
}

// PostgresConfig is the subset of db.Config the API server needs to
// dial Postgres. Mirrors the existing RedisConfig shape — small,
// explicit, no surprises.
type PostgresConfig struct {
	// DSN is the full libpq connection string, e.g.
	// "postgres://user:pass@host:5432/db?sslmode=disable".
	// Composed from POSTGRES_* env vars in Load() if not set
	// directly — prefer that path so prod can still inject a raw DSN.
	DSN string
}

// SQSConfig groups the AWS-side knobs both the API publisher and the
// worker consumer need. Kept distinct from queue.AWSConfig because the
// API only knows about SQS via config, not via the queue package's types.
type SQSConfig struct {
	Region      string // AWS region (e.g. "us-east-1")
	EndpointURL string // empty in prod; "http://localhost:9324" locally
	QueueURL    string // the reservations queue URL
}

// RedisConfig is the subset of go-redis Options needed by the API server.
type RedisConfig struct {
	Addr string
	DB   int
}

// WaitRoomConfig mirrors redis.WaitRoomConfig so config stays free of
// importing internal/redis (avoids an import cycle since redis already
// imports nothing from config — but keeping types local is cleaner here).
type WaitRoomConfig struct {
	// Capacity is the max tokens (burst size) per event.
	// E.g. 100 means up to 100 users can be admitted "instantly."
	Capacity int
	// RefillRate is tokens per second per event.
	// E.g. 10 means 10 users admitted per second once the burst is gone.
	RefillRate float64
	// QueueTTLSeconds is how long a queued user stays in the ZSET before
	// being auto-removed (prevents abandoned sessions from growing the queue forever).
	QueueTTLSeconds int
}

// IPLimitConfig mirrors iplimit.Config for the same reason as WaitRoomConfig.
type IPLimitConfig struct {
	// Capacity is the max tokens per IP (burst size).
	Capacity int
	// RefillRate is tokens per second per IP.
	RefillRate float64
}

// Load reads configuration from the process environment (after
// overlaying a .env file, if one is present in the working directory)
// and validates the result.
//
// The .env file is best-effort: it is ignored when missing because
// production containers typically inject env vars via Docker / k8s.
// All required values come from the OS environment.
func Load() (*Config, error) {
	// Best-effort .env overlay. ErrNotExist is normal in prod.
	_ = godotenv.Load()

	portStr := getEnv("API_PORT", "8080")
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("API_PORT must be a valid port (1-65535), got %q", portStr)
	}

	secret := os.Getenv("JWT_SECRET")
	if len(secret) < 32 {
		return nil, errors.New("JWT_SECRET must be at least 32 bytes (use `openssl rand -hex 32`)")
	}

	cfg := &Config{
		Addr:      ":" + strconv.Itoa(port),
		JWTSecret: []byte(secret),
		Redis: RedisConfig{
			Addr: getEnv("REDIS_ADDR", "localhost:6379"),
			DB:   atoiOr(getEnv("REDIS_DB", "0"), 0),
		},
		IPLimit: IPLimitConfig{
			Capacity:   atoiOr(getEnv("IP_LIMIT_CAPACITY", "10"), 10),
			RefillRate: atofOr(getEnv("IP_LIMIT_REFILL_RATE", "2"), 2.0),
		},
		WaitRoom: WaitRoomConfig{
			Capacity:        atoiOr(getEnv("WAIT_ROOM_CAPACITY", "100"), 100),
			RefillRate:      atofOr(getEnv("WAIT_ROOM_REFILL_RATE", "10"), 10.0),
			QueueTTLSeconds: atoiOr(getEnv("WAIT_ROOM_QUEUE_TTL", "300"), 300),
		},
		ReserveHoldTTL: atoiOr(getEnv("RESERVE_HOLD_TTL_SECONDS", "600"), 600),
		SQS: SQSConfig{
			Region:      getEnv("AWS_REGION", "us-east-1"),
			EndpointURL: getEnv("SQS_ENDPOINT_URL", ""),
			QueueURL:    getEnv("SQS_QUEUE_URL", ""),
		},
		Postgres: PostgresConfig{
			// Prefer an explicit DATABASE_URL if one is set — it can
			// encode sslmode, IAM-based authentication, or anything
			// else. Fall back to the POSTGRES_* family by composing
			// a DSN. The fallback shape matches what .env.example
			// documents.
			DSN: getEnv("DATABASE_URL", buildPostgresDSNFromEnv()),
		},
	}

	stripeSecret := os.Getenv("STRIPE_SECRET_KEY")
	stripeWebhookSecret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	switch {
	case stripeSecret == "" && stripeWebhookSecret == "":
		// Stripe is not configured. The feature is off and the Stripe
		// routes answer 503. This is deliberate — see StripeConfig's
		// doc comment.
	case stripeSecret == "" || stripeWebhookSecret == "":
		return nil, errors.New(
			"STRIPE_SECRET_KEY and STRIPE_WEBHOOK_SECRET must be set together (both present, or neither)")
	}
	if stripeSecret != "" {
		// Fail fast on a MALFORMED key, even though an absent key is
		// allowed. A key that cannot possibly work should be caught
		// at startup, not at the first /pay request.
		if !strings.HasPrefix(stripeSecret, "sk_") {
			return nil, errors.New("STRIPE_SECRET_KEY must look like sk_test_... or sk_live_...")
		}
		if !strings.HasPrefix(stripeWebhookSecret, "whsec_") {
			return nil, errors.New("STRIPE_WEBHOOK_SECRET must look like whsec_...")
		}
	}
	cfg.Stripe = StripeConfig{
		SecretKey:      stripeSecret,
		WebhookSecret:  stripeWebhookSecret,
		PublishableKey: getEnv("STRIPE_PUBLISHABLE_KEY", ""),
		EventsQueueURL: getEnv("STRIPE_EVENTS_QUEUE_URL", ""),
		Enabled:        stripeSecret != "",
	}

	return cfg, nil
}

// atoiOr parses s as int, returns def on error or empty string.
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

// atofOr parses s as float64, returns def on error or empty string.
func atofOr(s string, def float64) float64 {
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return v
}

// getEnv is defined at the bottom so all helpers are grouped together.
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// buildPostgresDSNFromEnv composes a libpq-style DSN from the
// POSTGRES_USER / POSTGRES_PASSWORD / POSTGRES_HOST / POSTGRES_PORT /
// POSTGRES_DB env vars (each with a default). Used as the fallback
// when DATABASE_URL isn't set — keeps the existing .env workflow
// working for tools that use the part-by-part form.
//
// Output: "postgres://user:pass@host:port/db?sslmode=disable"
// Empty parts fall back to safe defaults so a partially-set env
// still produces a working DSN.
func buildPostgresDSNFromEnv() string {
	user := getEnv("POSTGRES_USER", "tickets")
	pass := getEnv("POSTGRES_PASSWORD", "tickets")
	host := getEnv("POSTGRES_HOST", "localhost")
	port := getEnv("POSTGRES_PORT", "5432")
	db := getEnv("POSTGRES_DB", "tickets")
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", user, pass, host, port, db)
}
