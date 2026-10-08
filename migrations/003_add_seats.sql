-- 002_add_seats.sql — add seats column to reservations
--
-- Phase 5's reserve handler publishes Seats in the JSON payload and
-- Phase 6's worker accepts the field but never persists it. Phase 7
-- needs Seats back out of the DB to size the inventory-restoring
-- INCRBY correctly (multi-seat group purchases must return all the
-- seats they held, not just 1).
--
-- Default 1 keeps existing rows valid without a backfill; the wire
-- payload has always carried Seats so no data is lost on upgrade.
--
-- CHECK constraint matches the Lua reserve script's limit (≤ 10 seats
-- per call — see docs/architecture.md §4).
ALTER TABLE reservations
    ADD COLUMN seats INTEGER NOT NULL DEFAULT 1
        CHECK (seats >= 1 AND seats <= 10);