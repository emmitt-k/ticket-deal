-- 010_event_id_uuid.sql — the event id becomes a UUID.
--
-- `events.id` was a BIGINT running counter, which means two things go
-- wrong at once:
--
--   1. It is GUESSABLE. A customer can walk id=1, 2, 3… and
--      enumerate the whole catalogue — and with it the name, the
--      price and the inventory of every drop the organizer has.
--   2. It is allocated BY HAND. Every id in the system had to be
--      invented by whatever wrote the row, so an event could not be
--      created without someone deciding its id first.
--
-- The id becomes a UUID, minted by the APPLICATION — exactly like
-- `reservations.reservation_id` already is. The column therefore has
-- NO DEFAULT: the app always supplies the value. One convention for
-- one idea; two conventions for the same thing is worse than one.
--
-- In Go the id is a plain `string`, like `reservation_id` already is.
-- Not a `uuid.UUID` type — one representation, consistent with the
-- rest of the project.
--
-- ── Why this is a NEW migration and not an edit of 001_init.sql ──────
--
-- Migrations are a ledger, not a document to be rewritten. 001 created
-- the BIGINT column and every migration after it was written against
-- it. Editing history would make a fresh database and an upgraded
-- database disagree — and the fresh one would never see the shape the
-- upgraded one has.
--
-- ── The mapping ────────────────────────────────────────────────────────
--
-- Existing rows keep their identity, but the BIGINT id has to become
-- a UUID. The mapping is DETERMINISTIC:
--
--     new_uuid = md5(old_id::text || ':ticket-deal:event')::uuid
--
-- Deterministic matters for two reasons:
--
--   - Re-running a half-finished migration must not produce a
--     DIFFERENT mapping. A random uuid would silently re-point every
--     reservation at a different event on the second run.
--   - The mapping has to be reproducible by hand, from the old id,
--     without reading a table. md5 gives you that: id=1 is always
--     136782c2-5228-69d0-6dd5-26f1101a0d0c.
--
-- The uuid version nibble is not meaningful here — md5 is not a
-- version-4 generator. Postgres accepts any 128-bit value as a uuid,
-- and the application only ever compares these ids for equality, so
-- the version nibble buys nothing.
--
-- ── The order of operations ───────────────────────────────────────────
--
-- Swap the column, not the type in place. A `ALTER COLUMN ... TYPE
-- uuid USING ...` would rewrite the table AND would need a cast
-- expression anyway; adding a column, backfilling, and swapping keeps
-- every step individually checkable and lets us abort on an orphan
-- before anything destructive has happened.
--
-- The whole thing is ONE transaction. If any step fails, nothing
-- changes — the database is either fully old or fully new.

BEGIN;

-- ── 1. The new UUID id on events ────────────────────────────────────
-- Added without a DEFAULT on purpose: the application mints the id.
--
-- IF NOT EXISTS because migrations/001_init.sql already declares
-- events.id as UUID — a fresh database gets the final shape directly,
-- and this migration then has nothing to swap. Without the guard, the
-- ADD COLUMN would fail on every fresh database.
ALTER TABLE events
    ADD COLUMN IF NOT EXISTS id_uuid UUID;

-- ── 2. Map every event onto a UUID ──────────────────────────────────
-- Deterministic (see the header). Deterministic beats random here
-- because a re-run of a partially-applied migration must land on the
-- same mapping.
--
-- The CASE handles the two shapes this column can have:
--
--   - BIGINT (an existing database, upgraded from the old schema):
--     the id is derived from the number.
--   - UUID (a fresh database, where 001 already created it as UUID):
--     the id IS its own uuid — copying it across keeps the two
--     databases identical for the same logical event.
--
-- The branch is on pg_typeof, not on a regex over the text, because
-- Postgres resolves a cast at PARSE time, not per row: a CASE that
-- contains `id::uuid` is rejected outright on a bigint column, even
-- when that branch would never be taken. So the two shapes are
-- handled by two separate statements, each of which only mentions
-- the cast its own shape supports.
--
-- First: the fresh-database case. This statement only parses when
-- the column really is a uuid — but Postgres parses the whole
-- migration before running it, so it cannot be conditional on the
-- column's type.
--
-- The way out is to let the SERVER decide, with a PL/pgSQL block:
-- inside a DO block, each statement is parsed when it executes, so
-- the type check happens at the right moment.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
         WHERE table_name = 'events'
           AND column_name = 'id'
           AND data_type = 'uuid'
    ) THEN
        -- Fresh database: the id is already its own uuid.
        UPDATE events
           SET id_uuid = id
         WHERE id_uuid IS NULL;
    ELSE
        -- Existing database: derive a uuid from the bigint id.
        UPDATE events
           SET id_uuid = md5(id::text || ':ticket-deal:event')::uuid
         WHERE id_uuid IS NULL;
    END IF;
END $$;

-- ── 3. Carry the reservations across ────────────────────────────────
-- The event_id on a reservation is the SAME fact as the event's id, so
-- it is derived from the mapping rather than computed independently —
-- that way the two can never disagree.
ALTER TABLE reservations
    ADD COLUMN IF NOT EXISTS event_id_uuid UUID;

UPDATE reservations r
   SET event_id_uuid = e.id_uuid
  FROM events e
 WHERE e.id::text = r.event_id::text
   AND r.event_id_uuid IS NULL;

-- ── 4. Refuse to continue if anything is unmapped ───────────────────
-- An orphan reservation would lose its event link on the swap, and
-- would then fail the re-added FOREIGN KEY. Better to abort the whole
-- migration with a loud error than to drop the link quietly.
--
-- This is the same instinct as Convention #13: the guard has to be
-- in the statement that does the damage, not in a helper that might
-- be skipped.
DO $$
DECLARE
    orphans BIGINT;
BEGIN
    SELECT count(*) INTO orphans
      FROM reservations
     WHERE event_id_uuid IS NULL;

    IF orphans > 0 THEN
        RAISE EXCEPTION
            'aborting 010_event_id_uuid: % reservation(s) could not be mapped to an event',
            orphans;
    END IF;
END $$;

-- ── 5. Swap the columns ─────────────────────────────────────────────
-- The old FK goes first — it points at the column being dropped.
-- NOT NULL is re-applied to the new column because the old one had it
-- and the constraint must survive the swap (a nullable event_id would
-- let a reservation point at no event at all).
ALTER TABLE reservations
    DROP CONSTRAINT IF EXISTS reservations_event_id_fkey;

ALTER TABLE reservations
    ALTER COLUMN event_id_uuid SET NOT NULL;

ALTER TABLE reservations
    DROP COLUMN event_id;

ALTER TABLE reservations
    RENAME COLUMN event_id_uuid TO event_id;

ALTER TABLE events
    DROP COLUMN id;

ALTER TABLE events
    RENAME COLUMN id_uuid TO id;

-- ── 6. Re-establish the keys ────────────────────────────────────────
-- The primary key and the foreign key are dropped by the column swap
-- (a PK is dropped with its column; the FK was dropped explicitly
-- above). Both are re-created against the UUID columns.
ALTER TABLE events
    ADD CONSTRAINT events_pkey PRIMARY KEY (id);

ALTER TABLE reservations
    ADD CONSTRAINT reservations_event_id_fkey
    FOREIGN KEY (event_id) REFERENCES events(id);

-- The lookup index that the admin API and the ops queries use
-- (reservations by event) was rebuilt by the column swap. Postgres
-- drops and recreates indexes when the indexed column is dropped, so
-- re-create it under the original name so nothing else has to change.
CREATE INDEX IF NOT EXISTS idx_reservations_event_status
    ON reservations(event_id, status);

COMMIT;
