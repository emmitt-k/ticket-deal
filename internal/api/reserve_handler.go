package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/metrics"
	"github.com/emmitt-k/ticket-deal/internal/queue"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// tracerName is the OTel "instrumentation library" identifier. Group
// your spans under a stable name so Jaeger can filter by who created
// them.
const tracerName = "github.com/emmitt-k/ticket-deal/internal/api"

var tracer = otel.Tracer(tracerName)

// ReserveHandler limits POST /api/tickets/reserve.
//
// Auth:        middleware has already verified the JWT and injected
//              *auth.Claims into the request context. We trust those
//              claims as the single source of identity — no body field
//              for user_id or event_id (they'd be attack vectors: a stale
//              token for event 1 plus a body saying event 2 would be
//              a privilege escalation if we cross-checked loosely).
//
// Body:        { "seats_requested": N }    (optional, default 1, max 10)
//
// Status codes:
//   200 { reservation_id, expires_in }                seat reserved
//   400 invalid_request                               bad seats_requested
//   401 missing_claims                                middleware not run (defensive)
//   404 event_not_found                              inventory key missing
//   409 sold_out                                      no seats remain
//   409 already_holding                               user already has one
//   500 internal_error                                Redis or marshal failure
//
// Phase 5 publishes the reservation event via ReservationPublisher
// (currently a log-only LogPublisher). Phase 6 swaps the publisher
// for a real SQS implementation without touching this handler.
func ReserveHandler(rdb *redis.Client, publisher ReservationPublisher, holdTTL time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ── 0. Open the handler-level span ────────────────────────
		//
		// otelhttp.NewHandler already created a "POST /api/tickets/reserve"
		// span for the whole request. This manual span sits *inside* that
		// one and groups the business-logic steps (claims → lua → publish)
		// so the waterfall shows the *flow*, not just the I/O.
		ctx, span := tracer.Start(r.Context(), "reserve.handle",
			trace.WithAttributes(
				attribute.String("reservation.user_id", ""), // filled in below
			),
		)
		defer span.End()
		// From here on, use ctx (not r.Context()) so the spans below
		// become children of reserve.handle.
		r = r.WithContext(ctx)

		// ── 1. Pull verified claims from context ──────────────────
		claims := auth.ClaimsFromContext(r.Context())
		if claims == nil {
			// Middleware should have rejected this request. If we
			// reach this branch, someone wired the route without
			// auth.Middleware — fail loud, not silent.
			apiutil.WriteError(w, http.StatusUnauthorized,
				"missing_claims", "auth middleware did not run")
			return
		}
		// Tag the span with the verified identity. Once claims exist,
		// overwrite the placeholder attribute set above.
		span.SetAttributes(
			attribute.Int64("reservation.event_id", int64(claims.EventID)),
			attribute.String("reservation.user_id", claims.Subject),
		)

		// ── 2. Parse optional seats_requested ─────────────────────
		// Body is optional; absent / empty body defaults to 1 seat.
		// JSON null also decodes to 0 → "use default". Any non-EOF
		// decode error (malformed JSON, wrong type) is a 400.
		seats := 1
		var req struct {
			SeatsRequested int `json:"seats_requested"`
		}
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
				apiutil.WriteError(w, http.StatusBadRequest,
					"invalid_request", "body must be JSON with optional seats_requested")
				return
			}
		}
		// 0 = "use default" (matches JSON null behaviour); -1 / >10 = error.
		if req.SeatsRequested != 0 {
			if req.SeatsRequested < 1 || req.SeatsRequested > 10 {
				apiutil.WriteError(w, http.StatusBadRequest,
					"invalid_request", "seats_requested must be between 1 and 10")
				return
			}
			seats = req.SeatsRequested
		}
		span.SetAttributes(attribute.Int("reservation.seats_requested", seats))

		// ── 3. Atomic seat lock ───────────────────────────────────
		//
		// The redisotel auto-instrumentation already produces a child
		// span for the EVALSHA call (e.g. span name "EVALSHA"), but
		// it doesn't know what we were trying to *do*. This manual
		// span adds the business meaning: "acquire a hold for this
		// user on this event". Jaeger shows both — auto span inside,
		// manual span outside, in the waterfall.
		_, holdSpan := tracer.Start(r.Context(), "redis.acquire-hold",
			trace.WithAttributes(
				attribute.Int64("hold.event_id", int64(claims.EventID)),
				attribute.Int64("hold.user_id_hash", hashUserID(claims.Subject)),
				attribute.Int("hold.ttl_seconds", int(holdTTL.Seconds())),
			),
		)
		result, err := redis.ReserveSeat(r.Context(), rdb,
			claims.EventID, claims.Subject,
			int(holdTTL.Seconds()), seats)
		if err != nil {
			holdSpan.RecordError(err)
			holdSpan.SetStatus(codes.Error, "lua reserve script failed")
			holdSpan.End()
			slog.ErrorContext(r.Context(), "reserve script failed",
				"event", claims.EventID,
				"user", claims.Subject,
				"error", err,
			)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "reservation failed, try again")
			return
		}
		// Tag the outcome so the Jaeger UI can group "sold_out" vs
		// "already_holding" vs "reserved" without parsing logs.
		holdSpan.SetAttributes(
			attribute.Int("hold.status_code", int(result.Status)),
			attribute.Int("hold.rejection_reason", int(result.Reason)),
		)
		holdSpan.End()

		// ── 4. Map Lua outcome to HTTP response ───────────────────
		switch result.Status {
		case redis.StatusEventNotFound:
			apiutil.WriteError(w, http.StatusNotFound,
				"event_not_found", "no inventory has been seeded for this event")
			return

		case redis.StatusRejected:
			switch result.Reason {
			case redis.ReasonSoldOut:
				metrics.ReservationsSoldOut.Inc()
				apiutil.WriteError(w, http.StatusConflict,
					"sold_out", "this event is sold out")
			case redis.ReasonAlreadyHolding:
				metrics.ReservationsAlreadyHolding.Inc()
				apiutil.WriteError(w, http.StatusConflict,
					"already_holding",
					"you already have an active hold for this event")
			default:
				apiutil.WriteError(w, http.StatusInternalServerError,
					"internal_error", "unexpected rejection reason")
			}
			return
		}

		if result.Status != redis.StatusReserved {
			// Shouldn't happen — the Lua script only returns one of the
			// three statuses above. Treat as a programming error.
			slog.ErrorContext(r.Context(), "reserve returned unknown status",
				"status", result.Status,
				"event", claims.EventID,
				"user", claims.Subject,
			)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "unexpected reservation outcome")
			return
		}

		// Seat successfully held in Redis. Count it as a hold
		// (regardless of whether the publish to SQS succeeds — the
		// hold is the source of truth for "did we get the seat").
		metrics.ReservationsHeld.Inc()

		// ── 5. Success: mint reservation_id, publish, respond ──────
		reservationID := uuid.NewString()
		now := time.Now()
		expiresAt := now.Add(holdTTL)
		span.SetAttributes(attribute.String("reservation.id", reservationID))

		// Build the wire payload via queue.Reservation so the JSON
		// shape is guaranteed to match what the worker parses.
		// Drift between producer and consumer breaks the build, not
		// production.
		payload, err := json.Marshal(queue.Reservation{
			ReservationID: reservationID,
			UserID:        claims.Subject,
			EventID:       claims.EventID,
			Seats:         seats,
			CreatedAt:     now.UTC().Format(time.RFC3339),
			ExpiresAt:     expiresAt.UTC().Format(time.RFC3339),
		})
		if err != nil {
			// json.Marshal on a primitive-only struct cannot fail at
			// runtime; treat as a programming error.
			slog.ErrorContext(r.Context(), "marshal publish payload failed", "error", err)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "failed to build event message")
			return
		}

		// Publish failure is logged but does NOT roll back the
		// reservation: Redis already created the hold, and the worker
		// will handle a redelivered message idempotently
		// (INSERT ... ON CONFLICT DO NOTHING).
		_, publishSpan := tracer.Start(r.Context(), "sqs.publish",
			trace.WithAttributes(
				attribute.String("messaging.system", "aws.sqs"),
				attribute.String("messaging.destination.name", "ticket-reservations"),
				attribute.Int("messaging.message.body.size", len(payload)),
			),
		)
		if err := publisher.Publish(r.Context(), payload); err != nil {
			publishSpan.RecordError(err)
			publishSpan.SetStatus(codes.Error, "sqs publish failed")
			slog.ErrorContext(r.Context(), "publish failed (reservation still valid)",
				"reservation_id", reservationID,
				"error", err,
			)
		} else {
			metrics.ReservationsCompleted.Inc()
		}
		publishSpan.End()

		apiutil.WriteJSON(w, http.StatusOK, map[string]any{
			"reservation_id": reservationID,
			"seats":          seats,
			"expires_in":     int(holdTTL.Seconds()),
		})
	}
}

// hashUserID returns a stable, short hash of the user ID for use as a
// span attribute. The raw user_id (which may be PII) is intentionally
// NOT set as a span attribute — only this hash is. Lets you correlate
// traces for the same user across requests without leaking identity.
func hashUserID(s string) int64 {
	var h int64
	for _, b := range []byte(s) {
		h = h*31 + int64(b)
	}
	if h < 0 {
		h = -h
	}
	return h
}