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
//	                StatusExpired, StatusCancelled — matches the SQL
//	                CHECK constraint in migrations/001_init.sql
//	seats           how many seats this reservation holds (from the SQS
//	                message; 1 today, up to 10)
//	expires_at      when the hold auto-releases; meaningful only for
//	                PENDING_PAYMENT
//	created_at      set on first INSERT (worker writes this)
//	updated_at      bumped by every successful TransitionStatus call
type ReservationRow struct {
	ReservationID string    `json:"reservation_id"`
	UserID        string    `json:"user_id"`
	EventID       int64     `json:"event_id"`
	EventName     string    `json:"event_name"`
	Status        string    `json:"status"`
	Seats         int       `json:"seats"`
	ExpiresAt     time.Time `json:"expires_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}
