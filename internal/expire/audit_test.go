package expire

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/db"
)

// ── Audit log coverage for the two expiry paths ────────────────────────
//
// Both expiry paths write an EXPIRED audit row, but they do it in
// different ways:
//
//   - the watcher (compensate.go) folds the audit INSERT into the same
//     statement as the UPDATE, using a data-modifying CTE;
//   - the sweep (sweep.go) does the same thing for its batch UPDATE.
//
// These tests assert the outcome both paths care about: exactly one
// EXPIRED audit row per expired reservation, naming the right actor.

// TestCompensate_WritesAuditRow — the watcher's keyspace-notification
// path records an EXPIRED row with actor_type='system' and
// actor_id='expiration-watcher'.
func TestCompensate_WritesAuditRow(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eID = int64(72001)
	seedEvent(t, pool, eID)
	uid := "audit-watcher-" + uuid.NewString()[:8]

	rid := insertPendingReservation(t, pool, eID, uid, 2,
		time.Now().UTC().Add(10*time.Minute))

	require.NoError(t, Compensate(context.Background(), pool, rdb, eID, uid))

	events, err := db.ListAuditEvents(context.Background(), pool, rid)
	require.NoError(t, err)
	require.Len(t, events, 2, "CREATED (from the insert) + EXPIRED")

	e := events[1]
	require.Equal(t, db.AuditEventExpired, e.EventType)
	require.Equal(t, db.ActorTypeSystem, e.ActorType)
	require.Equal(t, ActorIDWatcher, e.ActorID)
	require.Equal(t, db.StatusPendingPayment, e.OldStatus)
	require.Equal(t, db.StatusExpired, e.NewStatus)
	require.Equal(t, uid, e.UserID)
	require.Equal(t, float64(2), e.Metadata["seats"])
}

// TestCompensate_NoAuditRowWhenNothingExpired — an orphan hold key (no
// matching reservation row) must NOT produce an audit row. The audit
// log records facts about reservations, not about Redis keyspace
// noise.
func TestCompensate_NoAuditRowWhenNothingExpired(t *testing.T) {
	pool := newTestPool(t)
	rdb := newTestRedis(t)
	const eID = int64(72002)
	seedEvent(t, pool, eID)
	uid := "audit-orphan-" + uuid.NewString()[:8]

	// No reservation row for this user/event.
	require.NoError(t, Compensate(context.Background(), pool, rdb, eID, uid))

	var count int
	err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM reservation_events WHERE user_id = $1", uid).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 0, count, "an orphan hold key must not write an audit row")
}

// TestFindAndExpireSweptRows_WritesAuditRow — the sweep path records
// an EXPIRED row per swept reservation, with actor_id='expiration-sweep'
// and the service label in the metadata.
func TestFindAndExpireSweptRows_WritesAuditRow(t *testing.T) {
	pool := newTestPool(t)
	const eID = int64(72003)
	seedEvent(t, pool, eID)
	uid := "audit-sweep-" + uuid.NewString()[:8]

	// Already expired → the sweep picks it up.
	rid := insertPendingReservation(t, pool, eID, uid, 1,
		time.Now().UTC().Add(-time.Minute))

	rows, err := FindAndExpireSweptRows(context.Background(), pool, db.Audit{
		Actor:    db.SystemActor(ActorIDSweep),
		Metadata: map[string]any{"service": "test"},
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, rid, rows[0].ReservationID)

	events, err := db.ListAuditEvents(context.Background(), pool, rid)
	require.NoError(t, err)
	require.Len(t, events, 2, "CREATED + EXPIRED")

	e := events[1]
	require.Equal(t, db.AuditEventExpired, e.EventType)
	require.Equal(t, db.ActorTypeSystem, e.ActorType)
	require.Equal(t, ActorIDSweep, e.ActorID)
	require.Equal(t, db.StatusPendingPayment, e.OldStatus)
	require.Equal(t, db.StatusExpired, e.NewStatus)
	require.Equal(t, "test", e.Metadata["service"])
	require.Equal(t, float64(1), e.Metadata["seats"])
}

// TestFindAndExpireSweptRows_NoAuditRowOnSecondRun — the sweep is
// idempotent: the second pass finds no PENDING rows, so it writes no
// second EXPIRED audit row.
func TestFindAndExpireSweptRows_NoAuditRowOnSecondRun(t *testing.T) {
	pool := newTestPool(t)
	const eID = int64(72004)
	seedEvent(t, pool, eID)
	uid := "audit-sweep-idem-" + uuid.NewString()[:8]

	rid := insertPendingReservation(t, pool, eID, uid, 1,
		time.Now().UTC().Add(-time.Minute))

	audit := db.Audit{Actor: db.SystemActor(ActorIDSweep)}
	first, err := FindAndExpireSweptRows(context.Background(), pool, audit)
	require.NoError(t, err)
	require.Len(t, first, 1)

	second, err := FindAndExpireSweptRows(context.Background(), pool, audit)
	require.NoError(t, err)
	require.Empty(t, second, "the second pass finds nothing to sweep")

	events, err := db.ListAuditEvents(context.Background(), pool, rid)
	require.NoError(t, err)
	require.Len(t, events, 2, "exactly one EXPIRED row, no duplicate")
}
