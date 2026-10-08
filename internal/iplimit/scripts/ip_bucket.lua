-- Per-IP token bucket. Atomic: either admit one request or reject it.
-- Used as the first line of defense in /enter — blocks bulk bots before
-- they consume event-bucket tokens.
--
-- KEYS[1] = bucket key        e.g. "ip:1.2.3.4"
-- ARGV[1] = capacity          e.g. "10"   (max tokens per IP)
-- ARGV[2] = refill_rate       e.g. "2"    (tokens per second)
-- ARGV[3] = now_ms            current time in milliseconds (caller passes this
--                                for testability — never use Lua's TIME)
--
-- Return shape: { status_code, payload_string }
--   status_code = 1 → "admitted"   (a token was consumed)
--   status_code = 0 → "rejected"   (IP is exhausted — hard reject, no queue)
--
-- Notes:
--   - No queue: IP exhaustion is a hard 429, not a ZSET wait.
--   - Bucket state: Redis Hash {tokens, last_refill_ms}.
--   - On first call (no state), bucket starts FULL and last_refill = now.
--   - Tokens refill lazily — only when Check() runs.
--   - 1-day TTL on bucket state (safety net against stale entries).

local capacity    = tonumber(ARGV[1])
local refill_rate = tonumber(ARGV[2])
local now_ms      = tonumber(ARGV[3])

-- 1) Refill bucket based on elapsed time since last refill
local bucket       = redis.call('HMGET', KEYS[1], 'tokens', 'last_refill_ms')
local tokens        = tonumber(bucket[1])
local last_refill_ms = tonumber(bucket[2])

if tokens == nil then
  -- First caller for this IP: start full + last_refill = now
  tokens           = capacity
  last_refill_ms   = now_ms
else
  local elapsed_secs = math.max(0, (now_ms - last_refill_ms) / 1000)
  tokens = math.min(capacity, tokens + elapsed_secs * refill_rate)
end

-- 2) Try to admit
if tokens >= 1 then
  tokens = tokens - 1
  redis.call('HMSET', KEYS[1], 'tokens', tokens, 'last_refill_ms', now_ms)
  redis.call('EXPIRE', KEYS[1], 86400)   -- 1-day TTL on bucket state
  return {1, 'admitted'}
end

-- 3) IP exhausted — hard reject (no queue)
return {0, 'rejected'}
