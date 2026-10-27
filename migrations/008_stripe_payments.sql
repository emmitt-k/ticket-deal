-- 008_stripe_payments.sql — the payment record.
--
-- One row per reservation that has been sent to Stripe. The table
-- exists so that a reservation and its payment are joined by a
-- database constraint, not by application discipline:
--
--   reservation_id  UNIQUE  → one reservation, one PaymentIntent
--   stripe_payment_intent_id UNIQUE → one PaymentIntent, one row
--
-- WHY the reservation_id is UNIQUE: it makes POST /pay safely
-- replayable. Two concurrent /pay calls for the same reservation
-- cannot create two PaymentIntents, because the second INSERT hits
-- the unique constraint. That is a database-level guarantee, not
-- something the handler has to remember.
--
-- WHY stripe_payment_intent_id is NULLABLE: the row is written
-- BEFORE Stripe's Create call returns, so that the intent to pay is
-- durable before we call a third party. In the window between the
-- INSERT and the UPDATE, the id genuinely does not exist yet —
-- nullable is the honest representation of that intermediate state.
--
-- The crash window this creates is closed by an idempotency key: the
-- /pay handler passes the reservation_id as Stripe's own
-- Idempotency-Key, so a retried Create returns the SAME
-- PaymentIntent instead of creating a second one. Without that,
-- "the row exists but the id does not" would be a double-charge.
--
-- amount_cents is the TOTAL for the reservation (unit price × seats),
-- copied from the reservation's own price snapshot. It is stored
-- rather than re-derived so that "what did we ask Stripe for" is
-- answerable after the fact, even if the event's price changes later.
--
-- status tracks STRIPE's view of the PaymentIntent (uppercased), plus
-- one state of our own: CREATED means "the row exists, Stripe's
-- Create has not returned yet". The reservation's own status remains
-- the business truth — this column is the payment provider's view of
-- the same thing.

CREATE TABLE IF NOT EXISTS payment_intents (
    id                       BIGSERIAL PRIMARY KEY,
    reservation_id           UUID NOT NULL UNIQUE
                             REFERENCES reservations(reservation_id),
    user_id                  TEXT NOT NULL,
    stripe_payment_intent_id TEXT UNIQUE,
    amount_cents             BIGINT NOT NULL CHECK (amount_cents > 0),
    currency                 TEXT NOT NULL CHECK (currency ~ '^[a-z]{3}$'),
    status                   TEXT NOT NULL DEFAULT 'CREATED'
                             CHECK (status IN (
                                 'CREATED',
                                 'REQUIRES_PAYMENT_METHOD',
                                 'REQUIRES_CONFIRMATION',
                                 'REQUIRES_ACTION',
                                 'REQUIRES_CAPTURE',
                                 'PROCESSING',
                                 'SUCCEEDED',
                                 'CANCELED'
                             )),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The webhook looks a payment up by Stripe's id on every event. The
-- UNIQUE constraint on stripe_payment_intent_id already provides that
-- index — a second index on the same column would be pure duplication,
-- so this file adds none.
