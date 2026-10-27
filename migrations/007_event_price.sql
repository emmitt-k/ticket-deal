-- 007_event_price.sql — give the seats a price.
--
-- Until now a seat had no price at all. The `events` table had no money
-- column and `reservations` had no amount, so "how much is this seat?"
-- had no answer anywhere in the system. Layer 2 item 7 (Stripe) needs
-- an amount — and Stripe rejects amounts below $0.50 — so the price
-- could not stay an implicit assumption.
--
-- TWO new columns on `events` — the price of ONE seat at that event:
--
--   price_cents   the per-seat price, in the currency's minor unit
--   currency      the 3-letter ISO code that price is in
--
-- And TWO on `reservations` — the SNAPSHOT of that price, frozen at
-- the moment the seat was held:
--
--   unit_price_cents   the per-seat price at hold time
--   currency           the currency at hold time
--
-- Why the snapshot, and not a live lookup at pay time: a price change
-- must never retroactively alter a seat that is already held or
-- already paid. If the organizer raises the price mid-drop, the seats
-- already held keep the price they were quoted. Postgres stays the
-- single source of truth for the price; the reservation row is a
-- frozen copy of it, written in the same statement that created the
-- row.
--
-- The total the customer owes is `unit_price_cents * seats`. Both
-- numbers are frozen at the same instant, so the total is exact — no
-- recomputation, no rounding, no second source of truth.
--
-- Cents, not a float: money in binary floating point is a bug waiting
-- for the wrong invoice. Minor units (cents, satang) are integers.
--
-- Currency is a column, not a hardcoded 'usd': Stripe requires an
-- explicit currency, and money without a currency is an assumption
-- that quietly becomes a fact in every downstream number.

ALTER TABLE events
    ADD COLUMN IF NOT EXISTS price_cents BIGINT NOT NULL DEFAULT 0
        CHECK (price_cents >= 0);

ALTER TABLE events
    ADD COLUMN IF NOT EXISTS currency TEXT NOT NULL DEFAULT 'usd'
        CHECK (currency ~ '^[a-z]{3}$');

ALTER TABLE reservations
    ADD COLUMN IF NOT EXISTS unit_price_cents BIGINT NOT NULL DEFAULT 0
        CHECK (unit_price_cents >= 0);

ALTER TABLE reservations
    ADD COLUMN IF NOT EXISTS currency TEXT NOT NULL DEFAULT 'usd'
        CHECK (currency ~ '^[a-z]{3}$');

-- The dev seed event needs a price.
--
-- migrations/002_seed.sql runs BEFORE this file (the Makefile applies
-- migrations/0*.sql in filename order), and 002 predates the price
-- column — so on a fresh database the seed event is inserted without
-- one and lands on the DEFAULT 0. On an existing database the seed is
-- a no-op (ON CONFLICT DO NOTHING), so it is still at 0.
--
-- A $0.00 event is a dead end for Stripe (amounts below $0.50 are
-- rejected), so give the demo event a demo price. Only rows still at
-- the default are touched — an event that was deliberately priced
-- keeps its price.
--
-- This mirrors what 002_seed.sql itself does: dev fixture data, with
-- the dev fixture's own id.
UPDATE events
   SET price_cents = 2500,
       currency     = 'usd'
 WHERE id = 1
   AND price_cents = 0;
