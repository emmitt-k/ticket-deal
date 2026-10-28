-- 009_admin_audit.sql — one more audit event type: SEAT_RELEASED.
--
-- Layer 2, item 9 (the admin/ops API) needs an event for the ONE
-- admin action that does not change a reservation's status: releasing
-- a seat back onto sale. The status stays PENDING_PAYMENT, so there is
-- no edge to derive an event type from — but the action is exactly the
-- kind of thing an operator must be able to answer for later ("who
-- put that seat back on sale, and when?").
--
-- So the event type is added explicitly, the same way the schema
-- already anticipated 'admin' as an actor_type.
--
-- It is an EVENT and not a STATUS, in the same sense the partial refund
-- is: the audit row records what was DONE (a seat went back on sale),
-- and the reservation's status records where it ended up (still
-- PENDING_PAYMENT). old_status == new_status on that row.
--
-- The constraint is dropped and re-added with the wider list. The
-- append-only trigger (migrations/005_audit_log.sql) is untouched: it
-- guards the COLUMNS, not the CHECK constraint.

ALTER TABLE reservation_events
    DROP CONSTRAINT IF EXISTS reservation_events_event_type_check;

ALTER TABLE reservation_events
    ADD CONSTRAINT reservation_events_event_type_check
    CHECK (event_type IN (
        'CREATED',
        'CONFIRMED',
        'EXPIRED',
        'CANCELLED',
        'REFUND_INITIATED',
        'SEAT_RELEASED'
    ));
