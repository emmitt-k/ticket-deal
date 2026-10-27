// Package api — pay_handler.go: POST /api/reservations/{id}/pay.
//
// Layer 2, item 7 of docs/product-roadmap.md — the endpoint that turns
// a held reservation into a payment.
//
// The flow, in order:
//
//  1. Verify the JWT matches the row's user_id        → 403 if not
//  2. Verify the status is PENDING_PAYMENT            → 409 otherwise
//  3. Compute the amount from the reservation's PRICE SNAPSHOT
//     (never from the request — the client cannot name a price)
//  4. Write the payment row (durable intent BEFORE the call)
//  5. Create the PaymentIntent at Stripe, with the reservation_id
//     as Stripe's idempotency key
//  6. Record Stripe's PaymentIntent id on the row
//  7. Return the client_secret so the client can confirm the payment
//
// Why the order matters (step 4 before step 5): the payment row is
// written FIRST, so the intent to pay is durable before we call a
// third party. If the process dies between 4 and 5, the row exists
// with a NULL stripe_payment_intent_id — and because step 5 passes
// the reservation_id as Stripe's Idempotency-Key, the retry converges
// on the SAME PaymentIntent instead of creating a second one.
//
// Why the idempotency key is the reservation_id and not a fresh
// random value: a random key would make every retry a NEW charge.
// The reservation id is stable, so "pay for this reservation" is
// expressed as "create the one PaymentIntent for this reservation".
//
// This endpoint does NOT confirm the payment. Confirmation happens on
// the client with the client_secret (Stripe Elements), and the
// server learns the outcome from the webhook — because a client that
// confirms and then never reports back is a case the server must not
// depend on.
package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/stripe"
)

// PaymentIntentCreator is the API's view of Stripe: the one method
// the /pay handler needs.
//
// It is an interface so the handler can be tested without a network
// or a Stripe account — the same seam the reservation path uses for
// its publisher.
type PaymentIntentCreator interface {
	CreatePaymentIntent(ctx context.Context, amountCents int64, currency, idempotencyKey, reservationID string) (*stripe.PaymentIntent, error)
}

// PayHandler returns the http.HandlerFunc for
// POST /api/reservations/{id}/pay.
//
// Auth: the route must be wired with auth.Middleware (main.go does).
// Deps: Postgres for the reservation + the payment row, and the Stripe
// client. Both are mandatory — the amount comes from the database,
// never from the request.
func PayHandler(pool *db.Pool, payments PaymentIntentCreator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "pay_reservation.handle")
		defer span.End()
		r = r.WithContext(ctx)

		claims := auth.ClaimsFromContext(r.Context())
		if claims == nil {
			// Defensive — main.go wires auth.Middleware, so this only
			// fires if someone miswires a test.
			apiutil.WriteError(w, http.StatusUnauthorized,
				"missing_claims", "auth middleware did not run")
			span.SetStatus(codes.Error, "missing claims")
			return
		}

		id := chi.URLParam(r, "id")
		if id == "" {
			apiutil.WriteError(w, http.StatusBadRequest,
				"missing_id", "reservation id is required")
			span.SetStatus(codes.Error, "missing id")
			return
		}

		row, err := db.GetReservation(ctx, pool, id)
		if err != nil {
			if db.IsNoRows(err) {
				apiutil.WriteError(w, http.StatusNotFound,
					"reservation_not_found", "no reservation with that id")
				span.SetStatus(codes.Error, "not found")
				return
			}
			slog.ErrorContext(ctx, "get reservation failed", "error", err)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "could not load the reservation")
			span.SetStatus(codes.Error, "db error")
			return
		}

		// The reservation must belong to the caller. 403 not 404 —
		// the UUIDs are unguessable, so 403 leaks nothing, and the
		// client can tell "not yours" from "doesn't exist".
		if row.UserID != claims.Subject {
			apiutil.WriteError(w, http.StatusForbidden,
				"not_your_reservation", "this reservation belongs to another user")
			span.SetStatus(codes.Error, "not your reservation")
			return
		}

		// Only a held reservation can be paid for. Each terminal-ish
		// state gets its own error, because they are different facts
		// and the client renders them differently.
		switch row.Status {
		case db.StatusPendingPayment:
			// The one state that can be paid.
		case db.StatusConfirmed:
			apiutil.WriteError(w, http.StatusConflict,
				"already_paid", "this reservation is already paid for")
			span.SetAttributes(attribute.String("pay.outcome", "already_paid"))
			return
		case db.StatusExpired:
			apiutil.WriteError(w, http.StatusConflict,
				"already_expired", "the hold expired before the payment")
			span.SetAttributes(attribute.String("pay.outcome", "already_expired"))
			return
		case db.StatusCancelled:
			apiutil.WriteError(w, http.StatusConflict,
				"already_cancelled", "this reservation was cancelled")
			span.SetAttributes(attribute.String("pay.outcome", "already_cancelled"))
			return
		case db.StatusRefunded:
			apiutil.WriteError(w, http.StatusConflict,
				"already_refunded", "this reservation was refunded")
			span.SetAttributes(attribute.String("pay.outcome", "already_refunded"))
			return
		default:
			apiutil.WriteError(w, http.StatusConflict,
				"invalid_status", "this reservation cannot be paid for")
			span.SetAttributes(attribute.String("pay.outcome", "invalid_status"))
			return
		}

		// ── The amount ──
		//
		// From the reservation's frozen price snapshot, never from the
		// request. unit_price_cents × seats — both frozen at hold
		// time, so the total is exactly what the customer was
		// quoted, whatever the event's price does later.
		amountCents := row.UnitPriceCents * int64(row.Seats)
		if amountCents <= 0 {
			// An event with no price cannot be charged (and Stripe
			// rejects amounts below $0.50 anyway). This is a
			// configuration problem, not a client error — but it IS
			// the client's request that surfaced it, so 409 with a
			// clear code beats a 500 from the DB CHECK constraint.
			apiutil.WriteError(w, http.StatusConflict,
				"event_not_priced", "this event has no price set")
			span.SetAttributes(attribute.String("pay.outcome", "event_not_priced"))
			return
		}

		// ── Step 4: the payment row, BEFORE the Stripe call ──
		//
		// ON CONFLICT DO NOTHING on reservation_id: a replay (or a
		// concurrent second /pay) does not create a second row, so it
		// cannot create a second PaymentIntent.
		created, err := db.InsertPaymentIntent(ctx, pool,
			row.ReservationID, row.UserID, amountCents, row.Currency)
		if err != nil {
			slog.ErrorContext(ctx, "insert payment intent failed", "error", err)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "could not record the payment")
			span.SetStatus(codes.Error, "db error")
			return
		}
		if !created {
			slog.InfoContext(ctx, "payment row already existed (replay)",
				"reservation_id", row.ReservationID)
		}

		// ── Step 5: create the PaymentIntent at Stripe ──
		//
		// The idempotency key is the reservation id, so a retried
		// call returns the SAME PaymentIntent rather than charging
		// twice.
		pi, err := payments.CreatePaymentIntent(ctx, amountCents, row.Currency,
			row.ReservationID, row.ReservationID)
		if err != nil {
			// 502: Stripe is a third party, and the request may well
			// be fine. The client should retry — and because the
			// idempotency key is stable, a retry is safe.
			slog.ErrorContext(ctx, "stripe create payment intent failed", "error", err)
			apiutil.WriteError(w, http.StatusBadGateway,
				"payment_provider_error", "the payment provider rejected the request")
			span.SetStatus(codes.Error, "stripe error")
			return
		}

		// ── Step 6: record Stripe's id ──
		if err := db.SetStripePaymentIntentID(ctx, pool,
			row.ReservationID, pi.ID, pi.Status); err != nil {
			slog.ErrorContext(ctx, "record stripe payment intent id failed", "error", err)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "could not record the payment")
			span.SetStatus(codes.Error, "db error")
			return
		}

		span.SetAttributes(
			attribute.String("pay.outcome", "payment_intent_created"),
			attribute.String("payment.intent_id", pi.ID),
			attribute.Int64("payment.amount_cents", amountCents),
			attribute.String("payment.currency", row.Currency),
		)

		// ── Step 7: the client_secret ──
		//
		// The client_secret authorises exactly this one payment, so
		// it goes to the client and nowhere else — it is never
		// logged, never stored, and never included in an error
		// response.
		apiutil.WriteJSON(w, http.StatusOK, map[string]any{
			"payment_intent_id": pi.ID,
			"client_secret":     pi.ClientSecret,
			"amount_cents":      amountCents,
			"currency":          row.Currency,
			"seats":             row.Seats,
			// The same vocabulary as the reservation statuses and the
			// database — a client should not have to learn that one
			// status field is lowercase and another is uppercase.
			"status": db.NormalizePaymentStatus(pi.Status),
		})
	}
}
