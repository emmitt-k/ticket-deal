package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/emmitt-k/ticket-deal/internal/queue"
)

// ReservationStatus values mirror the CHECK constraint in
// migrations/001_init.sql. Defining them here (not as inline strings)
// means a typo at the call site fails to compile.
const (
	StatusPendingPayment = "PENDING_PAYMENT"
	StatusConfirmed      = "CONFIRMED"
	StatusExpired        = "EXPIRED"
	StatusCancelled      = "CANCELLED"
)

// InsertIfAbsent writes a reservation row using INSERT ... ON CONFLICT
// DO NOTHING, and appends the CREATED audit row in the same
// transaction. The semantics:
//
//   - First insert for this reservation_id  → row created, returns nil
//   - Second insert (duplicate from SQS)    → 0 rows affected, returns nil
//
// The "0 rows affected" case is NOT an error: the worker's contract is
// "eventually at least one row exists for this reservation_id," and we
// just verified (or created) that condition. Returning nil keeps the
// SQS message on track to be deleted, avoiding an infinite redelivery
// loop on duplicate publishes.
//
// This is the implementation-plan's "Idempotency" requirement: the same
// message published twice produces exactly one row — and exactly one
// CREATED audit row, because the audit write is inside the same
// transaction and is skipped when the insert was a no-op.
//
// The audit row records the reservation as CREATED with the user as the
// actor: the user asked for the ticket (POST /api/tickets/reserve); the
// worker is just the process that wrote the row. The trace_id on the
// audit row is the ORIGINAL /reserve request's trace, inherited through
// SQS trace propagation — so an ops person holding the user's trace_id
// can find the row.
func InsertIfAbsent(ctx context.Context, pool *Pool, r queue.Reservation) error {
	parsed, err := r.Parse()
	if err != nil {
		// Malformed timestamps → can't write a sensible row.
		// Return so the message stays visible and gets logged by the
		// consumer; an operator can drain bad messages manually.
		return fmt.Errorf("db: parse reservation timestamps: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: insert reservation begin: %w", err)
	}
	// Rollback after a successful Commit is a no-op (pgx returns
	// ErrTxClosed), so this guarantees the transaction is closed on
	// every path.
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `
		INSERT INTO reservations (reservation_id, user_id, event_id, status, expires_at, seats)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (reservation_id) DO NOTHING
	`
	tag, err := tx.Exec(ctx, q,
		parsed.ReservationID,
		parsed.UserID,
		parsed.EventID,
		StatusPendingPayment,
		parsed.ExpiresAtT,
		parsed.Seats,
	)
	if err != nil {
		return fmt.Errorf("db: insert reservation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Duplicate. Nothing to do — the row already exists from an
		// earlier successful delivery. We do NOT want to UPDATE
		// updated_at, which is why this is ON CONFLICT DO NOTHING
		// rather than DO UPDATE.
		// No audit row either: CREATED is recorded exactly once per
		// reservation, by the insert that actually created the row.
		// (Quiet log path; uncomment to debug duplicate publishes.)
		// log.Printf("worker: reservation %s already exists, skipping", parsed.ReservationID)
		return nil
	}

	entry := AuditEntry{
		EventType: AuditEventCreated,
		Actor:     UserActor(parsed.UserID),
		NewStatus: StatusPendingPayment,
		Metadata: map[string]any{
			"seats":    parsed.Seats,
			"event_id": parsed.EventID,
		},
	}
	if err := WriteAuditEvent(ctx, tx, parsed.ReservationID, parsed.UserID, entry); err != nil {
		return fmt.Errorf("db: insert reservation audit: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: insert reservation commit: %w", err)
	}
	return nil
}

// ReservationRow has moved to internal/db/types.go (where API + SQL
// helpers can both depend on it without an import cycle). It's still
// the same struct; existing references via `db.ReservationRow` keep
// working — Go's package-level visibility doesn't care which file in
// the package declares the type.

// GetReservation is a small helper used by the smoke test / Phase 8
// verify.sh to read back a row by ID. Returns pgx.ErrNoRows when
// the row doesn't exist. JOINs events to populate EventName so API
// callers can render a human-readable title without a second round-trip.
func GetReservation(ctx context.Context, pool *Pool, reservationID string) (ReservationRow, error) {
	const q = `
		SELECT r.reservation_id, r.user_id, r.event_id, e.name AS event_name,
		       r.status, r.seats, r.expires_at, r.created_at, r.updated_at
		FROM reservations r
		LEFT JOIN events e ON e.id = r.event_id
		WHERE r.reservation_id = $1
	`
	var row ReservationRow
	err := pool.QueryRow(ctx, q, reservationID).Scan(
		&row.ReservationID,
		&row.UserID,
		&row.EventID,
		&row.EventName,
		&row.Status,
		&row.Seats,
		&row.ExpiresAt,
		&row.CreatedAt,
		&row.UpdatedAt,
	)
	if err != nil {
		return ReservationRow{}, err
	}
	return row, nil
}

// IsNoRows is a small wrapper around pgx.ErrNoRows for callers that
// want errors.Is without importing pgx directly. Useful in the worker's
// reconciliation code (Phase 7) and tests.
func IsNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}