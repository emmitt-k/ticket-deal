package payment

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/stripe"
)

// testDSN returns the Postgres DSN for the tests, falling back to the
// local dev database. Mirrors the same helper in internal/db and
// internal/expire — the packages cannot share it without an import
// cycle, and three copies of four lines is cheaper than a test-only
// package.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://tickets:tickets@localhost:5432/tickets?sslmode=disable"
	}
	return dsn
}

// ── Event builders ────────────────────────────────────────────────────
//
// The tests build the snapshot directly rather than going through
// Stripe's wire format: the wire format is the webhook handler's
// job, and it is tested there. What matters HERE is the state
// machine's reaction to each kind of event.

// stripeEventSucceeded builds a payment_intent.succeeded snapshot.
func stripeEventSucceeded(eventID, piID string, amountCents int64) stripe.Event {
	return stripe.Event{
		ID:              eventID,
		Type:            stripe.EventTypePaymentIntentSucceeded,
		ObjectID:        piID,
		PaymentIntentID: piID,
		AmountCents:     amountCents,
		Currency:        "usd",
	}
}

// stripeEventFailed builds a payment_intent.payment_failed snapshot.
func stripeEventFailed(eventID, piID string) stripe.Event {
	return stripe.Event{
		ID:              eventID,
		Type:            stripe.EventTypePaymentIntentPaymentFailed,
		ObjectID:        piID,
		PaymentIntentID: piID,
		Currency:        "usd",
	}
}

// stripeEventRefunded builds a charge.refunded snapshot. The
// refunded amount is separate from the total, because a partial
// refund is the interesting case.
func stripeEventRefunded(eventID, chargeID, piID string, amountCents, refundedCents int64) stripe.Event {
	return stripe.Event{
		ID:                  eventID,
		Type:                stripe.EventTypeChargeRefunded,
		ObjectID:            chargeID,
		PaymentIntentID:     piID,
		AmountCents:         amountCents,
		AmountRefundedCents: refundedCents,
		Currency:            "usd",
	}
}

// marshalEvent serialises a snapshot the way the webhook handler
// publishes it.
func marshalEvent(t *testing.T, ev stripe.Event) []byte {
	t.Helper()
	body, err := json.Marshal(ev)
	require.NoError(t, err)
	return body
}
