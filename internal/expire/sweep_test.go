package expire

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	redisclient "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/db"
	redispkg "github.com/emmitt-k/ticket-deal/internal/redis"
)

// TestFindAndExpireSweptRows_PicksUpExpired — the headline test:
// a PENDING_PAYMENT row with expires_at in the past is transitioned
// to EXPIRED and returned to the caller for compensating INCRBY.
func TestFindAndExpireSweptRows_PicksUpExpired(t *testing.T) {
	pool := newTestPool(t)
	const eID = int64(72001)
	seedEvent(t, pool, eID)
	uid := "sweep-alice-" + uuid.NewString()[:8]

	id := insertPendingReservation(t, pool, eID, uid, 1,
		time.Now().Add(-1*time.Hour)) // expired an hour ago

	rows, err := FindAndExpireSweptRows(context.Background(), pool)
	require.NoError(t, err)

	var found *SweptRow
	for i := range rows {
		if rows[i].ReservationID == id {
			found = &rows[i]
			break
		}
	}
	require.NotNil(t, found, "expired row should be returned")
	require.Equal(t, uid, found.UserID)
	require.Equal(t, eID, found.EventID)
	require.Equal(t, 1, found.Seats)

	// DB row is now EXPIRED.
	row, err := db.GetReservation(context.Background(), pool, id)
	require.NoError(t, err)
	require.Equal(t, db.StatusExpired, row.Status)
}

// TestFindAndExpireSweptRows_IgnoresNotExpired — a row that's still
// PENDING (expires_at in the future) must NOT be touched.
func TestFindAndExpireSweptRows_IgnoresNotExpired(t *testing.T) {
	pool := newTestPool(t)
	const eID = int64(72002)
	seedEvent(t, pool, eID)
	uid := "future-" + uuid.NewString()[:8]

	id := insertPendingReservation(t, pool, eID, uid, 1,
		time.Now().Add(10*time.Minute))

	rows, err := FindAndExpireSweptRows(context.Background(), pool)
	require.NoError(t, err)
	for _, r := range rows {
		require.NotEqual(t, id, r.ReservationID,
			"future-expiry row should not be returned")
	}

	// Status still PENDING_PAYMENT.
	row, err := db.GetReservation(context.Background(), pool, id)
	require.NoError(t, err)
	require.Equal(t, db.StatusPendingPayment, row.Status)
}

// TestFindAndExpireSweptRows_IgnoresNonPending — expired_at is past
// but status != PENDING_PAYMENT (e.g. CONFIRMED). The WHERE clause
// filters these out.
func TestFindAndExpireSweptRows_IgnoresNonPending(t *testing.T) {
	pool := newTestPool(t)
	const eID = int64(72003)
	seedEvent(t, pool, eID)
	uid := "confirmed-" + uuid.NewString()[:8]

	id := insertPendingReservation(t, pool, eID, uid, 1,
		time.Now().Add(-1*time.Hour))
	// Flip to CONFIRMED.
	_, err := pool.Exec(context.Background(),
		"UPDATE reservations SET status = 'CONFIRMED' WHERE reservation_id = $1", id)
	require.NoError(t, err)

	rows, err := FindAndExpireSweptRows(context.Background(), pool)
	require.NoError(t, err)
	for _, r := range rows {
		require.NotEqual(t, id, r.ReservationID,
			"non-PENDING row should not be returned")
	}

	// Status still CONFIRMED (untouched).
	row, err := db.GetReservation(context.Background(), pool, id)
	require.NoError(t, err)
	require.Equal(t, db.StatusConfirmed, row.Status)
}

// TestFindAndExpireSweptRows_Idempotent — calling twice in a row:
// second call returns nothing because all matching rows are already
// EXPIRED.
func TestFindAndExpireSweptRows_Idempotent(t *testing.T) {
	pool := newTestPool(t)
	const eID = int64(72004)
	seedEvent(t, pool, eID)
	uid := "idemp-" + uuid.NewString()[:8]

	id := insertPendingReservation(t, pool, eID, uid, 1,
		time.Now().Add(-1*time.Hour))

	first, err := FindAndExpireSweptRows(context.Background(), pool)
	require.NoError(t, err)
	var foundInFirst bool
	for _, r := range first {
		if r.ReservationID == id {
			foundInFirst = true
		}
	}
	require.True(t, foundInFirst, "first call returns the row")

	second, err := FindAndExpireSweptRows(context.Background(), pool)
	require.NoError(t, err)
	for _, r := range second {
		require.NotEqual(t, id, r.ReservationID,
			"second call returns nothing — already EXPIRED")
	}
}

// TestFindAndExpireSweptRows_MultiSeat — a row with seats=3 reports 3
// to the caller (so the INCRBY is sized right).
func TestFindAndExpireSweptRows_MultiSeat(t *testing.T) {
	pool := newTestPool(t)
	const eID = int64(72005)
	seedEvent(t, pool, eID)
	uid := "group-" + uuid.NewString()[:8]

	id := insertPendingReservation(t, pool, eID, uid, 3,
		time.Now().Add(-1*time.Hour))

	rows, err := FindAndExpireSweptRows(context.Background(), pool)
	require.NoError(t, err)

	var found *SweptRow
	for i := range rows {
		if rows[i].ReservationID == id {
			found = &rows[i]
			break
		}
	}
	require.NotNil(t, found)
	require.Equal(t, 3, found.Seats, "multi-seat row reports its seat count")
}

// TestRunSweep_ContextCancel — RunSweep bails on ctx cancel.
func TestRunSweep_ContextCancel(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before starting

	err := RunSweep(ctx, pool, rdb, "test", time.Minute)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

// TestRunSweep_Integration — a full sweep tick: insert an expired
// PENDING_PAYMENT row, set inventory to 5, run one tick (50 ms
// interval), see inventory go up by the seat count and row flip.
//
// This is the "real" test: it exercises both the SQL UPDATE and
// the Redis INCRBY together.
func TestRunSweep_Integration(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eID = int64(72006)
	seedEvent(t, pool, eID)
	uid := "integ-" + uuid.NewString()[:8]

	require.NoError(t, rdb.Set(context.Background(),
		redispkg.InventoryKey(eID), 5, 0).Err())
	t.Cleanup(func() { rdb.Del(context.Background(), redispkg.InventoryKey(eID)) })

	id := insertPendingReservation(t, pool, eID, uid, 2,
		time.Now().Add(-1*time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Short interval so the test runs fast.
	done := make(chan error, 1)
	go func() {
		done <- RunSweep(ctx, pool, rdb, "test", 50*time.Millisecond)
	}()

	// Poll inventory for up to 2s; expect 7 (5 + 2 seats).
	deadline := time.Now().Add(2 * time.Second)
	var inv int
	var err error
	for time.Now().Before(deadline) {
		inv, err = rdb.Get(context.Background(), redispkg.InventoryKey(eID)).Int()
		require.NoError(t, err)
		if inv == 7 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Equal(t, 7, inv, "sweep should have INCRBYed by seats (5 → 7)")

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	// DB row is EXPIRED.
	row, err := db.GetReservation(context.Background(), pool, id)
	require.NoError(t, err)
	require.Equal(t, db.StatusExpired, row.Status)
}

// silence unused-import warning for redisclient kept for future test helpers.
var _ redisclient.Cmdable = (*redisclient.Client)(nil)