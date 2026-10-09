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
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/emmitt-k/ticket-deal/internal/api"
	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/config"
	"github.com/emmitt-k/ticket-deal/internal/metrics"
	"github.com/emmitt-k/ticket-deal/internal/queue"
	"github.com/emmitt-k/ticket-deal/internal/redis"
	"github.com/emmitt-k/ticket-deal/internal/tracing"
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

	// ── OpenTelemetry tracing ─────────────────────────────────────
	//
	// Init must happen BEFORE the HTTP server starts so that
	// otelhttp.Middleware below can intercept the very first request.
	// The shutdown func is registered with the SIGINT/SIGTERM handler
	// so the last few spans don't get lost on Ctrl+C.
	tracingShutdown, err := tracing.Init(context.Background(), "api")
	if err != nil {
		log.Printf("api: tracing init failed (continuing without traces): %v", err)
	}

	// ── Redis client (shared by handlers and drainer) ──────────────
	rdb := redis.NewClient(redis.Config{
		Addr: cfg.Redis.Addr,
		DB:   cfg.Redis.DB,
	})
	defer rdb.Close()
	if err := redis.Ping(context.Background(), rdb); err != nil {
		return err // fail fast if Redis is unreachable
	}
	// Auto-instrument every Redis call with a span (SET/GET/EVALSHA/etc.).
	// Without this, redis.ReserveSeat would be a black box — you see the
	// outer HTTP span but not the 3ms Lua call inside.
	if err := redisotel.InstrumentTracing(rdb); err != nil {
		log.Printf("api: redisotel instrument tracing failed: %v", err)
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

	// ── Metrics state updater ────────────────────────────────────
	//
	// Refreshes the redis_seats_available and waiting_room_queue_length
	// gauges every 5s by polling Redis. The hot path uses the Lua
	// script (atomic), but those gauges need someone to actually
	// read Redis to populate them — Prometheus can only expose what
	// is Set() in code.
	//
	// Event ID is configurable via METRICS_EVENT_ID so the dashboard
	// can be pointed at a different event without rebuilding.
	metricsEventID := parseInt64Or(os.Getenv("METRICS_EVENT_ID"), 1)
	metrics.StartStateUpdater(
		drainerCtx,
		&redisSeatsAdapter{rdb: rdb},
		&redisQueueAdapter{rdb: rdb},
		metricsEventID,
		5*time.Second,
	)

	// ── HTTP router ────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Wrap every route in a middleware that records HTTP request
	// counts and latency for Prometheus. Must be added before any
	// routes are registered.
	r.Use(metricsMiddleware("api"))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		apiutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Prometheus scrape endpoint. Standard /metrics path, no auth
	// (Prometheus runs on the same Docker network; in production,
	// this would be network-firewalled or behind a sidecar).
	r.Method(http.MethodGet, "/metrics", metrics.Handler())

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
	// otelhttp.NewHandler wraps the router so every incoming request
	// becomes a span named "HTTP <method> <path>". If the upstream
	// caller sent a `traceparent` header, this span becomes a child of
	// the upstream trace; otherwise it's a root span.
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           otelhttp.NewHandler(r, "api"),
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

	// Flush any buffered spans before the process exits. Without this
	// the last 1-2 seconds of spans are silently dropped (the batch
	// processor hasn't fired yet).
	if tracingShutdown != nil {
		traceCtx, traceCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = tracingShutdown(traceCtx)
		traceCancel()
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

// metricsMiddleware returns a chi-compatible middleware that records
// HTTP request counts and latency to the shared Prometheus metrics.
// `service` is a label so the same metric can be aggregated across
// services in PromQL.
func metricsMiddleware(service string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			// Wrap the ResponseWriter so we can capture the status code.
			ww := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(ww, r)

			// Don't record /metrics itself — would cause a feedback
			// loop where each scrape increments its own counter.
			if r.URL.Path == "/metrics" {
				return
			}

			path := r.URL.Path
			status := http.StatusText(ww.status)
			metrics.HTTPRequestsTotal.WithLabelValues(service, path, r.Method, status).Inc()
			metrics.HTTPRequestDurationSeconds.WithLabelValues(service, path).Observe(time.Since(start).Seconds())
		})
	}
}

// statusRecorder wraps http.ResponseWriter to capture the status code
// written by downstream handlers, since the stdlib doesn't expose it
// by default. Default is 200 (the stdlib's default).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// ── Metrics state adapters ─────────────────────────────────────────
//
// These wrap the redis.Client to satisfy the
// metrics.SeatsAvailableGetter / metrics.QueueSizeGetter interfaces
// without putting a hard dependency on internal/redis in the metrics
// package (which would be circular for the api binary).

type redisSeatsAdapter struct {
	rdb *redis.Client
}

func (a *redisSeatsAdapter) GetAvailableSeats(ctx context.Context, eventID int64) (int, error) {
	return redis.AvailableSeats(ctx, a.rdb, eventID)
}

type redisQueueAdapter struct {
	rdb *redis.Client
}

func (a *redisQueueAdapter) GetQueueSize(ctx context.Context, eventID int64) (int, error) {
	return redis.QueueSize(ctx, a.rdb, eventID)
}

// parseInt64Or parses a string as int64, falling back to def on
// missing or unparseable input. Used for env-var config.
func parseInt64Or(s string, def int64) int64 {
	if s == "" {
		return def
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return v
}
