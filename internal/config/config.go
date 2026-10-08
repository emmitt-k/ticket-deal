// Package config loads and validates runtime configuration for the API
// server. All values are validated at Load time so the caller can fail
// fast at startup, before any goroutines, pools, or sockets are open.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"

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
