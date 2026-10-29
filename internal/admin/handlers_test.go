package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/queue"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// These tests need a real Postgres and a real Redis — the admin API's
// correctness is about what it does to the seat count and the audit
// trail, and neither is provable against a fake. Per the repo
// convention, a missing dependency means SKIP, not fail.

// eventIDBase keeps the test event ids out of the ranges the other
// packages use. Every package in this repo owns a range, and an
// overlapping id is a duplicate-key error at best.
const eventIDBase = 95000

// eventCounter hands out the per-test event ids.
var eventCounter atomic.Int64

// runSuffix makes the test user ids unique per run.
//
// The hold key is hold:event:<id>:user:<user> and it carries a
// 10-minute TTL, so a hold left behind by an earlier run would still
// be there when the next run reuses the same event id — and the
// reserve script would answer `already_holding` instead of holding the
// seat. A unique suffix per run sidesteps that entirely.
var runSuffix = strconv.FormatInt(time.Now().UnixNano(), 36)

// newTestPool returns a pool or skips.
func newTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://tickets:tickets@localhost:5432/tickets?sslmode=disable"
	}
	pool, err := db.NewPool(context.Background(), db.Config{DSN: dsn, MaxConns: 3, MinConns: 1})
	if err != nil {
		t.Skipf("Postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newTestRedis returns a Redis client or skips.
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	c := redis.NewClient(redis.Config{Addr: addr})
	if err := redis.Ping(context.Background(), c); err != nil {
		t.Skipf("Redis unavailable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// fixture is a seeded event with inventory, and the machinery to make
// reservations in it.
type fixture struct {
	pool    *db.Pool
	rdb     *redis.Client
	eventID int64
	verif   *Verifier
	router  chi.Router
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := newTestPool(t)
	rdb := newTestRedis(t)

	f := &fixture{
		pool:  pool,
		rdb:   rdb,
		verif: NewVerifier([]string{testSecret}),
	}
	f.eventID = f.newEvent(t)
	f.router = f.buildRouter()
	return f
}

// newEvent creates an event with its own inventory.
//
// EVENT IDS MUST BE GLOBALLY UNIQUE ACROSS PACKAGES. Every test
// package in this repo talks to the SAME Postgres database and `go
// test ./...` runs them in parallel — so two packages using the same
// event id collide on the primary key, and the failure shows up in
// whichever package lost the race. This package owns the 95000 range.
func (f *fixture) newEvent(t *testing.T) int64 {
	t.Helper()

	eventID := eventIDBase + eventCounter.Add(1)
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO events (id, name, initial_inventory, price_cents, currency)
		 VALUES ($1, $2, 100, 2500, 'usd')`,
		eventID, "admin-test-"+uuid.NewString())
	require.NoError(t, err, "seed the event")

	require.NoError(t, f.rdb.Set(context.Background(), redis.InventoryKey(eventID), 100, 0).Err())
	t.Cleanup(func() { f.purgeEvent(eventID) })
	return eventID
}

// purgeEvent removes everything the test created for one event.
//
// The order matters, and every step of it is an FK:
//
//	payment_intents   -> references reservations
//	reservations      -> references events
//	reservation_events -> references reservations
//	events            -> the last one
//
// The audit log is APPEND-ONLY, so its delete needs the documented
// escape hatch (SET LOCAL ticket_deal.audit_purge = 'on'), and the
// whole thing has to be in ONE transaction because SET LOCAL is
// transaction-scoped.
//
// Getting this wrong is not cosmetic: a leaked reservation row makes
// the NEXT test run's lookup return extra rows, and a leaked event row
// makes its seed INSERT hit a duplicate key.
func (f *fixture) purgeEvent(eventID int64) {
	ctx := context.Background()

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, _ = tx.Exec(ctx, "SET LOCAL ticket_deal.audit_purge = 'on'")
	_, _ = tx.Exec(ctx,
		`DELETE FROM reservation_events WHERE reservation_id IN
		   (SELECT reservation_id FROM reservations WHERE event_id = $1)`, eventID)
	_, _ = tx.Exec(ctx,
		`DELETE FROM payment_intents WHERE reservation_id IN
		   (SELECT reservation_id FROM reservations WHERE event_id = $1)`, eventID)
	_, _ = tx.Exec(ctx, "DELETE FROM reservations WHERE event_id = $1", eventID)
	_, _ = tx.Exec(ctx, "DELETE FROM events WHERE id = $1", eventID)
	_ = tx.Commit(ctx)

	// The Redis side: the inventory counter.
	_ = f.rdb.Del(ctx, redis.InventoryKey(eventID)).Err()
}

func (f *fixture) buildRouter() chi.Router {
	r := chi.NewRouter()
	h := &Handlers{Pool: f.pool, Rdb: f.rdb, Verifier: f.verif}
	h.Register(r)
	return r
}

// user returns a run-unique user id. See runSuffix for why.
func (f *fixture) user(name string) string {
	return name + "-" + runSuffix
}

// hold creates a reservation the way the system does: a real Redis
// hold (which decrements the inventory) plus the Postgres row the
// worker writes.
func (f *fixture) hold(t *testing.T, eventID int64, userID string, seats int) (string, db.ReservationRow) {
	t.Helper()
	ctx := context.Background()

	res, err := redis.ReserveSeat(ctx, f.rdb, eventID, userID, 600, seats)
	require.NoError(t, err)
	require.Equal(t, redis.StatusReserved, res.Status,
		"the seat must be held (reason=%d)", res.Reason)

	// The hold key carries a 10-minute TTL, so it outlives the test.
	// Remove it, or a later run's hold on the same event id would be
	// rejected with `already_holding`.
	t.Cleanup(func() {
		_ = f.rdb.Del(ctx, redis.HoldKey(eventID, userID)).Err()
	})

	rid := uuid.NewString()
	now := time.Now()
	require.NoError(t, db.InsertIfAbsent(ctx, f.pool, queue.Reservation{
		ReservationID: rid,
		UserID:        userID,
		EventID:       eventID,
		Seats:         seats,
		CreatedAt:     now.UTC().Format(time.RFC3339),
		ExpiresAt:     now.Add(10 * time.Minute).UTC().Format(time.RFC3339),
	}))

	row, err := db.GetReservation(ctx, f.pool, rid)
	require.NoError(t, err)
	return rid, row
}

// do issues a signed admin request and returns the response.
func (f *fixture) do(t *testing.T, method, uri string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	ts := time.Now().Unix()
	req := httptest.NewRequest(method, uri, strings.NewReader(string(body)))
	req.Header.Set(HeaderKey, Sign(testSecret, method, uri, body, ts))
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) inventory(t *testing.T) int {
	t.Helper()
	n, err := f.rdb.Get(context.Background(), redis.InventoryKey(f.eventID)).Int()
	require.NoError(t, err)
	return n
}

// ── The cancel ────────────────────────────────────────────────────────

// TestCancel_HappyPath — the admin cancels a held reservation: the
// status flips, the seat goes back on sale, and the audit trail records
// the ADMIN as the actor.
func TestCancel_HappyPath(t *testing.T) {
	f := newFixture(t)
	rid, _ := f.hold(t, f.eventID, f.user("admin-cancel-user"), 2)
	require.Equal(t, 98, f.inventory(t), "the hold took two seats")

	rec := f.do(t, http.MethodPost, "/admin/reservations/"+rid+"/cancel", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	row, err := db.GetReservation(context.Background(), f.pool, rid)
	require.NoError(t, err)
	require.Equal(t, db.StatusCancelled, row.Status)
	require.Equal(t, 100, f.inventory(t), "the seats must be back on sale")

	// The audit trail is the point of the whole endpoint.
	events, err := db.ListAuditEvents(context.Background(), f.pool, rid)
	require.NoError(t, err)

	var cancelEvent *db.AuditEventRow
	for i := range events {
		if events[i].EventType == db.AuditEventCancelled {
			cancelEvent = &events[i]
		}
	}
	require.NotNil(t, cancelEvent, "the cancellation must be audited")
	require.Equal(t, db.ActorTypeAdmin, cancelEvent.ActorType)
	require.Equal(t, db.StatusPendingPayment, cancelEvent.OldStatus)
	require.Equal(t, db.StatusCancelled, cancelEvent.NewStatus)
	require.NotEmpty(t, cancelEvent.ActorID, "the actor id is the key fingerprint")
}

// TestCancel_RefusesAPaidReservation — a CONFIRMED reservation is
// REFUNDED, not cancelled. The state machine has no CONFIRMED →
// CANCELLED edge, and the error says so.
func TestCancel_RefusesAPaidReservation(t *testing.T) {
	f := newFixture(t)
	rid, _ := f.hold(t, f.eventID, f.user("admin-paid-user"), 1)

	_, err := db.TransitionStatus(context.Background(), f.pool, rid,
		db.StatusPendingPayment, db.StatusConfirmed,
		db.Audit{Actor: db.StripeWebhookActor("evt_test")})
	require.NoError(t, err)

	rec := f.do(t, http.MethodPost, "/admin/reservations/"+rid+"/cancel", nil)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "already_paid")
	require.Contains(t, rec.Body.String(), "REFUNDED",
		"the message must point at the refund path")
}

// TestCancel_RefusesOtherTerminalStates — EXPIRED, REFUNDED and
// CANCELLED each get their own code, because they are different facts.
func TestCancel_RefusesOtherTerminalStates(t *testing.T) {
	f := newFixture(t)

	t.Run("expired", func(t *testing.T) {
		rid, _ := f.hold(t, f.eventID, f.user("admin-expired-user"), 1)
		_, err := db.TransitionStatus(context.Background(), f.pool, rid,
			db.StatusPendingPayment, db.StatusExpired,
			db.Audit{Actor: db.SystemActor("test")})
		require.NoError(t, err)

		rec := f.do(t, http.MethodPost, "/admin/reservations/"+rid+"/cancel", nil)
		require.Equal(t, http.StatusConflict, rec.Code)
		require.Contains(t, rec.Body.String(), "already_expired")
	})

	t.Run("refunded", func(t *testing.T) {
		rid, _ := f.hold(t, f.eventID, f.user("admin-refunded-user"), 1)
		_, err := db.TransitionStatus(context.Background(), f.pool, rid,
			db.StatusPendingPayment, db.StatusConfirmed,
			db.Audit{Actor: db.StripeWebhookActor("evt_test")})
		require.NoError(t, err)
		_, err = db.TransitionStatus(context.Background(), f.pool, rid,
			db.StatusConfirmed, db.StatusRefunded,
			db.Audit{Actor: db.StripeWebhookActor("evt_test")})
		require.NoError(t, err)

		rec := f.do(t, http.MethodPost, "/admin/reservations/"+rid+"/cancel", nil)
		require.Equal(t, http.StatusConflict, rec.Code)
		require.Contains(t, rec.Body.String(), "already_refunded")
	})
}

// TestCancel_IsIdempotent — cancelling an already-cancelled
// reservation is a 200, not a 409. A retried admin call must not look
// like a failure.
func TestCancel_IsIdempotent(t *testing.T) {
	f := newFixture(t)
	rid, _ := f.hold(t, f.eventID, f.user("admin-idem-user"), 1)

	rec := f.do(t, http.MethodPost, "/admin/reservations/"+rid+"/cancel", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	rec = f.do(t, http.MethodPost, "/admin/reservations/"+rid+"/cancel", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "already cancelled")

	// And the seat was released exactly once.
	require.Equal(t, 100, f.inventory(t))
}

// TestCancel_NotFound — a reservation that does not exist.
func TestCancel_NotFound(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodPost, "/admin/reservations/"+uuid.NewString()+"/cancel", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "not_found")
}

// ── The release-seat ──────────────────────────────────────────────────

// TestReleaseSeat_HappyPath — the seat goes back on sale, the status
// does NOT change, and the audit trail records the action as an event
// (old_status == new_status).
func TestReleaseSeat_HappyPath(t *testing.T) {
	f := newFixture(t)
	rid, _ := f.hold(t, f.eventID, f.user("admin-release-user"), 2)
	require.Equal(t, 98, f.inventory(t))

	rec := f.do(t, http.MethodPost, "/admin/reservations/"+rid+"/release-seat", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Equal(t, 100, f.inventory(t), "the seat must be back on sale")

	row, err := db.GetReservation(context.Background(), f.pool, rid)
	require.NoError(t, err)
	require.Equal(t, db.StatusPendingPayment, row.Status,
		"release-seat does NOT change the status")

	events, err := db.ListAuditEvents(context.Background(), f.pool, rid)
	require.NoError(t, err)

	var seatEvent *db.AuditEventRow
	for i := range events {
		if events[i].EventType == db.AuditEventSeatReleased {
			seatEvent = &events[i]
		}
	}
	require.NotNil(t, seatEvent, "the release must be audited")
	require.Equal(t, db.ActorTypeAdmin, seatEvent.ActorType)
	require.Equal(t, row.Status, seatEvent.OldStatus)
	require.Equal(t, row.Status, seatEvent.NewStatus,
		"an event, not a state: old_status == new_status")
}

// TestReleaseSeat_RefusesWhenNotHeld — THE oversell guard.
//
// When the hold has already expired, the seat is ALREADY back on sale
// (the expiration watcher put it there). Releasing it again would make
// the inventory too high — a phantom seat, which is an oversell. So
// the endpoint refuses, and the inventory is untouched.
func TestReleaseSeat_RefusesWhenNotHeld(t *testing.T) {
	f := newFixture(t)
	rid, _ := f.hold(t, f.eventID, f.user("admin-not-held-user"), 1)

	// Simulate the hold having expired and the watcher having released
	// the seat: the hold key is gone and the inventory is back.
	require.NoError(t, f.rdb.Del(context.Background(),
		redis.HoldKey(f.eventID, f.user("admin-not-held-user"))).Err())
	require.NoError(t, f.rdb.Set(context.Background(),
		redis.InventoryKey(f.eventID), 100, 0).Err())

	rec := f.do(t, http.MethodPost, "/admin/reservations/"+rid+"/release-seat", nil)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "seat_not_held")

	require.Equal(t, 100, f.inventory(t),
		"the inventory must NOT be incremented — that would oversell")
}

// TestReleaseSeat_TwiceDoesNotDoubleCount — the atomicity guard.
// Two concurrent releases of the same hold must release the seat ONCE.
func TestReleaseSeat_TwiceDoesNotDoubleCount(t *testing.T) {
	f := newFixture(t)
	rid, _ := f.hold(t, f.eventID, f.user("admin-double-user"), 1)
	require.Equal(t, 99, f.inventory(t))

	rec := f.do(t, http.MethodPost, "/admin/reservations/"+rid+"/release-seat", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	rec = f.do(t, http.MethodPost, "/admin/reservations/"+rid+"/release-seat", nil)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "seat_not_held")

	require.Equal(t, 100, f.inventory(t), "exactly one release, never two")
}

// ── The lookups ───────────────────────────────────────────────────────

// TestGetOne — support looks up one reservation.
func TestGetOne(t *testing.T) {
	f := newFixture(t)
	rid, row := f.hold(t, f.eventID, f.user("admin-lookup-user"), 1)

	rec := f.do(t, http.MethodGet, "/admin/reservations/"+rid, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var out db.ReservationRow
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, row.ReservationID, out.ReservationID)
	require.Equal(t, db.StatusPendingPayment, out.Status)
}

// TestGetOne_NotFound.
func TestGetOne_NotFound(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/admin/reservations/"+uuid.NewString(), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestLookupByUser — support finds everything a customer has.
func TestLookupByUser(t *testing.T) {
	f := newFixture(t)
	user := f.user("admin-user-lookup")
	rid, _ := f.hold(t, f.eventID, user, 1)
	// A second ticket for the same customer, on a SECOND event — the
	// reserve script refuses two holds on one event.
	other := f.newEvent(t)
	f.hold(t, other, user, 2)
	f.hold(t, other, f.user("admin-someone-else"), 1)

	rec := f.do(t, http.MethodGet, "/admin/reservations/lookup?user_id="+user, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var out struct {
		UserID       string              `json:"user_id"`
		Reservations []db.ReservationRow `json:"reservations"`
		Count        int                 `json:"count"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, user, out.UserID)
	require.Equal(t, 2, out.Count)
	for _, r := range out.Reservations {
		require.Equal(t, user, r.UserID, "the lookup must not leak another user's rows")
	}
	// The list is ordered by created_at DESC, but two reservations made
	// in the same second have EQUAL timestamps — so the order between
	// them is not defined. Assert the set, not the position.
	var ids []string
	for _, r := range out.Reservations {
		ids = append(ids, r.ReservationID)
	}
	require.Contains(t, ids, rid)
}

// TestLookupByUser_RequiresUserID — a lookup with no user_id is a
// client error, not an empty list. Returning an empty list would look
// like "this customer has nothing", which is a different (and wrong)
// answer.
func TestLookupByUser_RequiresUserID(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/admin/reservations/lookup", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "missing_user_id")
}

// ── The whole surface, unauthenticated ────────────────────────────────

// TestRoutesRequireSignature — every admin route is behind the
// verifier. A request with no credentials gets 401, and the handler
// never runs.
func TestRoutesRequireSignature(t *testing.T) {
	f := newFixture(t)

	for _, tc := range []struct{ method, uri string }{
		{http.MethodGet, "/admin/reservations/lookup?user_id=x"},
		{http.MethodGet, "/admin/reservations/" + uuid.NewString()},
		{http.MethodPost, "/admin/reservations/" + uuid.NewString() + "/cancel"},
		{http.MethodPost, "/admin/reservations/" + uuid.NewString() + "/release-seat"},
	} {
		t.Run(fmt.Sprintf("%s %s", tc.method, tc.uri), func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.uri, nil)
			rec := httptest.NewRecorder()
			f.router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

// TestRegister_NoKeysMountsNothing — with no admin key configured the
// routes do not exist at all. The feature is off, not "open".
func TestRegister_NoKeysMountsNothing(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)

	r := chi.NewRouter()
	h := &Handlers{Pool: pool, Rdb: rdb, Verifier: NewVerifier(nil)}
	h.Register(r)

	req := httptest.NewRequest(http.MethodGet, "/admin/reservations/lookup?user_id=x", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code,
		"the /admin tree must not exist when no key is configured")
}
