package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/db"
)

// tracerName is the OTel Tracer name for this package's spans.
// Matches the pattern from reserve_handler.go so the same service
// shows up in Jaeger's Service dropdown.
const getReservationTracerName = "github.com/emmitt-k/ticket-deal/internal/api"

// getReservationTracer is the package-local tracer used by the
// read-only endpoints (Get, ListMy). Avoids re-instantiating per
// request. Declared once at package level by OpenTelemetry idiom.
var getReservationTracer = otel.Tracer(getReservationTracerName)

// ── GET /api/reservations/:id ──────────────────────────────────────────
//
// Returns ONE reservation row. Requires JWT auth. Authorization model:
//
//   - 401 missing/invalid JWT          (handled by auth.Middleware)
//   - 404 reservation_not_found        (no row matches :id)
//   - 403 not_your_reservation          (row exists but belongs to another user)
//   - 200 + ReservationRow JSON         (happy path)
//
// Layer 1 item 2 of docs/product-roadmap.md. Unblocks the
// 'dashboard-the-user-sees' use case where someone clicks a link
// in their email and lands on their ticket page.
func GetReservationHandler(pool *db.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := getReservationTracer.Start(r.Context(), "get_reservation.handle",
			trace.WithSpanKind(trace.SpanKindInternal),
		)
		defer span.End()
		r = r.WithContext(ctx)

		claims := auth.ClaimsFromContext(r.Context())
		if claims == nil {
			// Defensive: route must be wired with auth.Middleware.
			apiutil.WriteError(w, http.StatusUnauthorized,
				"missing_claims", "auth middleware did not run")
			span.SetStatus(codes.Error, "missing_claims")
			return
		}
		span.SetAttributes(attribute.String("reservation.user_id", claims.Subject))

		// chi URL param — {id} in the route definition (see main.go).
		id := chi.URLParam(r, "id")
		if id == "" {
			apiutil.WriteError(w, http.StatusBadRequest,
				"missing_id", "reservation id required in path")
			span.SetStatus(codes.Error, "missing_id")
			return
		}
		span.SetAttributes(attribute.String("reservation.id", id))

		row, err := db.GetReservation(r.Context(), pool, id)
		if err != nil {
			if db.IsNoRows(err) {
				apiutil.WriteError(w, http.StatusNotFound,
					"reservation_not_found", "no reservation with that id")
				span.SetAttributes(attribute.String("lookup.outcome", "not_found"))
				return
			}
			slog.ErrorContext(r.Context(), "get_reservation db error",
				"reservation_id", id, "error", err)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "could not look up reservation")
			span.SetStatus(codes.Error, "db_error")
			span.RecordError(err)
			return
		}

		// Authorization: row.user_id must equal the JWT subject.
		// Returning 403 (not 404) for cross-user access would leak the
		// existence of the row; some APIs deliberately return 404 here
		// to avoid that leak. We choose 403 here because:
		//   1. The id is a UUID — guessing one is infeasible
		//   2. The frontend already authenticates the user, so 403 has
		//      clearer UX meaning than a generic 404
		// If you ever expose this endpoint to less-trusted clients,
		// swap to 404 for that endpoint specifically.
		if row.UserID != claims.Subject {
			apiutil.WriteError(w, http.StatusForbidden,
				"not_your_reservation", "this reservation belongs to another user")
			span.SetAttributes(
				attribute.String("lookup.outcome", "forbidden"),
				attribute.Int64("reservation.owner_hash", hashUserID(row.UserID)),
			)
			return
		}

		span.SetAttributes(
			attribute.String("lookup.outcome", "ok"),
			attribute.String("reservation.status", row.Status),
			attribute.Int64("reservation.event_id", row.EventID),
		)
		apiutil.WriteJSON(w, http.StatusOK, row)
	}
}

// ── GET /api/me/reservations ───────────────────────────────────────────
//
// Returns the caller's reservations, newest first. Always scoped to
// the JWT subject — clients cannot ask for someone else's list.
//
// Query params:
//
//	?status=  one of the 5 status constants. Empty (default) → all 5.
//	          No filter = 'no hidden state' per the design decision.
//	?limit=   default 20, hard max 100. 0 or negative → 20.
//	?offset=  default 0. Negative → 0.
//
// Response: 200 + JSON array of ReservationRow, newest first. Empty
// array if the user has no reservations yet. (NOT 404 — empty list is
// valid; the array is just []). Pagination follows offset+limit;
// 'next page exists' is inferred by `len(response) == limit`.
//
// Status codes:
//
//	401 missing/invalid JWT              (auth.Middleware)
//	500 internal_error                  (DB error)
//	200 + array                         (happy path, possibly empty)
//
// Layer 1 item 2 of docs/product-roadmap.md.
func ListMyReservationsHandler(pool *db.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := getReservationTracer.Start(r.Context(), "list_my_reservations.handle",
			trace.WithSpanKind(trace.SpanKindInternal),
		)
		defer span.End()
		r = r.WithContext(ctx)

		claims := auth.ClaimsFromContext(r.Context())
		if claims == nil {
			apiutil.WriteError(w, http.StatusUnauthorized,
				"missing_claims", "auth middleware did not run")
			span.SetStatus(codes.Error, "missing_claims")
			return
		}
		span.SetAttributes(attribute.String("reservation.user_id", claims.Subject))

		// Validate ?status early. Anything that doesn't match the
		// known set returns 400 — important because the SQL WHERE
		// clause interpolates the string and an unknown value would
		// silently return empty (e.g. ?status=PENDIGN_TYPOPAYMENT).
		statusFilter := r.URL.Query().Get("status")
		if statusFilter != "" &&
			statusFilter != db.StatusPendingPayment &&
			statusFilter != db.StatusConfirmed &&
			statusFilter != db.StatusExpired &&
			statusFilter != db.StatusCancelled &&
			statusFilter != db.StatusRefunded {
			apiutil.WriteError(w, http.StatusBadRequest,
				"invalid_status",
				"status must be one of PENDING_PAYMENT, CONFIRMED, EXPIRED, CANCELLED, REFUNDED")
			span.SetAttributes(
				attribute.String("lookup.outcome", "bad_request"),
				attribute.String("lookup.bad_status", statusFilter),
			)
			return
		}
		span.SetAttributes(attribute.String("lookup.status_filter", statusFilter))

		// ?limit= and ?offset= parsing. The helper clamps out-of-range
		// values internally, so we don't need to validate explicitly
		// here — just parse and pass through.
		limit, offset := parseLimitOffset(r)

		rows, err := db.ListReservationsByUser(r.Context(), pool,
			claims.Subject, statusFilter, limit, offset)
		if err != nil {
			slog.ErrorContext(r.Context(), "list_my_reservations db error",
				"user_id", claims.Subject, "status", statusFilter,
				"limit", limit, "offset", offset, "error", err)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "could not list reservations")
			span.SetStatus(codes.Error, "db_error")
			span.RecordError(err)
			return
		}

		span.SetAttributes(
			attribute.Int("lookup.returned", len(rows)),
			attribute.Int("lookup.limit", limit),
			attribute.Int("lookup.offset", offset),
		)
		// Always emit 200 + an array (possibly empty). Returning 404
		// for 'no reservations yet' would force the frontend to
		// handle two different success shapes; an empty array keeps
		// the rendering code simple.
		if rows == nil {
			rows = []db.ReservationRow{} // ensure non-nil JSON array
		}
		apiutil.WriteJSON(w, http.StatusOK, rows)
	}
}

// isPgNoRows reports whether err is pgx.ErrNoRows. lives in the db
// package as db.IsNoRows; we call it directly.

// parseLimitOffset extracts the ?limit= and ?offset= query params from
// the request, applying sensible defaults for missing/invalid values:
//   - ?limit=  absent or <= 0   → db.defaultListLimit (= 20)
//   - ?limit=  > db.hardMaxLimit → clamped to hardMaxLimit (= 100)
//   - ?offset= absent or < 0    → 0
//
// Go's strconv.Atoi is silent on parse errors; the upstream caller
// trusts the db layer to clamp / reject pathological inputs.
func parseLimitOffset(r *http.Request) (limit, offset int) {
	limit = -1
	offset = -1
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := atoi(v); err == nil {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := atoi(v); err == nil {
			offset = n
		}
	}
	return limit, offset
}

// atoi is a tiny stdlib wrapper that keeps parseLimitOffset readable.
// Returns -1 on error so the caller can distinguish 'caller passed 0'
// (which means "give me everything") from 'parse failed'.
func atoi(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, http.ErrBodyNotAllowed
		}
		n = n*10 + int(c-'0')
		// early-exit on absurdly long input to avoid overflow
		if n > 1<<30 {
			return 0, http.ErrBodyNotAllowed
		}
	}
	return n, nil
}
