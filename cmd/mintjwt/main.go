// Command mintjwt issues a short-lived HS256 JWT for smoke-testing the
// /api/tickets/reserve endpoint (or anything else that needs an
// auth.Middleware-validated token). It's a developer convenience — the
// real auth flow is POST /api/tickets/enter, which both rate-limits
// (Phase 4) and mints a 2-minute token scoped to (user_id, event_id).
//
// Before this command existed, the equivalent helper lived at
// scripts/mintjwt.go with the JWT secret hardcoded in source. That
// was safe-ish (only the dev .env secret, not a prod key) but gross
// — committing a secret-shaped string invites accidents. This
// version reads JWT_SECRET from the environment (godotenv loads
// .env in dev), refuses to run if the secret is missing, and never
// embeds any credential in the binary.
//
// Usage:
//
//	go run ./cmd/mintjwt
//	# mints for user="alice" (default), event=42 (default)
//
//	USER_ID=smoke-user EVENT_ID=1 go run ./cmd/mintjwt
//	# mints a 2-minute token for smoke-user on event 1
//
//	JWT=$(USER_ID=u1 EVENT_ID=1 go run ./cmd/mintjwt)
//	curl -H "Authorization: Bearer $JWT" http://localhost:8080/api/tickets/reserve
//
// TTL defaults to 120 s (matching the auth.Middleware cap set by
// POST /api/tickets/enter). Override with TTL_SECONDS=N for longer
// or shorter windows.
package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"

	"github.com/emmitt-k/ticket-deal/internal/auth"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("mintjwt: %v", err)
	}
}

func run() error {
	// Dev ergonomics — same shim as cmd/worker, cmd/expiration-watcher,
	// cmd/seed-inventory. In production, env comes from the
	// orchestrator and .env doesn't exist; the error is ignored.
	_ = godotenv.Load()

	// ── Required: JWT_SECRET (≥32 bytes per internal/config) ───────
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return errors.New(
			"JWT_SECRET is required (set it in .env or your shell — see .env.example)")
	}
	if len(secret) < 32 {
		return fmt.Errorf(
			"JWT_SECRET must be at least 32 bytes (got %d); "+
				"generate one with: openssl rand -hex 32",
			len(secret))
	}

	// ── Optional: USER_ID (default alice), EVENT_ID (default 42) ──
	userID := getEnv("USER_ID", "alice")
	eventIDStr := getEnv("EVENT_ID", "42")
	eventID, err := strconv.ParseInt(eventIDStr, 10, 64)
	if err != nil || eventID <= 0 {
		return fmt.Errorf("EVENT_ID must be a positive integer (got %q)", eventIDStr)
	}

	// ── Optional: TTL_SECONDS (default 120) ───────────────────────
	ttl := 120 * time.Second
	if raw := os.Getenv("TTL_SECONDS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return fmt.Errorf("TTL_SECONDS must be a positive integer (got %q)", raw)
		}
		ttl = time.Duration(n) * time.Second
	}

	// ── Issue the token via the SAME code path the API uses ──────
	// No shortcuts, no re-implementation — drift would let our test
	// token out-evolve the real one.
	tok, err := auth.Issue(userID, eventID, []byte(secret), ttl)
	if err != nil {
		return fmt.Errorf("auth.Issue: %w", err)
	}

	// Print ONLY the token to stdout so it composes in shell pipes
	// (e.g. `JWT=$(go run ./cmd/mintjwt)`). Status / hints go to
	// stderr so they don't pollute the captured value.
	fmt.Fprintln(os.Stderr,
		fmt.Sprintf("mintjwt: issued for user=%s event=%d ttl=%s (len=%d)",
			userID, eventID, ttl, len(tok)))
	fmt.Print(tok) // no trailing newline — pipe-friendly
	return nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
