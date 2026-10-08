package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/queue"
)

// testDSN returns the connection string for the docker-compose Postgres,
// or skips the test if it can't connect.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://tickets:tickets@localhost:5432/tickets?sslmode=disable"
	}
	return dsn
}

// newTestPool returns a Pool to the test Postgres, or skips if
// unreachable (CI without Postgres can still pass other packages).
func newTestPool(t *testing.T) *Pool {
	t.Helper()
	p, err := NewPool(context.Background(), Config{
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

// cleanupReservation registers a t.Cleanup that deletes the test row
// so each test starts from a clean slate.
func cleanupReservation(t *testing.T, pool *Pool, reservationID string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM reservations WHERE reservation_id = $1", reservationID)
	})
}

// seedEvent creates a stub event row so the FK constraint on
// reservations.event_id is satisfied. Uses a UUID-based name to
// avoid clashes if multiple tests run in parallel.
//
// The cleanup deletes the event row (after the reservations
// cleanup has run, so we don't hit a FK violation in reverse).
func seedEvent(t *testing.T, pool *Pool, eventID int64) {
	t.Helper()
	name := "test-event-" + uuid.NewString()
	_, err := pool.Exec(context.Background(),
		"INSERT INTO events (id, name, initial_inventory) VALUES ($1, $2, 100)",
		eventID, name)
	require.NoError(t, err)
	t.Cleanup(func() {
		// reservations cleanup runs first (registered later via
		// cleanupReservation). After that, this delete is safe.
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM events WHERE id = $1", eventID)
	})
}

// nowRFC3339 returns an RFC 3339 timestamp in UTC for use as CreatedAt.
// ExpiresAt is created+10 minutes to match the production hold TTL.
func nowRFC3339(t *testing.T) (created, expires string) {
	t.Helper()
	now := time.Now().UTC()
	return now.Format(time.RFC3339),
		now.Add(10 * time.Minute).Format(time.RFC3339)
}

// validReservation returns a reservation that's about to be inserted.
func validReservation(t *testing.T, eventID int64) queue.Reservation {
	t.Helper()
	created, expires := nowRFC3339(t)
	return queue.Reservation{
		ReservationID: uuid.NewString(),
		UserID:        "u-" + uuid.NewString()[:8],
		EventID:       eventID,
		Seats:         1,
		CreatedAt:     created,
		ExpiresAt:     expires,
	}
}

// TestInsertIfAbsent_HappyPath — first call inserts the row.
func TestInsertIfAbsent_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(61001)

	// Register event cleanup FIRST (LIFO order) so reservations
	// cleanup runs first.
	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)

	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	// Row exists with the right values.
	row, err := GetReservation(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Equal(t, r.UserID, row.UserID)
	require.Equal(t, eventID, row.EventID)
	require.Equal(t, StatusPendingPayment, row.Status)
}

// TestInsertIfAbsent_Idempotency — the headline test of Phase 6.
// Calling InsertIfAbsent with the same reservation_id twice creates
// exactly one row. Second call is a no-op.
//
// This is what makes SQS at-least-once delivery safe: duplicate
// publishes produce zero extra Postgres rows.
func TestInsertIfAbsent_Idempotency(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(61002)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)

	// First insert
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	// Second insert with the SAME reservation_id must succeed (no-op)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	// Verify exactly one row
	var count int
	err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM reservations WHERE reservation_id = $1",
		r.ReservationID).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count, "duplicate insert created zero extra rows")
}

// TestInsertIfAbsent_MultipleDifferentReservations — different IDs
// produce different rows; idempotency only collapses same-ID writes.
func TestInsertIfAbsent_MultipleDifferentReservations(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(61003)

	seedEvent(t, pool, eventID)
	r1 := validReservation(t, eventID)
	r2 := validReservation(t, eventID)
	cleanupReservation(t, pool, r1.ReservationID)
	cleanupReservation(t, pool, r2.ReservationID)

	require.NoError(t, InsertIfAbsent(context.Background(), pool, r1))
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r2))

	var count int
	err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM reservations WHERE reservation_id IN ($1, $2)",
		r1.ReservationID, r2.ReservationID).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

// TestInsertIfAbsent_BadTimestamp returns an error so the worker
// can log + delete the malformed message instead of retrying forever.
func TestInsertIfAbsent_BadTimestamp(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(61004)

	r := validReservation(t, eventID)
	r.CreatedAt = "not-a-timestamp"

	err := InsertIfAbsent(context.Background(), pool, r)
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse reservation timestamps")
}

// TestInsertIfAbsent_FKConstraint — event_id without a matching events
// row should fail the FK insert. This guards against typos in event_id.
func TestInsertIfAbsent_FKConstraint(t *testing.T) {
	pool := newTestPool(t)

	r := validReservation(t, 99999999) // not in events table
	cleanupReservation(t, pool, r.ReservationID)

	err := InsertIfAbsent(context.Background(), pool, r)
	require.Error(t, err)
	require.Contains(t, err.Error(), "insert reservation")
}

// TestInsertIfAbsent_ConcurrentSameID — 50 goroutines race to insert
// the same reservation_id. Exactly one row exists at the end.
// This is the strong idempotency test: under concurrency, the ON CONFLICT
// DO NOTHING semantics are preserved.
func TestInsertIfAbsent_ConcurrentSameID(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(61005)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)

	const N = 50
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			errCh <- InsertIfAbsent(context.Background(), pool, r)
		}()
	}
	for i := 0; i < N; i++ {
		require.NoError(t, <-errCh, "every concurrent insert should succeed (idempotent no-op)")
	}

	var count int
	err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM reservations WHERE reservation_id = $1",
		r.ReservationID).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count, "50 concurrent inserts → exactly 1 row")
}

// TestGetReservation_NotFound — a nonexistent reservation_id returns
// pgx.ErrNoRows, surfaced by IsNoRows.
func TestGetReservation_NotFound(t *testing.T) {
	pool := newTestPool(t)
	_, err := GetReservation(context.Background(), pool, uuid.NewString())
	require.Error(t, err)
	require.True(t, IsNoRows(err), "IsNoRows should detect pgx.ErrNoRows")
}

// TestNewPool_BadDSN — malformed DSN returns an error rather than
// silently using a default. Fail-fast at startup.
func TestNewPool_BadDSN(t *testing.T) {
	_, err := NewPool(context.Background(), Config{
		DSN: "not-a-valid-postgres-url",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse DSN")
}

// TestStatusConstants — the constants match the CHECK constraint in
// migrations/001_init.sql. If someone renames the constraint, this
// fails loudly.
func TestStatusConstants(t *testing.T) {
	require.Equal(t, "PENDING_PAYMENT", StatusPendingPayment)
	require.Equal(t, "CONFIRMED", StatusConfirmed)
	require.Equal(t, "EXPIRED", StatusExpired)
	require.Equal(t, "CANCELLED", StatusCancelled)
}

// ensure fmt import is used (for any future debug helper)
var _ = fmt.Sprintf