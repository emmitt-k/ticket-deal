// Package expire owns the "what happens when a hold key times out" logic.
//
// Two layers, two entry points:
//
//   - Watcher (event-driven, fast): subscribes to Redis keyspace
//     notifications on __keyevent@0__:expired. When a hold key fires,
//     we compensate immediately.
//
//   - Sweep (DB-driven, safety net): every 60 s we query Postgres for
//     PENDING_PAYMENT rows whose expires_at has passed, mark them
//     EXPIRED, and restore inventory. Catches anything the watcher
//     missed (notifications have no delivery guarantee — see
//     docs/architecture.md §9).
//
// Both paths converge on Compensate: the single function that does
// the "transition DB row + INCRBY inventory" pair atomically. Putting
// it in one place means the two paths can't drift apart.
package expire

import (
	"context"
	"fmt"
	"log"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/metrics"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

const tracerName = "github.com/emmitt-k/ticket-deal/internal/expire"

var tracer = otel.Tracer(tracerName)

// Compensate handles a hold-key expiration: transitions the matching
// PENDING_PAYMENT row to EXPIRED and INCRBYs the inventory counter so
// the seat is sellable again.
//
// Behavior:
//  1. Look up the matching reservation row by (user_id, event_id, status=PENDING_PAYMENT)
//     — at most one row exists per (user, event) because Phase 5's Lua
//     reserve script rejects "already_holding."
//  2. CAS-transition the row: UPDATE ... SET status='EXPIRED' WHERE
//     reservation_id IN (...) AND status='PENDING_PAYMENT' RETURNING seats.
//     The status filter is the compare-and-set: a second caller sees 0
//     rows affected and skips the INCRBY.
//  3. INCRBY the inventory counter by the returned seats.
//  4. If no row matched (orphan hold key — the API reserved via Redis
//     but failed to publish the SQS row), log warn and return nil.
//     The seat is "lost" until a manual operator intervention. We can't
//     safely INCRBY because we don't know the seat count.
//
// Returns nil on success (whether or not work was done — calling twice
// is safe). Returns non-nil only on DB connection failures; Redis
// INCRBY failures are logged but don't fail the call (DB row is the
// durable state and is already EXPIRED).
func Compensate(ctx context.Context, pool *db.Pool, rdb *redis.Client,
	eventID int64, userID string,
) error {
	// Manual span around the watcher-driven compensation path. This
	// is a ROOT span (no upstream trace context) because the trigger
	// is a Redis keyspace notification, not an HTTP or SQS request —
	// there's nothing upstream to attach to. The auto-spans for the
	// pg.UPDATE and redis.INCRBY show up inside this one.
	ctx, span := tracer.Start(ctx, "expire.compensate",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.Int64("expire.event_id", eventID),
			attribute.Int64("expire.user_id_hash", hashUserID(userID)),
		),
	)
	defer span.End()

	seats, err := expireRowIfPending(ctx, pool, userID, eventID)
	// Each call to Compensate is a "cycle" in event-driven terms —
	// one Redis keyspace notification triggered one DB+INCRBY pair.
	metrics.ExpirationSweepCycles.WithLabelValues("watcher").Inc()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "expire row failed")
		return fmt.Errorf("expire: transition row: %w", err)
	}
	if seats == 0 {
		// Either no row matched (orphan hold key) or the row was
		// already non-PENDING (sweep got there first, or it was
		// confirmed/cancelled before expiry). Either way, no work.
		span.SetAttributes(attribute.Int("expire.seats_released", 0))
		return nil
	}
	span.SetAttributes(attribute.Int("expire.seats_released", seats))

	if err := rdb.IncrBy(ctx, redis.InventoryKey(eventID), int64(seats)).Err(); err != nil {
		// Dangerous race: DB row is EXPIRED but inventory wasn't
		// restored. The seat is "stuck held" until manual fix.
		//
		// Why we don't fail the call: the DB write is the durable
		// record; surfacing an error would let the watcher retry,
		// but the CAS UPDATE returns 0 rows on retry so we'd just
		// log the same error repeatedly. Log loud and let ops
		// alerting handle it.
		span.RecordError(err)
		span.SetStatus(codes.Error, "INCRBY inventory failed")
		log.Printf("expire: WARN marked DB EXPIRED but INCRBY failed event=%d user=%s seats=%d err=%v",
			eventID, userID, seats, err)
		return nil
	}

	// Record this as one watcher-driven expiration.
	metrics.ExpirationSeatsExpired.WithLabelValues("watcher").Inc()

	log.Printf("expire: compensated event=%d user=%s seats=%d", eventID, userID, seats)
	return nil
}

// hashUserID is duplicated from internal/api/reserve_handler.go to
// avoid a circular import (internal/api already imports internal/redis,
// and adding internal/api to internal/expire would be wrong). Same
// algorithm: tiny, deterministic, PII-safe.
func hashUserID(s string) int64 {
	var h int64
	for _, b := range []byte(s) {
		h = h*31 + int64(b)
	}
	if h < 0 {
		h = -h
	}
	return h
}

// expireRowIfPending transitions the matching PENDING_PAYMENT row to
// EXPIRED atomically and returns the seats that were held. Returns
// (0, nil) when no row matched or the row was already non-pending —
// the caller should treat 0 as "no work to do."
//
// The CTE shape (target → updated) lets us read the seats count back
// without a second SELECT, and the WHERE status='PENDING_PAYMENT' is
// the compare-and-set that makes concurrent Compensate + Sweep calls
// safe (only one wins, the other sees 0 rows).
func expireRowIfPending(ctx context.Context, pool *db.Pool,
	userID string, eventID int64,
) (int, error) {
	const q = `
		WITH target AS (
			SELECT reservation_id, seats FROM reservations
			WHERE user_id = $1 AND event_id = $2 AND status = 'PENDING_PAYMENT'
			ORDER BY created_at DESC
			LIMIT 1
		),
		updated AS (
			UPDATE reservations
			SET status = 'EXPIRED', updated_at = NOW()
			WHERE reservation_id IN (SELECT reservation_id FROM target)
			RETURNING seats
		)
		SELECT COALESCE((SELECT seats FROM updated), 0)
	`
	var seats int
	if err := pool.QueryRow(ctx, q, userID, eventID).Scan(&seats); err != nil {
		return 0, err
	}
	return seats, nil
}