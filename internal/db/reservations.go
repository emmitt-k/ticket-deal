package db

import (
	"context"
	"errors"
	"fmt"
	"time"

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
// DO NOTHING. The semantics:
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
// message published twice produces exactly one row.
func InsertIfAbsent(ctx context.Context, pool *Pool, r queue.Reservation) error {
	parsed, err := r.Parse()
	if err != nil {
		// Malformed timestamps → can't write a sensible row.
		// Return so the message stays visible and gets logged by the
		// consumer; an operator can drain bad messages manually.
		return fmt.Errorf("db: parse reservation timestamps: %w", err)
	}

	const q = `
		INSERT INTO reservations (reservation_id, user_id, event_id, status, expires_at, seats)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (reservation_id) DO NOTHING
	`
	tag, err := pool.Exec(ctx, q,
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
		// (Quiet log path; uncomment to debug duplicate publishes.)
		// log.Printf("worker: reservation %s already exists, skipping", parsed.ReservationID)
	}
	return nil
}

// ReservationRow mirrors the table layout for read-back helpers
// (e.g. tests that want to assert the row shape). Not used in
// production code yet — Phase 7's worker might use it.
type ReservationRow struct {
	ReservationID string
	UserID        string
	EventID       int64
	Status        string
	ExpiresAt     time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// GetReservation is a small helper used by the smoke test / Phase 8
// verify.sh to read back a row by ID. Returns pgx.ErrNoRows when
// the row doesn't exist.
func GetReservation(ctx context.Context, pool *Pool, reservationID string) (ReservationRow, error) {
	const q = `
		SELECT reservation_id, user_id, event_id, status, expires_at, created_at, updated_at
		FROM reservations
		WHERE reservation_id = $1
	`
	var row ReservationRow
	err := pool.QueryRow(ctx, q, reservationID).Scan(
		&row.ReservationID,
		&row.UserID,
		&row.EventID,
		&row.Status,
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