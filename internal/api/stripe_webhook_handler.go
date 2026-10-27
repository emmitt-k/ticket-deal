// Package api — stripe_webhook_handler.go: POST /webhooks/stripe.
//
// Layer 2, item 7 of docs/product-roadmap.md — the endpoint Stripe
// calls when something happens to a payment.
//
// ── The design, and why ──
//
// This handler does exactly three things, and the ORDER is the
// whole design:
//
//  1. Verify Stripe's signature over the RAW body   → 400 on failure
//  2. Snapshot the event into the small struct we use
//  3. Publish the snapshot to the `stripe-events` queue → 200
//
// It does NOT touch the database and it does NOT change any status.
// The state machine lives in the stripe-worker, behind a queue —
// the same shape as the reservation path (reserve → SQS → worker).
// That keeps the webhook fast (Stripe gives you a short timeout and
// retries anything that isn't a 2xx) and it keeps the state change
// in one place.
//
// The failure mode this design has to get right is "event accepted
// but the publish failed". The fix is ORDERING: publish FIRST, then
// return 200. If the publish fails we return 500 and Stripe retries
// the delivery. The window between "we said 200" and "the event is
// durable" therefore does not exist — by the time we say 200, the
// event is already on the queue.
//
// WHY THE SIGNATURE IS CHECKED AGAINST THE RAW BYTES: the HMAC is
// computed over the exact bytes Stripe sent. A body that has been
// parsed into a map and re-serialised does not sign identically, so
// the handler must read the body as bytes and hand those exact bytes
// to the verifier.
//
// WHY THERE IS NO AUTH MIDDLEWARE: Stripe's signature IS the
// authentication. There is no bearer token to check, and adding one
// would be a second, weaker secret.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/stripe"
)

// maxWebhookBodyBytes caps the request body we will read.
//
// Stripe's payloads are small (a few KB). The cap is not about
// Stripe — it is about not letting an unauthenticated caller make us
// allocate an arbitrary amount of memory. The endpoint is public, so
// the body size is attacker-controlled until the signature verifies.
const maxWebhookBodyBytes = 1 << 20 // 1 MiB

// WebhookVerifier is the API's view of Stripe's signature check.
//
// It returns the stripe package's OWN Event struct — the SDK's types
// do not cross the package boundary, so the api package never has to
// know what a Stripe event looks like.
//
// An event type this app does not act on comes back as an error
// wrapping stripe.ErrUnhandledEventType, which the handler answers
// with 200 (the event was fine, we just have no interest in it).
// A signature failure is any other error, and gets a 400.
type WebhookVerifier interface {
	VerifyWebhook(payload []byte, header string) (*stripe.Event, error)
}

// EventPublisher publishes the verified event to the stripe-events
// queue.
//
// It is the same shape as the reservation path's publisher seam
// (publish raw bytes), so one implementation serves both.
type EventPublisher interface {
	Publish(ctx context.Context, body []byte) error
}

// StripeWebhookHandler returns the http.HandlerFunc for
// POST /webhooks/stripe.
//
// No auth middleware: the Stripe signature is the authentication.
func StripeWebhookHandler(verifier WebhookVerifier, publisher EventPublisher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "stripe_webhook.handle")
		defer span.End()
		r = r.WithContext(ctx)

		// ── 1. Read the RAW body ──
		//
		// Exactly these bytes are what Stripe signed. Read them with
		// a hard cap, because this endpoint is public and the body
		// size is attacker-controlled until the signature verifies.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))
		if err != nil {
			slog.WarnContext(ctx, "read webhook body failed", "error", err)
			apiutil.WriteError(w, http.StatusBadRequest,
				"invalid_body", "could not read the request body")
			span.SetStatus(codes.Error, "body read failed")
			return
		}

		// ── 2. Verify the signature ──
		//
		// A bad signature is a 400: the request did not come from
		// Stripe. Stripe never retries a 4xx (it only retries on a
		// timeout or a 5xx), so this is exactly right — a forged
		// request should not cause Stripe to keep re-sending it.
		snap, err := verifier.VerifyWebhook(body, r.Header.Get("Stripe-Signature"))
		if err != nil {
			// An event type we don't act on is NOT a failure. Stripe
			// sends many event types and we only care about three, so
			// we answer 200 and let it stop sending this one. A
			// genuine signature failure is a 400 — and Stripe does
			// not retry a 4xx, so a forged request cannot make it
			// hammer us.
			if errors.Is(err, stripe.ErrUnhandledEventType) {
				slog.InfoContext(ctx, "ignoring unhandled stripe event", "error", err)
				span.SetAttributes(attribute.String("webhook.outcome", "unhandled_event"))
				apiutil.WriteJSON(w, http.StatusOK, map[string]any{"received": true, "handled": false})
				return
			}
			slog.WarnContext(ctx, "stripe webhook signature verification failed", "error", err)
			apiutil.WriteError(w, http.StatusBadRequest,
				"invalid_signature", "signature verification failed")
			span.SetStatus(codes.Error, "invalid signature")
			return
		}

		// ── 4. Publish — BEFORE the 200 ──
		//
		// If the publish fails we return 500 so Stripe retries the
		// delivery. By the time we return 200 the event is already
		// durable on the queue.
		payload, err := json.Marshal(snap)
		if err != nil {
			slog.ErrorContext(ctx, "marshal stripe event failed", "error", err)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "could not serialise the event")
			span.SetStatus(codes.Error, "marshal failed")
			return
		}

		if err := publisher.Publish(ctx, payload); err != nil {
			// 500 → Stripe retries. The event is idempotent on the
			// other end (the worker keys off the event id), so a
			// retry is safe.
			slog.ErrorContext(ctx, "publish stripe event failed (stripe will retry)", "error", err)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"publish_failed", "could not queue the event")
			span.SetStatus(codes.Error, "publish failed")
			return
		}

		span.SetAttributes(
			attribute.String("webhook.outcome", "queued"),
			attribute.String("stripe.event_id", snap.ID),
			attribute.String("stripe.event_type", snap.Type),
		)
		apiutil.WriteJSON(w, http.StatusOK, map[string]any{"received": true, "handled": true})
	}
}
