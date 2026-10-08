-- Token bucket with overflow queue. Atomic: either admit one user or queue them.
-- Used by /api/tickets/enter (Phase 4) to throttle admission to the hot path.
--
-- KEYS[1] = bucket state key    e.g. "bucket:event:42"
-- KEYS[2] = queue key           e.g. "queue:event:42"
-- ARGV[1] = capacity            e.g. "100"  (max tokens)
-- ARGV[2] = refill_rate         e.g. "10"   (tokens per second)
-- ARGV[3] = now_ms              current time in milliseconds (passed in by caller
--                              for testability — never use TIME inside Lua)
-- ARGV[4] = user_id             e.g. "u1"   (queue member if not admitted)
-- ARGV[5] = queue_ttl_seconds   e.g. "300"  (auto-cleanup of stale queue entries)
--
-- Return shape: { status_code, payload_string }
--   status_code = 1 → "admitted"   (a token was consumed)
--   status_code = 0 → "<position>"  (1-based queue position, queued)
--
-- Notes:
--   - Bucket state is stored as Redis Hash {tokens, last_refill_ms}.
--   - On first call (no state), bucket starts FULL and last_refill = now.
--   - Tokens refill lazily — only when TryAdmit runs.
--   - Queue auto-trims entries older than queue_ttl_seconds (prevents
--     unbounded growth from abandoned sessions).

local capacity       = tonumber(ARGV[1])
local refill_rate    = tonumber(ARGV[2])
local now_ms         = tonumber(ARGV[3])
local user_id        = ARGV[4]
local queue_ttl_secs = tonumber(ARGV[5])

-- 1) Refill bucket based on elapsed time since last refill
local bucket = redis.call('HMGET', KEYS[1], 'tokens', 'last_refill_ms')
local tokens = tonumber(bucket[1])
local last_refill_ms = tonumber(bucket[2])

if tokens == nil then
  -- First caller for this bucket: start full + last_refill = now
  tokens = capacity
  last_refill_ms = now_ms
else
  local elapsed_secs = math.max(0, (now_ms - last_refill_ms) / 1000)
  tokens = math.min(capacity, tokens + elapsed_secs * refill_rate)
end

-- 2) Try to admit
if tokens >= 1 then
  tokens = tokens - 1
  redis.call('HMSET', KEYS[1], 'tokens', tokens, 'last_refill_ms', now_ms)
  redis.call('EXPIRE', KEYS[1], 86400)   -- 1-day TTL on bucket state (safety net)
  return {1, 'admitted'}
end

-- 3) Queue: clean stale entries first, then add this user
local cutoff_ms = now_ms - (queue_ttl_secs * 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', cutoff_ms)

redis.call('ZADD', KEYS[2], now_ms, user_id)
redis.call('EXPIRE', KEYS[2], queue_ttl_secs)

-- Position is 1-based for human-friendly "you're 5th in line"
local zero_based_rank = redis.call('ZRANK', KEYS[2], user_id)
local position = zero_based_rank + 1

return {0, tostring(position)}
