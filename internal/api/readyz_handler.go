// Package api — readyz_handler.go: GET /readyz — deep dependency check.
//
// Layer 1 item 4 of docs/product-roadmap.md. The split between /healthz
// (process is alive) and /readyz (deps reachable) is the standard
// K8s pattern: a failed Postgres should kick the pod out of the load
// balancer's ready set without triggering a restart loop.
//
// Three checks run in parallel with a per-check timeout:
//
//   - Redis    → rdb.Ping(ctx)
//   - Postgres → pool.Ping(ctx)        (pgx has a built-in 1-roundtrip Ping)
//   - SQS      → publisher.Ping(ctx)   (GetQueueAttributes; commit 1 added it)
//
// Per the spec:
//
//   1. Each check has a short timeout (~500ms). Long enough for a
//      healthy dep on a cold day, short enough that the K8s readiness
//      probe (typically 1 s) responds before our check times out.
//   2. Checks run in PARALLEL so one slow dep doesn't block the others.
//   3. Response is a JSON object naming each check + a top-level
//      "ready" boolean. 200 if true, 503 if false.
//
// Dev-mode SQS: when main.go hasn't wired a queue (LogPublisher is
// the publisher), the SQS check is nil → handler reports sqs:
// "not_configured" and treats that as healthy. Local API still
// reports ready=true, which makes manual testing / dev loops
// much nicer than a permanent 503.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
)

// ReadinessChecks carries the three functions the handler invokes
// in parallel. Each function MUST honour the caller's context — the
// handler wraps every call in a 500ms-bounded context so a slow dep
// never blocks the readiness response past the K8s probe deadline.
//
// SQS may be nil, meaning "not configured" (dev mode with
// LogPublisher). The handler reports sqs: "not_configured" and
// treats it as healthy.
type ReadinessChecks struct {
	Redis    func(ctx context.Context) error
	Postgres func(ctx context.Context) error
	SQS      func(ctx context.Context) error // nil = not_configured
}

// perCheckTimeout is the upper bound on each individual check.
// Chosen per docs/product-roadmap.md §4 ("short timeout ~500ms");
// also chosen so two checks don't add up to more than 1 s (typical
// K8s readinessProbe timeoutSeconds default).
const perCheckTimeout = 500 * time.Millisecond

// checkResult is what each parallel goroutine returns. status is
// pre-computed for the "not_configured" case (where the goroutine
// doesn't call a Ping function at all); for the live checks it's
// computed by formatStatus from err in the consumer side.
type checkResult struct {
	name   string
	status string // pre-computed ("not_configured"); empty when err drives the status
	err    error
}

// readyZResponse is the JSON body returned both on 200 and 503.
// Field order matches the docs/product-roadmap.md example, so the
// wire-format is stable regardless of which checks ran. (encoding/json
// marshals in struct-declaration order.)
type readyZResponse struct {
	Redis    string `json:"redis"`
	Postgres string `json:"postgres"`
	SQS      string `json:"sqs"`
	Ready    bool   `json:"ready"`
}

// ReadyZHandler returns the http.HandlerFunc for GET /readyz.
//
// Wiring responsibilities (in cmd/api/main.go):
//
//   Redis check:    func(ctx) error { return redis.Ping(ctx, rdb) }
//   Postgres check: func(ctx) error { return pool.Ping(ctx) }
//   SQS check:      publisher.Ping  (or nil for dev/LogPublisher mode)
//
// Returns 200 + `{ready: true, ...}` when every check is "ok" or
// "not_configured". Returns 503 + `{ready: false, ...}` otherwise.
func ReadyZHandler(checks ReadinessChecks) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "readyz.handle")
		defer span.End()
		r = r.WithContext(ctx)

		// ── Parallel execution via 3 buffered goroutines ──────────────
		//
		// Channel is buffered to 3 so the goroutines never block
		// even if the request goroutine is slow. The collector loop
		// reads exactly 3 times — guaranteed not to deadlock.
		out := make(chan checkResult, 3)

		go func() {
			c, cancel := context.WithTimeout(r.Context(), perCheckTimeout)
			defer cancel()
			err := safeCheck("redis", checks.Redis, c)
			out <- checkResult{name: "redis", err: err}
		}()

		go func() {
			c, cancel := context.WithTimeout(r.Context(), perCheckTimeout)
			defer cancel()
			err := safeCheck("postgres", checks.Postgres, c)
			out <- checkResult{name: "postgres", err: err}
		}()

		go func() {
			if checks.SQS == nil {
				// Dev mode / no queue configured → not a failure,
				// but signal it in the response so ops can tell the
				// difference between "SQS ping ok" and "SQS not
				// configured". Both are healthy for K8s purposes.
				out <- checkResult{name: "sqs", status: "not_configured"}
				return
			}
			c, cancel := context.WithTimeout(r.Context(), perCheckTimeout)
			defer cancel()
			err := safeCheck("sqs", checks.SQS, c)
			out <- checkResult{name: "sqs", err: err}
		}()

		// ── Aggregate ─────────────────────────────────────────────────
		resp := readyZResponse{}
		for i := 0; i < 3; i++ {
			res := <-out
			switch res.name {
			case "redis":
				resp.Redis = formatStatus(res.status, res.err)
			case "postgres":
				resp.Postgres = formatStatus(res.status, res.err)
			case "sqs":
				resp.SQS = formatStatus(res.status, res.err)
			}
		}

		resp.Ready = isHealthy(resp.Redis) && isHealthy(resp.Postgres) && isHealthy(resp.SQS)

		// Span attributes: per-check outcomes + aggregate.
		span.SetAttributes(
			attribute.Bool("readyz.ready", resp.Ready),
			attribute.String("readyz.redis", resp.Redis),
			attribute.String("readyz.postgres", resp.Postgres),
			attribute.String("readyz.sqs", resp.SQS),
		)

		// 503 on any failure; otherwise 200. K8s readinessProbe matches
		// on HTTP status by default (200-399 = ready), so this maps
		// directly to a Service endpoint inclusion decision.
		statusCode := http.StatusOK
		if !resp.Ready {
			statusCode = http.StatusServiceUnavailable
			span.SetStatus(codes.Error, "not_ready")
		}

		// Log "not ready" at WARN — it's the kind of thing ops wants
		// in Loki even when K8s is already reacting to the 503.
		if !resp.Ready {
			slog.WarnContext(r.Context(), "readyz: not ready",
				"redis", resp.Redis,
				"postgres", resp.Postgres,
				"sqs", resp.SQS,
			)
		}

		apiutil.WriteJSON(w, statusCode, resp)
	}
}

// safeCheck runs a check function, treating a nil func as
// "not_configured" and any panic as "error". The panic recovery is
// defensive — the checks we wire (redis.Ping, pool.Ping, publisher.Ping)
// shouldn't panic, but a nil-deref in the SDK during transient
// network errors is something we don't want to take down the
// readiness handler entirely.
func safeCheck(name string, fn func(ctx context.Context) error, ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("readyz: %s check panicked: %v", name, r)
		}
	}()
	if fn == nil {
		return errNotConfigured
	}
	return fn(ctx)
}

// errNotConfigured is a private sentinel — only safeCheck / the
// not_configured goroutine in ReadyZHandler reach it. External callers
// don't depend on this value.
var errNotConfigured = errors.New("readyz: check not configured")

// formatStatus renders a per-check status string. Three inputs:
//
//   - precomputed ("not_configured") → pass through verbatim
//   - err == nil                     → "ok"
//   - err == context.DeadlineExceeded (or wraps it) → "timeout after 500ms"
//   - other error                    → "error: <msg>"
//
// Keeping the formatting here means the SQS goroutine's special
// "not_configured" handling and the time-out detection (which is the
// whole point of the bounded context) live in one place.
func formatStatus(precomputed string, err error) string {
	if precomputed != "" {
		return precomputed
	}
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errNotConfigured) {
		if errors.Is(err, errNotConfigured) {
			return "not_configured"
		}
		return fmt.Sprintf("timeout after %dms", perCheckTimeout.Milliseconds())
	}
	return "error: " + err.Error()
}

// isHealthy reports whether a per-check status counts as healthy for
// the aggregate "ready" boolean. Spec says the only two healthy
// non-OK outcomes are "ok" itself and the dev-mode "not_configured".
// Any other string (timeout, error, future "degraded", etc.) -> unhealthy.
func isHealthy(status string) bool {
	return status == "ok" || status == "not_configured"
}
