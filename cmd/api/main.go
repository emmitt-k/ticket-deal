// Command api starts the Mini-Ticketmaster HTTP server.
//
// Responsibilities (Phase 3 scope):
//   - Load and validate config (fail fast on missing/short JWT_SECRET)
//   - Wire the chi router with logger + recoverer middleware
//   - Mount the public /healthz endpoint
//   - Mount the /api/tickets/* routes, with auth.Middleware() guarding
//     /reserve from day 1
//   - /enter and /reserve are stubs returning 501; their real handlers
//     land in Phase 4 and Phase 5
//   - Handle SIGINT/SIGTERM with a clean shutdown (5 s grace)
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/config"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := run(); err != nil {
		log.Fatalf("api: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log.Printf("api: config loaded (addr=%s, jwt_secret=%d bytes)",
		cfg.Addr, len(cfg.JWTSecret))

	r := chi.NewRouter()
	r.Use(middleware.Logger)   // formatted request log to stdout
	r.Use(middleware.Recoverer) // converts panics in handlers to 500

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		apiutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// /api/tickets/enter — public, implemented in Phase 4
	// /api/tickets/reserve — auth-protected, implemented in Phase 5
	r.Route("/api/tickets", func(r chi.Router) {
		r.Post("/enter", enterStub)
		r.With(auth.Middleware(cfg.JWTSecret)).Post("/reserve", reserveStub)
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second, // slow-loris protection
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("api: listening on %s", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Printf("api: shutdown signal received, draining...")
	case err := <-errCh:
		return err
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return err
	}
	log.Printf("api: clean shutdown complete")
	return nil
}

// ─────────────────────────────────────────────────────────────────────
// Stubs (replaced by real handlers in Phase 4 / Phase 5)
// ─────────────────────────────────────────────────────────────────────

// enterStub is the placeholder for the waiting-room admission handler.
// Phase 4 will: rate-limit by IP, check the per-event token bucket,
// enqueue or admit, then issue a JWT.
func enterStub(w http.ResponseWriter, _ *http.Request) {
	apiutil.WriteError(w, http.StatusNotImplemented, "not_implemented",
		"POST /api/tickets/enter lands in Phase 4 (waiting room)")
}

// reserveStub is the placeholder for the seat-locking handler. The
// middleware already verified the JWT and put claims in context, so
// downstream handlers can rely on auth.ClaimsFromContext(r.Context()).
func reserveStub(w http.ResponseWriter, r *http.Request) {
	if c := auth.ClaimsFromContext(r.Context()); c != nil {
		log.Printf("api: reserve stub (Phase 5 pending) reached with sub=%s event_id=%d",
			c.Subject, c.EventID)
	}
	apiutil.WriteError(w, http.StatusNotImplemented, "not_implemented",
		"POST /api/tickets/reserve lands in Phase 5 (Lua reserve)")
}
