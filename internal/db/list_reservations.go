// Package db — list_reservations.go: "show me my reservations" SQL helper.
//
// Powers GET /api/me/reservations (Layer 1 item 2 of
// docs/product-roadmap.md). The query is parameterised:
//
//   userID       the JWT subject — required, no default
//   statusFilter empty → all four statuses returned (terminal + active);
//                non-empty → only that status. Allows callers to
//                page through 'just my PENDING_PAYMENT' or 'just my
//                expired' without post-filtering in Go.
//   limit        caller-supplied; clamped to [1, 100]. 0 → default 20.
//   offset       caller-supplied; >= 0. 0 → first page.
//
// The query:
//
//   SELECT …
//   FROM reservations r
//   LEFT JOIN events e ON e.id = r.event_id
//   WHERE r.user_id = $1
//     [AND r.status = $2]
//   ORDER BY r.created_at DESC
//   LIMIT $3 OFFSET $4
//
// Returns:
//   - ([]ReservationRow, nil) on success — possibly empty if the user
//     has no reservations yet (do NOT treat as an error)
//   - ([]ReservationRow{}, error) on DB errors etc.
package db

import (
	"context"
	"fmt"
)

// Default list-page size when the caller asks for 0.
// Picked from product-roadmap.md's documented example URL.
const defaultListLimit = 20

// hardMaxLimit is the upper bound even if the caller asks for more.
// Defence against 'give me a million' DoS via /me/reservations.
const hardMaxLimit = 100

// ListReservationsByUser fetches a page of a user's reservations,
// newest first. See package-level comment for the query plan.
//
// Inputs:
//   userID       required
//   statusFilter "" → no filter; non-empty → exact match on r.status
//   limit        if <=0 → defaultListLimit; if >hardMaxLimit → hardMaxLimit
//   offset       if <0 → 0
//
// Output: slice of ReservationRow (possibly empty) + error
func ListReservationsByUser(ctx context.Context, pool *Pool,
	userID, statusFilter string, limit, offset int,
) ([]ReservationRow, error) {
	// Normalise inputs at the boundary so the SQL builder below stays simple.
	if userID == "" {
		return nil, fmt.Errorf("db: ListReservationsByUser requires userID")
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > hardMaxLimit {
		limit = hardMaxLimit
	}
	if offset < 0 {
		offset = 0
	}

	// Two near-identical queries: with-status-filter vs without. We could
	// build it dynamically (e.g. via squirrel), but for two query shapes
	// the readability of two const strings wins. The planner will pick the
	// same plan for both once we add the (user_id, created_at DESC)
	// index — see migrations/004_user_lookup_index.sql.
	if statusFilter != "" {
		return listReservationsByUserAndStatus(ctx, pool, userID, statusFilter, limit, offset)
	}
	return listReservationsByUser(ctx, pool, userID, limit, offset)
}

func listReservationsByUser(ctx context.Context, pool *Pool,
	userID string, limit, offset int,
) ([]ReservationRow, error) {
	const q = `
		SELECT r.reservation_id, r.user_id, r.event_id, e.name AS event_name,
		       r.status, r.seats, r.expires_at, r.created_at, r.updated_at
		FROM reservations r
		LEFT JOIN events e ON e.id = r.event_id
		WHERE r.user_id = $1
		ORDER BY r.created_at DESC
		LIMIT $2 OFFSET $3
	`
	return scanReservations(ctx, pool, q, userID, limit, offset)
}

func listReservationsByUserAndStatus(ctx context.Context, pool *Pool,
	userID, statusFilter string, limit, offset int,
) ([]ReservationRow, error) {
	const q = `
		SELECT r.reservation_id, r.user_id, r.event_id, e.name AS event_name,
		       r.status, r.seats, r.expires_at, r.created_at, r.updated_at
		FROM reservations r
		LEFT JOIN events e ON e.id = r.event_id
		WHERE r.user_id = $1 AND r.status = $2
		ORDER BY r.created_at DESC
		LIMIT $3 OFFSET $4
	`
	return scanReservations(ctx, pool, q, userID, statusFilter, limit, offset)
}

// scanReservations centralises the row → struct mapping so the two
// query helpers stay tiny. Returns ([]ReservationRow{}, nil) when the
// user has no matching rows — NOT an error.
func scanReservations(ctx context.Context, pool *Pool, q string, args ...any) ([]ReservationRow, error) {
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list reservations: %w", err)
	}
	defer rows.Close()

	// Start from a non-nil empty slice: the documented contract is
	// "([]ReservationRow{}, nil) — NOT nil", because a nil slice
	// serialises to JSON `null` and a client has to handle two shapes
	// for "no rows". `var out []ReservationRow` would be nil here.
	out := []ReservationRow{}
	for rows.Next() {
		var r ReservationRow
		if scanErr := rows.Scan(
			&r.ReservationID,
			&r.UserID,
			&r.EventID,
			&r.EventName,
			&r.Status,
			&r.Seats,
			&r.ExpiresAt,
			&r.CreatedAt,
			&r.UpdatedAt,
		); scanErr != nil {
			return nil, fmt.Errorf("db: scan reservation row: %w", scanErr)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate reservation rows: %w", err)
	}
	return out, nil
}
