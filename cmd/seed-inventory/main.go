// Command seed-inventory copies each event's initial_inventory from
// Postgres into Redis under the canonical inventory key. This is the
// "DB → Redis at-rest" bootstrap that the API's reserve handler
// expects to be done before any /reserve traffic.
//
// Before this command existed, the Quick Start README told new
// developers to run a redis-cli SET by hand. That was easy to forget
// and produced confusing 404 event_not_found errors on the first
// POST /reserve. Now Quick Start just says "make seed-inventory" (or
// `go run ./cmd/seed-inventory`).
//
// Idempotency: every run is a hard SET (not INCR), so re-running
// resets the counter to initial_inventory. That's the right behavior
// for a dev tool: it mirrors the "drop time" pattern where an admin
// re-seats the venue and the counter is wiped before the new drop.
//
// Production note: this command is *only* safe to run while no holds
// are outstanding. Running it during a live drop would silently
// destroy the in-flight inventory counter. Production re-seeding
// should use a separate "admin reset" workflow with hold draining —
// not this command.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/joho/godotenv"

	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := run(); err != nil {
		log.Fatalf("seed-inventory: %v", err)
	}
}

func run() error {
	// Same dev-ergonomics shim as cmd/worker and cmd/expiration-watcher.
	// In production, env vars come from the orchestrator (k8s, ECS, etc.)
	// and .env doesn't exist — that's fine, the error is ignored.
	_ = godotenv.Load()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required (set it in .env or your shell)")
	}
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// ── Postgres: open a tiny pool, just enough for the one SELECT ──
	pool, err := db.NewPool(ctx, db.Config{
		DSN:      databaseURL,
		MaxConns: 2,
		MinConns: 1,
	})
	if err != nil {
		return fmt.Errorf("open DB pool: %w", err)
	}
	defer pool.Close()

	// ── Redis: client + ping for fail-fast ─────────────────────────
	rdb := redis.NewClient(redis.Config{Addr: redisAddr})
	defer rdb.Close()
	if err := redis.Ping(ctx, rdb); err != nil {
		return fmt.Errorf("ping Redis: %w", err)
	}

	// ── Read every event from Postgres ─────────────────────────────
	rows, err := pool.Query(ctx,
		`SELECT id, name, initial_inventory FROM events ORDER BY id`)
	if err != nil {
		return fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	// ── Write each inventory counter ───────────────────────────────
	count := 0
	for rows.Next() {
		var (
			id   int64
			name string
			inv  int
		)
		if err := rows.Scan(&id, &name, &inv); err != nil {
			return fmt.Errorf("scan event row: %w", err)
		}
		key := redis.InventoryKey(id) // canonical: "inventory:event:<id>"
		if err := rdb.Set(ctx, key, inv, 0).Err(); err != nil {
			return fmt.Errorf("redis SET %s = %d: %w", key, inv, err)
		}
		log.Printf("seed: %s = %d (event %d: %q)", key, inv, id, name)
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate event rows: %w", err)
	}

	log.Printf("seed: done — %d event(s) seeded into Redis (addr=%s)", count, redisAddr)
	return nil
}
