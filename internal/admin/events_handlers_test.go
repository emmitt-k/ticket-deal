package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// These tests need a real Postgres and a real Redis — the admin
// event API's correctness is about what it does to the seat count
// across TWO stores, and that is not provable against a fake.

// ── The lifecycle ──────────────────────────────────────────────────────

// TestCreateEvent_SeedsTheInventory is the all-or-nothing rule, from
// the happy side.
//
// The event lands in Postgres AND its inventory lands in Redis. The
// second write is what makes the drop sellable — without it, every
// reserve against the new event answers "event_not_found".
func TestCreateEvent_SeedsTheInventory(t *testing.T) {
	f := newFixture(t)

	body := []byte(`{"name":"Fresh Drop","initial_inventory":50,"price_cents":4200,"currency":"usd"}`)
	rec := f.do(t, http.MethodPost, "/admin/events", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp struct {
		Event db.EventRow `json:"event"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	require.NotEmpty(t, resp.Event.ID)
	require.Equal(t, "Fresh Drop", resp.Event.Name)
	require.Equal(t, db.EventStatusActive, resp.Event.Status)
	require.Equal(t, 50, resp.Event.InitialInventory)
	require.Equal(t, int64(4200), resp.Event.PriceCents)
	require.Equal(t, 50, resp.Event.SeatsAvailable)
	require.True(t, resp.Event.SeatsAvailableKnown)

	// The inventory is really in Redis.
	n, err := f.rdb.Get(context.Background(), redis.InventoryKey(resp.Event.ID)).Int()
	require.NoError(t, err)
	require.Equal(t, 50, n)

	// And the row is really in Postgres.
	row, err := db.GetEvent(context.Background(), f.pool, resp.Event.ID)
	require.NoError(t, err)
	require.Equal(t, "Fresh Drop", row.Name)

	f.cleanupEvent(t, resp.Event.ID)
}

// TestCreateEvent_TheIdIsMintedByTheServer — the caller does not
// supply the id.
//
// The id is a UUID minted by the application (item 9a), and there is
// exactly one way an event id comes into existence. A client that
// could name the id would break that.
func TestCreateEvent_TheIdIsMintedByTheServer(t *testing.T) {
	f := newFixture(t)

	body := []byte(`{"name":"Server Minted","initial_inventory":10,"id":"00000000-0000-0000-0000-000000000001"}`)
	rec := f.do(t, http.MethodPost, "/admin/events", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp struct {
		Event db.EventRow `json:"event"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEqual(t, "00000000-0000-0000-0000-000000000001", resp.Event.ID,
		"the server must mint the id, not accept one")
	require.Len(t, resp.Event.ID, 36)

	f.cleanupEvent(t, resp.Event.ID)
}

func TestCreateEvent_RejectsBadInput(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"no_name", `{"initial_inventory":10}`, http.StatusBadRequest},
		{"empty_body", ``, http.StatusBadRequest},
		{"not_json", `nonsense`, http.StatusBadRequest},
		{"negative_inventory", `{"name":"x","initial_inventory":-1}`, http.StatusBadRequest},
		{"negative_price", `{"name":"x","price_cents":-5}`, http.StatusBadRequest},
		{"bad_currency", `{"name":"x","currency":"US"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, "/admin/events", []byte(tc.body))
			require.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}
}

// TestCreateEvent_DefaultsAreUsable — an event created with nothing
// but a name must still be sellable.
//
// The price default is $25.00 (a $0.00 event is a dead end for
// Stripe) and the inventory default is 0 seats. Both come from the
// COLUMN defaults, so the caller does not have to know them.
func TestCreateEvent_DefaultsAreUsable(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodPost, "/admin/events", []byte(`{"name":"Minimal"}`))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp struct {
		Event db.EventRow `json:"event"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, int64(2500), resp.Event.PriceCents)
	require.Equal(t, "usd", resp.Event.Currency)
	require.Equal(t, 0, resp.Event.InitialInventory)

	f.cleanupEvent(t, resp.Event.ID)
}

// ── Read ───────────────────────────────────────────────────────────────

// TestListEvents_ShowsCancelledButThePublicDoesNot — the admin view
// includes the tombstones, the public one does not.
func TestListEvents_ShowsCancelledButThePublicDoesNot(t *testing.T) {
	f := newFixture(t)

	// One live event and one cancelled one.
	live := f.createEvent(t, "Live Drop", 10)
	dead := f.createEvent(t, "Dead Drop", 10)

	rec := f.do(t, http.MethodPost, "/admin/events/"+dead+"/cancel", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The admin view shows both. (The fixture's own event is in the
	// table too, so this asserts on MEMBERSHIP rather than on a
	// count — the count would be a brittle assertion that breaks
	// whenever the fixture grows.)
	rec = f.do(t, http.MethodGet, "/admin/events", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var adminResp struct {
		Events []db.EventRow `json:"events"`
		Count  int           `json:"count"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &adminResp))
	require.True(t, containsEvent(adminResp.Events, dead),
		"the admin view must show the tombstone")
	require.True(t, containsEvent(adminResp.Events, live))
	require.Equal(t, len(adminResp.Events), adminResp.Count,
		"count must match the number of events returned")

	f.cleanupEvent(t, live)
	f.cleanupEvent(t, dead)
}

func TestGetEvent(t *testing.T) {
	f := newFixture(t)
	id := f.createEvent(t, "Readable", 3)

	rec := f.do(t, http.MethodGet, "/admin/events/"+id, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Event db.EventRow `json:"event"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, id, resp.Event.ID)
	require.Equal(t, "Readable", resp.Event.Name)
	require.Equal(t, 3, resp.Event.SeatsAvailable)

	f.cleanupEvent(t, id)
}

func TestGetEvent_NotFound(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodGet, "/admin/events/00000000-0000-4000-8000-000000000009", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// ── Update ─────────────────────────────────────────────────────────────

func TestPatchEvent_ChangesWhatItIsGiven(t *testing.T) {
	f := newFixture(t)
	id := f.createEvent(t, "Before", 10)

	rec := f.do(t, http.MethodPatch, "/admin/events/"+id,
		[]byte(`{"name":"After","price_cents":3300}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Event db.EventRow `json:"event"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "After", resp.Event.Name)
	require.Equal(t, int64(3300), resp.Event.PriceCents)
	// The currency was not supplied, so it must be unchanged.
	require.Equal(t, "usd", resp.Event.Currency)

	f.cleanupEvent(t, id)
}

// TestPatchEvent_AddSeatsGoesToTheLiveCounter — adding seats changes
// REDIS, not Postgres.
//
// The live counter is the source of truth at sell-time. Postgres's
// initial_inventory is the event's HISTORY ("this drop was created
// with 100 seats"), and the counter is the PRESENT ("there are 137
// left"). Keeping them separate means the two can never disagree
// about what the event was.
func TestPatchEvent_AddSeatsGoesToTheLiveCounter(t *testing.T) {
	f := newFixture(t)
	id := f.createEvent(t, "Adding Seats", 10)

	rec := f.do(t, http.MethodPatch, "/admin/events/"+id, []byte(`{"add_seats":25}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Event      db.EventRow `json:"event"`
		SeatsAdded int         `json:"seats_added"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 25, resp.SeatsAdded)
	require.Equal(t, 35, resp.Event.SeatsAvailable, "10 + 25 seats on sale")

	// Postgres still says the event was CREATED with 10.
	row, err := db.GetEvent(context.Background(), f.pool, id)
	require.NoError(t, err)
	require.Equal(t, 10, row.InitialInventory,
		"initial_inventory is the event's history and must not be rewritten")

	f.cleanupEvent(t, id)
}

func TestPatchEvent_RefusesACancelledEvent(t *testing.T) {
	f := newFixture(t)
	id := f.createEvent(t, "Tombstone", 10)

	rec := f.do(t, http.MethodPost, "/admin/events/"+id+"/cancel", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = f.do(t, http.MethodPatch, "/admin/events/"+id, []byte(`{"name":"Rewritten"}`))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	// The tombstone still says what it always said.
	row, err := db.GetEvent(context.Background(), f.pool, id)
	require.NoError(t, err)
	require.Equal(t, "Tombstone", row.Name)

	f.cleanupEvent(t, id)
}

// ── Cancel ─────────────────────────────────────────────────────────────

// TestCancelEvent_SucceedsAndTombstones — the row is never deleted.
func TestCancelEvent_SucceedsAndTombstones(t *testing.T) {
	f := newFixture(t)
	id := f.createEvent(t, "Doomed", 10)

	rec := f.do(t, http.MethodPost, "/admin/events/"+id+"/cancel", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Event db.EventRow `json:"event"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, db.EventStatusCancelled, resp.Event.Status)

	// The row is still there — a tombstone, not a deletion.
	row, err := db.GetEvent(context.Background(), f.pool, id)
	require.NoError(t, err)
	require.Equal(t, db.EventStatusCancelled, row.Status)

	f.cleanupEvent(t, id)
}

// TestCancelEvent_RefusedWhenItHasReservations — the guard.
func TestCancelEvent_RefusedWhenItHasReservations(t *testing.T) {
	f := newFixture(t)
	id := f.createEvent(t, "Sold", 10)

	_, _ = f.hold(t, id, f.user("cancel-guard"), 1)

	rec := f.do(t, http.MethodPost, "/admin/events/"+id+"/cancel", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "event_has_reservations", body["error"])
	require.Contains(t, body["message"], "1 reservation")

	// The event is still live AND still sellable — the refused cancel
	// must not have taken it off sale.
	row, err := db.GetEvent(context.Background(), f.pool, id)
	require.NoError(t, err)
	require.Equal(t, db.EventStatusActive, row.Status)

	n, err := f.rdb.Get(context.Background(), redis.InventoryKey(id)).Int()
	require.NoError(t, err)
	require.Equal(t, 9, n, "a refused cancel must leave the inventory alone")

	f.cleanupEvent(t, id)
}

// TestCancelEvent_AlreadyCancelledIsIdempotent — a retried admin call
// is not a failure.
func TestCancelEvent_AlreadyCancelledIsIdempotent(t *testing.T) {
	f := newFixture(t)
	id := f.createEvent(t, "Twice", 10)

	rec := f.do(t, http.MethodPost, "/admin/events/"+id+"/cancel", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = f.do(t, http.MethodPost, "/admin/events/"+id+"/cancel", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	f.cleanupEvent(t, id)
}

// TestCancelEvent_TakesTheInventoryOffSale — Redis first.
//
// The order matters: Redis FIRST, then Postgres. If the Postgres write
// fails after the Redis DEL, the event is live but has no inventory —
// so reserve.lua answers "event_not_found" and NO seat is sold. The
// other order would leave Redis reporting seats available against a
// cancelled event, and a seat would go missing with no reservation
// row to explain it.
func TestCancelEvent_TakesTheInventoryOffSale(t *testing.T) {
	f := newFixture(t)
	id := f.createEvent(t, "Off Sale", 10)

	rec := f.do(t, http.MethodPost, "/admin/events/"+id+"/cancel", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The counter is gone.
	exists, err := f.rdb.Exists(context.Background(), redis.InventoryKey(id)).Result()
	require.NoError(t, err)
	require.Equal(t, int64(0), exists,
		"a cancelled event must have no inventory counter")

	// And reserving against it is refused, not oversold.
	res, err := redis.ReserveSeat(context.Background(), f.rdb, id, f.user("after-cancel"), 600, 1)
	require.NoError(t, err)
	require.Equal(t, redis.StatusEventNotFound, res.Status,
		"a cancelled event must sell no seats")

	f.cleanupEvent(t, id)
}

// ── Harmonization ──────────────────────────────────────────────────────

// TestEventRoutes_RequireTheSignature — the whole /admin tree sits
// behind the verifier, and the new routes are no exception.
func TestEventRoutes_RequireTheSignature(t *testing.T) {
	f := newFixture(t)

	for _, tc := range []struct {
		method string
		uri    string
	}{
		{http.MethodPost, "/admin/events"},
		{http.MethodGet, "/admin/events"},
		{http.MethodGet, "/admin/events/abc"},
		{http.MethodPatch, "/admin/events/abc"},
		{http.MethodPost, "/admin/events/abc/cancel"},
	} {
		t.Run(tc.method+" "+tc.uri, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.uri, nil)
			rec := httptest.NewRecorder()
			f.router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusUnauthorized, rec.Code,
				"an unsigned admin request must be refused")
		})
	}
}

// TestEventRoutes_AreNotMountedWithoutKeys — the feature is OFF, not
// OPEN. The same shape as the optional Stripe integration.
func TestEventRoutes_AreNotMountedWithoutKeys(t *testing.T) {
	r := chi.NewRouter()
	h := &Handlers{Pool: newTestPool(t), Rdb: newTestRedis(t), Verifier: NewVerifier(nil)}
	h.Register(r)

	req := httptest.NewRequest(http.MethodGet, "/admin/events", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestErrorShapeMatchesTheExistingAdminAPI — the harmonization
// requirement, stated as a test.
//
// Every admin error is {"error": code, "message": text}. A new
// endpoint that invents its own shape is a new endpoint every client
// has to special-case.
func TestErrorShapeMatchesTheExistingAdminAPI(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodGet, "/admin/events/00000000-0000-4000-8000-000000000009", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body, 2, "the error body has exactly two keys")
	require.Contains(t, body, "error")
	require.Contains(t, body, "message")
	require.Equal(t, "not_found", body["error"])
}

// ── Helpers ────────────────────────────────────────────────────────────

// createEvent creates an event through the API and returns its id.
func (f *fixture) createEvent(t *testing.T, name string, inventory int) string {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"initial_inventory":%d}`, name, inventory)
	rec := f.do(t, http.MethodPost, "/admin/events", []byte(body))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp struct {
		Event db.EventRow `json:"event"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Event.ID
}

// cleanupEvent removes an event and its inventory.
func (f *fixture) cleanupEvent(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_ = f.rdb.Del(ctx, redis.InventoryKey(id)).Err()
		_, _ = f.pool.Exec(ctx, "DELETE FROM events WHERE id = $1", id)
	})
}

func containsEvent(rows []db.EventRow, id string) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}
