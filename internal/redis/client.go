// Package redis wraps the connection-pooled go-redis client and the
// Lua scripts that drive the hot path.
//
// Why Lua? Redis runs EVAL on a single thread — the script body
// executes atomically with respect to every other client. That gives
// us the seat-locking guarantee without optimistic-locking retries
// or WATCH/MULTI ceremony. See docs/architecture.md §4, §6.
package redis

import (
	"context"
	"time"

	redisclient "github.com/redis/go-redis/v9"
)

// Config holds the connection parameters.
type Config struct {
	Addr string // "localhost:6379"
	DB   int    // default 0
}

// NewClient builds a connection-pooled Redis client.
//
// PoolSize = 100 is generous for a 1000-concurrent Lua race — pool
// acquisition happens concurrently, and each EVAL completes in ~50µs
// so the same goroutine returns the conn to the pool within ~ms.
//
// Caller owns the client; must call .Close() at shutdown to drain
// in-flight EVALs and close TCP connections cleanly.
func NewClient(cfg Config) *redisclient.Client {
	return redisclient.NewClient(&redisclient.Options{
		Addr:         cfg.Addr,
		DB:           cfg.DB,
		PoolSize:     100,
		MinIdleConns: 5,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
}

// Ping checks connectivity. Use at startup to fail fast — a misconfigured
// Redis is the kind of bug you want to learn about *before* serving traffic.
func Ping(ctx context.Context, c *redisclient.Client) error {
	return c.Ping(ctx).Err()
}
