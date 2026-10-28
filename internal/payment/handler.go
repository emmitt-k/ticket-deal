// Package payment is the Stripe payment state machine.
//
// It is the consumer side of the `stripe-events` queue: it takes a
// verified, snapshotted Stripe event and turns it into a reservation
// status change — or, for a partial refund, into an audit record.
//
// The API's webhook handler does NOT do any of this. It verifies the
// signature and queues the event; this package is where the state
// machine lives. That split keeps the webhook fast (Stripe has a
// short timeout) and keeps every status change in one place.
//
// The three events it acts on:
//
//	payment_intent.succeeded   → PENDING_PAYMENT → CONFIRMED
//	payment_intent.payment_failed → the reservation stays PENDING_PAYMENT
//	                             (the seat is still held; the customer
//	                             can retry with another card)
//	charge.refunded            → CONFIRMED → REFUNDED + the seat goes
//	                             back on sale — but only for a FULL
//	                             refund. A partial refund records the
//	                             event and leaves the status alone.
package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/metrics"
	"github.com/emmitt-k/ticket-deal/internal/redis"
	"github.com/emmitt-k/ticket-deal/internal/stripe"
)

// Handler applies Stripe events to reservations.
//
// Deps: Postgres (the reservations and the payment rows) and Redis
// (to put a refunded seat back on sale). Both are needed for a
// correct refund; the payment-succeeded path only needs Postgres.
type Handler struct {
	pool *db.Pool
	rdb  *redis.Client
}

// NewHandler builds a Handler.
func NewHandler(pool *db.Pool, rdb *redis.Client) *Handler {
	return &Handler{pool: pool, rdb: rdb}
}

// HandleMessage implements queue.MessageHandler: it deserialises the
// snapshot and applies it.
//
// A message that will never parse is DROPPED, not redelivered. SQS
// redelivers a message whose handler returns an error, forever — a
// poison message would block the queue behind it. A malformed
// snapshot is a bug in the publisher, and the only way to make
// progress is to log it loudly and move on.
func (h *Handler) HandleMessage(ctx context.Context, body []byte) error {
	var ev stripe.Event
	if err := json.Unmarshal(body, &ev); err != nil {
		slog.ErrorContext(ctx, "malformed stripe event dropped", "error", err,
			"body_bytes", len(body))
		metrics.StripeEventsMalformed.Inc()
		return nil
	}
	return h.HandleEvent(ctx, ev)
}

// HandleEvent applies one verified Stripe event.
func (h *Handler) HandleEvent(ctx context.Context, ev stripe.Event) error {
	switch ev.Type {
	case stripe.EventTypePaymentIntentSucceeded:
		return h.handleSucceeded(ctx, ev)
	case stripe.EventTypePaymentIntentPaymentFailed:
		return h.handleFailed(ctx, ev)
	case stripe.EventTypeChargeRefunded:
		return h.handleRefunded(ctx, ev)
	default:
		// The webhook only publishes event types it handles, so this
		// is belt-and-braces. Not an error.
		slog.InfoContext(ctx, "ignoring unhandled stripe event type", "type", ev.Type)
		return nil
	}
}

// handleSucceeded — the money arrived. This is the ONLY path to the
// CONFIRMED status.
func (h *Handler) handleSucceeded(ctx context.Context, ev stripe.Event) error {
	pay, ok, err := h.lookupPayment(ctx, ev)
	if err != nil || !ok {
		return err
	}

	// Stripe's view of the payment first — it is the fact we were
	// told.
	if err := db.UpdatePaymentIntentStatus(ctx, h.pool, ev.PaymentIntentID,
		db.PaymentStatusSucceeded); err != nil && !errors.Is(err, db.ErrPaymentIntentNotFound) {
		return fmt.Errorf("payment: update payment status: %w", err)
	}

	// Then the business state change.
	changed, err := db.TransitionStatus(ctx, h.pool, pay.ReservationID,
		db.StatusPendingPayment, db.StatusConfirmed,
		db.Audit{
			Actor: db.StripeWebhookActor(ev.ID),
			Metadata: map[string]any{
				"payment_intent_id": ev.PaymentIntentID,
				"amount_cents":      ev.AmountCents,
				"currency":          ev.Currency,
			},
		})
	if err != nil && !errors.Is(err, db.ErrInvalidTransition) {
		return fmt.Errorf("payment: transition to confirmed: %w", err)
	}
	if changed {
		metrics.StripePaymentsSucceeded.Inc()
		slog.InfoContext(ctx, "payment confirmed",
			"reservation_id", pay.ReservationID,
			"event_id", ev.ID,
			"amount_cents", ev.AmountCents)
		return nil
	}

	// The row was not PENDING_PAYMENT. Find out what it is, because
	// the reasons are very different.
	row, err := db.GetReservation(ctx, h.pool, pay.ReservationID)
	if err != nil {
		return fmt.Errorf("payment: read reservation after lost CAS: %w", err)
	}

	switch row.Status {
	case db.StatusConfirmed:
		// A duplicate delivery. Stripe delivers the same event id
		// more than once when the first delivery was not
		// acknowledged, and it also sends both
		// payment_intent.succeeded and charge.succeeded for the
		// same payment. Idempotent — nothing to do.
		slog.InfoContext(ctx, "payment already confirmed (duplicate delivery)",
			"reservation_id", pay.ReservationID,
			"event_id", ev.ID)
		return nil

	case db.StatusExpired:
		// ── THE RACE ──
		//
		// The hold expired (the customer sat on the payment page for
		// longer than the hold TTL) and the seat went back on sale —
		// and then the payment succeeded. We have taken money for a
		// seat that may already be sold.
		//
		// We do NOT auto-refund. A refund on a race is its own class
		// of bug: it moves money on a path that did not reason about
		// partial refunds or about who actually owns the seat. A
		// human decides. The metric and the log line are the alert;
		// the payment row says SUCCEEDED while the reservation says
		// EXPIRED, which is exactly the mismatch to look for.
		metrics.StripePaymentAfterExpiry.Inc()
		slog.ErrorContext(ctx,
			"PAYMENT SUCCEEDED AFTER THE HOLD EXPIRED — a refund is needed and the seat may be oversold",
			"reservation_id", pay.ReservationID,
			"event_id", ev.ID,
			"payment_intent_id", ev.PaymentIntentID,
			"amount_cents", ev.AmountCents,
			"currency", ev.Currency,
			"seats", row.Seats,
		)
		return nil

	default:
		// CANCELLED (the customer cancelled while paying) or
		// REFUNDED. Same shape of problem as the expiry race: money
		// arrived for a seat we do not hold. Alert, do not act.
		metrics.StripePaymentAfterTerminal.Inc()
		slog.ErrorContext(ctx,
			"payment succeeded for a reservation that is no longer payable — a refund is needed",
			"reservation_id", pay.ReservationID,
			"status", row.Status,
			"event_id", ev.ID,
			"payment_intent_id", ev.PaymentIntentID,
			"amount_cents", ev.AmountCents,
		)
		return nil
	}
}

// handleFailed — the payment did not go through.
//
// The reservation stays PENDING_PAYMENT: the seat is still held, the
// customer can retry with a different card, and the hold TTL is still
// ticking. Stripe's PaymentIntent status does not change on a failed
// confirmation (it goes back to requires_payment_method), so there is
// nothing to write to the payment row — this is a signal, not a state.
func (h *Handler) handleFailed(ctx context.Context, ev stripe.Event) error {
	metrics.StripePaymentsFailed.Inc()
	slog.WarnContext(ctx, "stripe payment failed",
		"event_id", ev.ID,
		"payment_intent_id", ev.PaymentIntentID,
		"amount_cents", ev.AmountCents,
		"currency", ev.Currency,
	)
	return nil
}

// handleRefunded — the money went back.
//
// A FULL refund moves the reservation to REFUNDED and puts the seat
// back on sale. A PARTIAL refund records the event and leaves the
// status at CONFIRMED — which is exactly why the audit EVENT and the
// reservation STATUS are separate concepts: the event records what
// was DONE (money came back), the status records where the
// reservation ended up (still a valid ticket).
func (h *Handler) handleRefunded(ctx context.Context, ev stripe.Event) error {
	pay, ok, err := h.lookupPayment(ctx, ev)
	if err != nil || !ok {
		return err
	}

	row, err := db.GetReservation(ctx, h.pool, pay.ReservationID)
	if err != nil {
		return fmt.Errorf("payment: read reservation for refund: %w", err)
	}

	metadata := map[string]any{
		"payment_intent_id":     ev.PaymentIntentID,
		"charge_id":             ev.ObjectID,
		"amount_cents":          ev.AmountCents,
		"amount_refunded_cents": ev.AmountRefundedCents,
		"currency":              ev.Currency,
	}

	// ── A partial refund: an event, not a state ──
	if !ev.IsFullRefund() {
		metrics.StripePartialRefunds.Inc()
		if err := db.WriteAuditEvent(ctx, h.pool, pay.ReservationID, pay.UserID,
			db.AuditEntry{
				EventType: db.AuditEventRefundInitiated,
				Actor:     db.StripeWebhookActor(ev.ID),
				// The status does not change — the ticket is still
				// valid. old_status == new_status says "the money
				// moved, the reservation did not".
				OldStatus: row.Status,
				NewStatus: row.Status,
				Metadata:  metadata,
			}); err != nil {
			return fmt.Errorf("payment: write partial refund audit: %w", err)
		}
		slog.InfoContext(ctx, "partial refund recorded (the reservation is unchanged)",
			"reservation_id", pay.ReservationID,
			"event_id", ev.ID,
			"amount_refunded_cents", ev.AmountRefundedCents,
			"amount_cents", ev.AmountCents)
		return nil
	}

	// ── A full refund: CONFIRMED → REFUNDED ──
	changed, err := db.TransitionStatus(ctx, h.pool, pay.ReservationID,
		db.StatusConfirmed, db.StatusRefunded,
		db.Audit{
			Actor:    db.StripeWebhookActor(ev.ID),
			Metadata: metadata,
		})
	if err != nil && !errors.Is(err, db.ErrInvalidTransition) {
		return fmt.Errorf("payment: transition to refunded: %w", err)
	}
	if !changed {
		// Either it was already refunded (a duplicate delivery), or
		// it was never confirmed. Both are "nothing to do" — but the
		// second one is worth a metric, because it means money came
		// back for a ticket we do not think was paid for.
		if row.Status == db.StatusRefunded {
			slog.InfoContext(ctx, "reservation already refunded (duplicate delivery)",
				"reservation_id", pay.ReservationID,
				"event_id", ev.ID)
			return nil
		}
		metrics.StripePaymentAfterTerminal.Inc()
		slog.ErrorContext(ctx, "refund for a reservation that was not confirmed",
			"reservation_id", pay.ReservationID,
			"status", row.Status,
			"event_id", ev.ID,
			"amount_refunded_cents", ev.AmountRefundedCents)
		return nil
	}

	metrics.StripeRefunds.Inc()
	slog.InfoContext(ctx, "reservation refunded",
		"reservation_id", pay.ReservationID,
		"event_id", ev.ID,
		"amount_refunded_cents", ev.AmountRefundedCents)

	// ── Put the seat back on sale ──
	//
	// A refunded ticket is a ticket we can sell again. The Redis
	// failure is logged but does NOT fail the event: the database
	// transition is the durable fact, and an inventory discrepancy is
	// a reconcilable operator problem (the same policy as the cancel
	// handler's seat release).
	invKey := redis.InventoryKey(row.EventID)
	if _, err := h.rdb.IncrBy(ctx, invKey, int64(row.Seats)).Result(); err != nil {
		slog.ErrorContext(ctx, "refund: redis INCRBY failed; manual reconciliation may be required",
			"reservation_id", pay.ReservationID,
			"event_id", row.EventID,
			"seats", row.Seats,
			"inventory_key", invKey,
			"error", err,
		)
	}
	return nil
}

// lookupPayment finds the payment row for an event.
//
// The "ok" flag is false when we have no record of the payment — a
// payment for a reservation we do not know about. That is worth
// alerting on (it means either a stale webhook or something we did
// not create), but it is not an error: there is nothing to retry.
func (h *Handler) lookupPayment(ctx context.Context, ev stripe.Event) (db.PaymentIntentRow, bool, error) {
	if ev.PaymentIntentID == "" {
		metrics.StripePaymentsUnknown.Inc()
		slog.ErrorContext(ctx, "stripe event carries no payment intent id",
			"event_id", ev.ID, "event_type", ev.Type)
		return db.PaymentIntentRow{}, false, nil
	}

	pay, err := db.GetPaymentIntentByStripeID(ctx, h.pool, ev.PaymentIntentID)
	if err != nil {
		if db.IsNoRows(err) {
			metrics.StripePaymentsUnknown.Inc()
			slog.ErrorContext(ctx, "stripe event for an unknown payment intent",
				"event_id", ev.ID,
				"event_type", ev.Type,
				"payment_intent_id", ev.PaymentIntentID)
			return db.PaymentIntentRow{}, false, nil
		}
		return db.PaymentIntentRow{}, false, fmt.Errorf("payment: look up payment intent: %w", err)
	}
	return pay, true, nil
}
