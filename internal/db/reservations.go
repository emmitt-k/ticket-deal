package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/emmitt-k/ticket-deal/internal/queue"
)

// ReservationStatus values mirror the CHECK constraint in
// migrations/001_init.sql (widened by 006_refunded_status.sql).
// Defining them here (not as inline strings) means a typo at the
// call site fails to compile.
//
// The five values, and what each one MEANS:
//
//	PENDING_PAYMENT  a hold exists; the seat is spoken for, no money yet
//	CONFIRMED        the payment succeeded; the seat is the customer's
//	EXPIRED          the hold timed out before payment; the seat went back
//	CANCELLED        cancelled BEFORE payment; the seat went back
//	REFUNDED         paid and then reversed; the money came back and the
//	                 seat went back
//
// CANCELLED and REFUNDED are deliberately separate. Both release the
// seat, but they are different facts: nobody paid vs the customer paid
// and we gave the money back. Before 006_refunded_status.sql a refund
// was stored as CANCELLED, which made "we lost the sale" and "we
// refunded the customer" the same query.
const (
	StatusPendingPayment = "PENDING_PAYMENT"
	StatusConfirmed      = "CONFIRMED"
	StatusExpired        = "EXPIRED"
	StatusCancelled      = "CANCELLED"
	StatusRefunded       = "REFUNDED"
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

	// The price is a SNAPSHOT, taken in the same statement that
	// creates the row: two scalar subqueries read the event's price
	// and currency at insert time. A later price change (a different
	// event, an admin edit) must never retroactively alter a seat
	// that is already held or already paid — so the reservation row
	// carries its own frozen copy rather than a live lookup at pay
	// time.
	//
	// COALESCE is deliberate: if the event row does not exist the
	// subqueries yield NULL, and COALESCE turns that into a valid
	// price so the INSERT proceeds and fails on the event_id FOREIGN
	// KEY instead — which reports "event 999 does not exist" rather
	// than "null value in column unit_price_cents". The COALESCE
	// values are never stored, because the row never lands.
	const q = `
		INSERT INTO reservations (reservation_id, user_id, event_id, status, expires_at, seats, unit_price_cents, currency)
		VALUES ($1, $2, $3, $4, $5, $6,
			COALESCE((SELECT price_cents FROM events WHERE id = $3), 0),
			COALESCE((SELECT currency    FROM events WHERE id = $3), 'usd'))
		ON CONFLICT (reservation_id) DO NOTHING
		RETURNING unit_price_cents, currency
	`
	// RETURNING (instead of Exec) gives us the exact price that was
	// written, so the audit row records the price the seat was
	// actually sold at — not a re-read that might disagree.
	var unitPriceCents int64
	var currency string
	err = tx.QueryRow(ctx, q,
		parsed.ReservationID,
		parsed.UserID,
		parsed.EventID,
		StatusPendingPayment,
		parsed.ExpiresAtT,
		parsed.Seats,
	).Scan(&unitPriceCents, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
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
	if err != nil {
		return fmt.Errorf("db: insert reservation: %w", err)
	}

	entry := AuditEntry{
		EventType: AuditEventCreated,
		Actor:     UserActor(parsed.UserID),
		NewStatus: StatusPendingPayment,
		Metadata: map[string]any{
			"seats":            parsed.Seats,
			"event_id":         parsed.EventID,
			"unit_price_cents": unitPriceCents,
			"currency":         currency,
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
		       r.status, r.seats, r.expires_at, r.created_at, r.updated_at,
		       r.unit_price_cents, r.currency,
		       r.unit_price_cents * r.seats AS total_price_cents
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
		&row.UnitPriceCents,
		&row.Currency,
		&row.TotalPriceCents,
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
