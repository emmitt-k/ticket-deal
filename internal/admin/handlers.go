package admin

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// Handlers holds the admin API's dependencies.
type Handlers struct {
	Pool     *db.Pool
	Rdb      *redis.Client
	Verifier *Verifier
}

// Register mounts the admin routes.
//
// The whole /admin tree sits behind the signature verifier. When no
// key is configured the routes are NOT mounted at all and the caller
// answers 503 — the same shape as the optional Stripe integration:
// absent is a choice, half-configured is a bug.
func (h *Handlers) Register(r chi.Router) {
	if !h.Verifier.Enabled() {
		return
	}

	r.Route("/admin", func(r chi.Router) {
		r.Use(h.Verifier.Middleware)

		// Support lookups.
		r.Get("/reservations/lookup", h.LookupByUser)
		r.Get("/reservations/{id}", h.GetOne)

		// The two writes. Both are audited with actor_type='admin'.
		r.Post("/reservations/{id}/cancel", h.Cancel)
		r.Post("/reservations/{id}/release-seat", h.ReleaseSeat)
	})
}

// ── Lookups ───────────────────────────────────────────────────────────

// GetOne returns one reservation.
//
// Support usually has a reservation id in front of them ("the customer
// forwarded this link").
func (h *Handlers) GetOne(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	row, err := db.GetReservation(r.Context(), h.Pool, id)
	if err != nil {
		if db.IsNoRows(err) {
			writeError(w, http.StatusNotFound, "not_found",
				"no reservation with that id")
			return
		}
		slog.ErrorContext(r.Context(), "admin lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"the lookup failed")
		return
	}

	h.logAccess(r, "get_reservation", row.ReservationID)
	apiutil.WriteJSON(w, http.StatusOK, row)
}

// LookupByUser returns a user's reservations.
//
// This is the endpoint support actually reaches for: "find everything
// this customer has".
func (h *Handlers) LookupByUser(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		writeError(w, http.StatusBadRequest, "missing_user_id",
			"user_id is required")
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	rows, err := db.ListReservationsByUser(r.Context(), h.Pool, userID, "", limit, offset)
	if err != nil {
		slog.ErrorContext(r.Context(), "admin lookup failed",
			"user_id", userID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"the lookup failed")
		return
	}
	if rows == nil {
		rows = []db.ReservationRow{}
	}

	h.logAccess(r, "lookup_user", userID)
	apiutil.WriteJSON(w, http.StatusOK, map[string]any{
		"user_id":      userID,
		"reservations": rows,
		"count":        len(rows),
	})
}

// ── The two writes ────────────────────────────────────────────────────

// Cancel cancels a held reservation and releases its seat.
//
// It is the same operation a customer can do themselves, except the
// audit trail records the ADMIN as the actor — which is the whole
// point of the endpoint: "who cancelled this, and why".
//
// A CONFIRMED reservation is REFUNDED, not cancelled. That is not a
// naming preference: the state machine has no CONFIRMED → CANCELLED
// edge, because "the customer never paid" and "the customer paid and
// we gave the money back" are different facts. A paid reservation is
// reversed by the Stripe refund path, and an admin cancel refuses it
// with a code that says so.
func (h *Handlers) Cancel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actor := ActorFromContext(r.Context())

	row, err := db.GetReservation(r.Context(), h.Pool, id)
	if err != nil {
		if db.IsNoRows(err) {
			writeError(w, http.StatusNotFound, "not_found",
				"no reservation with that id")
			return
		}
		slog.ErrorContext(r.Context(), "admin cancel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"the cancellation failed")
		return
	}

	// A status that forbids the transition gets its own code — they
	// are different facts and the operator needs to tell them apart.
	switch row.Status {
	case db.StatusPendingPayment:
		// the normal path
	case db.StatusCancelled:
		// Idempotent: it is already cancelled, and the seat was
		// already released. Returning 200 (rather than 409) means a
		// retried admin call is not an error.
		h.logAccess(r, "cancel", id)
		apiutil.WriteJSON(w, http.StatusOK, map[string]any{
			"reservation":   row,
			"seat_released": false,
			"note":          "already cancelled",
		})
		return
	case db.StatusConfirmed:
		writeError(w, http.StatusConflict, "already_paid",
			"this reservation was paid — it must be REFUNDED, not cancelled")
		return
	case db.StatusRefunded:
		writeError(w, http.StatusConflict, "already_refunded",
			"this reservation was already refunded")
		return
	case db.StatusExpired:
		writeError(w, http.StatusConflict, "already_expired",
			"this reservation already expired")
		return
	default:
		writeError(w, http.StatusConflict, "invalid_status",
			"this reservation cannot be cancelled from its current status")
		return
	}

	// The compare-and-set. The audit row is written in the SAME
	// transaction, with the admin as the actor — that is why
	// db.TransitionStatus takes a mandatory db.Audit.
	changed, err := db.TransitionStatus(r.Context(), h.Pool, id,
		db.StatusPendingPayment, db.StatusCancelled,
		db.Audit{
			Actor: db.AdminActor(actor),
			Metadata: map[string]any{
				"seats":             row.Seats,
				"via":               "admin_api",
				"actor_fingerprint": actor,
			},
		})
	if err != nil {
		slog.ErrorContext(r.Context(), "admin cancel failed",
			"reservation_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"the cancellation failed")
		return
	}
	if !changed {
		// Lost the race — someone else moved the row between the read
		// and the transition. Re-read to see who won.
		current, rerr := db.GetReservation(r.Context(), h.Pool, id)
		if rerr == nil {
			writeError(w, http.StatusConflict, "cancel_race",
				"the reservation changed while the cancellation was in flight (now "+current.Status+")")
			return
		}
		writeError(w, http.StatusConflict, "cancel_race",
			"the reservation changed while the cancellation was in flight")
		return
	}

	// The seat goes back on sale. A failure here is NOT a 5xx: the
	// status change is committed, and returning 5xx would make the
	// client retry into an idempotent 200 while the seat stays
	// phantom-held. It is logged loudly and reported in the response.
	seatReleased := h.releaseSeat(r, row)

	updated, err := db.GetReservation(r.Context(), h.Pool, id)
	if err == nil {
		row = updated
	}

	h.logAccess(r, "cancel", id)
	apiutil.WriteJSON(w, http.StatusOK, map[string]any{
		"reservation":   row,
		"seat_released": seatReleased,
	})
}

// ReleaseSeat puts a seat back on sale WITHOUT changing the
// reservation's status.
//
// This is the ops override for a seat that is stuck: the inventory
// says sold, but it should not be. The hold key is the guard — a
// missing hold means the seat is ALREADY back on sale (the hold
// expired and the expiration watcher released it), and releasing it
// again would oversell.
//
// Unlike the cancel, this changes no status, so there is no
// compare-and-set to arbitrate between two operators doing it at once.
// The Redis script is what makes it safe: the "is it still held?"
// check and the release are one atomic operation.
//
// The audit row records the action with old_status == new_status — it
// is an EVENT (a seat went back on sale), not a state change.
func (h *Handlers) ReleaseSeat(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actor := ActorFromContext(r.Context())

	row, err := db.GetReservation(r.Context(), h.Pool, id)
	if err != nil {
		if db.IsNoRows(err) {
			writeError(w, http.StatusNotFound, "not_found",
				"no reservation with that id")
			return
		}
		slog.ErrorContext(r.Context(), "admin release-seat failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"the release failed")
		return
	}

	outcome, err := redis.ReleaseSeat(r.Context(), h.Rdb, row.EventID, row.UserID, row.Seats)
	if err != nil {
		slog.ErrorContext(r.Context(), "admin release-seat failed",
			"reservation_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"the release failed")
		return
	}

	switch outcome {
	case redis.ReleaseSeatNoInventory:
		writeError(w, http.StatusConflict, "no_inventory",
			"this event has no inventory counter — the seat cannot be released")
		return
	case redis.ReleaseSeatNotHeld:
		writeError(w, http.StatusConflict, "seat_not_held",
			"the hold has already expired, so the seat is already back on sale — releasing it again would oversell")
		return
	}

	// The audit row. old_status == new_status: the reservation did
	// not change state; what changed is the inventory.
	if err := db.WriteAuditEvent(r.Context(), h.Pool, row.ReservationID, row.UserID,
		db.AuditEntry{
			EventType: db.AuditEventSeatReleased,
			Actor:     db.AdminActor(actor),
			OldStatus: row.Status,
			NewStatus: row.Status,
			Metadata: map[string]any{
				"seats":             row.Seats,
				"via":               "admin_api",
				"actor_fingerprint": actor,
			},
		}); err != nil {
		// The seat IS released at this point. Failing the request
		// would tell the operator it did not happen, which is worse
		// than a missing audit row — so it is logged loudly and the
		// response says the release succeeded but the audit did not.
		slog.ErrorContext(r.Context(), "admin release-seat: the audit write FAILED",
			"reservation_id", id, "actor", actor, "error", err)
		writeError(w, http.StatusInternalServerError, "audit_write_failed",
			"the seat was released but the audit row could not be written — treat this as an incident")
		return
	}

	h.logAccess(r, "release_seat", id)
	apiutil.WriteJSON(w, http.StatusOK, map[string]any{
		"reservation":   row,
		"seat_released": true,
	})
}

// ── Helpers ───────────────────────────────────────────────────────────

// releaseSeat puts the seats back on sale and reports whether it
// worked.
//
// A Redis failure after a committed status change is NOT a 5xx (see
// the project's "partial Redis failure" convention): the client
// retrying would get an idempotent answer while the seat stayed
// phantom-held. It is logged at ERROR and reported in the response so
// an operator can reconcile it.
func (h *Handlers) releaseSeat(r *http.Request, row db.ReservationRow) bool {
	outcome, err := redis.ReleaseSeat(r.Context(), h.Rdb, row.EventID, row.UserID, row.Seats)
	if err != nil {
		slog.ErrorContext(r.Context(), "admin: the seat was NOT released",
			"reservation_id", row.ReservationID,
			"event_id", row.EventID,
			"seats", row.Seats,
			"error", err)
		return false
	}
	if outcome != redis.ReleaseSeatDone {
		slog.WarnContext(r.Context(), "admin: the seat was not held",
			"reservation_id", row.ReservationID,
			"event_id", row.EventID,
			"seats", row.Seats)
		return false
	}
	return true
}

// logAccess records an admin read. The audit table is a state-transition
// trail, so a READ is not an event in it — but "support looked up this
// customer's data" is exactly the kind of question a compliance
// reviewer asks, so it goes to the structured log with the actor.
func (h *Handlers) logAccess(r *http.Request, action, subject string) {
	slog.InfoContext(r.Context(), "admin api access",
		"action", action,
		"subject", subject,
		"actor_fingerprint", ActorFromContext(r.Context()),
		"method", r.Method,
		"path", r.URL.Path,
	)
}

// writeError is the admin package's error writer. It mirrors the
// project's apiutil.WriteError shape: {"error": code, "message": text}.
func writeError(w http.ResponseWriter, status int, code, message string) {
	apiutil.WriteError(w, status, code, message)
}
