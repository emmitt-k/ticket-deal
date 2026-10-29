-- release_seat.lua — put a held seat back on sale, atomically.
--
-- KEYS[1] = user_hold_key        e.g. "hold:event:42:user:abc123"
-- KEYS[2] = seat_inventory_key   e.g. "inventory:event:42"
-- ARGV[1] = seats                 e.g. "2"
--
-- Return shape: { status_code, reason_string }
--   status_code =  1 → released (the hold existed; the seat is back on sale)
--   status_code =  0 → not_held (nothing to release)
--   status_code = -1 → no_inventory (the event was never seeded)
--
-- WHY A SCRIPT AND NOT TWO COMMANDS:
-- Releasing a seat is INCRBY the inventory + DEL the hold key. Done as
-- two round-trips, a second caller can INCRBY between them, and the
-- seat goes back on sale TWICE — a phantom seat, which is an
-- oversell. The script makes the "is it still held?" check and the
-- release ONE atomic operation, so exactly one caller can ever
-- release a given hold.
--
-- WHY THE HOLD KEY IS THE GUARD:
-- A hold carries a TTL, and when it expires the expiration-watcher
-- puts the seat back on sale. So a MISSING hold key means "this seat
-- is already back on sale" — and releasing it again would oversell.
-- The script refuses instead of double-releasing.
--
-- (The user-facing cancel path is safe without this, because its
-- compare-and-set on the reservation status arbitrates between the
-- cancel and the expiry. The admin release-seat path has no status
-- change, so nothing else arbitrates — this guard is what makes it
-- safe.)

if redis.call('EXISTS', KEYS[2]) == 0 then
    return {-1, 'no_inventory'}      -- the event was never seeded
end

if redis.call('EXISTS', KEYS[1]) == 0 then
    return {0, 'not_held'}            -- already released — do NOT double-count
end

redis.call('INCRBY', KEYS[2], tonumber(ARGV[1]))   -- the seat is sellable again
redis.call('DEL', KEYS[1])                          -- and the hold is gone

return {1, 'released'}
