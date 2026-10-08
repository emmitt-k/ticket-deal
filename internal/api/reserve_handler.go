package api

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/queue"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

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

		// ── 3. Atomic seat lock ───────────────────────────────────
		result, err := redis.ReserveSeat(r.Context(), rdb,
			claims.EventID, claims.Subject,
			int(holdTTL.Seconds()), seats)
		if err != nil {
			log.Printf("api: reserve script failed event=%d user=%s: %v",
				claims.EventID, claims.Subject, err)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "reservation failed, try again")
			return
		}

		// ── 4. Map Lua outcome to HTTP response ───────────────────
		switch result.Status {
		case redis.StatusEventNotFound:
			apiutil.WriteError(w, http.StatusNotFound,
				"event_not_found", "no inventory has been seeded for this event")
			return

		case redis.StatusRejected:
			switch result.Reason {
			case redis.ReasonSoldOut:
				apiutil.WriteError(w, http.StatusConflict,
					"sold_out", "this event is sold out")
			case redis.ReasonAlreadyHolding:
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
			log.Printf("api: reserve returned unknown status %d event=%d user=%s",
				result.Status, claims.EventID, claims.Subject)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "unexpected reservation outcome")
			return
		}

		// ── 5. Success: mint reservation_id, publish, respond ──────
		reservationID := uuid.NewString()
		now := time.Now()
		expiresAt := now.Add(holdTTL)

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
			log.Printf("api: marshal publish payload failed: %v", err)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "failed to build event message")
			return
		}

		// Publish failure is logged but does NOT roll back the
		// reservation: Redis already created the hold, and the worker
		// will handle a redelivered message idempotently
		// (INSERT ... ON CONFLICT DO NOTHING).
		if err := publisher.Publish(r.Context(), payload); err != nil {
			log.Printf("api: publish failed (reservation still valid) res=%s: %v",
				reservationID, err)
		}

		apiutil.WriteJSON(w, http.StatusOK, map[string]any{
			"reservation_id": reservationID,
			"seats":          seats,
			"expires_in":     int(holdTTL.Seconds()),
		})
	}
}