-- ───── Layer 1 item 2: index for "my reservations" listing ─────
--
-- A user looking up their own tickets (GET /api/me/reservations) hits a
-- WHERE user_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ? path.
-- Without an index, that's a full table scan + sort — fine for 100k
-- rows, ugly at 10M. Adding it now while the table is still small is
-- free; adding it later means a CONCURRENTLY online rebuild.
--
-- Composite (user_id, created_at DESC) so the index ordering matches
-- the ORDER BY exactly — Postgres can satisfy both filter and sort
-- from the index in one pass, no sort node in the plan.
--
-- IF NOT EXISTS so this migration is safe to re-apply if a partial
-- run left it half-done. This isn't a unique index, so no FK
-- implications.
CREATE INDEX IF NOT EXISTS idx_reservations_user_created
    ON reservations(user_id, created_at DESC);
