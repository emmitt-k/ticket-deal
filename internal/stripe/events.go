package stripe

import (
	"encoding/json"
	"fmt"

	stripeapi "github.com/stripe/stripe-go/v82"
)

// The Stripe event types this app acts on.
//
// The values come from the SDK's own typed constants — a typo in a
// string literal would compile happily and then silently never
// match. The SDK's thin-event type names (v1.billing…) have no
// constants, but this app only needs the three below.
const (
	// EventTypePaymentIntentSucceeded — the money arrived. This is
	// the only path to the CONFIRMED status.
	EventTypePaymentIntentSucceeded = string(stripeapi.EventTypePaymentIntentSucceeded)
	// EventTypePaymentIntentPaymentFailed — the payment did not go
	// through. The reservation stays PENDING_PAYMENT (the seat is
	// still held, the user can retry); we record the failure and
	// emit a metric.
	EventTypePaymentIntentPaymentFailed = string(stripeapi.EventTypePaymentIntentPaymentFailed)
	// EventTypeChargeRefunded — the money went back. A FULL refund
	// moves the reservation to REFUNDED and releases the seat; a
	// partial refund records the event and leaves the status alone.
	EventTypeChargeRefunded = string(stripeapi.EventTypeChargeRefunded)
)

// Event is the snapshot of a Stripe webhook event that this app cares
// about.
//
// WHY A SNAPSHOT AND NOT THE RAW EVENT: Stripe sends whole objects
// (a PaymentIntent is dozens of fields, most of them irrelevant here).
// The snapshot keeps eight fields, in a shape we control, so the
// queue carries a readable and version-stable message and the worker
// never has to parse a Stripe payload.
//
// It also means a Stripe API version bump cannot silently change the
// meaning of a message that is already sitting in the queue.
//
// The type is JSON-serialisable end to end: the webhook handler
// snapshots, the message goes on SQS, the worker deserialises.
type Event struct {
	// ID is Stripe's event id (evt_…). It is the natural
	// deduplication key: Stripe delivers the same event id more
	// than once if the first delivery was not acknowledged.
	ID string `json:"id"`
	// Type is one of the EventType* constants above.
	Type string `json:"type"`
	// ObjectID is the id of the object the event is about — pi_… for
	// a PaymentIntent event, ch_… for a Charge event.
	ObjectID string `json:"object_id"`
	// PaymentIntentID is the PaymentIntent this event concerns. For
	// a charge.refunded event it is the PaymentIntent the charge
	// belongs to — which is how a refund finds its reservation.
	PaymentIntentID string `json:"payment_intent_id,omitempty"`
	// AmountCents is the amount involved, in the currency's minor
	// unit.
	AmountCents int64 `json:"amount_cents,omitempty"`
	// AmountRefundedCents is how much was refunded. Only meaningful
	// for charge.refunded.
	AmountRefundedCents int64 `json:"amount_refunded_cents,omitempty"`
	// Currency is the 3-letter lowercase ISO code.
	Currency string `json:"currency,omitempty"`
}

// IsFullRefund reports whether a refund event returned the entire
// amount.
//
// The distinction is load-bearing, and it is exactly why the audit
// EVENT and the reservation STATUS are separate concepts: a partial
// refund is an event (money went back) whose outcome is "still
// confirmed" — only a FULL refund becomes a state.
//
// A refund event with no PaymentIntentID is treated as full, because
// the alternative (assuming a partial refund) would leave a
// fully-refunded reservation stuck in CONFIRMED forever.
func (e Event) IsFullRefund() bool {
	return e.AmountRefundedCents <= 0 || e.AmountRefundedCents >= e.AmountCents
}

// SnapshotEvent distils a verified Stripe event into the small
// struct this app works with.
//
// It returns an error for an event type this app does not handle, so
// the caller can log-and-ignore it rather than guess. It returns
// (Event{}, nil) semantics only for known types.
func SnapshotEvent(e *stripeapi.Event) (Event, error) {
	if e == nil {
		return Event{}, fmt.Errorf("stripe: nil event")
	}
	if e.Data == nil {
		return Event{}, fmt.Errorf("stripe: event %s has no data", e.ID)
	}

	switch string(e.Type) {
	case EventTypePaymentIntentSucceeded, EventTypePaymentIntentPaymentFailed:
		var pi stripeapi.PaymentIntent
		if err := json.Unmarshal(e.Data.Raw, &pi); err != nil {
			return Event{}, fmt.Errorf("stripe: decode payment intent from event %s: %w", e.ID, err)
		}
		return Event{
			ID:              e.ID,
			Type:            string(e.Type),
			ObjectID:        pi.ID,
			PaymentIntentID: pi.ID,
			AmountCents:     pi.Amount,
			Currency:        string(pi.Currency),
		}, nil

	case EventTypeChargeRefunded:
		var ch stripeapi.Charge
		if err := json.Unmarshal(e.Data.Raw, &ch); err != nil {
			return Event{}, fmt.Errorf("stripe: decode charge from event %s: %w", e.ID, err)
		}
		ev := Event{
			ID:                  e.ID,
			Type:                string(e.Type),
			ObjectID:            ch.ID,
			AmountCents:         ch.Amount,
			AmountRefundedCents: ch.AmountRefunded,
			Currency:            string(ch.Currency),
		}
		// The charge carries the PaymentIntent it belongs to — that
		// is how a refund finds the reservation it is reversing.
		if ch.PaymentIntent != nil {
			ev.PaymentIntentID = ch.PaymentIntent.ID
		}
		return ev, nil

	default:
		return Event{}, fmt.Errorf("stripe: unhandled event type %q", e.Type)
	}
}
