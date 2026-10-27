package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/queue"
)

// getReservationTestSecret is a fixed 32-byte secret used across all
// handler tests in this package. Mirrors testSecret from
// reserve_handler_test.go; declared independently because Go test
// helpers are package-scoped.
var getReservationTestSecret = []byte("test-secret-not-a-real-secret-1234567890")

// ── Helpers: routing the request through the chi URL semantics ──────────

// doGetReservation wraps GetReservationHandler in auth.Middleware,
// mints a JWT for userID, and fires a GET against /api/reservations/{id}.
// Returns the response + parsed JSON body. Mirrors doReserve from
// reserve_handler_test.go.
func doGetReservation(t *testing.T, pool *db.Pool, userID, reservationID string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	// Mounted on a real chi router so chi.URLParam(r, "id") resolves
	// the way it does in production — see testsupport_test.go.
	handler := apiTestRouter(t, getReservationTestSecret, func(r chi.Router) {
		r.Get("/reservations/{id}", GetReservationHandler(pool))
	})

	req := httptest.NewRequest(http.MethodGet, "/api/reservations/"+reservationID, nil)
	if userID != "" {
		token, err := auth.Issue(userID, 1, getReservationTestSecret, 2*time.Minute)
		require.NoError(t, err, "Issue test JWT")
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var parsed map[string]any
	body := apiBodyBytes(t, rec)
	if len(body) > 0 {
		_ = json.Unmarshal(body, &parsed)
	}
	return rec, parsed
}

// doListMyReservations wraps ListMyReservationsHandler in
// auth.Middleware, mints a JWT for userID, and fires a GET against
// /api/me/reservations?{query}. Returns the response + parsed array body.
func doListMyReservations(t *testing.T, pool *db.Pool, userID, query string) (*httptest.ResponseRecorder, []map[string]any) {
	t.Helper()
	handler := apiTestRouter(t, getReservationTestSecret, func(r chi.Router) {
		r.Get("/me/reservations", ListMyReservationsHandler(pool))
	})

	url := "/api/me/reservations"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if userID != "" {
		token, err := auth.Issue(userID, 1, getReservationTestSecret, 2*time.Minute)
		require.NoError(t, err, "Issue test JWT")
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var parsed []map[string]any
	body := apiBodyBytes(t, rec)
	if len(body) > 0 {
		_ = json.Unmarshal(body, &parsed)
	}
	return rec, parsed
}

// ── Helpers: Postgres + seed (duplicated from internal/db so each
//    package owns its own; the price of cross-package test isolation) ──

func apiNewTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://tickets:tickets@localhost:5432/tickets?sslmode=disable"
	}
	p, err := db.NewPool(context.Background(), db.Config{
		DSN:      dsn,
		MaxConns: 3,
		MinConns: 1,
	})
	if err != nil {
		t.Skipf("Postgres unavailable: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func apiSeedEvent(t *testing.T, pool *db.Pool, eventID int64) {
	t.Helper()
	name := "test-event-" + uuid.NewString()
	_, err := pool.Exec(context.Background(),
		"INSERT INTO events (id, name, initial_inventory) VALUES ($1, $2, 100)",
		eventID, name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", eventID)
	})
}

// apiCleanupReservation deletes the test row (and its audit rows) so
// each test starts from a clean slate.
//
// The audit rows go first, and they need the purge escape hatch:
// reservation_events is append-only (see migrations/005_audit_log.sql),
// so a plain DELETE is refused by the trigger. SET LOCAL scopes the
// override to this transaction only — it can't leak to another pooled
// connection.
func apiCleanupReservation(t *testing.T, pool *db.Pool, reservationID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		tx, err := pool.Begin(ctx)
		if err == nil {
			_, _ = tx.Exec(ctx, "SET LOCAL ticket_deal.audit_purge = 'on'")
			_, _ = tx.Exec(ctx, "DELETE FROM reservation_events WHERE reservation_id = $1", reservationID)
			_ = tx.Commit(ctx)
		}
		_, _ = pool.Exec(ctx,
			"DELETE FROM reservations WHERE reservation_id = $1", reservationID)
	})
}

// apiValidReservationFor builds a queue.Reservation with a chosen user_id.
// Mirrors the same-named helper in internal/db/list_reservations_test.go.
func apiValidReservationFor(t *testing.T, eventID int64, userID string) queue.Reservation {
	t.Helper()
	now := time.Now().UTC()
	return queue.Reservation{
		ReservationID: uuid.NewString(),
		UserID:        userID,
		EventID:       eventID,
		Seats:         1,
		CreatedAt:     now.Format(time.RFC3339),
		ExpiresAt:     now.Add(10 * time.Minute).Format(time.RFC3339),
	}
}

// newGetReservationPool wraps apiNewTestPool + apiSeedEvent +
// apiCleanupReservation + a single InsertIfAbsent for `owner`. Returns
// the pool, event id, and inserted reservation id.
func newGetReservationPool(t *testing.T, owner string) (pool *db.Pool, eventID int64, reservationID string) {
	t.Helper()
	pool = apiNewTestPool(t)
	const id = int64(64001)
	apiSeedEvent(t, pool, id)
	r := apiValidReservationFor(t, id, owner)
	apiCleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, db.InsertIfAbsent(context.Background(), pool, r))
	return pool, id, r.ReservationID
}

// ── GET /api/reservations/{id} ─────────────────────────────────────────

// TestGetReservation_HappyPath — row owner hits the endpoint with
// their own JWT. 200 + JSON with all expected fields populated, including
// event_name (the JOIN).
func TestGetReservation_HappyPath(t *testing.T) {
	const owner = "u-happy-1"
	pool, eventID, rid := newGetReservationPool(t, owner)

	rec, body := doGetReservation(t, pool, owner, rid)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, owner, body["user_id"])
	require.Equal(t, rid, body["reservation_id"])
	require.Equal(t, float64(eventID), body["event_id"], "encoding/json parses int64 as float64")
	require.Equal(t, db.StatusPendingPayment, body["status"])
	require.NotEmpty(t, body["event_name"], "LEFT JOIN events should populate event_name")
}

// TestGetReservation_Forbidden_CrossUser — another user's JWT against
// this row. 403 not_your_reservation. (404 vs 403 — see handler comment.)
func TestGetReservation_Forbidden_CrossUser(t *testing.T) {
	const owner = "u-owner-1"
	const other = "u-other-2"
	pool, _, rid := newGetReservationPool(t, owner)

	rec, body := doGetReservation(t, pool, other, rid)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "not_your_reservation", body["error"])
}

// TestGetReservation_NotFound — happy JWT, real-but-unknown UUID.
// 404 reservation_not_found.
func TestGetReservation_NotFound(t *testing.T) {
	const owner = "u-nf-1"
	pool, _, _ := newGetReservationPool(t, owner)
	bogusID := uuid.NewString() // UUID format matches, no row in DB

	rec, body := doGetReservation(t, pool, owner, bogusID)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "reservation_not_found", body["error"])
}

// TestGetReservation_MissingJWT — no Authorization header. The auth
// middleware rejects before we reach the handler. 401.
func TestGetReservation_MissingJWT(t *testing.T) {
	const owner = "u-noauth-1"
	pool, _, rid := newGetReservationPool(t, owner)

	rec, _ := doGetReservation(t, pool, "" /* no JWT */, rid)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// ── GET /api/me/reservations ───────────────────────────────────────────

// TestListMyReservations_Empty — caller has no reservations yet.
// 200 + empty array (NOT 404). Decoder: an empty body returns nil;
// the handler guards against that with the explicit empty-slice
// coercion so JSON is always `[]` not `null`.
func TestListMyReservations_Empty(t *testing.T) {
	const owner = "u-empty-1"
	pool := apiNewTestPool(t)

	rec, rows := doListMyReservations(t, pool, owner, "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, rows, "must be a JSON array, even when empty — not null")
	require.Len(t, rows, 0)
}

// TestListMyReservations_HappyPath — 3 rows for one user, all returned
// in newest-first order, with event_name populated.
func TestListMyReservations_HappyPath(t *testing.T) {
	const owner = "u-list-1"
	pool := apiNewTestPool(t)
	const eventID = int64(64002)
	apiSeedEvent(t, pool, eventID)

	ids := make([]string, 3)
	for i := 0; i < 3; i++ {
		r := apiValidReservationFor(t, eventID, owner)
		apiCleanupReservation(t, pool, r.ReservationID)
		require.NoError(t, db.InsertIfAbsent(context.Background(), pool, r))
		ids[i] = r.ReservationID
	}

	rec, rows := doListMyReservations(t, pool, owner, "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, rows, 3)

	// Newest first → ids[2] (last inserted) sits at top.
	gotIDs := []string{
		rows[0]["reservation_id"].(string),
		rows[1]["reservation_id"].(string),
		rows[2]["reservation_id"].(string),
	}
	require.Equal(t, []string{ids[2], ids[1], ids[0]}, gotIDs,
		"rows should be ordered created_at DESC")

	for _, r := range rows {
		require.Equal(t, owner, r["user_id"])
		require.Equal(t, db.StatusPendingPayment, r["status"])
		require.NotEmpty(t, r["event_name"])
	}
}

// TestListMyReservations_StatusFilter — ?status= scopes the query to
// one status; default (no param) returns all rows.
func TestListMyReservations_StatusFilter(t *testing.T) {
	const owner = "u-filter-1"
	pool := apiNewTestPool(t)
	const eventID = int64(64003)
	apiSeedEvent(t, pool, eventID)

	r1 := apiValidReservationFor(t, eventID, owner)
	apiCleanupReservation(t, pool, r1.ReservationID)
	require.NoError(t, db.InsertIfAbsent(context.Background(), pool, r1))

	r2 := apiValidReservationFor(t, eventID, owner)
	apiCleanupReservation(t, pool, r2.ReservationID)
	require.NoError(t, db.InsertIfAbsent(context.Background(), pool, r2))

	// Flip r1 → EXPIRED so we can filter by status.
	changed, err := db.TransitionStatus(context.Background(), pool,
		r1.ReservationID, db.StatusPendingPayment, db.StatusExpired, db.Audit{Actor: db.UserActor("test-user")})
	require.NoError(t, err)
	require.True(t, changed)

	// No filter → both rows.
	_, all := doListMyReservations(t, pool, owner, "")
	require.Len(t, all, 2)

	// ?status=PENDING_PAYMENT → only r2.
	rec, pending := doListMyReservations(t, pool, owner, "status=PENDING_PAYMENT")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, pending, 1)
	require.Equal(t, r2.ReservationID, pending[0]["reservation_id"])

	// ?status=EXPIRED → only r1.
	rec, expired := doListMyReservations(t, pool, owner, "status=EXPIRED")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, expired, 1)
	require.Equal(t, r1.ReservationID, expired[0]["reservation_id"])
}

// TestListMyReservations_Pagination — limit + offset slice the
// result set correctly; combined pages cover the full set without
// duplication.
func TestListMyReservations_Pagination(t *testing.T) {
	const owner = "u-page-1"
	pool := apiNewTestPool(t)
	const eventID = int64(64004)
	apiSeedEvent(t, pool, eventID)

	allIDs := make(map[string]bool)
	for i := 0; i < 4; i++ {
		r := apiValidReservationFor(t, eventID, owner)
		apiCleanupReservation(t, pool, r.ReservationID)
		require.NoError(t, db.InsertIfAbsent(context.Background(), pool, r))
		allIDs[r.ReservationID] = true
	}

	_, page1 := doListMyReservations(t, pool, owner, "limit=2&offset=0")
	require.Len(t, page1, 2)

	_, page2 := doListMyReservations(t, pool, owner, "limit=2&offset=2")
	require.Len(t, page2, 2)

	// Combined: 4 distinct ids, no duplicates.
	seen := map[string]bool{}
	for _, r := range append(page1, page2...) {
		seen[r["reservation_id"].(string)] = true
	}
	require.Len(t, seen, 4)
	require.Equal(t, allIDs, seen)
}

// TestListMyReservations_InvalidStatus — ?status=NOT_A_STATUS is
// rejected at the API with 400 invalid_status (not silently empty,
// which would mask typos).
func TestListMyReservations_InvalidStatus(t *testing.T) {
	const owner = "u-badstatus-1"
	pool := apiNewTestPool(t)

	rec, _ := doListMyReservations(t, pool, owner, "status=NOT_A_STATUS")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	// Error responses are {error, message} objects, not arrays — decode here.
	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	require.Equal(t, "invalid_status", body["error"])
}

// TestListMyReservations_MissingJWT — no token → 401 (middleware).
func TestListMyReservations_MissingJWT(t *testing.T) {
	pool := apiNewTestPool(t)

	rec, _ := doListMyReservations(t, pool, "" /* no JWT */, "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestListMyReservations_DoesNotLeak — the JWT subject scopes the
// query; user B asking sees only their rows, not user A's.
func TestListMyReservations_DoesNotLeak(t *testing.T) {
	const owner = "u-leak-1"
	const other = "u-leak-other-1"
	pool := apiNewTestPool(t)
	const eventID = int64(64005)
	apiSeedEvent(t, pool, eventID)

	// 3 rows for owner, 1 for other.
	for i := 0; i < 3; i++ {
		r := apiValidReservationFor(t, eventID, owner)
		apiCleanupReservation(t, pool, r.ReservationID)
		require.NoError(t, db.InsertIfAbsent(context.Background(), pool, r))
	}
	rB := apiValidReservationFor(t, eventID, other)
	apiCleanupReservation(t, pool, rB.ReservationID)
	require.NoError(t, db.InsertIfAbsent(context.Background(), pool, rB))

	_, ownerRows := doListMyReservations(t, pool, owner, "")
	require.Len(t, ownerRows, 3)
	for _, r := range ownerRows {
		require.Equal(t, owner, r["user_id"])
	}

	_, otherRows := doListMyReservations(t, pool, other, "")
	require.Len(t, otherRows, 1)
	require.Equal(t, other, otherRows[0]["user_id"])
}

// TestListMyReservations_RefundedIsItsOwnStatus — a REFUNDED
// reservation is NOT a CANCELLED one, and the filter has to keep them
// apart: `?status=CANCELLED` must not return a refunded reservation,
// and `?status=REFUNDED` must be a valid filter value at all.
//
// This is the user-visible half of the "refund is not a cancel" fix —
// before it, a customer who was refunded showed up under CANCELLED.
func TestListMyReservations_RefundedIsItsOwnStatus(t *testing.T) {
	const owner = "u-refunded-filter-1"
	pool := apiNewTestPool(t)
	const eventID = int64(64004)
	apiSeedEvent(t, pool, eventID)

	// One cancelled (never paid), one refunded (paid, then reversed).
	cancelled := apiValidReservationFor(t, eventID, owner)
	apiCleanupReservation(t, pool, cancelled.ReservationID)
	require.NoError(t, db.InsertIfAbsent(context.Background(), pool, cancelled))

	refunded := apiValidReservationFor(t, eventID, owner)
	apiCleanupReservation(t, pool, refunded.ReservationID)
	require.NoError(t, db.InsertIfAbsent(context.Background(), pool, refunded))

	changed, err := db.TransitionStatus(context.Background(), pool,
		cancelled.ReservationID, db.StatusPendingPayment, db.StatusCancelled, db.Audit{Actor: db.UserActor("test-user")})
	require.NoError(t, err)
	require.True(t, changed)

	changed, err = db.TransitionStatus(context.Background(), pool,
		refunded.ReservationID, db.StatusPendingPayment, db.StatusConfirmed, db.Audit{Actor: db.UserActor("test-user")})
	require.NoError(t, err)
	require.True(t, changed)

	changed, err = db.TransitionStatus(context.Background(), pool,
		refunded.ReservationID, db.StatusConfirmed, db.StatusRefunded, db.Audit{Actor: db.StripeWebhookActor("evt_test")})
	require.NoError(t, err)
	require.True(t, changed)

	// ?status=REFUNDED is a valid filter → only the refunded row.
	rec, byRefunded := doListMyReservations(t, pool, owner, "status=REFUNDED")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, byRefunded, 1)
	require.Equal(t, refunded.ReservationID, byRefunded[0]["reservation_id"])

	// ?status=CANCELLED → only the cancelled row. The refunded one must
	// NOT leak in here.
	rec, byCancelled := doListMyReservations(t, pool, owner, "status=CANCELLED")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, byCancelled, 1)
	require.Equal(t, cancelled.ReservationID, byCancelled[0]["reservation_id"])

	// No filter → both.
	_, all := doListMyReservations(t, pool, owner, "")
	require.Len(t, all, 2)
}
