package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/queue"
)

// TestListReservationsByUser_NoRows — totally-empty case. Must return
// ([]ReservationRow{}, nil), NOT nil-or-error. Frontend handles
// 'no tickets yet' from the empty slice.
func TestListReservationsByUser_NoRows(t *testing.T) {
	pool := newTestPool(t)

	got, err := ListReservationsByUser(context.Background(), pool,
		"user-does-not-exist-"+uuid.NewString(), "", 20, 0)
	require.NoError(t, err, "empty result is not an error")
	require.NotNil(t, got, "should return empty slice, not nil — makes JSON encoding predictable")
	require.Empty(t, got)
}

// TestListReservationsByUser_FiltersByUser — sanity: rows for user A
// don't leak into user B's list.
func TestListReservationsByUser_FiltersByUser(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(63001)

	seedEvent(t, pool, eventID)
	uA := "user-a-" + uuid.NewString()[:8]
	uB := "user-b-" + uuid.NewString()[:8]

	// Insert 2 reservations for A and 1 for B.
	for i := 0; i < 2; i++ {
		r := validReservationFor(t, eventID, uA)
		cleanupReservation(t, pool, r.ReservationID)
		require.NoError(t, InsertIfAbsent(context.Background(), pool, r))
	}
	rB := validReservationFor(t, eventID, uB)
	cleanupReservation(t, pool, rB.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, rB))

	// Listing for A returns exactly the 2 A-rows.
	gotA, err := ListReservationsByUser(context.Background(), pool, uA, "", 20, 0)
	require.NoError(t, err)
	require.Len(t, gotA, 2)
	for _, r := range gotA {
		require.Equal(t, uA, r.UserID)
		require.Equal(t, eventID, r.EventID)
		require.Equal(t, StatusPendingPayment, r.Status)
		require.NotEmpty(t, r.EventName, "JOIN with events should populate event_name")
	}

	// Listing for B returns exactly 1.
	gotB, err := ListReservationsByUser(context.Background(), pool, uB, "", 20, 0)
	require.NoError(t, err)
	require.Len(t, gotB, 1)
	require.Equal(t, uB, gotB[0].UserID)
}

// TestListReservationsByUser_StatusFilter — when ?status=EXPIRED is
// passed, only EXPIRED rows come back.
func TestListReservationsByUser_StatusFilter(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(63002)

	seedEvent(t, pool, eventID)
	u := "user-status-" + uuid.NewString()[:8]

	// Insert 2 PENDING_PAYMENT rows for the same user, then flip one to EXPIRED.
	r1 := validReservationFor(t, eventID, u)
	cleanupReservation(t, pool, r1.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r1))

	r2 := validReservationFor(t, eventID, u)
	cleanupReservation(t, pool, r2.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r2))

	changed, err := TransitionStatus(context.Background(), pool,
		r1.ReservationID, StatusPendingPayment, StatusExpired)
	require.NoError(t, err)
	require.True(t, changed)

	// status=EXPIRED → 1 row (r1).
	gotExpired, err := ListReservationsByUser(context.Background(), pool, u, StatusExpired, 20, 0)
	require.NoError(t, err)
	require.Len(t, gotExpired, 1)
	require.Equal(t, r1.ReservationID, gotExpired[0].ReservationID)
	require.Equal(t, StatusExpired, gotExpired[0].Status)

	// status=PENDING_PAYMENT → 1 row (r2).
	gotPending, err := ListReservationsByUser(context.Background(), pool, u, StatusPendingPayment, 20, 0)
	require.NoError(t, err)
	require.Len(t, gotPending, 1)
	require.Equal(t, r2.ReservationID, gotPending[0].ReservationID)
	require.Equal(t, StatusPendingPayment, gotPending[0].Status)

	// No status filter → both rows, newest first.
	gotAll, err := ListReservationsByUser(context.Background(), pool, u, "", 20, 0)
	require.NoError(t, err)
	require.Len(t, gotAll, 2)
	// r2 has a later created_at (inserted second), so it should be first.
	require.Equal(t, r2.ReservationID, gotAll[0].ReservationID)
	require.Equal(t, r1.ReservationID, gotAll[1].ReservationID)
}

// TestListReservationsByUser_Pagination — limit + offset correctly
// page through results; default + max bounds enforced.
func TestListReservationsByUser_Pagination(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(63003)

	seedEvent(t, pool, eventID)
	u := "user-page-" + uuid.NewString()[:8]

	const N = 5
	ids := make([]string, N)
	for i := 0; i < N; i++ {
		r := validReservationFor(t, eventID, u)
		ids[i] = r.ReservationID
		cleanupReservation(t, pool, r.ReservationID)
		require.NoError(t, InsertIfAbsent(context.Background(), pool, r))
	}

	// limit=2, offset=0 → first 2 (newest first = ids[N-1], ids[N-2])
	page1, err := ListReservationsByUser(context.Background(), pool, u, "", 2, 0)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	require.Equal(t, ids[N-1], page1[0].ReservationID)
	require.Equal(t, ids[N-2], page1[1].ReservationID)

	// limit=2, offset=2 → next 2 (ids[N-3], ids[N-4])
	page2, err := ListReservationsByUser(context.Background(), pool, u, "", 2, 2)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	require.Equal(t, ids[N-3], page2[0].ReservationID)
	require.Equal(t, ids[N-4], page2[1].ReservationID)

	// limit=2, offset=4 → 1 leftover
	page3, err := ListReservationsByUser(context.Background(), pool, u, "", 2, 4)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	require.Equal(t, ids[0], page3[0].ReservationID)

	// limit=0 → defaults to 20 (we have 5 rows so all come back)
	all, err := ListReservationsByUser(context.Background(), pool, u, "", 0, 0)
	require.NoError(t, err)
	require.Len(t, all, N)

	// limit=10000 → clamped to 100 (still < N, no observable effect here,
	// but exercises the clamp branch).
	clamped, err := ListReservationsByUser(context.Background(), pool, u, "", 10000, 0)
	require.NoError(t, err)
	require.Len(t, clamped, N)
}

// TestListReservationsByUser_RequiresUserID — programmatic error
// guard; the API layer can't accidentally query the whole table.
func TestListReservationsByUser_RequiresUserID(t *testing.T) {
	pool := newTestPool(t)

	_, err := ListReservationsByUser(context.Background(), pool, "", "", 20, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires userID")
}

// ── Helper for these tests ────────────────────────────────────────────

// validReservationFor is a copy of validReservation() that takes an
// explicit userID instead of generating one. Lets a single test use
// multiple distinct users without colliding on the unique constraint.
func validReservationFor(t *testing.T, eventID int64, userID string) queue.Reservation {
	t.Helper()
	r := validReservation(t, eventID) // reuses reservations_test.go's helper
	r.UserID = userID
	return r
}
