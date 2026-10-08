// Command api starts the Mini-Ticketmaster HTTP server.
//
// Responsibilities (Phase 5 scope):
//   - Load and validate config (fail fast on missing/short JWT_SECRET)
//   - Wire the chi router with logger + recoverer middleware
//   - Mount the public /healthz endpoint
//   - Mount the /api/tickets/* routes, with auth.Middleware() guarding
//     /reserve from day 1
//   - /enter (Phase 4) — IP bucket + event token bucket → JWT or queue
//   - /queue (Phase 4) — SSE stream of queue position events
//   - /reserve (Phase 5) — JWT → Lua reserve → publisher (SQS stub)
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

	"github.com/emmitt-k/ticket-deal/internal/api"
	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/config"
	"github.com/emmitt-k/ticket-deal/internal/queue"
	"github.com/emmitt-k/ticket-deal/internal/redis"
	"github.com/emmitt-k/ticket-deal/internal/waitingroom"
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
	log.Printf("api: config loaded (addr=%s, jwt_secret=%d bytes, redis=%s)",
		cfg.Addr, len(cfg.JWTSecret), cfg.Redis.Addr)

	// ── Redis client (shared by handlers and drainer) ──────────────
	rdb := redis.NewClient(redis.Config{
		Addr: cfg.Redis.Addr,
		DB:   cfg.Redis.DB,
	})
	defer rdb.Close()
	if err := redis.Ping(context.Background(), rdb); err != nil {
		return err // fail fast if Redis is unreachable
	}

	// ── Background drainer (promotes queued users as tokens refill) ─
	drainerCfg := waitingroom.DrainerConfig{
		RDB: rdb,
		WaitRoom: redis.WaitRoomConfig{
			Capacity:        cfg.WaitRoom.Capacity,
			RefillRate:      cfg.WaitRoom.RefillRate,
			QueueTTLSeconds: cfg.WaitRoom.QueueTTLSeconds,
		},
		JWTSecret:   cfg.JWTSecret,
		TickInterval: 100 * time.Millisecond,
	}
	// drainerCtx is derived from the server ctx so shutting down the
	// server cancels the drainer automatically.
	drainerCtx, drainerCancel := context.WithCancel(context.Background())
	defer drainerCancel()
	waitingroom.StartDrainer(drainerCtx, drainerCfg)

	// ── HTTP router ────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		apiutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Phase 4 + 5 routes — the waiting room and reservation
	r.Route("/api/tickets", func(r chi.Router) {
		r.Post("/enter", waitingroom.EnterHandler(*cfg, rdb))
		r.Get("/queue", waitingroom.QueueSSEHandler(rdb))
		// Phase 5/6: /reserve (JWT-protected) — Lua reserve → SQS publish
		r.With(auth.Middleware(cfg.JWTSecret)).Post("/reserve",
			api.ReserveHandler(rdb, buildPublisher(*cfg),
				time.Duration(cfg.ReserveHoldTTL)*time.Second))
	})

	// ── HTTP server ────────────────────────────────────────────────
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// ─── Shutdown plumbing ────────────────────────────────────────
	// Hook our "please stop" signals (Ctrl+C, `docker stop`'s
	// SIGTERM) into a context. When one of those signals arrives,
	// Go marks this context as "cancelled" — anything waiting on
	// it (the select{} below) will wake up.
	//
	// The `defer stop()` is the tidy-up move: when main() returns,
	// we tell the OS "we're done listening for signals now" so we
	// don't leave a zombie handler.
	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A `chan error` is a one-way inbox that goroutines can drop
	// messages into. The `1` is the *buffer size* — how many
	// messages it can hold before the sender has to wait. We only
	// ever send at most one (the first fatal error from the
	// server), so 1 is plenty.
	errCh := make(chan error, 1)

	// Run the HTTP server in the *background* (a goroutine).
	// Why? Because ListenAndServe is what Go calls "blocking" —
	// once you call it, the function doesn't return until the
	// server stops. If we called it in the foreground, we'd be
	// stuck there forever and never get to the select{} below.
	// Pushing it into a goroutine lets the main function keep
	// going and watch for shutdown signals at the same time.
	go func() {
		log.Printf("api: listening on %s", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			// ErrServerClosed is what Shutdown() returns from inside
			// ListenAndServe when WE asked it to stop — that one
			// we ignore (it's expected). Anything else is a real
			// problem (e.g. port in use), so we mail it back to
			// main() via errCh.
			errCh <- err
		}
	}()

	// Now we wait. `select` is Go's "wait on multiple things at
	// once" statement. Imagine you're sitting with two phones:
	//   - Phone A rings  → user asked us to stop (ctx.Done())
	//   - Phone B rings  → the server crashed (errCh)
	// Whichever rings first, we handle that case. The other
	// branch is just dropped. Until one rings, we're idle here.
	select {
	case <-ctx.Done():
		// User pressed Ctrl+C or `docker stop` was sent.
		log.Printf("api: shutdown signal received, draining...")
	case err := <-errCh:
		// Server failed to start (or crashed). Propagate so
		// main() can return non-zero.
		return err
	}

	// Politely ask the server to stop. Shutdown does two things:
	//   1. Stop accepting new connections immediately
	//   2. Wait for in-flight requests to finish naturally
	// We give it 5 seconds (shutCtx's timeout). Anything still
	// running after that gets cut off — that's the tradeoff for
	// not hanging forever. 5s is the sweet spot: long enough for
	// most requests to finish, short enough that orchestrators
	// (k8s, ECS) don't kill us first.
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return err
	}
	log.Printf("api: clean shutdown complete")
	return nil
}

// buildPublisher constructs the Phase 6 SQS publisher wired into the
// /reserve handler. The same binary works against real AWS (EndpointURL
// is empty) and local ElasticMQ (EndpointURL = http://localhost:9324).
//
// In dev environments where SQS_QUEUE_URL is unset (e.g. someone
// running the API before ElasticMQ is up), we fall back to
// api.LogPublisher, which logs the message body instead of sending
// it to SQS. This keeps the API runnable for handler-level work
// without requiring the worker side to be configured.
func buildPublisher(cfg config.Config) api.ReservationPublisher {
	if cfg.SQS.QueueURL == "" {
		log.Printf("api: SQS_QUEUE_URL not set — using LogPublisher (messages will not reach worker)")
		return api.LogPublisher{}
	}

	sqsClient, err := queue.NewSQSClient(context.Background(), queue.AWSConfig{
		Region:      cfg.SQS.Region,
		EndpointURL: cfg.SQS.EndpointURL,
		QueueURL:    cfg.SQS.QueueURL,
	})
	if err != nil {
		// Fail-fast: if we can't reach SQS at startup, the /reserve
		// hot path is broken. Better to crash now than to discover
		// it after the first batch of reservations.
		log.Fatalf("api: build SQS client: %v", err)
	}

	log.Printf("api: SQS publisher wired (region=%s, endpoint=%q, queue=%s)",
		cfg.SQS.Region, cfg.SQS.EndpointURL, cfg.SQS.QueueURL)
	return queue.NewReservationAPIPublisher(queue.NewSQSPublisher(sqsClient, cfg.SQS.QueueURL))
}
