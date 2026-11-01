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

-- ── The default price for a NEW event ─────────────────────────────────
--
-- There used to be a dev seed event here (migrations/002_seed.sql), and
-- this file gave it a demo price so the Stripe path had something
-- non-zero to work with. The seed is gone — events are created through
-- the admin API (Layer 2, item 9b) — so there is no longer a row to
-- UPDATE here.
--
-- Instead the DEFAULT on the column becomes a real, usable price: a
-- $0.00 event is a dead end for Stripe (amounts below $0.50 are
-- rejected), so an event that is created without an explicit price
-- gets a $25.00 one and can be sold immediately. An organizer who
-- wants a different price sets it explicitly at creation time.
--
-- The default lives on the COLUMN, not in a data-fixing UPDATE, so it
-- applies to every event created from now on — including the ones a
-- future CRUD API creates — without this migration having to know
-- about them.
--
-- The column DEFAULT is changed here rather than in 010 because this
-- is the migration that introduced the concept of a price; 010 only
-- changes the shape of the id.
ALTER TABLE events
    ALTER COLUMN price_cents SET DEFAULT 2500;

