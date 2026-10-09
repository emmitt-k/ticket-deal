// Package db owns the Postgres connection pool and the queries
// the worker uses to write the source-of-truth row for each
// reservation.
//
// The API server does NOT touch this package — Postgres is off the
// hot path. Only the worker uses it.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is the connection pool used by the worker. Wrapping pgxpool.Pool
// behind a named type lets us add worker-specific helpers (CloseWithCtx,
// health checks, etc.) without leaking pgx into callers.
type Pool struct {
	*pgxpool.Pool
}

// Config holds the DSN and pool-tuning knobs. We default sane values
// for MaxConns / MinConns because the worker is the sole Postgres
// client in this system — over-tuning would just be theatre.
type Config struct {
	// DSN is a Postgres connection URL, e.g.
	//   postgres://tickets:tickets@localhost:5432/tickets?sslmode=disable
	DSN string
	// MaxConns caps the connection pool size. Default 10.
	MaxConns int32
	// MinConns keeps at least N idle connections warm. Default 1.
	MinConns int32
	// HealthCheckPeriod is how often the pool pings idle conns.
	// Default 1 min.
	HealthCheckPeriod time.Duration
	// Tracer installs the otelpgx tracer on the pgxpool. If nil, a
	// tracer using the global OTel TracerProvider is installed (which
	// is a no-op if InitTracing was never called). Pass your own
	// `*otelpgx.Tracer` if you want to customize span attributes
	// (e.g. omit query text for PII reasons).
	Tracer *otelpgx.Tracer
}

// NewPool builds a pgxpool.Config, applies defaults, and opens a pool.
// The pool pings the DB on creation so callers fail fast if the DSN is
// wrong or the server is down — better to crash at startup than at
// first message.
func NewPool(ctx context.Context, cfg Config) (*Pool, error) {
	pgxCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("db: parse DSN: %w", err)
	}

	if cfg.MaxConns > 0 {
		pgxCfg.MaxConns = cfg.MaxConns
	} else {
		pgxCfg.MaxConns = 10
	}
	if cfg.MinConns > 0 {
		pgxCfg.MinConns = cfg.MinConns
	} else {
		pgxCfg.MinConns = 1
	}
	if cfg.HealthCheckPeriod > 0 {
		pgxCfg.HealthCheckPeriod = cfg.HealthCheckPeriod
	} else {
		pgxCfg.HealthCheckPeriod = time.Minute
	}

	// Install pgx tracer so every query gets a span. The default
	// tracer (when cfg.Tracer is nil) uses otel.GetTracerProvider(),
	// which returns a no-op until the OTel SDK is Init()'d. That makes
	// the pool safe to use in tests that don't bring up a collector.
	tracer := cfg.Tracer
	if tracer == nil {
		tracer = otelpgx.NewTracer()
	}
	pgxCfg.ConnConfig.Tracer = tracer

	pool, err := pgxpool.NewWithConfig(ctx, pgxCfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}

	// Fail fast on bad DSN / unreachable server. 5 s is a generous
	// budget for local Docker Postgres; bump if running on a busy CI.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping failed: %w", err)
	}

	return &Pool{Pool: pool}, nil
}