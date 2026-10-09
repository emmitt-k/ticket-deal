// Command expiration-watcher is the event-driven side of Phase 7's
// belt-and-suspenders expiry design. It subscribes to Redis keyspace
// notifications (__keyevent@0__:expired) and runs the compensating
// "mark DB row EXPIRED + INCRBY inventory" the instant a hold key
// fires its TTL.
//
// The DB-driven sweep (cmd/worker) is the safety net for cases the
// watcher misses: Redis drop, watcher crash, race against worker
// INSERT. Together they guarantee the seat eventually returns to
// inventory within at most one sweep interval (60 s).
//
// Redis must be started with --notify-keyspace-events Ex for any of
// this to work. docker-compose.yml already configures that.
//
// Hot path:
//
//	hold:event:<id>:user:<user>  TTL expires
//	    → __keyevent@0__:expired fires
//	    → RunWatcher parses the key
//	    → expire.Compensate transitions DB row + INCRBYs inventory
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
	"syscall"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/extra/redisotel/v9"

	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/expire"
	"github.com/emmitt-k/ticket-deal/internal/logging"
	"github.com/emmitt-k/ticket-deal/internal/metrics"
	"github.com/emmitt-k/ticket-deal/internal/redis"
	"github.com/emmitt-k/ticket-deal/internal/tracing"
)

func main() {
	logging.Init(logging.Config{
		Level:   logging.LevelFromEnv(),
		Format:  logging.FormatFromEnv(),
		Service: "expiration-watcher",
		Version: os.Getenv("SERVICE_VERSION"),
	})
	if err := run(); err != nil {
		slog.Error("expiration-watcher startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	slog.Info("expiration-watcher starting",
		"redis", cfg.RedisAddr,
		"db", redactDSN(cfg.DatabaseURL),
	)

	// ── OpenTelemetry tracing ─────────────────────────────────
	tracingShutdown, err := tracing.Init(context.Background(), "expiration-watcher")
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
	pool, err := db.NewPool(ctx, db.Config{
		DSN:      cfg.DatabaseURL,
		MaxConns: 3,
		MinConns: 1,
		Tracer:   otelpgx.NewTracer(),
	})
	if err != nil {
		return fmt.Errorf("open DB pool: %w", err)
	}
	defer pool.Close()
	slog.Info("DB pool open", "max", 3, "min", 1)

	// ── Redis client ───────────────────────────────────────────
	rdb := redis.NewClient(redis.Config{Addr: cfg.RedisAddr})
	defer rdb.Close()
	if err := redis.Ping(ctx, rdb); err != nil {
		return fmt.Errorf("ping Redis: %w", err)
	}
	if err := redisotel.InstrumentTracing(rdb); err != nil {
		slog.Warn("redisotel instrument tracing failed", "error", err)
	}
	slog.Info("Redis ping OK")

	// ── Metrics HTTP server (Prometheus scrape target) ────────────
	//
	// The expiration-watcher has no inbound HTTP — it's purely
	// event-driven (Redis keyspace notifications). Spin up a tiny
	// server on cfg.MetricsAddr (default :8083) just for /metrics.
	metricsAddr := getEnv("METRICS_ADDR", ":8083")
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

	// ── Watcher (blocks until ctx canceled) ────────────────────
	//
	// RunWatcher returns ctx.Err() on graceful shutdown. We log
	// non-canceled errors so the operator sees them.
	if err := expire.RunWatcher(ctx, rdb, pool); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("expire watcher: %w", err)
	}
	slog.Info("clean shutdown")
	return nil
}

// ── Config loading ─────────────────────────────────────────────────────────

type config struct {
	RedisAddr   string
	DatabaseURL string
}

func loadConfig() (*config, error) {
	// Dev ergonomics: load .env so `./bin/expiration-watcher` works
	// without `set -a; source .env; set +a` in the shell. The error
	// is ignored because production runs inject real env vars
	// (Kubernetes, ECS, systemd) and don't have a .env file.
	// Matches internal/config/config.go:95 for the API and the same
	// pattern in cmd/worker/main.go.
	_ = godotenv.Load()

	c := &config{
		RedisAddr:   getEnv("REDIS_ADDR", "localhost:6379"),
		DatabaseURL: getEnv("DATABASE_URL", ""),
	}
	if c.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	return c, nil
}

// ── Tiny env helpers (watcher-local; not shared with internal/config) ──────

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// redactDSN hides the password in a Postgres URL for log lines.
// Duplicated from cmd/worker — small enough that a shared helper
// package isn't worth it.
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