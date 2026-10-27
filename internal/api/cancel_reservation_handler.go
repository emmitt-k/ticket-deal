// Package api — cancel_reservation_handler.go: POST /api/reservations/{id}/cancel.
//
// This is Layer 1 item 3 of docs/product-roadmap.md — the "user changes their
// mind" path. Five steps from the spec, in order:
//
//  1. Verify JWT matches the row's user_id           → 403 if not
//  2. Verify status == PENDING_PAYMENT               → 409 otherwise
//  3. db.TransitionStatus(PENDING_PAYMENT → CANCELLED) → CAS-safe against
//     watcher's EXPIRED sweep,
//     and appends the CANCELLED
//     audit row in the same tx
//  4. INCRBY inventory:event:<id> <seats>            → release the seat back to Redis
//  5. (best-effort) DEL hold:event:<id>:user:<u>     → so the user can re-reserve
//
// POST is naturally idempotent in REST; a second cancel on the same row is a
// 200 with the row (decision from this branch's design discussion). The CAS
// in step 3 makes a race with the expiration watcher safe — the loser sees
// (false, nil) from TransitionStatus and gets 409 already_expired.
//
// Partial-failure policy (also decided this branch): if step 4 or step 5
// fails (Redis blip after DB transition succeeded), we log WARN and STILL
// return 200 to the client. DB is the system-of-record for the user's
// intent; an inventory count discrepancy is a reconcilable operator problem,
// not a user-facing problem. Returning 500 here would force retries →
// subsequent calls would hit 409 already_cancelled → user confusion. The
// WARN line shows up in Loki for the operator.
package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// CancelReservationHandler returns the http.HandlerFunc for
// POST /api/reservations/{id}/cancel.
//
// Auth: route must be wired with auth.Middleware (the main.go mount).
// Deps: both Postgres (for the row + transition) and Redis (for the
// seat release + hold-key cleanup). Both are mandatory for correctness.
func CancelReservationHandler(pool *db.Pool, rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "cancel_reservation.handle")
		defer span.End()
		r = r.WithContext(ctx)

		claims := auth.ClaimsFromContext(r.Context())
		if claims == nil {
			// Defensive — main.go wires auth.Middleware, so this only
			// fires if someone miswires a test.
			apiutil.WriteError(w, http.StatusUnauthorized,
				"missing_claims", "auth middleware did not run")
			span.SetStatus(codes.Error, "missing_claims")
			return
		}
		span.SetAttributes(attribute.String("reservation.user_id", claims.Subject))

		id := chi.URLParam(r, "id")
		if id == "" {
			apiutil.WriteError(w, http.StatusBadRequest,
				"missing_id", "reservation id required in path")
			span.SetStatus(codes.Error, "missing_id")
			return
		}
		span.SetAttributes(attribute.String("reservation.id", id))

		// ── Steps 1+2: load the row, check ownership + status ─────────
		row, err := db.GetReservation(r.Context(), pool, id)
		if err != nil {
			if db.IsNoRows(err) {
				apiutil.WriteError(w, http.StatusNotFound,
					"reservation_not_found", "no reservation with that id")
				span.SetAttributes(attribute.String("cancel.outcome", "not_found"))
				return
			}
			slog.ErrorContext(r.Context(), "cancel: db lookup failed",
				"reservation_id", id, "error", err)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "could not look up reservation")
			span.SetStatus(codes.Error, "db_lookup_failed")
			span.RecordError(err)
			return
		}
		span.SetAttributes(
			attribute.Int64("reservation.event_id", row.EventID),
			attribute.Int("reservation.seats", row.Seats),
			attribute.String("reservation.current_status", row.Status),
		)

		// Authorization: row must belong to the JWT caller.
		// Same 403-vs-404 reasoning as get_reservation_handler — UUIDs are
		// unguessable so a 403 leaks nothing of consequence.
		if row.UserID != claims.Subject {
			apiutil.WriteError(w, http.StatusForbidden,
				"not_your_reservation", "this reservation belongs to another user")
			span.SetAttributes(
				attribute.String("cancel.outcome", "forbidden"),
				attribute.Int64("reservation.owner_hash", hashUserID(row.UserID)),
			)
			return
		}

		// Status pre-checks. CANCELLED is short-circuit idempotent:
		// the user already cancelled, no state change needed, just return
		// the row. CONFIRMED → cancellation is forbidden (refund, not cancel).
		// EXPIRED → too late, the watcher beat them to it.
		switch row.Status {
		case db.StatusPendingPayment:
			// fall through to the transition below
		case db.StatusCancelled:
			// IDEMPOTENT — second cancel on an already-cancelled row returns
			// 200 + the row. The user-facing outcome is identical to a fresh
			// cancel; the DB is already in the target state.
			span.SetAttributes(attribute.String("cancel.outcome", "idempotent_already_cancelled"))
			apiutil.WriteJSON(w, http.StatusOK, row)
			return
		case db.StatusConfirmed:
			apiutil.WriteError(w, http.StatusConflict,
				"already_paid", "confirmed reservations require a refund, not a cancel")
			span.SetAttributes(attribute.String("cancel.outcome", "already_paid"))
			return
		case db.StatusExpired:
			apiutil.WriteError(w, http.StatusConflict,
				"already_expired", "the hold expired before the cancel request landed")
			span.SetAttributes(attribute.String("cancel.outcome", "already_expired"))
			return
		default:
			// Defensive: an unknown status would have failed the SQL CHECK
			// at insert time, so this branch is unreachable in practice.
			// Still, type-narrowing is the friend of correctness.
			slog.ErrorContext(r.Context(), "cancel: unexpected current status",
				"reservation_id", id, "status", row.Status)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "unexpected reservation status")
			span.SetStatus(codes.Error, "unexpected_status")
			return
		}

		// ── Step 3: CAS transition. Race-safe against watcher's EXPIRED sweep. ──
		// The transition also appends a CANCELLED audit row, in the same
		// transaction as the status change. The actor is the user whose
		// JWT we're holding — "who cancelled this?" is exactly the
		// question the audit log exists to answer.
		changed, err := db.TransitionStatus(r.Context(), pool,
			id, db.StatusPendingPayment, db.StatusCancelled,
			db.Audit{Actor: db.UserActor(claims.Subject)})
		if err != nil {
			slog.ErrorContext(r.Context(), "cancel: transition failed",
				"reservation_id", id, "error", err)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "could not transition reservation status")
			span.SetStatus(codes.Error, "transition_failed")
			span.RecordError(err)
			return
		}

		if !changed {
			// The row existed and was readable a moment ago, but the CAS
			// reports 0 rows updated. The watcher most likely got there
			// first with EXPIRED, but it could also be that someone else
			// (or this same client, on retry) already cancelled. Re-fetch
			// to differentiate.
			latest, lookupErr := db.GetReservation(r.Context(), pool, id)
			if lookupErr != nil {
				slog.ErrorContext(r.Context(), "cancel: re-lookup after CAS miss failed",
					"reservation_id", id, "error", lookupErr)
				apiutil.WriteError(w, http.StatusInternalServerError,
					"internal_error", "race resolution failed")
				span.SetStatus(codes.Error, "cas_race_lookup_failed")
				return
			}
			switch latest.Status {
			case db.StatusCancelled:
				// Idempotent — somebody else (or a retried call) cancelled
				// before us. The DB is in the target state; return 200 + the row.
				span.SetAttributes(attribute.String("cancel.outcome", "cas_lost_to_already_cancelled"))
				apiutil.WriteJSON(w, http.StatusOK, latest)
				return
			case db.StatusExpired:
				apiutil.WriteError(w, http.StatusConflict,
					"already_expired", "the hold expired before the cancel request landed")
				span.SetAttributes(attribute.String("cancel.outcome", "cas_lost_to_expiry"))
				return
			default:
				// Could be PENDING_PAYMENT (very weird — CAS said miss but
				// status didn't change), CONFIRMED, or something else. Bail
				// with a generic conflict.
				apiutil.WriteError(w, http.StatusConflict,
					"cancel_race", "cancel raced with another state change; retry the GET to see current status")
				span.SetAttributes(
					attribute.String("cancel.outcome", "cas_lost_unknown"),
					attribute.String("cancel.other_status", latest.Status),
				)
				return
			}
		}

		// ── Step 4: release the seat back to Redis. ───────────────────────
		// Partial-failure policy: if this fails, log WARN but return 200.
		// See package comment for rationale.
		invKey := redis.InventoryKey(row.EventID)
		if _, incrErr := rdb.IncrBy(r.Context(), invKey, int64(row.Seats)).Result(); incrErr != nil {
			slog.WarnContext(r.Context(), "cancel: redis INCRBY failed; manual reconciliation may be required",
				"reservation_id", id,
				"event_id", row.EventID,
				"seats", row.Seats,
				"inventory_key", invKey,
				"error", incrErr,
			)
			span.SetAttributes(attribute.String("cancel.redis_release", "failed"))
		} else {
			span.SetAttributes(attribute.String("cancel.redis_release", "ok"))
		}

		// ── Step 5: best-effort hold-key cleanup. ─────────────────────────
		// DEL is idempotent — if the key already expired (TTL), DEL still
		// returns success. Doesn't change anything for the user, just lets
		// /reserve not 409 already_holding the next time around.
		holdKey := redis.HoldKey(row.EventID, claims.Subject)
		if delErr := rdb.Del(r.Context(), holdKey).Err(); delErr != nil {
			slog.WarnContext(r.Context(), "cancel: redis DEL hold failed",
				"reservation_id", id,
				"user_id", claims.Subject,
				"hold_key", holdKey,
				"error", delErr,
			)
			span.SetAttributes(attribute.String("cancel.hold_delete", "failed"))
		} else {
			span.SetAttributes(attribute.String("cancel.hold_delete", "ok"))
		}

		// ── Return the final row state. Re-read so the caller sees the
		// CANCELLED status we just set, not the stale PENDING we read above. ──
		final, fetchErr := db.GetReservation(r.Context(), pool, id)
		if fetchErr != nil {
			// Edge case: DB went down between transition + this re-read.
			// Log and return the in-memory row (which we know was just
			// transitioned; the database is authoritative only at this exact
			// instant in time).
			slog.ErrorContext(r.Context(), "cancel: post-transition re-read failed",
				"reservation_id", id, "error", fetchErr)
			// Construct a final state by hand so the response shape is consistent.
			row.Status = db.StatusCancelled
			apiutil.WriteJSON(w, http.StatusOK, row)
			return
		}
		span.SetAttributes(attribute.String("cancel.outcome", "ok"))
		apiutil.WriteJSON(w, http.StatusOK, final)
	}
}
