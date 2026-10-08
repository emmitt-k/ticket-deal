// Package queue is the seam between the API handler (which produces
// reservation events) and the worker (which consumes them).
//
// The Phase 5 handler publishes a JSON-encoded Reservation payload
// to a reservation-API queue. The Phase 6 worker reads the same
// payload, writes a Postgres row, and deletes the message.
//
// Defining the wire shape here (not in either caller) means the
// contract is enforced at compile time: any drift between producer
// and consumer fails to build, instead of being caught at runtime.
package queue

import "time"

// Reservation is the JSON payload published by the API and consumed
// by the worker. Both sides MUST round-trip through this struct so
// field renames break the build, not production.
//
// JSON field tags are the public contract. Timestamps use RFC 3339
// (with timezone offset) because that's what `time.RFC3339` formats
// and `time.Parse(time.RFC3339, ...)` parses. UTC is preferred but
// not required — the parser accepts any valid RFC 3339 timestamp.
type Reservation struct {
	// ReservationID is the UUID minted by the API handler. Stable
	// across retries — the Postgres ON CONFLICT clause relies on
	// it being identical for the same logical reservation.
	ReservationID string `json:"reservation_id"`

	// UserID is the JWT subject (verified by middleware).
	UserID string `json:"user_id"`

	// EventID is the JWT event_id claim (also verified).
	EventID int64 `json:"event_id"`

	// Seats is how many seats this reservation holds (default 1,
	// max 10 — see api.ReserveHandler).
	Seats int `json:"seats"`

	// CreatedAt is when the API handler first built this payload.
	CreatedAt string `json:"created_at"`

	// ExpiresAt is when the Redis hold key will be TTL'd (now +
	// RESERVE_HOLD_TTL_SECONDS, default 10 minutes).
	ExpiresAt string `json:"expires_at"`
}

// ParsedTimes is the same payload after CreatedAt and ExpiresAt are
// parsed into time.Time values. Workers use this to compute expiry
// logic without re-parsing the strings on every access.
//
// Zero value means the input string was empty or unparseable; the
// caller should treat that as a malformed message and skip/delete it.
type ParsedTimes struct {
	Reservation
	CreatedAtT time.Time
	ExpiresAtT time.Time
}

// Parse converts the raw wire payload into one with time.Time fields.
// Returns the zero ParsedTimes and an error if either timestamp is
// unparseable — callers should treat this as a malformed message.
func (r Reservation) Parse() (ParsedTimes, error) {
	createdAt, err := time.Parse(time.RFC3339, r.CreatedAt)
	if err != nil {
		return ParsedTimes{}, err
	}
	expiresAt, err := time.Parse(time.RFC3339, r.ExpiresAt)
	if err != nil {
		return ParsedTimes{}, err
	}
	return ParsedTimes{
		Reservation: r,
		CreatedAtT:  createdAt,
		ExpiresAtT:  expiresAt,
	}, nil
}