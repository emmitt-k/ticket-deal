// Package db — transition.go: the single gateway for all reservation-status
// transitions.
//
// Why this file exists:
//
// Every status change in the system flows through TransitionStatus. Two
// reasons centralisation matters:
//
//  1. Correctness. The actual write is a compare-and-set UPDATE:
//       UPDATE reservations
//       SET status = $to, updated_at = NOW()
//       WHERE reservation_id = $id AND status = $from
//     So two callers (a user's cancel endpoint racing the watcher's
//     expiry sweep, or a future Stripe webhook racing a manual cancel)
//     cannot both flip the same row. The first one wins; the second
//     observes 0 rows affected and gets (false, nil) — "someone else
//     already handled it, move on."
//
//  2. Evolvability. The validTransitions table encodes the entire state
//     machine as data. A future caller that asks for an impossible
//     transition (e.g. EXPIRED → CONFIRMED) fails fast at runtime with
//     ErrInvalidTransition — instead of silently writing a row that the
//     SQL CHECK rejects 100 ms later or, worse, the SQL CHECK allows but
//     the downstream invariant was violated.
//
//  3. Auditability. Since Layer 2 item 6, every transition also writes a
//     row to the append-only reservation_events table — in the SAME
//     transaction as the status change. The actor is a required
//     parameter, so "a transition happened but nobody recorded it" is
//     not a state the code can reach: if the audit row can't be
//     written, the status change is rolled back with it.
//
// Adding a new transition = edit validTransitions + add a unit test case.
// The SQL CHECK constraint in migrations/001_init.sql still guards the
// *values* (typo defence); this helper guards the *edges* of the
// directed graph.
package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrInvalidTransition is the sentinel returned when TransitionStatus is
// asked to perform a transition not present in validTransitions. Detect
// via errors.Is — the wrapped error carries from→to for diagnostics.
//
// Note: a *successful* CAS-miss (the row existed but was already in a
// different status, e.g. someone else cancelled first) returns (false,
// nil) — not ErrInvalidTransition. ErrInvalidTransition is for programmer
// errors only (caller passed a from→to pair the state machine forbids).
var ErrInvalidTransition = errors.New("db: invalid reservation status transition")

// validTransitions is the directed graph of allowed status changes.
//
// Edges today:
//
//   PENDING_PAYMENT → CONFIRMED   (Stripe webhook later — Layer 2, item 7)
//   PENDING_PAYMENT → EXPIRED     (keyspace notification OR 60 s sweep)
//   PENDING_PAYMENT → CANCELLED   (user cancel endpoint — Layer 1, item 3)
//   CONFIRMED      → CANCELLED   (admin "refund-as-cancel" — Layer 2, item 9)
//
// Terminal states (EXPIRED, CANCELLED) have no outgoing edges; once a
// row lands in a terminal state only the audit log records further
// activity.
//
// The keys of this map are the only legal `from` values; the values are
// the legal `to` values reachable from that `from`. To add a new edge,
// add a Status constant in reservations.go + a line here + a unit test
// case in transition_test.go.
var validTransitions = map[string][]string{
	StatusPendingPayment: {StatusConfirmed, StatusExpired, StatusCancelled},
	StatusConfirmed:      {StatusCancelled},
	// StatusExpired, StatusCancelled: terminal — empty slice (and absent
	// from the map keys to make the "no outgoing edges" rule obvious).
}

// isValidTransition reports whether the (from → to) edge is present in
// the state machine. Pure function — no I/O — so it's cheap to table-test
// without a DB.
func isValidTransition(from, to string) bool {
	allowed, ok := validTransitions[from]
	if !ok {
		return false
	}
	for _, candidate := range allowed {
		if candidate == to {
			return true
		}
	}
	return false
}

// TransitionStatus attempts a CAS transition on the reservation row
// identified by `id`, moving it from `from` to `to`, and appends an
// audit row describing it. Returns:
//
//   - (true,  nil)             CAS succeeded — row was in `from`, now in `to`,
//                              and the audit row was written.
//   - (false, nil)             CAS missed — row exists but was already not
//                              in `from` state, OR the row doesn't exist.
//                              This is NOT an error: the row is in some
//                              valid status, just not the expected one.
//                              Callers can read the row to learn its actual
//                              status if they care. Same return convention
//                              as FindAndExpireSweptRows in package expire.
//                              No audit row is written — nothing changed.
//   - (false, ErrInvalidTransition)
//                              Caller asked for a transition that is not
//                              in validTransitions. Programmer error.
//                              Should never happen via production code paths.
//   - (false, ErrInvalidAudit) Caller passed an Audit with no actor type.
//                              Programmer error — every transition must
//                              record who caused it.
//   - (false, other)           DB error etc.
//
// `from` and `to` are Status constants (StatusPendingPayment etc.) but
// the signature is `string` because that's how the column is typed;
// using constants at call sites gives compile-time typo protection.
//
// `audit` says who caused the transition and carries any extra context
// to record. Its Actor is mandatory.
//
// Concurrency: safe. The CAS is in a single UPDATE statement; Postgres
// row-level locks take care of the rest.
//
// Atomicity: the status change and the audit row are written in one
// transaction. There is no window where the status changed but the
// audit row is missing, and no window where the audit row exists but
// the status didn't change.
//
// Idempotency: no. Calling with the same (id, from, to) twice creates
// two updated_at timestamps; the second call sees the row already in
// `to` (not in `from`) and returns (false, nil). If you need
// idempotency at the "exactly once" level, wrap in a custom check.
func TransitionStatus(ctx context.Context, pool *Pool, id, from, to string, audit Audit) (bool, error) {
	if !isValidTransition(from, to) {
		return false, fmt.Errorf("%w: %q -> %q is not in validTransitions", ErrInvalidTransition, from, to)
	}
	if audit.Actor.Type == "" {
		return false, fmt.Errorf("%w: transition %q -> %q has no actor type", ErrInvalidAudit, from, to)
	}

	eventType, err := auditEventTypeForEdge(from, to)
	if err != nil {
		return false, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("db: transition begin: %w", err)
	}
	// Rollback after a successful Commit is a no-op (pgx returns
	// ErrTxClosed), so this guarantees the transaction is closed on
	// every path — including the early returns below.
	defer func() { _ = tx.Rollback(ctx) }()

	const updateQ = `
		UPDATE reservations
		SET status = $1, updated_at = NOW()
		WHERE reservation_id = $2 AND status = $3
		RETURNING user_id, seats
	`
	var userID string
	var seats int
	if err := tx.QueryRow(ctx, updateQ, to, id, from).Scan(&userID, &seats); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// CAS miss — the row is in some other status, or doesn't
			// exist. Nothing changed, so there is nothing to audit.
			return false, nil
		}
		return false, fmt.Errorf("db: transition update: %w", err)
	}

	// Merge the transition's own facts into the caller's metadata. The
	// seat count is worth recording: it tells ops how much inventory
	// the transition put back on sale.
	metadata := make(map[string]any, len(audit.Metadata)+1)
	for k, v := range audit.Metadata {
		metadata[k] = v
	}
	metadata["seats"] = seats

	entry := AuditEntry{
		EventType: eventType,
		Actor:     audit.Actor,
		OldStatus: from,
		NewStatus: to,
		Metadata:  metadata,
	}
	if err := WriteAuditEvent(ctx, tx, id, userID, entry); err != nil {
		return false, fmt.Errorf("db: transition audit: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("db: transition commit: %w", err)
	}
	return true, nil
}
