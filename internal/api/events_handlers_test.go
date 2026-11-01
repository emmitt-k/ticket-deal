package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// These tests need a real Postgres and a real Redis — the catalogue's
// correctness is about which events it shows and what the live seat
// count says, and neither is provable against a fake.

// catalogueRouter mounts just the two public catalogue routes.
func catalogueRouter(t *testing.T) (chi.Router, *db.Pool, *redis.Client) {
	t.Helper()
	pool := apiNewTestPool(t)
	rdb := newTestClient(t)

	r := chi.NewRouter()
	r.Get("/api/events", ListEventsHandler(pool, rdb))
	r.Get("/api/events/{id}", GetEventHandler(pool, rdb))
	return r, pool, rdb
}

// seedCatalogueEvent creates an event with inventory and cleans it up.
func seedCatalogueEvent(t *testing.T, pool *db.Pool, rdb *redis.Client, name string, inv int) string {
	t.Helper()
	price := int64(2500)
	row, err := db.CreateEvent(context.Background(), pool, db.EventCreate{
		Name:             name,
		InitialInventory: inv,
		PriceCents:       &price,
		Currency:         "usd",
	})
	require.NoError(t, err)

	require.NoError(t, rdb.Set(context.Background(), redis.InventoryKey(row.ID), inv, 0).Err())

	t.Cleanup(func() {
		ctx := context.Background()
		_ = rdb.Del(ctx, redis.InventoryKey(row.ID)).Err()
		_, _ = pool.Exec(ctx, "DELETE FROM events WHERE id = $1", row.ID)
	})
	return row.ID
}

// TestListEvents_PublicCatalogue — the shop window.
//
// It shows the live events, it does not show the cancelled ones, and
// it reports the live seat count.
func TestListEvents_PublicCatalogue(t *testing.T) {
	r, pool, rdb := catalogueRouter(t)

	live := seedCatalogueEvent(t, pool, rdb, "On Sale", 25)
	dead := seedCatalogueEvent(t, pool, rdb, "Sold Out (cancelled)", 25)

	// Cancel one of them.
	_, err := db.CancelEvent(context.Background(), pool, dead)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Events []db.EventRow `json:"events"`
		Count  int           `json:"count"`
		Limit  int           `json:"limit"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	for _, e := range resp.Events {
		require.NotEqual(t, dead, e.ID,
			"a cancelled event must not appear in the public catalogue")
	}

	// The live one IS there, with its seat count.
	var found *db.EventRow
	for i := range resp.Events {
		if resp.Events[i].ID == live {
			found = &resp.Events[i]
		}
	}
	require.NotNil(t, found, "the live event must be in the catalogue")
	require.Equal(t, 25, found.SeatsAvailable)
	require.True(t, found.SeatsAvailableKnown)
	require.Equal(t, "On Sale", found.Name)
	require.Equal(t, int64(2500), found.PriceCents)
}

// TestListEvents_ReturnsAnEmptyArrayNotNull — the JSON shape.
//
// A nil slice serialises to `null`, and a client doing
// `response.events.forEach(...)` gets a TypeError on a catalogue that
// is merely empty. An empty catalogue is a valid answer and it must
// be `[]`.
//
// The catalogue cannot be emptied here — every package in this repo
// shares ONE Postgres database, and `go test ./...` runs them in
// parallel, so other packages' events are in the table. What matters
// is the SHAPE, and the shape is the same whether the array holds
// eight rows or none: it must be `[]` and never `null`.
func TestListEvents_ReturnsAnEmptyArrayNotNull(t *testing.T) {
	r, _, _ := catalogueRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), `"events":null`,
		"the catalogue must never be null — an empty catalogue is []")
	require.Contains(t, rec.Body.String(), `"events":[`,
		"the catalogue must be a JSON array")
}

// TestGetEvent_PublicRead — one live drop.
func TestGetEvent_PublicRead(t *testing.T) {
	r, pool, rdb := catalogueRouter(t)
	id := seedCatalogueEvent(t, pool, rdb, "Readable Drop", 12)

	req := httptest.NewRequest(http.MethodGet, "/api/events/"+id, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Event db.EventRow `json:"event"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, id, resp.Event.ID)
	require.Equal(t, "Readable Drop", resp.Event.Name)
	require.Equal(t, 12, resp.Event.SeatsAvailable)
}

// TestGetEvent_CancelledIs404 — the public view does not know the
// drop ever existed.
//
// A cancelled event is a tombstone for the ops view. The public
// catalogue is a shop window, and a cancelled drop is not in it — so
// it is a 404, not a 409 that would tell a customer the drop used to
// exist and then went away.
func TestGetEvent_CancelledIs404(t *testing.T) {
	r, pool, rdb := catalogueRouter(t)
	id := seedCatalogueEvent(t, pool, rdb, "Gone", 10)

	_, err := db.CancelEvent(context.Background(), pool, id)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/events/"+id, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

func TestGetEvent_NotFound(t *testing.T) {
	r, _, _ := catalogueRouter(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/events/00000000-0000-4000-8000-0000000000ff", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestListEvents_PaginationIsCapped — the catalogue is not a
// scraping target.
//
// The bounds match the reservation listing: limit clamps to [1, 100]
// with a default of 20, offset is >= 0. A catalogue endpoint with no
// cap is a free data dump.
func TestListEvents_PaginationIsCapped(t *testing.T) {
	r, pool, rdb := catalogueRouter(t)
	for i := 0; i < 3; i++ {
		seedCatalogueEvent(t, pool, rdb, "Bulk", 5)
	}

	// A limit of 10000 is clamped to 100.
	req := httptest.NewRequest(http.MethodGet, "/api/events?limit=10000", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Limit int `json:"limit"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 100, resp.Limit)

	// A limit of 0 clamps up to 1 — the minimum.
	req = httptest.NewRequest(http.MethodGet, "/api/events?limit=0", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 1, resp.Limit)
}
