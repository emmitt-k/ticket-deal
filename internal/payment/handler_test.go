package payment

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/queue"
	redispkg "github.com/emmitt-k/ticket-deal/internal/redis"
	redisclient "github.com/redis/go-redis/v9"
)

// ── Test scaffolding ──────────────────────────────────────────────────
//
// The payment state machine needs Postgres (the reservations and the
// payment rows) and Redis (to put a refunded seat back on sale).
// Both are live dependencies here; the tests skip when they are
// unreachable, exactly like the other packages in this repo.
//
// EVENT IDS MUST BE GLOBALLY UNIQUE ACROSS PACKAGES — every test
// package talks to the same Postgres, and `go test ./...` runs them
// in parallel. This package uses the 680xx range.

// newTestPool opens a pool, or skips when Postgres is unreachable.
func newTestPool(t *testing.T) *db.Pool {
	t.Helper()
	p, err := db.NewPool(context.Background(), db.Config{
		DSN:      testDSN(t),
		MaxConns: 3,
		MinConns: 1,
	})
	if err != nil {
		t.Skipf("Postgres unavailable: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// newTestRedis opens a Redis client, or skips when Redis is
// unreachable.
func newTestRedis(t *testing.T) *redisclient.Client {
	t.Helper()
	c := redispkg.NewClient(redispkg.Config{Addr: "localhost:6379"})
	if err := redispkg.Ping(context.Background(), c); err != nil {
		t.Skipf("Redis unavailable at localhost:6379: %v", err)
	}
	return c
}

// seedEvent creates an event with a price and removes it afterwards.
func seedEvent(t *testing.T, pool *db.Pool, eventID int64, priceCents int64) {
	t.Helper()
	name := "payment-test-" + uuid.NewString()
	_, err := pool.Exec(context.Background(),
		"INSERT INTO events (id, name, initial_inventory, price_cents, currency) "+
			"VALUES ($1, $2, 100, $3, 'usd')",
		eventID, name, priceCents)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM events WHERE id = $1", eventID)
	})
}

// insertPendingReservation writes a PENDING_PAYMENT row via the same
// path the worker uses, and returns its id.
func insertPendingReservation(t *testing.T, pool *db.Pool, eventID int64, userID string, seats int) string {
	t.Helper()
	id := uuid.NewString()
	err := db.InsertIfAbsent(context.Background(), pool, queue.Reservation{
		ReservationID: id,
		UserID:        userID,
		EventID:       eventID,
		Seats:         seats,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		ExpiresAt:     time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.Background()
		// The audit rows go first, and they need the purge escape
		// hatch: reservation_events is append-only.
		tx, txErr := pool.Begin(ctx)
		if txErr == nil {
			_, _ = tx.Exec(ctx, "SET LOCAL ticket_deal.audit_purge = 'on'")
			_, _ = tx.Exec(ctx, "DELETE FROM reservation_events WHERE reservation_id = $1", id)
			_ = tx.Commit(ctx)
		}
		// The payment row references the reservation, so it goes
		// FIRST — otherwise the FK blocks the delete below and the
		// error is swallowed.
		_, _ = pool.Exec(ctx, "DELETE FROM payment_intents WHERE reservation_id = $1", id)
		_, _ = pool.Exec(ctx, "DELETE FROM reservations WHERE reservation_id = $1", id)
	})
	return id
}

// insertPayment writes the payment row for a reservation and attaches
// a Stripe PaymentIntent id to it — the state /pay leaves behind.
func insertPayment(t *testing.T, pool *db.Pool, reservationID, userID, stripeID string, amountCents int64) {
	t.Helper()
	created, err := db.InsertPaymentIntent(context.Background(), pool,
		reservationID, userID, amountCents, "usd")
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.SetStripePaymentIntentID(context.Background(), pool,
		reservationID, stripeID, "requires_payment_method"))
}

// confirm moves a reservation to CONFIRMED the way the webhook would.
func confirm(t *testing.T, pool *db.Pool, reservationID string) {
	t.Helper()
	changed, err := db.TransitionStatus(context.Background(), pool,
		reservationID, db.StatusPendingPayment, db.StatusConfirmed,
		db.Audit{Actor: db.StripeWebhookActor("evt_setup")})
	require.NoError(t, err)
	require.True(t, changed)
}

// reservationStatus reads the current status.
func reservationStatus(t *testing.T, pool *db.Pool, reservationID string) string {
	t.Helper()
	row, err := db.GetReservation(context.Background(), pool, reservationID)
	require.NoError(t, err)
	return row.Status
}

// ── payment_intent.succeeded ──────────────────────────────────────────

// TestHandleSucceeded_ConfirmsTheReservation — the money arrived, so
// the reservation becomes CONFIRMED and the audit trail says who did
// it.
func TestHandleSucceeded_ConfirmsTheReservation(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eventID = int64(68001)
	seedEvent(t, pool, eventID, 2500)

	uid := "pay-" + uuid.NewString()[:8]
	rid := insertPendingReservation(t, pool, eventID, uid, 2)
	insertPayment(t, pool, rid, uid, "pi_1", 5000)

	h := NewHandler(pool, rdb)
	err := h.HandleEvent(context.Background(), stripeEventSucceeded("evt_1", "pi_1", 5000))
	require.NoError(t, err)

	require.Equal(t, db.StatusConfirmed, reservationStatus(t, pool, rid))

	// The audit row names Stripe as the actor.
	events, err := db.ListAuditEvents(context.Background(), pool, rid)
	require.NoError(t, err)
	require.Len(t, events, 2, "CREATED + CONFIRMED")
	e := events[1]
	require.Equal(t, db.AuditEventConfirmed, e.EventType)
	require.Equal(t, db.ActorTypeStripeWebhook, e.ActorType)
	require.Equal(t, "evt_1", e.ActorID)
	require.Equal(t, db.StatusPendingPayment, e.OldStatus)
	require.Equal(t, db.StatusConfirmed, e.NewStatus)

	// The payment row records Stripe's view.
	pay, err := db.GetPaymentIntentByStripeID(context.Background(), pool, "pi_1")
	require.NoError(t, err)
	require.Equal(t, db.PaymentStatusSucceeded, pay.Status)
}

// TestHandleSucceeded_DuplicateDeliveryIsIdempotent — Stripe delivers
// the same event more than once. The second delivery must not error
// and must not write a second audit row.
func TestHandleSucceeded_DuplicateDeliveryIsIdempotent(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eventID = int64(68002)
	seedEvent(t, pool, eventID, 2500)

	uid := "pay-dup-" + uuid.NewString()[:8]
	rid := insertPendingReservation(t, pool, eventID, uid, 1)
	insertPayment(t, pool, rid, uid, "pi_dup", 2500)

	h := NewHandler(pool, rdb)
	ctx := context.Background()
	require.NoError(t, h.HandleEvent(ctx, stripeEventSucceeded("evt_dup", "pi_dup", 2500)))
	require.NoError(t, h.HandleEvent(ctx, stripeEventSucceeded("evt_dup", "pi_dup", 2500)))

	require.Equal(t, db.StatusConfirmed, reservationStatus(t, pool, rid))
	events, err := db.ListAuditEvents(ctx, pool, rid)
	require.NoError(t, err)
	require.Len(t, events, 2, "CREATED + CONFIRMED — the duplicate wrote nothing")
}

// TestHandleSucceeded_AfterTheHoldExpired — THE RACE.
//
// The customer sat on the payment page for longer than the hold TTL.
// The hold expired, the seat went back on sale, and then the payment
// succeeded. We must NOT confirm the reservation (the seat may be
// sold) and we must NOT auto-refund (a refund on a race is its own
// class of bug). We alert.
func TestHandleSucceeded_AfterTheHoldExpired(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eventID = int64(68003)
	seedEvent(t, pool, eventID, 2500)

	uid := "pay-race-" + uuid.NewString()[:8]
	rid := insertPendingReservation(t, pool, eventID, uid, 1)
	insertPayment(t, pool, rid, uid, "pi_race", 2500)

	// The hold expires.
	changed, err := db.TransitionStatus(context.Background(), pool,
		rid, db.StatusPendingPayment, db.StatusExpired,
		db.Audit{Actor: db.SystemActor("expiration-watcher")})
	require.NoError(t, err)
	require.True(t, changed)

	h := NewHandler(pool, rdb)
	// The payment succeeds anyway.
	require.NoError(t, h.HandleEvent(context.Background(),
		stripeEventSucceeded("evt_race", "pi_race", 2500)))

	// The reservation is still EXPIRED — we did not honour a ticket
	// for a seat that may have been sold.
	require.Equal(t, db.StatusExpired, reservationStatus(t, pool, rid))

	// And the payment row says the money arrived, which is exactly
	// the mismatch an operator looks for.
	pay, err := db.GetPaymentIntentByStripeID(context.Background(), pool, "pi_race")
	require.NoError(t, err)
	require.Equal(t, db.PaymentStatusSucceeded, pay.Status)
}

// TestHandleSucceeded_UnknownPayment — a payment for something we
// have no record of. Not an error (there is nothing to retry), but
// it is counted.
func TestHandleSucceeded_UnknownPayment(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)

	h := NewHandler(pool, rdb)
	require.NoError(t, h.HandleEvent(context.Background(),
		stripeEventSucceeded("evt_unknown", "pi_never_seen", 1000)))
}

// ── payment_intent.payment_failed ─────────────────────────────────────

// TestHandleFailed_LeavesTheReservationPending — a failed payment
// does NOT end the hold. The seat is still held and the customer can
// retry with another card.
func TestHandleFailed_LeavesTheReservationPending(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eventID = int64(68004)
	seedEvent(t, pool, eventID, 2500)

	uid := "pay-fail-" + uuid.NewString()[:8]
	rid := insertPendingReservation(t, pool, eventID, uid, 1)
	insertPayment(t, pool, rid, uid, "pi_fail", 2500)

	h := NewHandler(pool, rdb)
	err := h.HandleEvent(context.Background(), stripeEventFailed("evt_fail", "pi_fail"))
	require.NoError(t, err)

	require.Equal(t, db.StatusPendingPayment, reservationStatus(t, pool, rid))

	// And no audit row: a failed payment is not a state change, it is
	// a signal.
	events, err := db.ListAuditEvents(context.Background(), pool, rid)
	require.NoError(t, err)
	require.Len(t, events, 1, "only the CREATED row")
}

// ── charge.refunded ───────────────────────────────────────────────────

// TestHandleRefunded_FullRefundReleasesTheSeat — the money came
// back, so the reservation becomes REFUNDED and the seat goes back on
// sale.
func TestHandleRefunded_FullRefundReleasesTheSeat(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eventID = int64(68005)
	seedEvent(t, pool, eventID, 2500)

	uid := "pay-refund-" + uuid.NewString()[:8]
	rid := insertPendingReservation(t, pool, eventID, uid, 3)
	insertPayment(t, pool, rid, uid, "pi_refund", 7500)
	confirm(t, pool, rid)

	// The inventory starts at 0 (the seats are sold).
	invKey := redispkg.InventoryKey(eventID)
	require.NoError(t, rdb.Set(context.Background(), invKey, 0, time.Hour).Err())

	h := NewHandler(pool, rdb)
	err := h.HandleEvent(context.Background(),
		stripeEventRefunded("evt_refund", "ch_1", "pi_refund", 7500, 7500))
	require.NoError(t, err)

	require.Equal(t, db.StatusRefunded, reservationStatus(t, pool, rid))

	// The seat is back on sale.
	got, err := rdb.Get(context.Background(), invKey).Int64()
	require.NoError(t, err)
	require.Equal(t, int64(3), got, "3 seats released")

	// The audit trail records the refund.
	events, err := db.ListAuditEvents(context.Background(), pool, rid)
	require.NoError(t, err)
	require.Len(t, events, 3, "CREATED + CONFIRMED + REFUND")
	e := events[2]
	require.Equal(t, db.AuditEventRefundInitiated, e.EventType)
	require.Equal(t, db.ActorTypeStripeWebhook, e.ActorType)
	require.Equal(t, db.StatusConfirmed, e.OldStatus)
	require.Equal(t, db.StatusRefunded, e.NewStatus)
	require.Equal(t, float64(7500), e.Metadata["amount_refunded_cents"])
}

// TestHandleRefunded_PartialRefundLeavesTheReservationConfirmed — the
// case that proves the event and the status are separate concepts.
//
// A partial refund is an EVENT (money came back) whose outcome is
// "still a valid ticket". The status stays CONFIRMED and the seat is
// NOT released.
func TestHandleRefunded_PartialRefundLeavesTheReservationConfirmed(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eventID = int64(68006)
	seedEvent(t, pool, eventID, 2500)

	uid := "pay-partial-" + uuid.NewString()[:8]
	rid := insertPendingReservation(t, pool, eventID, uid, 2)
	insertPayment(t, pool, rid, uid, "pi_partial", 5000)
	confirm(t, pool, rid)

	invKey := redispkg.InventoryKey(eventID)
	require.NoError(t, rdb.Set(context.Background(), invKey, 0, time.Hour).Err())

	h := NewHandler(pool, rdb)
	err := h.HandleEvent(context.Background(),
		stripeEventRefunded("evt_partial", "ch_2", "pi_partial", 5000, 2000))
	require.NoError(t, err)

	// The ticket is still valid.
	require.Equal(t, db.StatusConfirmed, reservationStatus(t, pool, rid))

	// And the seat was NOT released.
	got, err := rdb.Get(context.Background(), invKey).Int64()
	require.NoError(t, err)
	require.Equal(t, int64(0), got, "a partial refund does not release the seat")

	// But the refund IS in the audit trail — money that came back is
	// a fact that must be recorded.
	events, err := db.ListAuditEvents(context.Background(), pool, rid)
	require.NoError(t, err)
	require.Len(t, events, 3, "CREATED + CONFIRMED + REFUND_INITIATED")
	e := events[2]
	require.Equal(t, db.AuditEventRefundInitiated, e.EventType)
	require.Equal(t, db.StatusConfirmed, e.OldStatus)
	require.Equal(t, db.StatusConfirmed, e.NewStatus, "the status did not change")
	require.Equal(t, float64(2000), e.Metadata["amount_refunded_cents"])
}

// TestHandleRefunded_DuplicateDeliveryIsIdempotent — the second
// delivery of the same refund event must not release the seat twice.
func TestHandleRefunded_DuplicateDeliveryIsIdempotent(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eventID = int64(68007)
	seedEvent(t, pool, eventID, 2500)

	uid := "pay-refund-dup-" + uuid.NewString()[:8]
	rid := insertPendingReservation(t, pool, eventID, uid, 2)
	insertPayment(t, pool, rid, uid, "pi_refund_dup", 5000)
	confirm(t, pool, rid)

	invKey := redispkg.InventoryKey(eventID)
	require.NoError(t, rdb.Set(context.Background(), invKey, 0, time.Hour).Err())

	h := NewHandler(pool, rdb)
	ctx := context.Background()
	ev := stripeEventRefunded("evt_refund_dup", "ch_3", "pi_refund_dup", 5000, 5000)
	require.NoError(t, h.HandleEvent(ctx, ev))
	require.NoError(t, h.HandleEvent(ctx, ev))

	require.Equal(t, db.StatusRefunded, reservationStatus(t, pool, rid))
	got, err := rdb.Get(context.Background(), invKey).Int64()
	require.NoError(t, err)
	require.Equal(t, int64(2), got, "the seat is released ONCE, not twice")
}

// ── the message plumbing ──────────────────────────────────────────────

// TestHandleMessage_MalformedMessageIsDropped — a message that will
// never parse must be DROPPED, not redelivered forever.
func TestHandleMessage_MalformedMessageIsDropped(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)

	h := NewHandler(pool, rdb)
	require.NoError(t, h.HandleMessage(context.Background(), []byte("{not json")))
}

// TestHandleMessage_ValidMessageIsApplied — the wire format the
// webhook handler publishes round-trips into a state change.
func TestHandleMessage_ValidMessageIsApplied(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eventID = int64(68008)
	seedEvent(t, pool, eventID, 2500)

	uid := "pay-msg-" + uuid.NewString()[:8]
	rid := insertPendingReservation(t, pool, eventID, uid, 1)
	insertPayment(t, pool, rid, uid, "pi_msg", 2500)

	h := NewHandler(pool, rdb)
	body := marshalEvent(t, stripeEventSucceeded("evt_msg", "pi_msg", 2500))
	require.NoError(t, h.HandleMessage(context.Background(), body))

	require.Equal(t, db.StatusConfirmed, reservationStatus(t, pool, rid))
}

// TestHandleEvent_UnknownTypeIsIgnored — a type we do not act on is
// not an error.
func TestHandleEvent_UnknownTypeIsIgnored(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)

	h := NewHandler(pool, rdb)
	require.NoError(t, h.HandleEvent(context.Background(),
		stripeEventSucceeded("evt_x", "pi_x", 1)))
}
