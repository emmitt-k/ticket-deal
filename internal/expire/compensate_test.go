package expire

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	redisclient "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/queue"
	redispkg "github.com/emmitt-k/ticket-deal/internal/redis"
)

// ── Test helpers (live Postgres + live Redis, like db/reservations_test.go) ──

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://tickets:tickets@localhost:5432/tickets?sslmode=disable"
	}
	return dsn
}

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

func newTestRedis(t *testing.T) *redisclient.Client {
	t.Helper()
	c := redispkg.NewClient(redispkg.Config{Addr: "localhost:6379"})
	if err := redispkg.Ping(context.Background(), c); err != nil {
		t.Skipf("Redis unavailable at localhost:6379: %v", err)
	}
	return c
}

func seedEvent(t *testing.T, pool *db.Pool, eventID int64) {
	t.Helper()
	name := "expire-test-" + uuid.NewString()
	_, err := pool.Exec(context.Background(),
		"INSERT INTO events (id, name, initial_inventory) VALUES ($1, $2, 100)",
		eventID, name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM events WHERE id = $1", eventID)
	})
}

// insertPendingReservation writes a PENDING_PAYMENT row directly via
// the same SQL the worker uses, so we don't depend on Phase 6 wiring.
func insertPendingReservation(t *testing.T, pool *db.Pool, eventID int64, userID string, seats int, expiresAt time.Time) string {
	t.Helper()
	id := uuid.NewString()
	err := db.InsertIfAbsent(context.Background(), pool, queue.Reservation{
		ReservationID: id,
		UserID:        userID,
		EventID:       eventID,
		Seats:         seats,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		ExpiresAt:     expiresAt.UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM reservations WHERE reservation_id = $1", id)
	})
	return id
}

// TestCompensate_RowFound — happy path: a PENDING_PAYMENT row exists
// for (user, event). Compensate transitions it to EXPIRED and INCRBYs
// inventory by seats.
func TestCompensate_RowFound(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eID = int64(71001)
	seedEvent(t, pool, eID)
	uid := "exp-alice-" + uuid.NewString()[:8]

	// Seed inventory at 7 so we can detect the +1.
	require.NoError(t, rdb.Set(context.Background(),
		redispkg.InventoryKey(eID), 7, 0).Err())
	t.Cleanup(func() { rdb.Del(context.Background(), redispkg.InventoryKey(eID)) })

	id := insertPendingReservation(t, pool, eID, uid, 1, time.Now().Add(10*time.Minute))

	require.NoError(t, Compensate(context.Background(), pool, rdb, eID, uid))

	// DB: row flipped to EXPIRED
	row, err := db.GetReservation(context.Background(), pool, id)
	require.NoError(t, err)
	require.Equal(t, db.StatusExpired, row.Status)

	// Redis: inventory went from 7 → 8
	inv, err := rdb.Get(context.Background(), redispkg.InventoryKey(eID)).Int()
	require.NoError(t, err)
	require.Equal(t, 8, inv, "inventory should have been incremented by 1")
}

// TestCompensate_MultiSeat — INCRBY is by seats (not by 1).
func TestCompensate_MultiSeat(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eID = int64(71002)
	seedEvent(t, pool, eID)
	uid := "exp-group-" + uuid.NewString()[:8]

	require.NoError(t, rdb.Set(context.Background(),
		redispkg.InventoryKey(eID), 2, 0).Err())
	t.Cleanup(func() { rdb.Del(context.Background(), redispkg.InventoryKey(eID)) })

	insertPendingReservation(t, pool, eID, uid, 3, time.Now().Add(10*time.Minute))

	require.NoError(t, Compensate(context.Background(), pool, rdb, eID, uid))

	inv, err := rdb.Get(context.Background(), redispkg.InventoryKey(eID)).Int()
	require.NoError(t, err)
	require.Equal(t, 5, inv, "inventory += 3 (2 → 5)")
}

// TestCompensate_NoRow — orphan hold key. No DB row matches → no error,
// no INCRBY. (We don't INCRBY a default because we don't know the seat count.)
func TestCompensate_NoRow(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eID = int64(71003)
	seedEvent(t, pool, eID)
	uid := "orphan-" + uuid.NewString()[:8]

	require.NoError(t, rdb.Set(context.Background(),
		redispkg.InventoryKey(eID), 4, 0).Err())
	t.Cleanup(func() { rdb.Del(context.Background(), redispkg.InventoryKey(eID)) })

	require.NoError(t, Compensate(context.Background(), pool, rdb, eID, uid))

	inv, err := rdb.Get(context.Background(), redispkg.InventoryKey(eID)).Int()
	require.NoError(t, err)
	require.Equal(t, 4, inv, "inventory unchanged when no DB row matches")
}

// TestCompensate_AlreadyNonPending — row exists but status != PENDING.
// No work done. Simulates the race where the watcher fires after the
// sweep has already transitioned the row.
func TestCompensate_AlreadyNonPending(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eID = int64(71004)
	seedEvent(t, pool, eID)
	uid := "already-" + uuid.NewString()[:8]

	require.NoError(t, rdb.Set(context.Background(),
		redispkg.InventoryKey(eID), 5, 0).Err())
	t.Cleanup(func() { rdb.Del(context.Background(), redispkg.InventoryKey(eID)) })

	id := insertPendingReservation(t, pool, eID, uid, 1, time.Now().Add(10*time.Minute))

	// Pre-flip the row to CONFIRMED.
	_, err := pool.Exec(context.Background(),
		"UPDATE reservations SET status = 'CONFIRMED' WHERE reservation_id = $1", id)
	require.NoError(t, err)

	require.NoError(t, Compensate(context.Background(), pool, rdb, eID, uid))

	// Inventory unchanged.
	inv, err := rdb.Get(context.Background(), redispkg.InventoryKey(eID)).Int()
	require.NoError(t, err)
	require.Equal(t, 5, inv, "inventory unchanged when row is non-PENDING")

	// Status still CONFIRMED (we didn't touch it).
	row, err := db.GetReservation(context.Background(), pool, id)
	require.NoError(t, err)
	require.Equal(t, db.StatusConfirmed, row.Status)
}

// TestCompensate_Idempotent — calling Compensate twice for the same
// (user, event) only INCRBYs once. The second call's CAS UPDATE
// finds 0 rows and returns 0 seats.
func TestCompensate_Idempotent(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eID = int64(71005)
	seedEvent(t, pool, eID)
	uid := "idemp-" + uuid.NewString()[:8]

	require.NoError(t, rdb.Set(context.Background(),
		redispkg.InventoryKey(eID), 6, 0).Err())
	t.Cleanup(func() { rdb.Del(context.Background(), redispkg.InventoryKey(eID)) })

	insertPendingReservation(t, pool, eID, uid, 1, time.Now().Add(10*time.Minute))

	require.NoError(t, Compensate(context.Background(), pool, rdb, eID, uid))
	require.NoError(t, Compensate(context.Background(), pool, rdb, eID, uid))

	// Exactly one INCRBY happened: 6 → 7, not 6 → 8.
	inv, err := rdb.Get(context.Background(), redispkg.InventoryKey(eID)).Int()
	require.NoError(t, err)
	require.Equal(t, 7, inv, "second Compensate should be a no-op (CAS)")
}

// TestCompensate_Concurrent — 50 goroutines race on Compensate for the
// same (user, event). Exactly one CAS wins; inventory only goes up by 1.
// Same property as the SQS worker idempotency test, just on a
// different code path.
func TestCompensate_Concurrent(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eID = int64(71006)
	seedEvent(t, pool, eID)
	uid := "concur-" + uuid.NewString()[:8]

	require.NoError(t, rdb.Set(context.Background(),
		redispkg.InventoryKey(eID), 0, 0).Err())
	t.Cleanup(func() { rdb.Del(context.Background(), redispkg.InventoryKey(eID)) })

	insertPendingReservation(t, pool, eID, uid, 1, time.Now().Add(10*time.Minute))

	const N = 50
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			errCh <- Compensate(context.Background(), pool, rdb, eID, uid)
		}()
	}
	for i := 0; i < N; i++ {
		require.NoError(t, <-errCh)
	}

	inv, err := rdb.Get(context.Background(), redispkg.InventoryKey(eID)).Int()
	require.NoError(t, err)
	require.Equal(t, 1, inv, "50 concurrent Compensate calls → 1 INCRBY (CAS winner)")
}

// silence unused-import warning for fmt kept for any future debug helper.
var _ = fmt.Sprintf