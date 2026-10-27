-- 006_refunded_status.sql — give refunds their own status.
--
-- Until now, a refund was modelled as CONFIRMED → CANCELLED ("refund =
-- cancel in our model"). That collapsed two different facts into one
-- status:
--
--   CANCELLED   "the user bailed before paying"     → seat back on sale
--   a refund    "the user paid, we gave the money back" → seat back on sale
--
-- The audit log could already tell them apart (a REFUND_INITIATED event
-- vs a CANCELLED event), but the reservations row could not. So
-- `status = 'CANCELLED'` conflated "we lost the sale" with "we refunded
-- the customer" — and the user-facing `?status=CANCELLED` filter did
-- the same thing.
--
-- REFUNDED gives the second case its own status, and the state
-- machine's edge changes with it:
--
--   CONFIRMED → CANCELLED   (was)   →   CONFIRMED → REFUNDED
--
-- CANCELLED now has exactly one meaning: "cancelled before payment".
-- A reservation that was paid and then reversed is REFUNDED, and the
-- audit log's REFUND_INITIATED event says who did it and why.
--
-- This is a CHECK-constraint change only — no rows are rewritten. No
-- reservation is currently REFUNDED, because nothing can reach
-- CONFIRMED yet: that's Layer 2, item 7 (Stripe).
--
-- For later: a PARTIAL refund is not a status. It leaves the status at
-- CONFIRMED and records a REFUND_INITIATED audit event. Only a full
-- refund becomes REFUNDED. (A Charge can be refunded several times and
-- partially — see docs/comprehensive-stripe.md §33 — so "is this
-- refunded?" is not a boolean, which is exactly why the event and the
-- status are separate concepts.)

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'reservations_status_check'
          AND pg_get_constraintdef(oid) NOT LIKE '%REFUNDED%'
    ) THEN
        ALTER TABLE reservations DROP CONSTRAINT reservations_status_check;
        ALTER TABLE reservations ADD CONSTRAINT reservations_status_check
            CHECK (status IN (
                'PENDING_PAYMENT', 'CONFIRMED', 'EXPIRED', 'CANCELLED', 'REFUNDED'
            ));
    END IF;
END;
$$;
