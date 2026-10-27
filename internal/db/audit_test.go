package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ── Pure helpers ────────────────────────────────────────────────────────

// TestAuditEventTypeForEdge — the edge → event-type mapping is a
// pure function, so it gets an exhaustive table test.
//
// The interesting case is CONFIRMED → CANCELLED: that edge is a
// REFUND (money moved back), not a user cancel. Getting it wrong
// would make every refund look like a cancellation in the audit log.
func TestAuditEventTypeForEdge(t *testing.T) {
	cases := []struct {
		from, to string
		want     string
	}{
		{StatusPendingPayment, StatusConfirmed, AuditEventConfirmed},
		{StatusPendingPayment, StatusExpired, AuditEventExpired},
		{StatusPendingPayment, StatusCancelled, AuditEventCancelled},
		{StatusConfirmed, StatusCancelled, AuditEventRefundInitiated},
	}

	for _, tc := range cases {
		t.Run(tc.from+"_to_"+tc.to, func(t *testing.T) {
			got, err := auditEventTypeForEdge(tc.from, tc.to)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestAuditEventTypeForEdge_UnknownEdge — an edge with no mapping is
// a programmer error and must fail loudly rather than write a
// mislabeled audit row.
func TestAuditEventTypeForEdge_UnknownEdge(t *testing.T) {
	_, err := auditEventTypeForEdge(StatusPendingPayment, StatusPendingPayment)
	require.Error(t, err, "self-loop has no audit event type")
}

// ── The actor is mandatory ──────────────────────────────────────────────

// TestTransitionStatus_RequiresActor — a transition with no actor is
// refused BEFORE any SQL runs. The row must not change.
func TestTransitionStatus_RequiresActor(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(64001)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	changed, err := TransitionStatus(context.Background(), pool,
		r.ReservationID, StatusPendingPayment, StatusExpired,
		Audit{Actor: AuditActor{}}) // no type
	require.ErrorIs(t, err, ErrInvalidAudit)
	require.False(t, changed)

	// The row is untouched.
	row, err := GetReservation(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Equal(t, StatusPendingPayment, row.Status)
}

// ── CREATED ─────────────────────────────────────────────────────────────

// TestInsertIfAbsent_WritesCreatedAuditRow — the worker's insert
// records a CREATED row naming the user as the actor.
func TestInsertIfAbsent_WritesCreatedAuditRow(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(64002)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)

	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	events, err := ListAuditEvents(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Len(t, events, 1, "exactly one audit row for a fresh reservation")

	e := events[0]
	require.Equal(t, AuditEventCreated, e.EventType)
	require.Equal(t, ActorTypeUser, e.ActorType)
	require.Equal(t, r.UserID, e.ActorID)
	require.Equal(t, r.UserID, e.UserID)
	require.Empty(t, e.OldStatus, "CREATED has no previous status")
	require.Equal(t, StatusPendingPayment, e.NewStatus)
	require.Equal(t, float64(r.Seats), e.Metadata["seats"])
	require.Equal(t, float64(eventID), e.Metadata["event_id"])
}

// TestInsertIfAbsent_Duplicate_NoSecondAuditRow — the duplicate-SQS
// path must not double-log. CREATED is recorded exactly once, by the
// insert that actually created the row.
func TestInsertIfAbsent_Duplicate_NoSecondAuditRow(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(64003)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)

	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	events, err := ListAuditEvents(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Len(t, events, 1, "a duplicate insert must not log a second CREATED")
}

// ── Transitions ─────────────────────────────────────────────────────────

// TestTransitionStatus_WritesAuditRow — the happy path: the status
// change and the audit row land together, with the actor, the old and
// new status, and the seat count in the metadata.
func TestTransitionStatus_WritesAuditRow(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(64004)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	changed, err := TransitionStatus(context.Background(), pool,
		r.ReservationID, StatusPendingPayment, StatusCancelled,
		Audit{Actor: UserActor(r.UserID)})
	require.NoError(t, err)
	require.True(t, changed)

	events, err := ListAuditEvents(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Len(t, events, 2, "CREATED + CANCELLED")

	e := events[1]
	require.Equal(t, AuditEventCancelled, e.EventType)
	require.Equal(t, ActorTypeUser, e.ActorType)
	require.Equal(t, r.UserID, e.ActorID)
	require.Equal(t, StatusPendingPayment, e.OldStatus)
	require.Equal(t, StatusCancelled, e.NewStatus)
	require.Equal(t, float64(r.Seats), e.Metadata["seats"])
}

// TestTransitionStatus_CASMiss_NoAuditRow — a transition that didn't
// happen must not be logged. The audit log records FACTS, not
// attempts.
func TestTransitionStatus_CASMiss_NoAuditRow(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(64005)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	changed, err := TransitionStatus(context.Background(), pool,
		r.ReservationID, StatusPendingPayment, StatusCancelled,
		Audit{Actor: UserActor(r.UserID)})
	require.NoError(t, err)
	require.True(t, changed)

	// Second attempt — the row is already CANCELLED, so the CAS misses.
	changed, err = TransitionStatus(context.Background(), pool,
		r.ReservationID, StatusPendingPayment, StatusCancelled,
		Audit{Actor: UserActor(r.UserID)})
	require.NoError(t, err)
	require.False(t, changed)

	events, err := ListAuditEvents(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Len(t, events, 2, "a lost CAS race must not write an audit row")
}

// TestTransitionStatus_RollsBackWhenAuditFails — THE atomicity test.
//
// An actor type that violates the CHECK constraint makes the audit
// INSERT fail. The status change must be rolled back with it: either
// both land or neither does. An audit log that can record a
// transition that didn't happen (or miss one that did) is worse than
// no audit log at all.
func TestTransitionStatus_RollsBackWhenAuditFails(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(64006)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	changed, err := TransitionStatus(context.Background(), pool,
		r.ReservationID, StatusPendingPayment, StatusCancelled,
		Audit{Actor: AuditActor{Type: "not-a-real-actor-type", ID: "x"}})
	require.Error(t, err, "the audit insert must fail on the CHECK constraint")
	require.False(t, changed)

	// The status change was rolled back with the audit row.
	row, err := GetReservation(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Equal(t, StatusPendingPayment, row.Status,
		"a failed audit write must roll the status change back")

	events, err := ListAuditEvents(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Len(t, events, 1, "only the CREATED row — the cancelled transition never landed")
}

// TestTransitionStatus_MergesCallerMetadata — the caller's metadata
// survives alongside the transition's own fields.
func TestTransitionStatus_MergesCallerMetadata(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(64007)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	changed, err := TransitionStatus(context.Background(), pool,
		r.ReservationID, StatusPendingPayment, StatusConfirmed,
		Audit{
			Actor:    StripeWebhookActor("evt_test_123"),
			Metadata: map[string]any{"payment_intent_id": "pi_test_123"},
		})
	require.NoError(t, err)
	require.True(t, changed)

	events, err := ListAuditEvents(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Len(t, events, 2)

	e := events[1]
	require.Equal(t, AuditEventConfirmed, e.EventType)
	require.Equal(t, ActorTypeStripeWebhook, e.ActorType)
	require.Equal(t, "evt_test_123", e.ActorID)
	require.Equal(t, "pi_test_123", e.Metadata["payment_intent_id"])
	require.Equal(t, float64(r.Seats), e.Metadata["seats"],
		"the transition's own seats field must not be clobbered")
}

// ── The append-only guarantee ───────────────────────────────────────────

// TestAuditLog_RejectsDelete — the table is sealed. A plain DELETE is
// refused by the trigger.
func TestAuditLog_RejectsDelete(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(64008)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	_, err := pool.Exec(context.Background(),
		"DELETE FROM reservation_events WHERE reservation_id = $1", r.ReservationID)
	require.Error(t, err, "DELETE must be refused by the append-only trigger")
	require.Contains(t, err.Error(), "append-only")
}

// TestAuditLog_RejectsHistoryRewrite — UPDATE is refused for every
// column except user_id (the GDPR anonymisation path).
func TestAuditLog_RejectsHistoryRewrite(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(64009)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	_, err := pool.Exec(context.Background(),
		"UPDATE reservation_events SET event_type = 'EXPIRED' WHERE reservation_id = $1",
		r.ReservationID)
	require.Error(t, err, "rewriting history must be refused")
	require.Contains(t, err.Error(), "append-only")

	// The row is unchanged.
	events, err := ListAuditEvents(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Equal(t, AuditEventCreated, events[0].EventType)
}

// TestAuditLog_AllowsGDPRUserIDRewrite — the ONE sanctioned UPDATE:
// anonymising the user id (Layer 3, item 14).
func TestAuditLog_AllowsGDPRUserIDRewrite(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(64010)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	_, err := pool.Exec(context.Background(),
		"UPDATE reservation_events SET user_id = 'redacted:abc123' WHERE reservation_id = $1",
		r.ReservationID)
	require.NoError(t, err, "the GDPR anonymisation path must be allowed")

	events, err := ListAuditEvents(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Equal(t, "redacted:abc123", events[0].UserID)
}

// ── Trace correlation ───────────────────────────────────────────────────

// TestAuditRow_CarriesTraceID — the audit row records the trace_id of
// the request that caused the transition. That's the join key into
// Grafana / Loki / Jaeger.
//
// The test installs a real (always-sampling) tracer provider so the
// span has a valid trace context, then restores the global provider.
func TestAuditRow_CarriesTraceID(t *testing.T) {
	previous := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = tp.Shutdown(context.Background())
	})

	pool := newTestPool(t)
	const eventID = int64(64011)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)

	ctx, span := tp.Tracer("test").Start(context.Background(), "audit-test")
	defer span.End()

	require.NoError(t, InsertIfAbsent(ctx, pool, r))

	events, err := ListAuditEvents(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Len(t, events, 1)

	traceID := span.SpanContext().TraceID().String()
	require.Equal(t, traceID, events[0].TraceID,
		"the audit row must carry the active trace id")
}

// TestAuditRow_NoTraceIDWithoutSpan — with no span in the context
// (a background sweep, a unit test), the trace_id is NULL rather than
// a made-up value.
func TestAuditRow_NoTraceIDWithoutSpan(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(64012)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)

	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	events, err := ListAuditEvents(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Empty(t, events[0].TraceID, "no span → no trace id")
}

// ── Reads ───────────────────────────────────────────────────────────────

// TestListAuditEvents_Chronological — events come back in the order
// they happened (event_id is the insertion order).
func TestListAuditEvents_Chronological(t *testing.T) {
	pool := newTestPool(t)
	const eventID = int64(64013)

	seedEvent(t, pool, eventID)
	r := validReservation(t, eventID)
	cleanupReservation(t, pool, r.ReservationID)
	require.NoError(t, InsertIfAbsent(context.Background(), pool, r))

	_, err := TransitionStatus(context.Background(), pool,
		r.ReservationID, StatusPendingPayment, StatusConfirmed,
		Audit{Actor: StripeWebhookActor("evt_1")})
	require.NoError(t, err)

	_, err = TransitionStatus(context.Background(), pool,
		r.ReservationID, StatusConfirmed, StatusCancelled,
		Audit{Actor: StripeWebhookActor("evt_2")})
	require.NoError(t, err)

	events, err := ListAuditEvents(context.Background(), pool, r.ReservationID)
	require.NoError(t, err)
	require.Len(t, events, 3)
	require.Equal(t, AuditEventCreated, events[0].EventType)
	require.Equal(t, AuditEventConfirmed, events[1].EventType)
	require.Equal(t, AuditEventRefundInitiated, events[2].EventType,
		"CONFIRMED → CANCELLED is a refund, not a cancel")
}

// TestListAuditEvents_EmptyIsNotNil — "no events" is an empty slice,
// not nil, so it serialises as [] and not null.
func TestListAuditEvents_EmptyIsNotNil(t *testing.T) {
	pool := newTestPool(t)

	events, err := ListAuditEvents(context.Background(), pool, uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, events, "must return an empty slice, not nil")
	require.Empty(t, events)
}
