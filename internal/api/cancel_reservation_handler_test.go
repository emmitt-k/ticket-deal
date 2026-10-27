package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// doCancelReservation wraps CancelReservationHandler in auth.Middleware,
// mints a JWT for userID, and POSTs /api/reservations/{id}/cancel. Returns
// the recorder + the parsed JSON body.
//
// The auth.Middleware wrapper is included so claims-injection goes through
// the real production path — handler tests then verify only the cancel
// logic, not the middleware (covered separately in middleware_test.go).
func doCancelReservation(t *testing.T, pool *db.Pool, rdb *redis.Client,
	userID, reservationID string,
) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	// Mounted on a real chi router so chi.URLParam(r, "id") resolves
	// the way it does in production — see testsupport_test.go.
	handler := apiTestRouter(t, testSecret, func(r chi.Router) {
		r.Post("/reservations/{id}/cancel", CancelReservationHandler(pool, rdb))
	})
	url := "/api/reservations/" + reservationID + "/cancel"
	req := httptest.NewRequest(http.MethodPost, url, nil)
	if userID != "" {
		token, err := auth.Issue(userID, 1, testSecret, 2*time.Minute)
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

// inventoryCount reads the current value of the event's inventory key.
// Used to assert that cancel incremented it by the expected amount.
func inventoryCount(t *testing.T, rdb *redis.Client, eventID int64) int {
	t.Helper()
	v, err := rdb.Get(context.Background(), redis.InventoryKey(eventID)).Int()
	if err != nil {
		// ErrNoRows means the key doesn't exist yet — count is 0.
		if err.Error() == "redis: nil" {
			return 0
		}
		t.Fatalf("read inventory: %v", err)
	}
	return v
}

// hasHoldKey reports whether the hold key for (eventID, userID) currently
// exists in Redis. Used to assert that cancel cleaned it up.
func hasHoldKey(t *testing.T, rdb *redis.Client, eventID int64, userID string) bool {
	t.Helper()
	n, err := rdb.Exists(context.Background(), redis.HoldKey(eventID, userID)).Result()
	if err != nil {
		t.Fatalf("exists check: %v", err)
	}
	return n > 0
}

// ── Happy path ─────────────────────────────────────────────────────────

// TestCancel_HappyPath — owner cancels a PENDING_PAYMENT reservation.
// Expect: 200 + response.status == CANCELLED, inventory incremented
// by seats, hold key deleted.
func TestCancel_HappyPath(t *testing.T) {
	const owner = "u-cancel-happy-1"
	pool, eventID, rid := newGetReservationPool(t, owner)
	rdb := newTestClient(t)

	// Seed inventory at 99 (1 sold via the row we just inserted) so the
	// INCRBY post-cancel should bring it to 100.
	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 99, 0).Err())
	cleanupReserveKeys(t, rdb, eventID, owner)

	// Seed a hold key for (event, owner); cancel must DEL it.
	require.NoError(t, rdb.Set(context.Background(),
		redis.HoldKey(eventID, owner), "1", 10*time.Minute).Err())

	rec, body := doCancelReservation(t, pool, rdb, owner, rid)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, db.StatusCancelled, body["status"])
	require.Equal(t, owner, body["user_id"])
	require.Equal(t, rid, body["reservation_id"])
	require.Equal(t, float64(eventID), body["event_id"])

	// Inventory went from 99 → 100 (1 seat returned).
	require.Equal(t, 100, inventoryCount(t, rdb, eventID),
		"cancel should INCRBY inventory by 1 seat")

	// Hold key cleaned up.
	require.False(t, hasHoldKey(t, rdb, eventID, owner),
		"cancel should DEL the hold key for this user")
}

// TestCancel_MultipleSeats — a reservation with seats=3 must release 3
// back to inventory. Catches the "multiply by row.Seats, not hardcode 1"
// bug.
func TestCancel_MultipleSeats(t *testing.T) {
	const owner = "u-cancel-multi-1"
	pool := apiNewTestPool(t)
	const eventID = int64(64111)
	apiSeedEvent(t, pool, eventID)
	rdb := newTestClient(t)
	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 50, 0).Err())
	cleanupReserveKeys(t, rdb, eventID, owner)

	// seats=3 reservation (apiValidReservationFor defaults to 1, so build by hand)
	r := apiValidReservationFor(t, eventID, owner)
	r.Seats = 3
	apiCleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, db.InsertIfAbsent(context.Background(), pool, r))

	rec, body := doCancelReservation(t, pool, rdb, owner, r.ReservationID)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, db.StatusCancelled, body["status"])
	require.Equal(t, float64(3), body["seats"])

	// Inventory went 50 → 53.
	require.Equal(t, 53, inventoryCount(t, rdb, eventID),
		"cancel should INCRBY inventory by row.Seats (3), not hardcoded 1")
}

// ── Auth errors ─────────────────────────────────────────────────────────

// TestCancel_MissingJWT — no Authorization header → 401 from middleware.
func TestCancel_MissingJWT(t *testing.T) {
	const owner = "u-cancel-noauth-1"
	pool, _, rid := newGetReservationPool(t, owner)
	rdb := newTestClient(t)

	rec, _ := doCancelReservation(t, pool, rdb, "" /* no JWT */, rid)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestCancel_Forbidden_CrossUser — different user's JWT against a row
// owned by someone else → 403 not_your_reservation.
func TestCancel_Forbidden_CrossUser(t *testing.T) {
	const owner = "u-cancel-owner-1"
	const other = "u-cancel-other-1"
	pool, eventID, rid := newGetReservationPool(t, owner)
	rdb := newTestClient(t)
	cleanupReserveKeys(t, rdb, eventID, owner)

	rec, body := doCancelReservation(t, pool, rdb, other, rid)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "not_your_reservation", body["error"])
}

// ── Row-not-found ───────────────────────────────────────────────────────

// TestCancel_NotFound — happy JWT, bogus UUID → 404 reservation_not_found.
func TestCancel_NotFound(t *testing.T) {
	const owner = "u-cancel-nf-1"
	pool := apiNewTestPool(t) // need a pool but no row needed
	rdb := newTestClient(t)

	bogus := uuid.NewString()
	rec, body := doCancelReservation(t, pool, rdb, owner, bogus)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "reservation_not_found", body["error"])
}

// ── Wrong-status errors ────────────────────────────────────────────────

// TestCancel_AlreadyPaid — CONFIRMED row → 409 already_paid (refund flow
// is Layer 2; cancel is forbidden for paid reservations).
func TestCancel_AlreadyPaid(t *testing.T) {
	const owner = "u-cancel-paid-1"
	pool, eventID, rid := newGetReservationPool(t, owner)
	rdb := newTestClient(t)
	cleanupReserveKeys(t, rdb, eventID, owner)

	// PENDING → CONFIRMED via TransitionStatus (the only legal edge).
	changed, err := db.TransitionStatus(context.Background(), pool,
		rid, db.StatusPendingPayment, db.StatusConfirmed, db.Audit{Actor: db.UserActor("test-user")})
	require.NoError(t, err)
	require.True(t, changed)

	rec, body := doCancelReservation(t, pool, rdb, owner, rid)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "already_paid", body["error"])
}

// TestCancel_AlreadyExpired — EXPIRED row → 409 already_expired (the
// watcher beat us to it).
func TestCancel_AlreadyExpired(t *testing.T) {
	const owner = "u-cancel-expired-1"
	pool, eventID, rid := newGetReservationPool(t, owner)
	rdb := newTestClient(t)
	cleanupReserveKeys(t, rdb, eventID, owner)

	changed, err := db.TransitionStatus(context.Background(), pool,
		rid, db.StatusPendingPayment, db.StatusExpired, db.Audit{Actor: db.UserActor("test-user")})
	require.NoError(t, err)
	require.True(t, changed)

	rec, body := doCancelReservation(t, pool, rdb, owner, rid)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "already_expired", body["error"])
}

// TestCancel_AlreadyCancelled_Idempotent — second cancel on an
// already-CANCELLED row returns 200 + the row (the design decision in
// this branch: POST is naturally idempotent in REST; repeated cancels
// don't change anything visible).
func TestCancel_AlreadyCancelled_Idempotent(t *testing.T) {
	const owner = "u-cancel-idem-1"
	pool, eventID, rid := newGetReservationPool(t, owner)
	rdb := newTestClient(t)

	// Seed inventory at 99 then cancel once; inventory → 100, hold deleted.
	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 99, 0).Err())
	require.NoError(t, rdb.Set(context.Background(),
		redis.HoldKey(eventID, owner), "1", 10*time.Minute).Err())
	cleanupReserveKeys(t, rdb, eventID, owner)

	// First cancel → 200.
	rec1, _ := doCancelReservation(t, pool, rdb, owner, rid)
	require.Equal(t, http.StatusOK, rec1.Code)
	require.Equal(t, 100, inventoryCount(t, rdb, eventID),
		"first cancel should have moved inventory 99 → 100")

	// Second cancel → still 200, inventory unchanged.
	rec2, body2 := doCancelReservation(t, pool, rdb, owner, rid)
	require.Equal(t, http.StatusOK, rec2.Code, "idempotent: second cancel must also be 200")
	require.Equal(t, db.StatusCancelled, body2["status"], "row should still be CANCELLED")
	require.Equal(t, 100, inventoryCount(t, rdb, eventID),
		"second cancel must NOT INCRBY again (idempotent)")

	// Hold key is already gone (TTL or first cancel's DEL); second cancel
	// also calls DEL on a missing key, which is a no-op (Redis returns 0).
	require.False(t, hasHoldKey(t, rdb, eventID, owner))
}

// ── Hold-key cleanup edge cases ─────────────────────────────────────────

// TestCancel_HoldKeyAlreadyGone — best-effort DEL on a non-existent hold
// key must NOT 500 (Redis DEL on a missing key is success-by-omission).
// This is a smoke test of the "DEL is idempotent" code path.
func TestCancel_HoldKeyAlreadyGone(t *testing.T) {
	const owner = "u-cancel-nohold-1"
	pool, eventID, rid := newGetReservationPool(t, owner)
	rdb := newTestClient(t)

	// Do NOT seed the hold key — it was never there (TTL already expired,
	// or this is a reservation from before hold-keys existed).
	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 99, 0).Err())
	cleanupReserveKeys(t, rdb, eventID, owner)

	rec, body := doCancelReservation(t, pool, rdb, owner, rid)
	require.Equal(t, http.StatusOK, rec.Code,
		"missing hold key must not 500 the request; got body=%s", rec.Body.String())
	require.Equal(t, db.StatusCancelled, body["status"])
	require.Equal(t, 100, inventoryCount(t, rdb, eventID))
}

// ── Audit log ──────────────────────────────────────────────────────────

// TestCancel_WritesAuditRow — cancelling appends a CANCELLED row to
// the audit log, naming the JWT holder as the actor.
//
// This is the end-to-end proof of the whole feature: the user-facing
// action and the audit record land together, and the audit record
// says WHO did it.
func TestCancel_WritesAuditRow(t *testing.T) {
	const owner = "u-cancel-audit-1"
	pool, _, rid := newGetReservationPool(t, owner)
	rdb := newTestClient(t)
	cleanupReserveKeys(t, rdb, 64001, owner)

	rec, _ := doCancelReservation(t, pool, rdb, owner, rid)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	events, err := db.ListAuditEvents(context.Background(), pool, rid)
	require.NoError(t, err)
	require.Len(t, events, 2, "CREATED (from the insert) + CANCELLED")

	e := events[1]
	require.Equal(t, db.AuditEventCancelled, e.EventType)
	require.Equal(t, db.ActorTypeUser, e.ActorType)
	require.Equal(t, owner, e.ActorID, "the audit row must name the user who cancelled")
	require.Equal(t, db.StatusPendingPayment, e.OldStatus)
	require.Equal(t, db.StatusCancelled, e.NewStatus)
}

// TestCancel_ForbiddenWritesNoAuditRow — a rejected cancel (not your
// reservation) must not touch the audit log. The log records what
// HAPPENED, not what was attempted.
func TestCancel_ForbiddenWritesNoAuditRow(t *testing.T) {
	const owner = "u-cancel-audit-owner"
	const other = "u-cancel-audit-other"
	pool, _, rid := newGetReservationPool(t, owner)
	rdb := newTestClient(t)
	cleanupReserveKeys(t, rdb, 64001, other)

	rec, _ := doCancelReservation(t, pool, rdb, other, rid)
	require.Equal(t, http.StatusForbidden, rec.Code)

	events, err := db.ListAuditEvents(context.Background(), pool, rid)
	require.NoError(t, err)
	require.Len(t, events, 1, "a rejected cancel must not write an audit row")
	require.Equal(t, db.AuditEventCreated, events[0].EventType)
}
