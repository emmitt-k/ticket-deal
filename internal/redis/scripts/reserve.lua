-- KEYS[1] = seat_inventory_key   e.g. "inventory:event:42"
-- KEYS[2] = user_hold_key        e.g. "hold:event:42:user:abc123"
-- ARGV[1] = hold_ttl_seconds     e.g. "600" (10 minutes)
-- ARGV[2] = seats_requested      e.g. "1"
--
-- Return shape: { status_code, reason_string }
--   status_code =  1 → reserved (and the seat was decremented, hold key set)
--   status_code =  0 → rejected (see reason_string: "sold_out" or "already_holding")
--   status_code = -1 → rejected (event_not_found: inventory key doesn't exist)
--
-- Verbatim from docs/architecture.md §4 "The Lua reservation script".
-- The script is atomic by Redis contract (single-threaded execution),
-- so 1000 concurrent calls produce exactly the right number of reservations.

local available = tonumber(redis.call('GET', KEYS[1]))

if available == nil then
    return {-1, 'event_not_found'}    -- event was never seeded
end

local requested = tonumber(ARGV[2])
if available < requested then
    return {0, 'sold_out'}            -- not enough seats
end

-- Guard: same user can't double-book within their hold TTL
local existing_hold = redis.call('GET', KEYS[2])
if existing_hold then
    return {0, 'already_holding'}
end

redis.call('DECRBY', KEYS[1], requested)              -- commit the decrement
redis.call('SET', KEYS[2], ARGV[2], 'EX', ARGV[1])   -- create the hold with TTL

return {1, 'reserved'}
