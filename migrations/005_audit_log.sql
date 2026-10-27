-- 005_audit_log.sql — Layer 2, item 6: the append-only reservation event log.
--
-- Every state change a reservation goes through gets a row here:
--
--   CREATED          the worker inserted the reservation row
--   CONFIRMED        a payment succeeded (Stripe webhook — Layer 2, item 7)
--   EXPIRED          the hold timed out (watcher or sweep)
--   CANCELLED        the user cancelled
--   REFUND_INITIATED a confirmed reservation was refunded (also a
--                    CONFIRMED → CANCELLED transition, but a different
--                    *reason* — so it gets its own event type)
--
-- The point of this table is to answer the SOC2 / support question:
-- "who did what, when, and can I prove it?" Today the reservations row
-- only holds the CURRENT status — the history of how it got there is
-- gone the moment a second transition overwrites it.
--
-- ── Why user_id is denormalised onto this table ──────────────────────────
-- The reservations row already carries user_id. Repeating it here is
-- deliberate:
--   - Layer 3, item 14 (GDPR) has to anonymise PII across the whole
--     system. It plans to run `UPDATE reservation_events SET user_id=...`
--     — which needs a user_id column on THIS table.
--   - "every event for user X" is a query ops will actually run, and it
--     shouldn't need a join back to reservations (whose rows may since
--     have been deleted).
--
-- ── Why trace_id is here ────────────────────────────────────────────────
-- It is the join key between this table and the Grafana/Loki/Jaeger
-- stack. An ops person holding a trace_id can pull up every log line
-- and span for the exact transition; an ops person holding an event_id
-- can pull up the trace_id that produced it. Both directions work
-- because both keys are stored.
--
-- ── Append-only enforcement ─────────────────────────────────────────────
-- The trigger at the bottom seals the table:
--   - DELETE is refused outright.
--   - UPDATE is refused EXCEPT for rewriting user_id, which is the
--     GDPR anonymisation path (Layer 3, item 14). Every other column
--     is immutable.
--
-- The one escape hatch is a session GUC, `ticket_deal.audit_purge = 'on'`.
-- It exists because "sealed forever" is not a real requirement — GDPR
-- erasure, retention policies, and test fixtures all need a way in.
-- The default is sealed; the override is explicit, greppable, and
-- transaction-scoped via SET LOCAL (so it can't leak between pooled
-- connections).

CREATE TABLE reservation_events (
    event_id        BIGSERIAL PRIMARY KEY,
    reservation_id  UUID NOT NULL REFERENCES reservations(reservation_id),
    user_id         TEXT NOT NULL,
    event_type      TEXT NOT NULL
                    CHECK (event_type IN (
                        'CREATED', 'CONFIRMED', 'EXPIRED', 'CANCELLED', 'REFUND_INITIATED'
                    )),
    actor_type      TEXT NOT NULL
                    CHECK (actor_type IN (
                        'user', 'system', 'admin', 'stripe_webhook'
                    )),
    actor_id        TEXT,
    old_status      TEXT,
    new_status      TEXT,
    trace_id        TEXT,
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- "Show me the history of this reservation" — the primary read pattern.
-- (reservation_id, event_id) because event_id is the insertion order, so
-- the index alone satisfies the ORDER BY.
CREATE INDEX idx_reservation_events_reservation
    ON reservation_events(reservation_id, event_id);

-- "Show me everything user X did" — Layer 3's GDPR sweep needs to find
-- a user's rows by user_id alone.
CREATE INDEX idx_reservation_events_user
    ON reservation_events(user_id, event_id);

-- "Find every transition that happened inside trace Y" — the join into
-- Jaeger/Loki. Partial, because most rows have no trace (background
-- sweeps with no span), and a full index over mostly-NULLs is waste.
CREATE INDEX idx_reservation_events_trace
    ON reservation_events(trace_id)
    WHERE trace_id IS NOT NULL;

-- ── The append-only guard ───────────────────────────────────────────────
CREATE OR REPLACE FUNCTION reservation_events_append_only() RETURNS trigger AS $$
DECLARE
    purge_allowed boolean :=
        COALESCE(current_setting('ticket_deal.audit_purge', true), 'off') = 'on';
BEGIN
    IF purge_allowed THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION
            'reservation_events is append-only: DELETE is not allowed (set ticket_deal.audit_purge=on to override)'
            USING ERRCODE = 'restrict_violation';
    END IF;

    -- UPDATE is allowed ONLY for GDPR anonymisation, which rewrites
    -- user_id to 'redacted:<hash>'. If any other column differs from
    -- OLD, somebody is rewriting history — refuse it.
    IF NEW.event_id        IS DISTINCT FROM OLD.event_id
       OR NEW.reservation_id IS DISTINCT FROM OLD.reservation_id
       OR NEW.event_type    IS DISTINCT FROM OLD.event_type
       OR NEW.actor_type    IS DISTINCT FROM OLD.actor_type
       OR NEW.actor_id      IS DISTINCT FROM OLD.actor_id
       OR NEW.old_status    IS DISTINCT FROM OLD.old_status
       OR NEW.new_status    IS DISTINCT FROM OLD.new_status
       OR NEW.trace_id      IS DISTINCT FROM OLD.trace_id
       OR NEW.metadata      IS DISTINCT FROM OLD.metadata
       OR NEW.created_at    IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION
            'reservation_events is append-only: only user_id may be updated (GDPR anonymisation)'
            USING ERRCODE = 'restrict_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER reservation_events_append_only_trg
    BEFORE UPDATE OR DELETE ON reservation_events
    FOR EACH ROW EXECUTE FUNCTION reservation_events_append_only();
