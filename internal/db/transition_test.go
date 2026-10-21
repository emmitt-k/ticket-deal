package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestIsValidTransition — pure-Go test of the state-machine graph.
// Drives the transition table exhaustively so adding a new edge (and
// forgetting to add a unit test case) gets caught at CI time.
func TestIsValidTransition(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		// Edges that exist
		{StatusPendingPayment, StatusConfirmed, true},
		{StatusPendingPayment, StatusExpired, true},
		{StatusPendingPayment, StatusCancelled, true},
		{StatusConfirmed, StatusCancelled, true},

		// No self-loops (would be a no-op transition; not in the spec)
		{StatusPendingPayment, StatusPendingPayment, false},
		{StatusConfirmed, StatusConfirmed, false},
		{StatusExpired, StatusExpired, false},
		{StatusCancelled, StatusCancelled, false},

		// Terminal states have no outgoing edges
		{StatusExpired, StatusConfirmed, false},
		{StatusExpired, StatusCancelled, false},
		{StatusExpired, StatusPendingPayment, false},
		{StatusCancelled, StatusConfirmed, false},
		{StatusCancelled, StatusExpired, false},
		{StatusCancelled, StatusPendingPayment, false},

		// CONFIRMED can only go to CANCELLED (refund-as-cancel)
		{StatusConfirmed, StatusPendingPayment, false},
		{StatusConfirmed, StatusExpired, false},

		// Illegal backward edges — no resurrection
		{StatusConfirmed, StatusPendingPayment, false}, // duplicate assertion above, kept for readability

		// Unknown from/to
		{"UNKNOWN", "PENDING_PAYMENT", false},
		{"PENDING_PAYMENT", "UNKNOWN", false},
		{"", "", false},
		{"FOO", "BAR", false},
	}

	for _, tc := range cases {
		t.Run(tc.from+"_to_"+tc.to, func(t *testing.T) {
			got := isValidTransition(tc.from, tc.to)
			require.Equal(t, tc.want, got,
				"isValidTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
		})
	}
}

// TestTransitionStatus_RejectsInvalidTransition — programmer error path.
// Even with a live DB, an invalid from→to never reaches the SQL —
// it's caught by isValidTransition before the UPDATE.
func TestTransitionStatus_RejectsInvalidTransition(t *testing.T) {
	pool := newTestPool(t)
	id := uuid.NewString() // a row we'll never actually have

	cases := []struct {
		name, from, to string
	}{
		{"unknown source", "UNKNOWN", StatusPendingPayment},
		{"terminal->terminal", StatusExpired, StatusPendingPayment},
		{"self-loop", StatusPendingPayment, StatusPendingPayment},
		{"empty strings", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := TransitionStatus(context.Background(), pool, id, tc.from, tc.to)
			require.Error(t, err, "TransitionStatus(%q, %q) should return an error", tc.from, tc.to)
			require.ErrorIs(t, err, ErrInvalidTransition,
				"caller should detect via errors.Is(err, ErrInvalidTransition)")
			require.False(t, changed, "no row should change when the transition is rejected")
		})
	}
}

// TestTransitionStatus_HappyPath — live DB: insert a PENDING row, then
// flip it to EXPIRED. Verify the returned (changed, err) = (true, nil)
// and the row's status is now EXPIRED. This is the foundation that
// Layer 1 item 3 (cancel endpoint) and Layer 2 item 6 (audit log) will
// build on.
func TestTransitionStatus_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(62001)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)

	// Seed a PENDING_PAYMENT row.
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	// Flip to EXPIRED.
	changed, err := TransitionStatus(context.Background(), pool,
		r.ReservationID, StatusPendingPayment, StatusExpired)
	require.NoError(t, err)
	require.True(t, changed, "expected the CAS to flip the row from PENDING to EXPIRED")

	row, err := GetReservation(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Equal(t, StatusExpired, row.Status)
}

// TestTransitionStatus_CASMiss — live DB: insert a PENDING row, flip it
// to EXPIRED, then ask for PENDING→EXPIRED again. The second call sees
// the row already in EXPIRED (not PENDING) so the CAS misses.
//
// Returns (false, nil) — NOT (false, ErrInvalidTransition). The row is
// already in a valid status (EXPIRED), it's just not the one we asked to
// leave from. This is the "watcher got there first" case.
func TestTransitionStatus_CASMiss(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(62002)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)

	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	// First call: actually flips it.
	changed, err := TransitionStatus(context.Background(), pool,
		r.ReservationID, StatusPendingPayment, StatusExpired)
	require.NoError(t, err)
	require.True(t, changed)

	// Second call: row is in EXPIRED, not PENDING → CAS misses.
	changed, err = TransitionStatus(context.Background(), pool,
		r.ReservationID, StatusPendingPayment, StatusExpired)
	require.NoError(t, err, "CAS miss must NOT return an error — the row is in a valid status")
	require.False(t, changed, "no rows should change on the second call")

	// Status should still be EXPIRED.
	row, err := GetReservation(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Equal(t, StatusExpired, row.Status)
}

// TestTransitionStatus_NonexistentRow — live DB: there's no row for this
// ID. CAS sees 0 rows affected. Returns (false, nil) — same convention
// as CASMiss; absence is indistinguishable from "row in some other
// status" via the WHERE filter alone.
func TestTransitionStatus_NonexistentRow(t *testing.T) {
	pool := newTestPool(t)
	id := uuid.NewString() // guaranteed unique

	changed, err := TransitionStatus(context.Background(), pool,
		id, StatusPendingPayment, StatusExpired)
	require.NoError(t, err, "nonexistent row is not an error")
	require.False(t, changed)
}

// ensure errors.Is is wired through correctly even if a future test
// silently swallows ErrInvalidTransition
var _ = errors.Is
