// Package stripe is a thin wrapper over the Stripe SDK.
//
// It exists so that the rest of the codebase depends on three small
// operations — create a PaymentIntent, verify a webhook signature,
// snapshot an event — rather than on the SDK's surface area. The
// SDK's types do not leak past this package boundary: everything
// outside it works with the small structs defined here.
//
// Three things this package is responsible for:
//
//  1. **The idempotency key.** CreatePaymentIntent always sets one,
//     and the caller supplies it. The /pay handler passes the
//     reservation_id, so a retried call returns the SAME
//     PaymentIntent instead of creating a second one. This is what
//     makes the crash window between "we wrote the payment row" and
//     "Stripe answered" a retry rather than a double-charge.
//
//  2. **The signature.** VerifyWebhookSignature is the only way to
//     turn an untrusted request body into a trusted event. Nothing
//     else in the codebase is allowed to parse a webhook body.
//
//  3. **The snapshot.** Stripe sends whole objects; this app needs
//     about eight fields. SnapshotEvent distils an event into the
//     small struct that goes on the queue, so the worker never has
//     to parse a Stripe payload — and so the queue carries a
//     readable, version-stable message instead of a blob.
package stripe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	stripeapi "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"
)

// Client wraps the Stripe SDK with exactly the operations this app
// needs.
type Client struct {
	api *stripeapi.Client
	// webhookSecret is the signing secret for THIS endpoint + mode.
	// It is never logged and never leaves the process.
	webhookSecret string
}

// NewClient builds a Stripe client.
//
// secretKey is the API key (sk_test_… / sk_live_…); webhookSecret is
// the signing secret for the webhook endpoint (whsec_…). Both are
// required — there is no "half-configured" Stripe.
func NewClient(secretKey, webhookSecret string) *Client {
	return &Client{
		api:           stripeapi.NewClient(secretKey),
		webhookSecret: webhookSecret,
	}
}

// PaymentIntent is our snapshot of a Stripe PaymentIntent — the
// fields this app actually uses. It deliberately does not embed the
// SDK's type, so nothing downstream depends on Stripe's shape.
type PaymentIntent struct {
	// ID is Stripe's PaymentIntent id (pi_…).
	ID string `json:"id"`
	// ClientSecret is what the client uses to confirm the payment.
	// It is sensitive: it authorises this one payment, so it must
	// never be logged and never stored.
	ClientSecret string `json:"client_secret"`
	// AmountCents is the amount Stripe will charge.
	AmountCents int64 `json:"amount_cents"`
	// Currency is the 3-letter lowercase ISO code.
	Currency string `json:"currency"`
	// Status is Stripe's PaymentIntent status.
	Status string `json:"status"`
}

// CreatePaymentIntent creates a PaymentIntent for the given amount.
//
// The idempotencyKey is REQUIRED and is sent as Stripe's
// Idempotency-Key header. Calling this twice with the same key
// returns the same PaymentIntent — it does not create a second one.
// The caller must therefore derive the key from something stable
// (the reservation_id), never from a random value or a timestamp.
//
// The PaymentIntent is created WITHOUT confirmation: the client
// confirms it with the client_secret, which is what lets a
// payment-method UI (Stripe Elements) do the confirming. Automatic
// payment methods are enabled so Stripe can offer the customer's
// available methods without us hard-coding a list.
//
// metadata is attached to the PaymentIntent (e.g. the reservation
// id), so a Stripe dashboard lookup can be traced back to a
// reservation without a database round-trip.
func (c *Client) CreatePaymentIntent(ctx context.Context, amountCents int64, currency, idempotencyKey, reservationID string) (*PaymentIntent, error) {
	if idempotencyKey == "" {
		// A missing idempotency key would silently turn "retry" into
		// "charge twice". Refuse rather than hope.
		return nil, fmt.Errorf("stripe: idempotency key is required")
	}
	if amountCents <= 0 {
		return nil, fmt.Errorf("stripe: amount must be positive, got %d", amountCents)
	}

	params := &stripeapi.PaymentIntentCreateParams{
		Amount:   stripeapi.Int64(amountCents),
		Currency: stripeapi.String(currency),
		AutomaticPaymentMethods: &stripeapi.PaymentIntentCreateAutomaticPaymentMethodsParams{
			Enabled: stripeapi.Bool(true),
		},
	}
	if reservationID != "" {
		params.Metadata = map[string]string{"reservation_id": reservationID}
	}
	params.SetIdempotencyKey(idempotencyKey)

	pi, err := c.api.V1PaymentIntents.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("stripe: create payment intent: %w", err)
	}

	return &PaymentIntent{
		ID:           pi.ID,
		ClientSecret: pi.ClientSecret,
		AmountCents:  pi.Amount,
		Currency:     string(pi.Currency),
		Status:       string(pi.Status),
	}, nil
}

// VerifyWebhook checks Stripe's signature header against the raw
// request body and returns the SNAPSHOT of the verified event.
//
// This is the ONLY way a webhook body becomes a trusted event. An
// error here means the request did not come from Stripe (or was
// tampered with, or is older than Stripe's tolerance window) — the
// caller must reject it.
//
// It returns OUR Event struct, not the SDK's — the SDK's types do
// not cross this package boundary. An event type this app does not
// handle comes back as an *UnhandledEventError, which the caller
// recognises with errors.Is(err, ErrUnhandledEventType).
//
// Note the signature is computed over the RAW bytes. A body that has
// been parsed and re-serialised does not sign identically, which is
// why the handler must read the body as bytes and hand those exact
// bytes here.
//
// ── On the API version ──
//
// The SDK's strict ConstructEvent rejects any event whose API version
// does not match the SDK's own release train ("2025-08-27.basil" →
// train "basil"). That is a SERIALIZATION compatibility check, not a
// security check — and taken literally it is a production landmine: a
// Stripe account whose webhook endpoint was created against an older
// API version would have every single event rejected, so payments
// would never confirm. Stripe would keep retrying a webhook we keep
// answering 400 to, and nothing in the logs would say "wrong API
// version" — it would say "signature invalid".
//
// So the version mismatch is ignored here, deliberately: the signature
// (the security property) is still verified strictly, and the fields
// this app reads — id, amount, currency, status, payment_intent.id —
// do not change shape across API versions. A mismatch is logged as a
// warning so it stays visible instead of becoming a silent surprise.
func (c *Client) VerifyWebhook(payload []byte, header string) (*Event, error) {
	if header == "" {
		return nil, fmt.Errorf("stripe: missing Stripe-Signature header")
	}
	event, err := webhook.ConstructEventWithOptions(payload, header, c.webhookSecret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		return nil, fmt.Errorf("stripe: webhook signature verification failed: %w", err)
	}
	if event.APIVersion != "" && event.APIVersion != stripeapi.APIVersion {
		slog.Warn("stripe webhook API version differs from the SDK's",
			"event_api_version", event.APIVersion,
			"sdk_api_version", stripeapi.APIVersion,
			"event_id", event.ID)
	}

	snap, err := SnapshotEvent(&event)
	if err != nil {
		// An event type we don't act on is not a failure of the
		// signature — it is a valid event we have no interest in.
		// Return it as a distinguishable error so the caller can
		// answer 200 (and stop Stripe sending it) instead of 400.
		var unhandled *UnhandledEventError
		if errors.As(err, &unhandled) {
			return nil, unhandled
		}
		return nil, err
	}
	return &snap, nil
}
