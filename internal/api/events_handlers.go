// events_handlers.go — the PUBLIC catalogue.
//
// Two endpoints, no auth:
//
//	GET /api/events        the live drops
//	GET /api/events/{id}   one live drop
//
// ── Why a public catalogue at all ──────────────────────────────────────
//
// A ticket system with no catalogue is a reservation kernel: the
// customer has to already know an opaque event id to buy anything.
// The id is a UUID now (item 9a), which is exactly right for a
// GUESSABLE-proof identifier and exactly wrong for something a
// customer is meant to browse. So the catalogue is what makes the
// UUID an implementation detail instead of a burden on the customer.
//
// ── What a customer sees ───────────────────────────────────────────────
//
// Only ACTIVE events. A cancelled event is a tombstone — a record of
// what happened, for the ops view — not something to sell.
//
// The live seat count comes from REDIS, not Postgres: the inventory
// counter is the source of truth at sell-time, and a Postgres number
// would be stale the moment a seat is held.
//
// The count is a HINT, not a promise. It can be wrong by the time the
// customer clicks, because a thousand people are clicking at once —
// that is what the 409 sold_out is for. But it is far better than
// showing nothing: a customer who sees "3 seats left" acts, and one
// who sees no number goes elsewhere.
//
// A Redis failure reports the count as UNKNOWN rather than zero. "Sold
// out" and "we could not read the counter" are different facts, and
// telling a customer an event is sold out when it is not is worse
// than saying nothing.

package api

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// ListEventsHandler serves GET /api/events — the public catalogue.
//
// The list is PAGINATED and capped, the same bounds the reservation
// listing uses: limit clamps to [1, 100] with a default of 20, and
// offset is >= 0. A catalogue endpoint with no cap is a scraping
// target.
//
// An out-of-range limit is CLAMPED, not ignored: a client asking for
// 10000 gets 100 (and is told so in the response), rather than
// silently getting the default. Ignoring it would mean a client that
// paginates with a large limit gets a different page size than it
// asked for, and never notices.
func ListEventsHandler(pool *db.Pool, rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 20
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		if limit < 1 {
			limit = 1
		}
		if limit > 100 {
			limit = 100
		}
		offset := 0
		if v := r.URL.Query().Get("offset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				offset = n
			}
		}

		// includeCancelled is FALSE — this is the public view.
		rows, err := db.ListEvents(r.Context(), pool, false, limit, offset)
		if err != nil {
			slog.ErrorContext(r.Context(), "list events failed", "error", err)
			apiutil.WriteError(w, http.StatusInternalServerError, "internal_error",
				"the catalogue is unavailable")
			return
		}

		withSeatCounts(r, rdb, rows)

		apiutil.WriteJSON(w, http.StatusOK, map[string]any{
			"events": rows,
			"count":  len(rows),
			"limit":  limit,
			"offset": offset,
		})
	}
}

// GetEventHandler serves GET /api/events/{id} — one live drop.
//
// A cancelled event answers 404 here, not 409. The public view does
// not know that the event ever existed: it is not on sale, so it is
// not there. (The admin view shows the tombstone — see
// admin.GetEvent.)
func GetEventHandler(pool *db.Pool, rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		row, err := db.GetEvent(r.Context(), pool, id)
		if err != nil {
			if db.IsNoRows(err) {
				apiutil.WriteError(w, http.StatusNotFound, "not_found",
					"no event with that id")
				return
			}
			slog.ErrorContext(r.Context(), "get event failed", "event_id", id, "error", err)
			apiutil.WriteError(w, http.StatusInternalServerError, "internal_error",
				"the lookup failed")
			return
		}
		if row.Status != db.EventStatusActive {
			// A cancelled drop is not on sale. 404, not 409 — the
			// public catalogue simply does not contain it.
			apiutil.WriteError(w, http.StatusNotFound, "not_found",
				"no event with that id")
			return
		}

		// withSeatCounts works on a slice, so the single row is
		// passed as a one-element slice and read back out. (Passing
		// it as a temporary would fill the temporary, not the row —
		// a slice is a header, and the copy is what gets written.)
		rows := []db.EventRow{row}
		withSeatCounts(r, rdb, rows)

		apiutil.WriteJSON(w, http.StatusOK, map[string]any{"event": rows[0]})
	}
}

// withSeatCounts fills in the live seat count for each row.
//
// The count comes from REDIS — the inventory counter is the source of
// truth at sell-time.
//
// A Redis failure leaves the count UNKNOWN (-1) rather than zero. A
// customer told "0 seats left" on a sold-out event goes elsewhere;
// one told "we don't know" might still try. Getting that backwards
// loses a sale.
func withSeatCounts(r *http.Request, rdb *redis.Client, rows []db.EventRow) {
	for i := range rows {
		seats, err := redis.AvailableSeats(r.Context(), rdb, rows[i].ID)
		if err != nil {
			slog.WarnContext(r.Context(), "could not read the inventory counter",
				"event_id", rows[i].ID, "error", err)
			rows[i].SeatsAvailable = -1
			rows[i].SeatsAvailableKnown = false
			continue
		}
		rows[i].SeatsAvailable = seats
		rows[i].SeatsAvailableKnown = true
	}
}
