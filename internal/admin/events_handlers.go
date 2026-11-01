// events_handlers.go — the admin/ops API for EVENTS.
//
// This is the second resource under /admin, joining the reservation
// lookups and overrides from item 9. It follows the same rules as
// every other admin handler:
//
//   - the same HMAC verifier (the whole /admin tree sits behind it)
//   - the same error shape: {"error": code, "message": text}
//   - the same access log with the actor's key fingerprint
//   - the same principle that a WRITE is audited and a READ is logged
//
// ── Why the event resource exists ──────────────────────────────────────
//
// Until this existed, the only way to get an event into the system
// was a migration. That made the system unusable for anything but a
// demo: no catalogue, no way for a customer to browse, and no way for
// an organizer to put a drop on sale.
//
// ── The two stores ─────────────────────────────────────────────────────
//
// An event lives in Postgres (the durable record) AND Redis (the live
// inventory counter). The counter is the source of truth AT SELL TIME
// — the Lua reserve script reads it atomically, and it is why zero
// overselling is possible.
//
// So creating an event writes to both, and it is ALL-OR-NOTHING: if
// the Redis write fails, the event is not created. An event in
// Postgres with no counter in Redis cannot sell a single seat, and it
// would answer "event_not_found" to every customer — a trap, not a
// feature.
//
// Cancelling an event is the mirror image, and the ORDER matters:
// Redis FIRST, then Postgres. See CancelEvent below.

package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// maxAdminBodyBytes bounds the request body the event handlers read.
// The admin API takes small JSON documents; an unbounded read is a
// memory denial-of-service.
const maxAdminBodyBytes = 1 << 20 // 1 MiB

// ── Requests ───────────────────────────────────────────────────────────

// createEventRequest is the body of POST /admin/events.
//
// The id is NOT in it — the application mints it. One convention for
// an app-minted identifier (see internal/eventid).
type createEventRequest struct {
	Name             string `json:"name"`
	InitialInventory *int   `json:"initial_inventory"`
	PriceCents       *int64 `json:"price_cents"`
	Currency         string `json:"currency"`
}

// patchEventRequest is the body of PATCH /admin/events/{id}.
//
// Every field is a pointer: "not supplied" and "supplied as zero" are
// different intents. A PATCH that omits price_cents must not set the
// price to 0.
type patchEventRequest struct {
	Name       *string `json:"name"`
	PriceCents *int64  `json:"price_cents"`
	Currency   *string `json:"currency"`
	AddSeats   *int    `json:"add_seats"`
}

// ── Handlers ───────────────────────────────────────────────────────────

// CreateEvent creates an event and puts its inventory on sale.
//
// It writes to BOTH stores, all-or-nothing:
//
//  1. insert the event row in Postgres
//  2. seed the inventory counter in Redis
//
// If step 2 fails, step 1 is rolled back — the event never existed as
// far as anyone can observe. The alternative is an event that exists
// but cannot sell a seat, which answers "event_not_found" to every
// customer and looks like a bug.
//
// The failure is reported as 409 inventory_seed_failed rather than
// 500: it is a conflict between two stores, not a server fault, and
// the operator can fix it by retrying.
func (h *Handlers) CreateEvent(w http.ResponseWriter, r *http.Request) {
	var req createEventRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}

	// The inventory defaults to 0 seats and the price to the COLUMN
	// default ($25.00). Both are pointers in the request so "not
	// supplied" is distinguishable from "supplied as 0" — a free
	// event is legal, and a zero-seat event is legal, and neither
	// should be a surprise.
	//
	// The price is only sent to the DB layer when the caller
	// supplied it. Sending 0 for "not supplied" would OVERRIDE the
	// column default and produce an event that Stripe refuses to
	// charge — which is exactly the trap the default exists to
	// avoid.
	inv := 0
	if req.InitialInventory != nil {
		inv = *req.InitialInventory
	}

	ev := db.EventCreate{
		Name:             req.Name,
		InitialInventory: inv,
		Currency:         req.Currency,
	}
	if req.PriceCents != nil {
		ev.PriceCents = req.PriceCents
	}
	if ev.Currency == "" {
		// The currency defaults to the column's default ('usd').
		// The DB layer needs a concrete value, so it is resolved
		// here rather than in the DB layer, where the default lives
		// in Postgres.
		ev.Currency = "usd"
	}

	created, err := db.CreateEvent(r.Context(), h.Pool, ev)
	if err != nil {
		slog.ErrorContext(r.Context(), "admin create event failed", "error", err)
		writeError(w, http.StatusBadRequest, "invalid_event", err.Error())
		return
	}

	// Seed the inventory. If this fails, the event row is rolled
	// back — see the function doc.
	if err := h.seedInventory(r, created.ID, inv); err != nil {
		slog.ErrorContext(r.Context(), "admin create event: the inventory seed FAILED, rolling back",
			"event_id", created.ID, "error", err)
		if _, delErr := h.Pool.Exec(r.Context(), "DELETE FROM events WHERE id = $1", created.ID); delErr != nil {
			slog.ErrorContext(r.Context(), "admin create event: the ROLLBACK FAILED — an event exists with no inventory",
				"event_id", created.ID, "error", delErr)
			writeError(w, http.StatusInternalServerError, "rollback_failed",
				"the inventory could not be seeded and the event could not be rolled back — treat this as an incident")
			return
		}
		writeError(w, http.StatusConflict, "inventory_seed_failed",
			"the event's inventory could not be seeded, so the event was not created — retry")
		return
	}

	created.SeatsAvailable = inv
	created.SeatsAvailableKnown = true

	h.logAccess(r, "create_event", created.ID)
	apiutil.WriteJSON(w, http.StatusCreated, map[string]any{
		"event": created,
	})
}

// ListEvents returns the events for the ops view.
//
// Unlike the public catalogue, this shows CANCELLED events too — the
// tombstone is a record of what happened, and the ops view is where
// you go to read it.
func (h *Handlers) ListEvents(w http.ResponseWriter, r *http.Request) {
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

	rows, err := db.ListEvents(r.Context(), h.Pool, true, limit, offset)
	if err != nil {
		slog.ErrorContext(r.Context(), "admin list events failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "the lookup failed")
		return
	}

	h.withSeatCounts(r, rows)

	h.logAccess(r, "list_events", "")
	apiutil.WriteJSON(w, http.StatusOK, map[string]any{
		"events": rows,
		"count":  len(rows),
	})
}

// GetEvent returns one event.
func (h *Handlers) GetEvent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	row, err := db.GetEvent(r.Context(), h.Pool, id)
	if err != nil {
		if db.IsNoRows(err) {
			writeError(w, http.StatusNotFound, "not_found", "no event with that id")
			return
		}
		slog.ErrorContext(r.Context(), "admin get event failed", "event_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "the lookup failed")
		return
	}

	h.withSeatCount(r, &row)

	h.logAccess(r, "get_event", id)
	apiutil.WriteJSON(w, http.StatusOK, map[string]any{"event": row})
}

// PatchEvent updates an event's name, price or currency, and
// optionally adds seats.
//
// Adding seats is the one field that touches REDIS: the live counter
// goes up by that many. Postgres's initial_inventory is NOT rewritten
// — it is the event's history ("this drop was created with 100
// seats"), and the live counter is the present ("there are 137 seats
// left"). Keeping them separate means the two can never disagree
// about what the event WAS.
func (h *Handlers) PatchEvent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actor := ActorFromContext(r.Context())

	var req patchEventRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	// A cancelled event is a tombstone. Editing it would make the
	// tombstone lie about what happened.
	current, err := db.GetEvent(r.Context(), h.Pool, id)
	if err != nil {
		if db.IsNoRows(err) {
			writeError(w, http.StatusNotFound, "not_found", "no event with that id")
			return
		}
		slog.ErrorContext(r.Context(), "admin patch event failed", "event_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "the lookup failed")
		return
	}
	if current.Status == db.EventStatusCancelled {
		writeError(w, http.StatusConflict, "event_cancelled",
			"a cancelled event is a record of what happened — it cannot be edited")
		return
	}

	row, err := db.UpdateEvent(r.Context(), h.Pool, id, db.EventPatch{
		Name:       req.Name,
		PriceCents: req.PriceCents,
		Currency:   req.Currency,
	})
	if err != nil {
		switch {
		case errors.Is(err, db.ErrEventNotFound):
			writeError(w, http.StatusNotFound, "not_found", "no event with that id")
			return
		case errors.Is(err, db.ErrEventCancelled):
			writeError(w, http.StatusConflict, "event_cancelled",
				"a cancelled event is a record of what happened — it cannot be edited")
			return
		default:
			slog.ErrorContext(r.Context(), "admin patch event failed", "event_id", id, "error", err)
			writeError(w, http.StatusBadRequest, "invalid_event", err.Error())
			return
		}
	}

	// Add the seats to the live counter, if asked.
	seatsAdded := 0
	if req.AddSeats != nil && *req.AddSeats != 0 {
		if *req.AddSeats < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "add_seats must be >= 0")
			return
		}
		// redis.AddSeats returns the NEW TOTAL; the amount added is
		// what the caller asked for, and that is what the response
		// reports.
		if _, err := redis.AddSeats(r.Context(), h.Rdb, id, *req.AddSeats); err != nil {
			slog.ErrorContext(r.Context(), "admin patch event: adding seats FAILED",
				"event_id", id, "add_seats", *req.AddSeats, "error", err)
			writeError(w, http.StatusConflict, "inventory_update_failed",
				"the seats could not be added to the live inventory — retry")
			return
		}
		seatsAdded = *req.AddSeats
	}

	h.withSeatCount(r, &row)

	h.logAccess(r, "patch_event", id)
	apiutil.WriteJSON(w, http.StatusOK, map[string]any{
		"event":       row,
		"seats_added": seatsAdded,
		"actor":       actor,
	})
}

// CancelEvent cancels an event.
//
// ── The guard ──────────────────────────────────────────────────────────
//
// An event may only be cancelled while it has ZERO reservations.
//
// The line is drawn at "any reservation EVER existed", not "any
// reservation is still live". An expired reservation still proves a
// customer interacted with the drop, and pulling the event out from
// under someone who tried to buy is worse than refusing.
//
// It is not an arbitrary line — it is where the schema already draws
// it. reservations.event_id REFERENCES events(id) has NO ON DELETE
// CASCADE, so a hard delete against an event with any reservation
// fails at the database level. The explicit check turns an ugly
// 23503 foreign_key_violation into a clear 409 with the count.
//
// ── The ORDER: Redis first, then Postgres ──────────────────────────────
//
// This is the opposite of the obvious order, and the reason is the
// phantom seat:
//
//   - Redis first (DEL the inventory counter), then Postgres: if the
//     Postgres write fails, the event is still live in Postgres but
//     has no inventory counter, so reserve.lua returns
//     "event_not_found". The drop is unavailable, but NO seat is sold
//     and NO reservation is lost. A retry fixes it.
//
//   - Postgres first, then Redis: if the Redis DEL fails, Redis STILL
//     reports seats available against a cancelled event. Reservations
//     keep succeeding, the Lua script decrements the counter, and the
//     worker then fails the foreign key — the seat is gone but no
//     reservation row exists. That is a phantom seat.
//
// The write that can leave a phantom must not be the one that goes
// first.
//
// A cancelled event is a TOMBSTONE — the row is never deleted.
func (h *Handlers) CancelEvent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actor := ActorFromContext(r.Context())

	row, err := db.GetEvent(r.Context(), h.Pool, id)
	if err != nil {
		if db.IsNoRows(err) {
			writeError(w, http.StatusNotFound, "not_found", "no event with that id")
			return
		}
		slog.ErrorContext(r.Context(), "admin cancel event failed", "event_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "the lookup failed")
		return
	}

	// Already cancelled → idempotent 200. A retried admin call is
	// not a failure.
	if row.Status == db.EventStatusCancelled {
		h.logAccess(r, "cancel_event", id)
		apiutil.WriteJSON(w, http.StatusOK, map[string]any{
			"event": row,
			"note":  "already cancelled",
		})
		return
	}

	// ── Step 1: Redis first ────────────────────────────────────────
	// Take the inventory off sale BEFORE the row is cancelled. See
	// the function doc for why the order is this way round.
	//
	// The CURRENT count is read first, because if the cancel is
	// then refused by the guard, the counter has to be put back
	// exactly as it was — not as the event's initial inventory. The
	// two differ whenever a seat is currently held, and restoring
	// the wrong one would resurrect a sold seat.
	available, seatsErr := redis.AvailableSeats(r.Context(), h.Rdb, id)
	if seatsErr != nil {
		slog.ErrorContext(r.Context(), "admin cancel event: could not read the inventory — not cancelling",
			"event_id", id, "error", seatsErr)
		writeError(w, http.StatusConflict, "inventory_update_failed",
			"the inventory could not be read, so the event was not cancelled — retry")
		return
	}

	if err := h.Rdb.Del(r.Context(), redis.InventoryKey(id)).Err(); err != nil {
		// A Redis failure here means we CANNOT safely proceed: if we
		// cancel in Postgres now and the DEL is what failed, the
		// counter still says "seats available" against a cancelled
		// event — the phantom seat.
		slog.ErrorContext(r.Context(), "admin cancel event: the inventory DEL FAILED — not cancelling",
			"event_id", id, "error", err)
		writeError(w, http.StatusConflict, "inventory_update_failed",
			"the inventory could not be taken off sale, so the event was not cancelled — retry")
		return
	}
	inventoryRemoved := true

	// ── Step 2: Postgres ───────────────────────────────────────────
	// The guard lives in the UPDATE's own WHERE clause, so two
	// admins cancelling at once cannot both win (Convention #13).
	cancelled, err := db.CancelEvent(r.Context(), h.Pool, id)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrEventNotFound):
			writeError(w, http.StatusNotFound, "not_found", "no event with that id")
			return
		case errors.Is(err, db.ErrEventCancelled):
			writeError(w, http.StatusOK, "already_cancelled", "this event is already cancelled")
			return
		case errors.Is(err, db.ErrEventHasReservations):
			// The guard fired. The event is still live, so its seats
			// must still be sellable.
			//
			// But the counter has to be restored to what it WAS,
			// not to the event's initial inventory: the inventory
			// may have been decremented by holds that happened
			// before this cancel. Restoring initial_inventory would
			// RESURRECT a sold seat — the drop would report more
			// seats than it ever had, and the oversell would not
			// show up until the seat count went negative.
			//
			// The DEL removed the key, so the previous value is
			// gone. It is recovered from the reservations that
			// exist: available = initial_inventory - held_seats.
			h.restoreInventory(r, id, available)
			count, _ := db.CountEventReservations(r.Context(), h.Pool, id)
			writeError(w, http.StatusConflict, "event_has_reservations",
				fmt.Sprintf("this event has %d reservation(s) and cannot be cancelled — "+
					"an event can only be cancelled before it sells anything", count))
			return
		default:
			slog.ErrorContext(r.Context(), "admin cancel event failed", "event_id", id, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "the cancellation failed")
			return
		}
	}

	h.withSeatCount(r, &cancelled)

	h.logAccess(r, "cancel_event", id)
	apiutil.WriteJSON(w, http.StatusOK, map[string]any{
		"event":             cancelled,
		"inventory_removed": inventoryRemoved,
		"actor":             actor,
	})
}

// ── Helpers ───────────────────────────────────────────────────────────

// seedInventory puts an event's seats on sale.
//
// It uses a hard SET, not an INCR — the same rule cmd/seed-inventory
// follows: seeding defines the count, it does not add to it.
func (h *Handlers) seedInventory(r *http.Request, eventID string, seats int) error {
	if err := h.Rdb.Set(r.Context(), redis.InventoryKey(eventID), seats, 0).Err(); err != nil {
		return fmt.Errorf("admin: seed inventory for %s: %w", eventID, err)
	}
	return nil
}

// restoreInventory puts an event's inventory back on sale.
//
// It is used when a cancel is refused AFTER the inventory was already
// taken off sale: the event is still live, so its seats must still be
// sellable.
//
// `seats` is the count that was there BEFORE the cancel — NOT the
// event's initial inventory. The two differ whenever a seat is
// currently held, and restoring initial_inventory would resurrect a
// sold seat: the drop would report more seats than it ever had, and
// the oversell would not show up until the counter went negative.
//
// A failure here is logged loudly and reported — an event that is live
// but has no inventory answers "event_not_found" to every customer.
func (h *Handlers) restoreInventory(r *http.Request, eventID string, seats int) {
	if err := h.Rdb.Set(r.Context(), redis.InventoryKey(eventID), seats, 0).Err(); err != nil {
		slog.ErrorContext(r.Context(), "admin: the inventory could NOT be restored",
			"event_id", eventID, "seats", seats, "error", err)
	}
}

// withSeatCount fills in the live seat count for one row.
//
// The count comes from REDIS, not from Postgres: the inventory counter
// is the source of truth at sell-time, and a Postgres number would be
// a snapshot that is stale the moment a seat is held.
//
// A Redis failure leaves the count unknown (-1) rather than zero —
// "sold out" and "we could not read the counter" are different facts,
// and a customer must never be told an event is sold out when it is
// not.
func (h *Handlers) withSeatCount(r *http.Request, row *db.EventRow) {
	if row.Status != db.EventStatusActive {
		// A cancelled event has no inventory by definition.
		row.SeatsAvailable = 0
		row.SeatsAvailableKnown = true
		return
	}
	seats, err := redis.AvailableSeats(r.Context(), h.Rdb, row.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "admin: could not read the inventory counter",
			"event_id", row.ID, "error", err)
		row.SeatsAvailable = -1
		row.SeatsAvailableKnown = false
		return
	}
	row.SeatsAvailable = seats
	row.SeatsAvailableKnown = true
}

// withSeatCounts fills in the live seat count for every row.
func (h *Handlers) withSeatCounts(r *http.Request, rows []db.EventRow) {
	for i := range rows {
		h.withSeatCount(r, &rows[i])
	}
}

// decodeJSON reads a small JSON body into v.
//
// An empty body is an error, not an empty struct: the caller said
// "create this" and sent nothing, which is a mistake worth naming.
func decodeJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAdminBodyBytes))
	if err != nil {
		return fmt.Errorf("could not read the request body")
	}
	if len(body) == 0 {
		return fmt.Errorf("a JSON body is required")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("the body must be valid JSON")
	}
	return nil
}
