// Package db — audit.go: the append-only reservation event log.
//
// Layer 2, item 6 of docs/product-roadmap.md.
//
// Why this file exists:
//
// The reservations table holds the CURRENT status of a reservation. That
// is enough to answer "can this user cancel?" but not "who did what,
// when, and can I prove it?" — the moment a second transition overwrites
// the status, the history is gone. A support ticket that says "I paid and
// it still says PENDING_PAYMENT" is unanswerable without it.
//
// So every transition writes a row here, in the same transaction as the
// status change. The rules:
//
//   - The row is written by the SAME transition that changed the status.
//     If the status change fails, no audit row exists. If the audit row
//     can't be written, the status change is rolled back with it. An
//     audit log that can silently miss rows isn't an audit log.
//
//   - The row carries the trace_id of the request that caused it, so the
//     table joins into the Grafana/Loki/Jaeger stack in both directions:
//     trace_id → rows, and row → trace_id.
//
//   - The row says WHO did it (actor_type + actor_id), not just what
//     happened. "EXPIRED" is a fact; "EXPIRED, by the expiration-watcher"
//     is an audit trail.
//
// The table itself — including the append-only trigger that seals it —
// lives in migrations/005_audit_log.sql.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/trace"
)

// ErrInvalidAudit is the sentinel returned when a transition is asked
// to record an audit row without a usable actor. Detect via
// errors.Is.
//
// This is a programmer error, in the same family as
// ErrInvalidTransition: every transition in this system must record
// who caused it, so a caller that forgets the actor has a bug. It is
// checked before any SQL runs.
var ErrInvalidAudit = errors.New("db: invalid audit actor")

// ── Audit event types ───────────────────────────────────────────────────
//
// These mirror the CHECK constraint on reservation_events.event_type in
// migrations/005_audit_log.sql. Each one names a *fact about the
// reservation*, not a status — and the difference matters: an EVENT
// records what was DONE, a STATUS records where the reservation ended
// up. REFUND_INITIATED is an event (money moved back) whose outcome is
// the REFUNDED status; a partial refund records the event and leaves
// the status at CONFIRMED.
const (
	// AuditEventCreated — the reservation row was first written. Emitted
	// by the worker, on the user's behalf (the user asked for it via
	// POST /api/tickets/reserve; the trace_id points back at that
	// request).
	AuditEventCreated = "CREATED"

	// AuditEventConfirmed — the reservation was paid for. Reached by the
	// Stripe webhook (Layer 2, item 7).
	AuditEventConfirmed = "CONFIRMED"

	// AuditEventExpired — the hold timed out and the seat went back on
	// sale. Emitted by the expiration-watcher (keyspace notification)
	// or the 60-second sweep.
	AuditEventExpired = "EXPIRED"

	// AuditEventCancelled — the user cancelled their own reservation.
	AuditEventCancelled = "CANCELLED"

	// AuditEventRefundInitiated — a confirmed reservation was refunded.
	// The event records the ACTION (money moved back); the status it
	// moves to, REFUNDED, records the OUTCOME. A partial refund is
	// recorded here too — it leaves the status at CONFIRMED, because a
	// partial refund is an event, not a state.
	AuditEventRefundInitiated = "REFUND_INITIATED"
)

// ── Actor types ────────────────────────────────────────────────────────
//
// These mirror the CHECK constraint on reservation_events.actor_type.
// "Who caused this?" has exactly four answers in this system.
const (
	// ActorTypeUser — a human acting through an authenticated endpoint.
	ActorTypeUser = "user"

	// ActorTypeSystem — a background process. actor_id says which one
	// ('expiration-watcher', 'expiration-sweep', ...).
	ActorTypeSystem = "system"

	// ActorTypeAdmin — a human acting through the ops API (Layer 2,
	// item 9), authenticated with the HMAC admin key.
	ActorTypeAdmin = "admin"

	// ActorTypeStripeWebhook — the Stripe webhook receiver (Layer 2,
	// item 7). actor_id is the Stripe event id (evt_...).
	ActorTypeStripeWebhook = "stripe_webhook"
)

// AuditActor identifies WHO caused a transition.
//
// Type is one of the ActorType* constants. ID is the actor's identity
// within that type: the user id, the system component name, the admin
// key fingerprint, or the Stripe event id. ID may be empty (an actor
// type with no finer-grained identity), in which case the column is
// stored as NULL.
type AuditActor struct {
	Type string
	ID   string
}

// UserActor returns the actor for a human acting through an
// authenticated endpoint.
func UserActor(userID string) AuditActor {
	return AuditActor{Type: ActorTypeUser, ID: userID}
}

// SystemActor returns the actor for a background process.
func SystemActor(systemID string) AuditActor {
	return AuditActor{Type: ActorTypeSystem, ID: systemID}
}

// AdminActor returns the actor for a human acting through the ops API.
func AdminActor(adminID string) AuditActor {
	return AuditActor{Type: ActorTypeAdmin, ID: adminID}
}

// StripeWebhookActor returns the actor for a Stripe webhook delivery.
// eventID is the Stripe event id (evt_...).
func StripeWebhookActor(eventID string) AuditActor {
	return AuditActor{Type: ActorTypeStripeWebhook, ID: eventID}
}

// Audit describes the WHO and the WHAT of a transition, as handed to
// TransitionStatus.
//
// Actor is mandatory — TransitionStatus rejects an empty actor type,
// so "every transition is audited" is a property of the type system
// rather than a habit.
//
// Metadata is optional, event-specific context: seat counts, Stripe
// charge ids, admin key fingerprints. The transition merges its own
// fields (e.g. the seat count) into it.
type Audit struct {
	Actor    AuditActor
	Metadata map[string]any
}

// AuditEntry is one row of the audit log — the "what" of a transition,
// before it is bound to a reservation.
type AuditEntry struct {
	// EventType is one of the AuditEvent* constants.
	EventType string

	// Actor is who caused it.
	Actor AuditActor

	// OldStatus is the status before the transition. Empty means NULL —
	// which is the correct value for CREATED (there was no previous
	// status).
	OldStatus string

	// NewStatus is the status after the transition. Empty means NULL.
	NewStatus string

	// Metadata is optional event-specific context.
	Metadata map[string]any
}

// AuditEventRow is the read model of an audit row — what
// ListAuditEvents returns.
type AuditEventRow struct {
	EventID       int64          `json:"event_id"`
	ReservationID string         `json:"reservation_id"`
	UserID        string         `json:"user_id"`
	EventType     string         `json:"event_type"`
	ActorType     string         `json:"actor_type"`
	ActorID       string         `json:"actor_id"`
	OldStatus     string         `json:"old_status"`
	NewStatus     string         `json:"new_status"`
	TraceID       string         `json:"trace_id"`
	Metadata      map[string]any `json:"metadata"`
	CreatedAt     time.Time      `json:"created_at"`
}

// Querier is the slice of pgx that both *pgxpool.Pool and pgx.Tx
// satisfy. It exists so the audit writer can run inside a transaction
// (the transition's own tx) or standalone (a one-off write).
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// auditInsertColumns is the canonical column list of
// reservation_events. Shared by every INSERT so the two call shapes
// (VALUES and SELECT-from-CTE) can never drift apart.
const auditInsertColumns = `reservation_id, user_id, event_type, actor_type, actor_id, old_status, new_status, trace_id, metadata`

// auditEntryParams derives the ordered parameters for the seven
// entry-specific columns of the audit insert:
//
//	event_type, actor_type, actor_id, old_status, new_status, trace_id, metadata
//
// It is shared by every writer — the standalone WriteAuditEvent and the
// data-modifying CTEs in package expire — so the trace extraction and
// the metadata marshalling happen in exactly one place.
//
// The trace_id is read from ctx: the active OTel span's trace id, or
// NULL when there is no span (background sweeps, unit tests). Callers
// don't pass it because the ctx always carries the right one — the
// worker's span is a child of the original /reserve request via SQS
// trace propagation, so the audit row points at the user's request,
// not at the worker's.
func auditEntryParams(ctx context.Context, e AuditEntry) ([]any, error) {
	metadata := e.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("db: marshal audit metadata: %w", err)
	}

	var actorID, oldStatus, newStatus, traceID any
	if e.Actor.ID != "" {
		actorID = e.Actor.ID
	}
	if e.OldStatus != "" {
		oldStatus = e.OldStatus
	}
	if e.NewStatus != "" {
		newStatus = e.NewStatus
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		tid := sc.TraceID().String()
		traceID = tid
	}

	return []any{
		e.EventType,
		e.Actor.Type,
		actorID,
		oldStatus,
		newStatus,
		traceID,
		string(metadataJSON),
	}, nil
}

// WriteAuditEvent appends one row to the audit log.
//
// `q` is the executor: pass the transition's own *pgx.Tx to make the
// audit row atomic with the status change, or the *Pool for a
// standalone write.
//
// The caller owns the transaction boundary — this function does not
// begin or commit one. That keeps the "status change + audit row" pair
// in a single transaction when the caller wants them atomic, and lets
// the expire paths fold the audit write into their own statement.
func WriteAuditEvent(ctx context.Context, q Querier, reservationID, userID string, e AuditEntry) error {
	params, err := auditEntryParams(ctx, e)
	if err != nil {
		return err
	}

	const stmt = `INSERT INTO reservation_events (` + auditInsertColumns + `)
	              VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	args := make([]any, 0, 9)
	args = append(args, reservationID, userID)
	args = append(args, params...)

	if _, err := q.Exec(ctx, stmt, args...); err != nil {
		return fmt.Errorf("db: insert audit event: %w", err)
	}
	return nil
}

// AuditInsertSelect returns the SQL and the parameters for the
// data-modifying-CTE form of the audit insert.
//
// `source` names a CTE (or subquery) that produces three columns:
// reservation_id, user_id, and audit_metadata (a jsonb document). The
// audit row's reservation_id and user_id are copied from it, and the
// entry's metadata is MERGED with audit_metadata — the entry's keys win
// on conflict.
//
// `firstParam` is the $n index of the first parameter the returned
// statement uses: pass 1 when the audit insert is the only
// parameterised part of the query, or 3 when the query already spends
// $1/$2 on something else.
//
// Why this helper exists: package expire writes its audit row from
// INSIDE the same statement as the status change (a data-modifying
// CTE), so it can't use WriteAuditEvent, which is a standalone
// INSERT. Both paths share the column list, the trace extraction and
// the metadata marshalling through this helper, so they can't drift.
func AuditInsertSelect(ctx context.Context, source string, entry AuditEntry, firstParam int) (string, []any, error) {
	params, err := auditEntryParams(ctx, entry)
	if err != nil {
		return "", nil, err
	}
	ph := func(i int) string { return "$" + strconv.Itoa(firstParam+i) }

	stmt := `INSERT INTO reservation_events (` + auditInsertColumns + `)
	         SELECT s.reservation_id, s.user_id, ` +
		ph(0) + `, ` + ph(1) + `, ` + ph(2) + `, ` +
		ph(3) + `, ` + ph(4) + `, ` + ph(5) + `, ` +
		ph(6) + `::jsonb || COALESCE(s.audit_metadata, '{}'::jsonb)
	         FROM ` + source + ` s`

	return stmt, params, nil
}

// ListAuditEvents returns the audit trail of one reservation, oldest
// first (event_id is the insertion order, so this is the true
// chronological order of transitions).
//
// Used by the tests, and by the admin/ops API (Layer 2, item 9) when
// it lands.
func ListAuditEvents(ctx context.Context, pool *Pool, reservationID string) ([]AuditEventRow, error) {
	const stmt = `
		SELECT event_id, reservation_id, user_id, event_type, actor_type,
		       COALESCE(actor_id, ''), COALESCE(old_status, ''),
		       COALESCE(new_status, ''), COALESCE(trace_id, ''),
		       metadata, created_at
		FROM reservation_events
		WHERE reservation_id = $1
		ORDER BY event_id ASC
	`

	rows, err := pool.Query(ctx, stmt, reservationID)
	if err != nil {
		return nil, fmt.Errorf("db: list audit events: %w", err)
	}
	defer rows.Close()

	// Non-nil empty slice for the same reason as
	// ListReservationsByUser: "no events" must serialise as `[]`,
	// not `null`.
	events := []AuditEventRow{}
	for rows.Next() {
		var e AuditEventRow
		if scanErr := rows.Scan(
			&e.EventID,
			&e.ReservationID,
			&e.UserID,
			&e.EventType,
			&e.ActorType,
			&e.ActorID,
			&e.OldStatus,
			&e.NewStatus,
			&e.TraceID,
			&e.Metadata,
			&e.CreatedAt,
		); scanErr != nil {
			return nil, fmt.Errorf("db: scan audit event: %w", scanErr)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list audit events: %w", err)
	}
	return events, nil
}

// auditEventTypeForEdge maps a state-machine edge (from → to) to the
// audit event type that records it.
//
// The mapping is a function of the EDGE, not of the destination status,
// because two different edges can land on the same status and mean
// different things:
//
//	PENDING_PAYMENT → CANCELLED  is a USER CANCEL (nothing was paid)
//	PENDING_PAYMENT → CONFIRMED  is a PAYMENT (money came in)
//
// And because an EVENT and a STATUS are different concepts:
//
//	CONFIRMED → REFUNDED  is the REFUND_INITIATED event — the ACTION
//	(the money went back), while the status REFUNDED is the OUTCOME
//	(the reservation is now refunded). Keeping them separate is what
//	lets a PARTIAL refund be recorded as an event while the status
//	stays CONFIRMED — only a full refund becomes a state.
//
// The edge table (validTransitions) is the single source of truth for
// which transitions exist; this function is the single source of truth
// for how each one is recorded. Adding an edge means adding a row
// here too — the switch's default case turns a forgotten edge into a
// loud error instead of a silently-mislabeled audit row.
func auditEventTypeForEdge(from, to string) (string, error) {
	switch {
	case from == StatusConfirmed && to == StatusRefunded:
		return AuditEventRefundInitiated, nil
	case to == StatusConfirmed:
		return AuditEventConfirmed, nil
	case to == StatusExpired:
		return AuditEventExpired, nil
	case to == StatusCancelled:
		return AuditEventCancelled, nil
	default:
		return "", fmt.Errorf("db: no audit event type for edge %q -> %q", from, to)
	}
}
