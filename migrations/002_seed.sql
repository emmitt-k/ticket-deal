-- 002_seed.sql — dev data
-- Idempotent: ON CONFLICT lets this file be re-run without errors.
-- The event id (1) is the default target for k6 loadtest in Phase 8.

INSERT INTO events (id, name, initial_inventory) VALUES
    (1, 'Dev Test Event: Ticketmaster-style Drop', 100)
ON CONFLICT (id) DO NOTHING;
