package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ── Payment intents ───────────────────────────────────────────────────
//
// The payment record for a reservation. One row per reservation
// (the reservation_id UNIQUE constraint enforces it), created before
// Stripe is called so the intent to pay is durable first.
//
// This file is the DB side of Layer 2, item 7 (Stripe). The Stripe
// client lives in internal/stripe; the state machine does not know
// or care that Stripe exists — it only sees a payment row and a
// webhook event.

// PaymentIntentStatus values. They mirror Stripe's own PaymentIntent
// statuses, uppercased, plus one state of our own.
//
// 'CREATED' is ours: the row exists but Stripe's Create has not
// returned yet, so we do not have a Stripe id yet. The reservation's
// own status remains the business truth — this column is the payment
// provider's view of the same thing.
const (
	PaymentStatusCreated               = "CREATED"
	PaymentStatusRequiresPaymentMethod = "REQUIRES_PAYMENT_METHOD"
	PaymentStatusRequiresConfirmation  = "REQUIRES_CONFIRMATION"
	PaymentStatusRequiresAction        = "REQUIRES_ACTION"
	PaymentStatusRequiresCapture       = "REQUIRES_CAPTURE"
	PaymentStatusProcessing            = "PROCESSING"
	PaymentStatusSucceeded             = "SUCCEEDED"
	PaymentStatusCanceled              = "CANCELED"
)

// ErrPaymentIntentNotFound is returned when a payment row cannot be
// found for the given reservation or Stripe id.
var ErrPaymentIntentNotFound = errors.New("db: payment intent not found")

// PaymentIntentRow is the read model of a payment_intents row.
//
// StripePaymentIntentID is a *string rather than a string because it
// is genuinely NULL before Stripe's Create returns — and "empty
// string" would be a lie that a caller could mistake for a real id.
type PaymentIntentRow struct {
	ID                    int64
	ReservationID         string
	UserID                string
	StripePaymentIntentID *string
	AmountCents           int64
	Currency              string
	Status                string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// InsertPaymentIntent writes the payment row for a reservation.
//
// The INSERT is ON CONFLICT DO NOTHING on reservation_id: a second
// call for the same reservation is a no-op, not an error. That is
// what makes POST /pay safely replayable — two concurrent calls
// cannot create two payment rows, and therefore cannot create two
// PaymentIntents.
//
// Returns whether a row was actually created. false means the
// reservation already had a payment row (a replay).
func InsertPaymentIntent(ctx context.Context, pool *Pool, reservationID, userID string, amountCents int64, currency string) (bool, error) {
	const q = `
		INSERT INTO payment_intents (reservation_id, user_id, amount_cents, currency)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (reservation_id) DO NOTHING
	`
	tag, err := pool.Exec(ctx, q, reservationID, userID, amountCents, currency)
	if err != nil {
		return false, fmt.Errorf("db: insert payment intent: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetStripePaymentIntentID attaches Stripe's PaymentIntent id to the
// row, and records the status Stripe reported.
//
// This is the second half of the two-step write: the row is created
// first (durable intent), then Stripe is called, then the id comes
// back and is written here. If the process dies in between, the row
// exists with a NULL id — and the retry converges, because /pay
// passes the reservation_id as Stripe's idempotency key.
func SetStripePaymentIntentID(ctx context.Context, pool *Pool, reservationID, stripeID, status string) error {
	const q = `
		UPDATE payment_intents
		SET stripe_payment_intent_id = $2,
		    status = $3,
		    updated_at = NOW()
		WHERE reservation_id = $1
	`
	tag, err := pool.Exec(ctx, q, reservationID, stripeID, status)
	if err != nil {
		return fmt.Errorf("db: set stripe payment intent id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrPaymentIntentNotFound
	}
	return nil
}

// UpdatePaymentIntentStatus records a status change reported by
// Stripe (the webhook's view of the payment).
//
// Returns ErrPaymentIntentNotFound if the row does not exist — which
// for the webhook means "a payment we have never heard of", worth
// alerting on but not worth failing the delivery over.
func UpdatePaymentIntentStatus(ctx context.Context, pool *Pool, stripeID, status string) error {
	const q = `
		UPDATE payment_intents
		SET status = $2, updated_at = NOW()
		WHERE stripe_payment_intent_id = $1
	`
	tag, err := pool.Exec(ctx, q, stripeID, status)
	if err != nil {
		return fmt.Errorf("db: update payment intent status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrPaymentIntentNotFound
	}
	return nil
}

// GetPaymentIntentByReservation reads the payment row for a
// reservation. Returns pgx.ErrNoRows when there is none (use
// IsNoRows to check).
func GetPaymentIntentByReservation(ctx context.Context, pool *Pool, reservationID string) (PaymentIntentRow, error) {
	const q = `
		SELECT id, reservation_id, user_id, stripe_payment_intent_id,
		       amount_cents, currency, status, created_at, updated_at
		FROM payment_intents
		WHERE reservation_id = $1
	`
	return scanPaymentIntent(pool.QueryRow(ctx, q, reservationID))
}

// GetPaymentIntentByStripeID reads the payment row by Stripe's
// PaymentIntent id. This is the webhook's lookup path: an event
// carries a Stripe id, and the reservation is what we actually need.
//
// Returns pgx.ErrNoRows when there is none.
func GetPaymentIntentByStripeID(ctx context.Context, pool *Pool, stripeID string) (PaymentIntentRow, error) {
	const q = `
		SELECT id, reservation_id, user_id, stripe_payment_intent_id,
		       amount_cents, currency, status, created_at, updated_at
		FROM payment_intents
		WHERE stripe_payment_intent_id = $1
	`
	return scanPaymentIntent(pool.QueryRow(ctx, q, stripeID))
}

// scanPaymentIntent maps one row to the struct. Shared by the two
// lookups so the column list and the scan order cannot drift.
func scanPaymentIntent(row pgx.Row) (PaymentIntentRow, error) {
	var r PaymentIntentRow
	err := row.Scan(
		&r.ID,
		&r.ReservationID,
		&r.UserID,
		&r.StripePaymentIntentID,
		&r.AmountCents,
		&r.Currency,
		&r.Status,
		&r.CreatedAt,
		&r.UpdatedAt,
	)
	if err != nil {
		return PaymentIntentRow{}, err
	}
	return r, nil
}
