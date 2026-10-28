package stripe

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	stripeapi "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"
)

// The webhook signing secret used across this file. A fixed value so
// the tests are deterministic — it is a test fixture, not a key.
const testWebhookSecret = "whsec_test_secret_not_a_real_secret"

// signedHeader builds a Stripe-Signature header the way Stripe does:
// "t=<unix seconds>,v1=<hex hmac of "<timestamp>.<payload>">".
//
// Using the SDK's own ComputeSignature means the test exercises the
// real format rather than a hand-rolled approximation of it.
func signedHeader(t *testing.T, payload []byte, secret string, at time.Time) string {
	t.Helper()
	sig := webhook.ComputeSignature(at, payload, secret)
	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(sig))
}

// ── Signature verification ────────────────────────────────────────────
//
// This is the security boundary of the whole integration: an
// unverified webhook body is an attacker saying "this reservation was
// paid for". So the negative cases matter more than the happy path.

// TestVerifyWebhookSignature_ValidSignature — a correctly signed
// payload verifies and returns the event.
func TestVerifyWebhookSignature_ValidSignature(t *testing.T) {
	c := NewClient("sk_test_dummy", testWebhookSecret)
	payload := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{"id":"pi_1"}}}`)

	header := signedHeader(t, payload, testWebhookSecret, time.Now())
	snap, err := c.VerifyWebhook(payload, header)
	require.NoError(t, err)
	require.Equal(t, "evt_1", snap.ID)
}

// TestVerifyWebhookSignature_MissingHeader — no header at all.
func TestVerifyWebhookSignature_MissingHeader(t *testing.T) {
	c := NewClient("sk_test_dummy", testWebhookSecret)
	_, err := c.VerifyWebhook([]byte(`{}`), "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing Stripe-Signature")
}

// TestVerifyWebhookSignature_TamperedPayload — the signature was
// computed over a different body. This is the attack the signature
// exists to stop.
func TestVerifyWebhookSignature_TamperedPayload(t *testing.T) {
	c := NewClient("sk_test_dummy", testWebhookSecret)
	original := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{"id":"pi_1"}}}`)
	tampered := []byte(`{"id":"evt_2","type":"payment_intent.succeeded","data":{"object":{"id":"pi_1"}}}`)

	header := signedHeader(t, original, testWebhookSecret, time.Now())
	_, err := c.VerifyWebhook(tampered, header)
	require.Error(t, err)
	require.Contains(t, err.Error(), "signature verification failed")
}

// TestVerifyWebhookSignature_WrongSecret — signed with a different
// secret (e.g. the live secret against a test endpoint).
func TestVerifyWebhookSignature_WrongSecret(t *testing.T) {
	c := NewClient("sk_test_dummy", testWebhookSecret)
	payload := []byte(`{"id":"evt_1","type":"payment_intent.succeeded"}`)

	header := signedHeader(t, payload, "whsec_a_different_secret", time.Now())
	_, err := c.VerifyWebhook(payload, header)
	require.Error(t, err)
}

// TestVerifyWebhookSignature_StaleTimestamp — a signature older than
// Stripe's tolerance window is rejected, so a captured-and-replayed
// request does not work forever.
func TestVerifyWebhookSignature_StaleTimestamp(t *testing.T) {
	c := NewClient("sk_test_dummy", testWebhookSecret)
	payload := []byte(`{"id":"evt_1","type":"payment_intent.succeeded"}`)

	old := time.Now().Add(-1 * time.Hour)
	header := signedHeader(t, payload, testWebhookSecret, old)
	_, err := c.VerifyWebhook(payload, header)
	require.Error(t, err)
}

// unmarshalEvent parses a Stripe event payload the way the webhook
// handler does — the SDK's own Event type, including its custom
// EventData unmarshaller.
func unmarshalEvent(payload []byte, e *stripeapi.Event) error {
	return json.Unmarshal(payload, e)
}

// TestVerifyWebhookSignature_OldAPIVersionStillVerifies — the SDK's
// strict mode rejects any event whose API version is not on the
// SDK's own release train. A Stripe account whose webhook endpoint
// was created against an older API version would then have EVERY
// event rejected — payments never confirm, and the logs say
// "signature invalid" rather than "wrong API version".
//
// We ignore the version mismatch deliberately (the signature, which
// is the security property, is still checked strictly), so this test
// pins that behaviour: an older event still verifies.
func TestVerifyWebhookSignature_OldAPIVersionStillVerifies(t *testing.T) {
	c := NewClient("sk_test_dummy", testWebhookSecret)
	payload := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","api_version":"2020-08-27.dodo","data":{"object":{"id":"pi_1"}}}`)

	header := signedHeader(t, payload, testWebhookSecret, time.Now())
	snap, err := c.VerifyWebhook(payload, header)
	require.NoError(t, err, "an API-version mismatch must not reject a correctly signed event")
	require.Equal(t, "evt_1", snap.ID)
}

// ── The event snapshot ────────────────────────────────────────────────

// paymentIntentEventJSON builds a minimal but realistic
// payment_intent.* event payload.
func paymentIntentEventJSON(id, piID, status string, amount int64) []byte {
	return []byte(fmt.Sprintf(`{
	  "id": %q,
	  "object": "event",
	  "api_version": %q,
	  "type": "payment_intent.%s",
	  "data": {
	    "object": {
	      "id": %q,
	      "object": "payment_intent",
	      "amount": %d,
	      "currency": "usd",
	      "status": %q
	    }
	  }
	}`, id, stripeapi.APIVersion, status, piID, amount, status))
}

// TestSnapshotEvent_PaymentIntentSucceeded — the snapshot carries the
// fields the worker needs, and nothing else.
func TestSnapshotEvent_PaymentIntentSucceeded(t *testing.T) {
	payload := paymentIntentEventJSON("evt_1", "pi_123", "succeeded", 2500)

	var e stripeapi.Event
	require.NoError(t, unmarshalEvent(payload, &e))

	snap, err := SnapshotEvent(&e)
	require.NoError(t, err)
	require.Equal(t, "evt_1", snap.ID)
	require.Equal(t, EventTypePaymentIntentSucceeded, snap.Type)
	require.Equal(t, "pi_123", snap.PaymentIntentID)
	require.Equal(t, "pi_123", snap.ObjectID)
	require.Equal(t, int64(2500), snap.AmountCents)
	require.Equal(t, "usd", snap.Currency)
}

// TestSnapshotEvent_PaymentIntentFailed — the failure event snapshots
// the same way; the difference is in what the worker does with it.
func TestSnapshotEvent_PaymentIntentFailed(t *testing.T) {
	payload := paymentIntentEventJSON("evt_2", "pi_456", "payment_failed", 2500)

	var e stripeapi.Event
	require.NoError(t, unmarshalEvent(payload, &e))

	snap, err := SnapshotEvent(&e)
	require.NoError(t, err)
	require.Equal(t, EventTypePaymentIntentPaymentFailed, snap.Type)
	require.Equal(t, "pi_456", snap.PaymentIntentID)
}

// TestSnapshotEvent_ChargeRefunded — a refund event must carry the
// PaymentIntent id, because that is how a refund finds the
// reservation it is reversing.
func TestSnapshotEvent_ChargeRefunded(t *testing.T) {
	payload := []byte(`{
	  "id": "evt_3",
	  "object": "event",
	  "api_version": "` + stripeapi.APIVersion + `",
	  "type": "charge.refunded",
	  "data": {
	    "object": {
	      "id": "ch_789",
	      "object": "charge",
	      "amount": 2500,
	      "amount_refunded": 2500,
	      "currency": "usd",
	      "payment_intent": { "id": "pi_123", "object": "payment_intent" }
	    }
	  }
	}`)

	var e stripeapi.Event
	require.NoError(t, unmarshalEvent(payload, &e))

	snap, err := SnapshotEvent(&e)
	require.NoError(t, err)
	require.Equal(t, EventTypeChargeRefunded, snap.Type)
	require.Equal(t, "ch_789", snap.ObjectID)
	require.Equal(t, "pi_123", snap.PaymentIntentID, "the refund must find its PaymentIntent")
	require.Equal(t, int64(2500), snap.AmountCents)
	require.Equal(t, int64(2500), snap.AmountRefundedCents)
	require.True(t, snap.IsFullRefund())
}

// TestSnapshotEvent_UnknownType — an event type this app does not
// handle is an error, not a silently-empty snapshot. The caller
// logs-and-ignores it; it must not be mistaken for something we
// processed.
func TestSnapshotEvent_UnknownType(t *testing.T) {
	payload := []byte(`{"id":"evt_9","type":"customer.created","data":{"object":{"id":"cus_1"}}}`)

	var e stripeapi.Event
	require.NoError(t, unmarshalEvent(payload, &e))

	_, err := SnapshotEvent(&e)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnhandledEventType)
	var unhandled *UnhandledEventError
	require.ErrorAs(t, err, &unhandled)
	require.Equal(t, "evt_9", unhandled.EventID)
	require.Equal(t, "customer.created", unhandled.Type)
}

// TestVerifyWebhook_UnhandledTypeIsDistinguishable — a valid event of
// an unhandled type must NOT look like a signature failure. The
// webhook handler answers 400 for a bad signature and 200 for an
// unhandled type, and it tells them apart with this sentinel.
func TestVerifyWebhook_UnhandledTypeIsDistinguishable(t *testing.T) {
	c := NewClient("sk_test_dummy", testWebhookSecret)
	payload := []byte(`{"id":"evt_9","type":"customer.created","api_version":"` + stripeapi.APIVersion + `","data":{"object":{"id":"cus_1"}}}`)

	header := signedHeader(t, payload, testWebhookSecret, time.Now())
	_, err := c.VerifyWebhook(payload, header)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnhandledEventType)
	require.NotContains(t, err.Error(), "signature verification failed")
}

// TestSnapshotEvent_NilData — a malformed event must not panic.
func TestSnapshotEvent_NilData(t *testing.T) {
	_, err := SnapshotEvent(nil)
	require.Error(t, err)

	_, err = SnapshotEvent(&stripeapi.Event{ID: "evt_x"})
	require.Error(t, err)
}

// ── Full vs partial refund ────────────────────────────────────────────

// TestIsFullRefund — the distinction that keeps a partial refund
// from releasing a seat that was not paid back.
func TestIsFullRefund(t *testing.T) {
	tests := []struct {
		name           string
		amountCents    int64
		refundedCents  int64
		wantFullRefund bool
	}{
		{"full refund", 2500, 2500, true},
		{"partial refund", 2500, 1000, false},
		{"nothing refunded", 2500, 0, true},
		{"unknown amount", 0, 0, true},
		{"over-refunded", 2500, 3000, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := Event{AmountCents: tc.amountCents, AmountRefundedCents: tc.refundedCents}
			require.Equal(t, tc.wantFullRefund, e.IsFullRefund())
		})
	}
}
