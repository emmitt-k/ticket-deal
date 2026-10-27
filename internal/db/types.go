// Package db — types.go: shared types used across the reservations API
// surface. HTTP handlers serialize these to JSON; SQL helpers Scan into
// them. Single source of truth for the row shape.
//
// Why here (not reservations.go): the API layer (internal/api) and the
// reservation DB helpers (internal/db/reservations.go) both want the
// same struct. Putting it in a small types.go file avoids an import
// cycle and prevents "which file did the struct come from again?"
// cognitive overhead. Both packages already depend on db; nothing else
// does.
package db

import "time"

// ReservationRow is the row shape returned by GetReservation and
// ListReservationsByUser. The JSON tags mirror the API contract (which
// is the same snake_case the worker uses for SQS bodies — see
// internal/queue/types.go) so an external client only learns one shape.
//
// Field-by-field:
//
//	reservation_id  UUID assigned at /reserve time (idempotency key)
//	user_id         token subject from JWT (sub claim)
//	event_id        the event this ticket is for
//	event_name      human-readable title (JOINed from events table; empty
//	                string if the row has been hard-deleted upstream and
//	                the FK was removed — shouldn't happen in normal flow,
//	                but defensive JSON shape)
//	status          one of StatusPendingPayment, StatusConfirmed,
//	                StatusExpired, StatusCancelled, StatusRefunded —
//	                matches the SQL CHECK constraint (migrations/
//	                001_init.sql, widened by 006_refunded_status.sql)
//	seats           how many seats this reservation holds (from the SQS
//	                message; 1 today, up to 10)
//	unit_price_cents  the price of ONE seat, snapshotted from the
//	                event when the seat was held. Frozen: a later price
//	                change never rewrites a held or paid reservation
//	currency        the 3-letter ISO code the price is in (also frozen)
//	total_price_cents  unit_price_cents * seats — the amount owed.
//	                Computed in SQL from the two frozen columns, so it
//	                is always exactly consistent with them
//	expires_at      when the hold auto-releases; meaningful only for
//	                PENDING_PAYMENT
//	created_at      set on first INSERT (worker writes this)
//	updated_at      bumped by every successful TransitionStatus call
type ReservationRow struct {
	ReservationID string `json:"reservation_id"`
	UserID        string `json:"user_id"`
	EventID       int64  `json:"event_id"`
	EventName     string `json:"event_name"`
	Status        string `json:"status"`
	Seats         int    `json:"seats"`
	// UnitPriceCents is the per-seat price frozen at hold time.
	UnitPriceCents int64 `json:"unit_price_cents"`
	// Currency is the ISO 4217 code the price is in (e.g. "usd").
	Currency string `json:"currency"`
	// TotalPriceCents is UnitPriceCents * Seats — the amount owed.
	TotalPriceCents int64     `json:"total_price_cents"`
	ExpiresAt       time.Time `json:"expires_at"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}
