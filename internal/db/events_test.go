package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// newEventPool returns a pool or skips.
func newEventPool(t *testing.T) *Pool {
	t.Helper()
	pool := newTestPool(t)
	return pool
}

// createTestEvent creates an event and removes it afterwards.
//
// The event's id is a fresh UUID (the application mints it), so there
// is no per-package id range to keep apart any more — a uuid
// collision is not a thing that can happen.
func createTestEvent(t *testing.T, pool *Pool, name string, inv int) EventRow {
	t.Helper()
	price := int64(2500)
	row, err := CreateEvent(context.Background(), pool, EventCreate{
		Name:             name,
		InitialInventory: inv,
		PriceCents:       &price,
		Currency:         "usd",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", row.ID)
	})
	return row
}

// ── Create ─────────────────────────────────────────────────────────────

func TestCreateEvent_MintsTheId(t *testing.T) {
	pool := newEventPool(t)

	price := int64(1500)
	row, err := CreateEvent(context.Background(), pool, EventCreate{
		Name:             "created-event",
		InitialInventory: 42,
		PriceCents:       &price,
		Currency:         "usd",
	})
	require.NoError(t, err)

	// The id is a UUID, minted by the app — not a counter, and not
	// something the caller supplied.
	require.Len(t, row.ID, 36)
	require.NotEqual(t, "00000000-0000-0000-0000-000000000000", row.ID)
	require.Equal(t, "created-event", row.Name)
	require.Equal(t, EventStatusActive, row.Status,
		"a new event is active by definition")
	require.Equal(t, 42, row.InitialInventory)
	require.Equal(t, int64(1500), row.PriceCents)
	require.Equal(t, "usd", row.Currency)
	require.False(t, row.CreatedAt.IsZero())

	// Two events must never share an id.
	otherPrice := int64(100)
	other, err := CreateEvent(context.Background(), pool, EventCreate{
		Name: "another", InitialInventory: 1, PriceCents: &otherPrice, Currency: "usd",
	})
	require.NoError(t, err)
	require.NotEqual(t, row.ID, other.ID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", other.ID)
	})
}

func TestCreateEvent_RejectsBadInput(t *testing.T) {
	pool := newEventPool(t)

	one := int64(1)
	negative := int64(-1)
	cases := []struct {
		name string
		ev   EventCreate
	}{
		{"no_name", EventCreate{InitialInventory: 1, PriceCents: &one, Currency: "usd"}},
		{"negative_inventory", EventCreate{Name: "x", InitialInventory: -1, PriceCents: &one, Currency: "usd"}},
		{"negative_price", EventCreate{Name: "x", InitialInventory: 1, PriceCents: &negative, Currency: "usd"}},
		{"no_currency", EventCreate{Name: "x", InitialInventory: 1, PriceCents: &one}},
		{"bad_currency", EventCreate{Name: "x", InitialInventory: 1, PriceCents: &one, Currency: "US"}},
		{"long_currency", EventCreate{Name: "x", InitialInventory: 1, PriceCents: &one, Currency: "usdd"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateEvent(context.Background(), pool, tc.ev)
			require.Error(t, err)
		})
	}
}

// ── Read ───────────────────────────────────────────────────────────────

func TestGetEvent(t *testing.T) {
	pool := newEventPool(t)
	created := createTestEvent(t, pool, "read-me", 7)

	got, err := GetEvent(context.Background(), pool, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	require.Equal(t, "read-me", got.Name)
	require.Equal(t, EventStatusActive, got.Status)
	require.Equal(t, 7, got.InitialInventory)
}

func TestGetEvent_NotFound(t *testing.T) {
	pool := newEventPool(t)

	_, err := GetEvent(context.Background(), pool, uuid.NewString())
	require.Error(t, err)
	require.True(t, IsNoRows(err))
}

// ── List ───────────────────────────────────────────────────────────────

func TestListEvents_PublicHidesCancelled(t *testing.T) {
	pool := newEventPool(t)

	active := createTestEvent(t, pool, "active-one", 5)
	cancelled := createTestEvent(t, pool, "cancelled-one", 5)

	// Cancel it. It has no reservations, so the guard passes.
	_, err := CancelEvent(context.Background(), pool, cancelled.ID)
	require.NoError(t, err)

	// The public view does not contain it.
	public, err := ListEvents(context.Background(), pool, false, 100, 0)
	require.NoError(t, err)
	for _, row := range public {
		require.NotEqual(t, cancelled.ID, row.ID,
			"a cancelled event must not appear in the public catalogue")
	}

	// The admin view does.
	admin, err := ListEvents(context.Background(), pool, true, 100, 0)
	require.NoError(t, err)
	found := false
	for _, row := range admin {
		if row.ID == cancelled.ID {
			found = true
		}
	}
	require.True(t, found, "a cancelled event must appear in the admin view")

	// And the active one is in both.
	for _, rows := range [][]EventRow{public, admin} {
		found := false
		for _, row := range rows {
			if row.ID == active.ID {
				found = true
			}
		}
		require.True(t, found)
	}
}

func TestListEvents_ReturnsAnEmptySliceNotNil(t *testing.T) {
	pool := newEventPool(t)

	// A slice that is nil serialises to `null` in JSON, and a client
	// expecting a list would have to special-case it. (The same bug
	// once lived in ListReservationsByUser.)
	rows, err := ListEvents(context.Background(), pool, false, 100, 0)
	require.NoError(t, err)
	require.NotNil(t, rows)
	require.IsType(t, []EventRow{}, rows)
}

// ── Update ─────────────────────────────────────────────────────────────

func TestUpdateEvent_ChangesOnlyWhatItIsGiven(t *testing.T) {
	pool := newEventPool(t)
	created := createTestEvent(t, pool, "before", 10)

	newName := "after"
	got, err := UpdateEvent(context.Background(), pool, created.ID, EventPatch{
		Name: &newName,
	})
	require.NoError(t, err)
	require.Equal(t, "after", got.Name)
	// The price and the currency were NOT supplied, so they must not
	// have changed. This is the whole point of the pointer fields.
	require.Equal(t, int64(2500), got.PriceCents)
	require.Equal(t, "usd", got.Currency)
	require.Equal(t, 10, got.InitialInventory)
}

func TestUpdateEvent_CanSetThePriceToZero(t *testing.T) {
	pool := newEventPool(t)
	created := createTestEvent(t, pool, "free", 10)

	// "not supplied" and "supplied as zero" are different intents, so
	// a PATCH CAN legitimately set the price to 0 (a free event is
	// legal — the CHECK allows it; Stripe rejects it, which is a
	// payment-layer concern).
	zero := int64(0)
	got, err := UpdateEvent(context.Background(), pool, created.ID, EventPatch{
		PriceCents: &zero,
	})
	require.NoError(t, err)
	require.Equal(t, int64(0), got.PriceCents)
}

func TestUpdateEvent_RefusesACancelledEvent(t *testing.T) {
	pool := newEventPool(t)
	created := createTestEvent(t, pool, "tombstone", 10)

	_, err := CancelEvent(context.Background(), pool, created.ID)
	require.NoError(t, err)

	// A tombstone is a record of what happened. Editing it would make
	// the record lie.
	name := "rewritten"
	_, err = UpdateEvent(context.Background(), pool, created.ID, EventPatch{
		Name: &name,
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrEventCancelled))

	// And the row really did not change.
	got, err := GetEvent(context.Background(), pool, created.ID)
	require.NoError(t, err)
	require.Equal(t, "tombstone", got.Name)
	require.Equal(t, EventStatusCancelled, got.Status)
}

// ── Cancel ─────────────────────────────────────────────────────────────

func TestCancelEvent_SucceedsWithNoReservations(t *testing.T) {
	pool := newEventPool(t)
	created := createTestEvent(t, pool, "cancellable", 10)

	got, err := CancelEvent(context.Background(), pool, created.ID)
	require.NoError(t, err)
	require.Equal(t, EventStatusCancelled, got.Status)

	// The row is NEVER deleted — it is a tombstone, so "what happened
	// to that drop?" still has an answer.
	still, err := GetEvent(context.Background(), pool, created.ID)
	require.NoError(t, err)
	require.Equal(t, EventStatusCancelled, still.Status)
}

func TestCancelEvent_RefusedWhenItHasReservations(t *testing.T) {
	pool := newEventPool(t)
	created := createTestEvent(t, pool, "sold", 10)

	// Give it a reservation. The guard is at "any reservation EVER",
	// not "any reservation is still live".
	r := validReservation(t, created.ID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	_, cancelErr := CancelEvent(context.Background(), pool, created.ID)
	require.Error(t, cancelErr)
	require.True(t, errors.Is(cancelErr, ErrEventHasReservations),
		"the guard must fire, got %v", cancelErr)

	// The event is still live.
	got, err := GetEvent(context.Background(), pool, created.ID)
	require.NoError(t, err)
	require.Equal(t, EventStatusActive, got.Status)

	// The error carries the count.
	count, cerr := CountEventReservations(context.Background(), pool, created.ID)
	require.NoError(t, cerr)
	require.Equal(t, 1, count)
	require.Contains(t, cancelErr.Error(), "1")
}

// TestCancelEvent_RefusedEvenForAnExpiredReservation is the threshold
// decision, stated as a test.
//
// The line is drawn at "any reservation EVER existed", not "any
// reservation is still live". An expired reservation still proves a
// customer interacted with the drop, and pulling the event out from
// under someone who tried to buy is worse than refusing.
func TestCancelEvent_RefusedEvenForAnExpiredReservation(t *testing.T) {
	pool := newEventPool(t)
	created := createTestEvent(t, pool, "expired-only", 10)

	r := validReservation(t, created.ID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	// Expire it.
	_, err := pool.Exec(context.Background(),
		"UPDATE reservations SET status = 'EXPIRED' WHERE reservation_id = $1",
		r.ReservationID)
	require.NoError(t, err)

	_, err = CancelEvent(context.Background(), pool, created.ID)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrEventHasReservations))
}

func TestCancelEvent_NotFound(t *testing.T) {
	pool := newEventPool(t)

	_, err := CancelEvent(context.Background(), pool, uuid.NewString())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrEventNotFound))
}

func TestCancelEvent_AlreadyCancelledIsNotAnError(t *testing.T) {
	pool := newEventPool(t)
	created := createTestEvent(t, pool, "twice", 10)

	_, err := CancelEvent(context.Background(), pool, created.ID)
	require.NoError(t, err)

	// A retried admin call is not a failure.
	_, err = CancelEvent(context.Background(), pool, created.ID)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrEventCancelled))
}

// TestCancelEvent_GuardIsAtomicWithTheUpdate is the TOCTOU test.
//
// The guard lives in the UPDATE's own WHERE clause, not in a separate
// SELECT. Two admins cancelling the same event at the same moment
// could both see zero reservations in a SELECT-then-UPDATE shape, and
// both write — which is the bug Convention #13 is about.
func TestCancelEvent_GuardIsAtomicWithTheUpdate(t *testing.T) {
	pool := newEventPool(t)
	created := createTestEvent(t, pool, "racy", 10)

	r := validReservation(t, created.ID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	// The reservation appears BETWEEN the guard's read and the write
	// in a naive implementation. Here the UPDATE's own WHERE clause
	// sees it, so the cancel must be refused.
	_, err := CancelEvent(context.Background(), pool, created.ID)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrEventHasReservations))
}
