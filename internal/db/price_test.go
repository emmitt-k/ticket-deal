package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ── The price snapshot ─────────────────────────────────────────────────
//
// Postgres is the single source of truth for the price. The
// reservation row carries a FROZEN COPY of it, taken in the same
// statement that creates the row (see InsertIfAbsent).
//
// Two properties matter, and these tests pin both:
//
//  1. the snapshot IS the event's price at hold time, and
//  2. it does NOT move when the event's price changes afterwards.
//
// (2) is the entire reason the snapshot exists. If the price were a
// live lookup, an organizer raising the price mid-drop would silently
// change the amount owed on seats that are already held — and the
// customer would be charged a number they never agreed to.

// seedPricedEvent creates an event with a price, and removes it when
// the test ends.
//
// This is a variant of seedEvent (reservations_test.go) that also
// sets the money columns. The ids live in the 660xx range — see the
// allocation convention in reservations_test.go: event ids must be
// globally unique across packages, because every test package shares
// one Postgres database and `go test ./...` runs them in parallel.
func seedPricedEvent(t *testing.T, pool *Pool, eventID int64, priceCents int64, currency string) {
	t.Helper()
	name := "priced-test-event-" + uuid.NewString()
	_, err := pool.Exec(context.Background(),
		"INSERT INTO events (id, name, initial_inventory, price_cents, currency) "+
			"VALUES ($1, $2, 100, $3, $4)",
		eventID, name, priceCents, currency)
	require.NoError(t, err)
	t.Cleanup(func() {
		// The reservations cleanup runs first (registered later via
		// cleanupReservation). After that, this delete is safe.
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM events WHERE id = $1", eventID)
	})
}

// TestInsertIfAbsent_SnapshotsTheEventPrice — the reservation records
// the event's price, and the total is unit price × seats.
func TestInsertIfAbsent_SnapshotsTheEventPrice(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(66001)

	seedPricedEvent(t, pool, eventID, 2500, "usd")

	r := validReservation(t, eventID)
	r.Seats = 3
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	row, err := GetReservation(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Equal(t, int64(2500), row.UnitPriceCents, "the per-seat price, frozen at hold time")
	require.Equal(t, "usd", row.Currency)
	require.Equal(t, int64(7500), row.TotalPriceCents, "3 seats × $25.00")
}

// TestInsertIfAbsent_PriceSnapshotDoesNotMove — the whole point of
// the snapshot. The organizer raises the price after the seat was
// held; the held reservation keeps the price it was quoted.
func TestInsertIfAbsent_PriceSnapshotDoesNotMove(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(66002)

	seedPricedEvent(t, pool, eventID, 2500, "usd")

	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	// The organizer raises the price mid-drop.
	_, err := pool.Exec(context.Background(),
		"UPDATE events SET price_cents = 9999 WHERE id = $1", eventID)
	require.NoError(t, err)

	row, err := GetReservation(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Equal(t, int64(2500), row.UnitPriceCents,
		"a held seat keeps the price it was quoted — a price change is not retroactive")
	require.Equal(t, int64(2500), row.TotalPriceCents)

	// A NEW hold gets the new price — the snapshot is per-reservation,
	// not a cached price for the event.
	r2 := validReservation(t, eventID)
	cleanupReservation(t, pool, r2.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r2))

	row2, err := GetReservation(context.Background(), pool, r2.ReservationID)
	require.NoError(t, err)
	require.Equal(t, int64(9999), row2.UnitPriceCents)
}

// TestInsertIfAbsent_AuditRecordsThePrice — the audit trail says what
// the seat was sold at. Money that is not in the audit log is money
// nobody can reconstruct.
func TestInsertIfAbsent_AuditRecordsThePrice(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(66003)

	seedPricedEvent(t, pool, eventID, 2500, "usd")

	r := validReservation(t, eventID)
	r.Seats = 2
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	events, err := ListAuditEvents(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Len(t, events, 1, "the CREATED row")
	require.Equal(t, AuditEventCreated, events[0].EventType)
	require.Equal(t, float64(2500), events[0].Metadata["unit_price_cents"])
	require.Equal(t, "usd", events[0].Metadata["currency"])
	require.Equal(t, float64(2), events[0].Metadata["seats"])
}

// TestInsertIfAbsent_UnpricedEventIsZero — an event with no price
// (the column default) produces a zero-priced reservation rather than
// an error. The column is NOT NULL with a default, so "no price
// configured" is a state, not a failure.
//
// (A zero price is a dead end for Stripe — it rejects amounts below
// $0.50 — but that is a payment-layer concern, not a reason for the
// write path to explode.)
func TestInsertIfAbsent_UnpricedEventIsZero(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(66004)

	// seedEvent inserts without the money columns → the defaults apply.
	seedEvent(t, pool, eventID)

	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	row, err := GetReservation(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Equal(t, int64(0), row.UnitPriceCents)
	require.Equal(t, "usd", row.Currency)
	require.Equal(t, int64(0), row.TotalPriceCents)
}

// TestInsertIfAbsent_DuplicateKeepsTheOriginalPrice — a redelivered
// SQS message must not re-price the reservation. ON CONFLICT DO
// NOTHING means the second insert writes nothing at all, so the
// original snapshot stands.
func TestInsertIfAbsent_DuplicateKeepsTheOriginalPrice(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(66005)

	seedPricedEvent(t, pool, eventID, 2500, "usd")

	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	// The price changes, then the message is redelivered.
	_, err := pool.Exec(context.Background(),
		"UPDATE events SET price_cents = 9999 WHERE id = $1", eventID)
	require.NoError(t, err)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	row, err := GetReservation(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Equal(t, int64(2500), row.UnitPriceCents,
		"a duplicate delivery must not re-price an existing reservation")
}
