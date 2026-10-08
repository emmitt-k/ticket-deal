-- 001_init.sql — initial schema
-- Creates the two tables every phase needs:
--   events         — what we're selling tickets for
--   reservations   — the long-lived row that the worker INSERTs on SQS success
--
-- The reservation_id UUID PRIMARY KEY is the idempotency key:
-- when SQS redelivers a message (worker crash, network hiccup), the second
-- INSERT hits ON CONFLICT DO NOTHING and the row count stays exactly correct.
-- Zero overselling is enforced at the worker write path, not here.

-- ───── events ────────────────────────────────────────────────────────────
-- One row per concert/show/screening. id is BIGINT (manual, not serial),
-- because event creation is a separate workflow from reservation workflow.
CREATE TABLE events (
    id                BIGINT PRIMARY KEY,
    name              TEXT NOT NULL,
    initial_inventory INTEGER NOT NULL CHECK (initial_inventory >= 0),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ───── reservations ──────────────────────────────────────────────────────
-- The row that proves someone bought a ticket. Created by the worker AFTER
-- Redis has already sold them a seat. So this table can lag Redis by a few
-- hundred ms — that's OK; Redis is the inventory source of truth at sell-time.
CREATE TABLE reservations (
    reservation_id    UUID PRIMARY KEY,                       -- idempotency key
    user_id           TEXT NOT NULL,
    event_id          BIGINT NOT NULL REFERENCES events(id),
    status            TEXT NOT NULL
                      CHECK (status IN ('PENDING_PAYMENT', 'CONFIRMED', 'EXPIRED', 'CANCELLED')),
    expires_at        TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Lookups by (event_id, status) — "how many PENDING_PAYMENT does this event have?"
-- and by (event_id, status='CONFIRMED') for revenue dashboards.
CREATE INDEX idx_reservations_event_status
    ON reservations(event_id, status);

-- Partial index that powers the expiry sweep job:
--   UPDATE reservations SET status='EXPIRED'
--   WHERE status='PENDING_PAYMENT' AND expires_at < NOW();
-- Only PENDING rows are indexed (typically <5% of total) so the index is tiny.
CREATE INDEX idx_reservations_pending_expiry
    ON reservations(expires_at)
    WHERE status = 'PENDING_PAYMENT';
