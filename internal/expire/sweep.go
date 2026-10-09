package expire

import (
	"context"
	"log/slog"
	"time"

	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/metrics"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// SweptRow is one row the sweep transitioned from PENDING_PAYMENT to EXPIRED.
// Returned by FindAndExpireSweptRows so the caller can run the
// compensating INCRBY in Redis (the SQL only flips the DB state).
type SweptRow struct {
	ReservationID string
	UserID        string
	EventID       int64
	Seats         int
}

// FindAndExpireSweptRows atomically transitions every PENDING_PAYMENT row
// whose expires_at has passed to EXPIRED, and returns the swept rows so
// the caller can run the Redis compensating INCRBY.
//
// Idempotent — the WHERE limits the UPDATE to rows currently PENDING
// (status='PENDING_PAYMENT' AND expires_at < NOW()), so a re-run finds
// none.
//
// We use UPDATE ... RETURNING instead of SELECT-then-UPDATE so the
// transition is atomic per row (no race where a row flips between
// SELECT and UPDATE).
func FindAndExpireSweptRows(ctx context.Context, pool *db.Pool) ([]SweptRow, error) {
	const q = `
		UPDATE reservations
		SET status = 'EXPIRED', updated_at = NOW()
		WHERE status = 'PENDING_PAYMENT' AND expires_at < NOW()
		RETURNING reservation_id, user_id, event_id, seats
	`
	rows, err := pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var swept []SweptRow
	for rows.Next() {
		var r SweptRow
		if scanErr := rows.Scan(&r.ReservationID, &r.UserID, &r.EventID, &r.Seats); scanErr != nil {
			return nil, scanErr
		}
		swept = append(swept, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return swept, nil
}

// RunSweep runs the expiry sweep every interval until ctx is canceled.
//
// service: label added to expiration_sweep_cycles_total and
//          expiration_seats_expired_total metrics. Pass "worker" or
//          "watcher" to disambiguate when both run sweeps.
//
// interval: typically 60 s. Lower = faster catch-up but more DB load;
// higher = seats stuck "held" for longer if the watcher is down.
//
// Behavior per tick:
//  1. Run FindAndExpireSweptRows (atomic UPDATE+RETURNING).
//  2. For each row returned, INCRBY the inventory counter so the
//     seat is sellable again.
//
// Idempotency: re-running on already-swept DB is a no-op (status
// already EXPIRED, won't match the WHERE clause).
//
// INCRBY failures are logged but do not block subsequent rows: each
// row is independent, and the next sweep won't re-process it (status
// already EXPIRED in DB). Operator alerting should fire on INCRBY errors.
func RunSweep(ctx context.Context, pool *db.Pool, rdb *redis.Client, service string, interval time.Duration) error {
	slog.InfoContext(ctx, "sweep starting", "service", service, "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run once immediately so we don't wait `interval` before the first
	// pass — useful for tests and for fast startup.
	sweepOnce(ctx, pool, rdb, service)

	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "sweep clean shutdown")
			return ctx.Err()
		case <-ticker.C:
			sweepOnce(ctx, pool, rdb, service)
		}
	}
}

// sweepOnce runs one pass of the sweep: UPDATE expired rows, then
// INCRBY inventory for each. Logs counts + per-row failures.
func sweepOnce(ctx context.Context, pool *db.Pool, rdb *redis.Client, service string) {
	metrics.ExpirationSweepCycles.WithLabelValues(service).Inc()
	rows, err := FindAndExpireSweptRows(ctx, pool)
	if err != nil {
		slog.ErrorContext(ctx, "sweep query failed", "error", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	metrics.ExpirationSeatsExpired.WithLabelValues(service).Add(float64(len(rows)))
	slog.InfoContext(ctx, "sweep transitioned rows to EXPIRED", "count", len(rows))
	for _, r := range rows {
		if err := rdb.IncrBy(ctx, redis.InventoryKey(r.EventID), int64(r.Seats)).Err(); err != nil {
			slog.ErrorContext(ctx, "sweep INCRBY failed",
				"event", r.EventID,
				"user", r.UserID,
				"seats", r.Seats,
				"error", err,
			)
			continue
		}
		slog.InfoContext(ctx, "sweep compensated",
			"event", r.EventID,
			"user", r.UserID,
			"seats", r.Seats,
		)
	}
}