# Comprehensive Redis

> What Redis is, how it works, and exactly how this project uses it.
> Pairs with [comprehensive-go.md](./comprehensive-go.md) — the Go client
> is the other half of the story.

## How to read this

- **Parts 1-3** build the mental model. Read them in order if you are
  new to Redis.
- **Parts 4-5** are the "building blocks" and the "how we use it" tour.
  Read them when you are ready to write code or understand the project's
  Redis layer.
- **Part 5** is grounded in actual files in this repo — every section
  points at a real script or Go wrapper. Read it after (or alongside)
  `internal/redis/reserve.go` and `internal/redis/waitingroom.go`.
- This document is long because every topic is short. Jump to a specific
  section when you need it. Use the table of contents.
- If you only have 30 minutes, start with Appendix A (reading order).

## Conventions used in this document

- `[In this repo]` callouts link to files in this codebase where the
  concept is actually used.
- `[Gotcha]` flags a frequent stumbling block.
- `[Go deeper]` points at the canonical reference.
- Lua code samples use the `redis.call()` convention that Redis Lua
  scripts use.
- Go code samples use `redisclient` as the import alias for
  `github.com/redis/go-redis/v9` (matching the project's own usage).

---

## Table of contents

- **Part 1 - The Mental Model**
  1. Hello Redis
  2. The single-threaded execution model
  3. Keys, values, namespacing
  4. TTL: automatic cleanup
  5. Eviction policies
  6. Persistence: RDB and AOF
  7. Redis vs Postgres vs Memcached
- **Part 2 - Data Types**
  8. Strings
  9. Atomic counters (INCR / DECR)
  10. Hashes
  11. Lists
  12. Sets
  13. Sorted Sets (ZSETs)
  14. Streams
  15. Bitmaps, HyperLogLog, Geo
- **Part 3 - Atomicity & Lua**
  16. TOCTOU — the problem atomicity solves
  17. Transactions (MULTI/EXEC)
  18. WATCH — optimistic locking
  19. Lua scripts (EVAL)
  20. Lua KEYS vs ARGV
  21. EVALSHA — script caching
  22. Atomic patterns in this project
  23. Lua pitfalls
- **Part 4 - Building Blocks**
  24. Pub/Sub
  25. Streams vs Pub/Sub vs Lists
  26. Pipelines
  27. SCAN, never KEYS
  28. Transactions vs Pipelines vs Lua
  29. Connections, blocking commands, timeouts
  30. The Go client (go-redis)
- **Part 5 - In This Project**
  31. Project layout
  32. The Redis service in docker-compose.yml
  33. The two-script model
  34. reserve.lua annotated
  35. reserve.go — typed Result, status enum
  36. token_bucket.lua annotated
  37. waitingroom.go — TryAdmit, GetPosition, Promote, Enqueue
  38. TestReserve_NoOversell — the headline 1000-goroutine proof
  39. Key naming conventions
  40. Pitfalls + future Redis usage
- **Appendix**
  A. Reading order for a quickstart
  B. Tools
  C. Glossary

---

# Part 1 - The Mental Model

## 1. Hello Redis

Redis (REmote DIctionary Server) started in 2009 by Salvatore Sanfilippo.
It is an in-memory data structure server — every value lives in RAM, which
is why reads are measured in microseconds rather than milliseconds.

Three things Redis is particularly good at:

1. **Microsecond latency** — reads in ~50 µs on a local instance. A single
   Redis node handles 100k-200k commands per second on commodity hardware.
2. **Rich data types** — not just strings, but hashes, lists, sorted sets,
   streams, bitmaps, and more. You pick the right structure for the
   access pattern.
3. **Lua scripting** — you can ship code to the server and have it execute
   atomically, with respect to every other client, on the server's single
   command thread.

The three-tier design of this project maps directly to those strengths:


| Tier  | Store      | Why here                                         |
| ------- | ------------ | -------------------------------------------------- |
| Hot   | Redis      | Sub-50ms latency, atomic Lua, per-key TTL        |
| Async | AWS SQS    | Durable queue, at-least-once delivery, decoupled |
| Cold  | PostgreSQL | ACID, durable, complex queries                   |

[In this repo] `internal/redis/client.go` is the connection entry point.
`docs/architecture.md §1` has the full system diagram showing where Redis
lives in the hot path.

When NOT to use Redis:

- **Need ACID transactions across multiple keys** — Redis has no multi-key
  transactions (MULTI/EXEC does not give you isolation between queued
  commands). Use Postgres.
- **Need complex queries** — no joins, no filters, no aggregations. Use
  Postgres or ClickHouse.
- **Data larger than memory** — Redis is an in-memory store. It can
  persist to disk, but the working set must fit in RAM.
- **Need durable storage you can trust** — Redis is not the system of
  record. Postgres or S3 for that.

[Go deeper] https://redis.io/docs/about/

---

## 2. The single-threaded execution model

This is the most important fact about Redis.

Redis runs all command execution on **one thread**. One. Not a pool,
not goroutines, not an event loop — one dedicated thread that processes
commands sequentially.

The I/O is handled by a separate event loop (the ae event loop in the
C source), but every command — GET, SET, EVAL, whatever — runs to
completion on that single thread before the next command begins.

Why this matters:

```
Client A sends EVAL reserve.lua     → thread runs script, 50 µs
Client B sends GET foo              → queued behind A, runs after A
Client C sends SET bar baz          → queued behind B
```

No interleaving. No race conditions at the command level. If you can
express your logic as a Lua script, it runs atomically with respect to
every other client.

This is also why Lua scripts must be fast. A script that takes 10 ms
blocks the entire server for 10 ms. For context, a normal command takes
1-100 µs. A 10 ms script is a 100x slowdown for every other client.

[In this repo] `docs/architecture.md §4` ("Why Redis Lua is atomic") has
the sequence diagram showing this in action:

```
Redis single-threaded command loop
  EVAL reserve.lua request A → GET → check → DECRBY → SET hold → return
  EVAL reserve.lua request B → queued → GET (now 0) → return sold_out
```

---

## 3. Keys, values, namespacing

Redis keys are binary-safe strings up to 512 MB. Values are one of
six data types (see Part 2).

**Namespacing convention** — use colons to separate segments:

```
inventory:event:42          ← event 42's seat counter
hold:event:42:user:abc123   ← user abc123's hold on event 42
bucket:event:42              ← event 42's token bucket state
queue:event:42               ← event 42's waiting room ZSET
```

Why colons:

- `redis-cli --scan --pattern 'inventory:*'` finds all inventory keys
- Human-readable when reading `redis-cli KEYS *`
- Tools (RedisInsight, redis-cli) show them grouped

Rules:

- Keep keys short. Each key name costs memory. `u:42:e:100` vs
  `user:42:event:100` — the difference is small but compounds.
- Avoid spaces, brackets, glob characters (`*`, `?`, `[`).
- Make the type implicit from the name only if you also have a type
  tag (some teams use `str:...`, `zset:...` prefixes — we don't).

This project's key families:


| Pattern                      | Type   | TTL          | Purpose                               |
| ------------------------------ | -------- | -------------- | --------------------------------------- |
| `inventory:event:{id}`       | STRING | none         | Available seat counter                |
| `hold:event:{id}:user:{uid}` | STRING | 600 s        | Per-user seat lock (anti-double-book) |
| `bucket:event:{id}`          | HASH   | 86400 s      | Token bucket state (tokens + refill)  |
| `queue:event:{id}`           | ZSET   | configurable | Waiting room queue (FIFO by score)    |

[In this repo] `internal/redis/reserve.go` defines `InventoryKey()` and
`HoldKey()`. `internal/redis/waitingroom.go` defines `bucketKey()` and
`queueKey()`.

---

## 4. TTL: automatic cleanup

TTL (Time To Live) is how Redis handles automatic expiration.

Three ways to set it:

```redis
SET mykey value EX 60          -- set and expire in 60 s (atomic)
SETEX mykey 60 value           -- older form, same thing
SET mykey value                 -- no expiry (unless you ADD EXPIRE later)
EXPIRE mykey 60                -- set TTL on an existing key
PEXPIRE mykey 60000             -- milliseconds
```

Read TTL:

```redis
TTL mykey       -- seconds remaining (-1 = no expiry, -2 = key missing)
PTTL mykey      -- milliseconds
PERSIST mykey   -- remove TTL (makes the key permanent)
```

TTL is the backbone of our hold-key pattern:

```
reserve.lua:
  redis.call('SET', KEYS[2], ARGV[2], 'EX', ARGV[1])
  -- KEYS[2] = hold:event:42:user:abc123
  -- ARGV[1] = 600 (seconds)
  -- After 600 s, Redis deletes the key automatically.
```

Three things TTL does for us simultaneously:

1. **Anti-double-booking** — the `GET` on the hold key blocks the same
   user from reserving twice within 10 minutes.
2. **Auto-release on abandonment** — if the user closes the tab, the key
   expires and the seat counter increments (via keyspace notification or
   sweep — see Phase 7).
3. **No cleanup goroutine** — Redis handles expiry. We never run a cron
   to delete hold keys.

[Gotcha] TTL survives Redis restarts if AOF or RDB persistence is on.
The key is restored with its remaining TTL. If you `FLUSHDB` or
`FLUSHALL`, TTLs are lost.

[In this repo] `scripts/reserve.lua` line 33: `SET ... 'EX' ...`. The
600-second hold TTL is the 10-minute payment window in Phase 3 flow
(`docs/architecture.md §8`).

---

## 5. Eviction policies

Redis evicts keys when `maxmemory` is reached. The policy controls *which*
keys are evicted.

This project's Redis service is configured with:

```
--maxmemory 512mb
--maxmemory-policy allkeys-lru
```

**allkeys-lru** means: when memory is full, remove the least recently
used key across *all* keys (not just keys with a TTL).

All available policies:


| Policy            | Which keys considered   | Notes                             |
| ------------------- | ------------------------- | ----------------------------------- |
| `noeviction`      | none — writes rejected | Default. Kills your app.          |
| `allkeys-lru`     | all keys                | What we use.                      |
| `volatile-lru`    | keys with a TTL only    | Same as allkeys-lru if everything |
|                   |                         | has a TTL.                        |
| `allkeys-lfu`     | all keys                | LFU = least frequently used.      |
| `volatile-lfu`    | keys with a TTL only    | Good for cache with TTL.          |
| `allkeys-random`  | all keys                | Deterministic-ish.                |
| `volatile-random` | keys with a TTL only    |                                   |
| `volatile-ttl`    | keys with a TTL only    | Evict shortest TTL first.         |

**LRU is approximated, not exact.** Redis samples 5 random keys (config:
`maxmemory-samples`) and evicts the one with the oldest last-access time.
This is fast and good enough for cache workloads.

**The LRU × TTL interaction:**

```
allkeys-lru  → evicts anything (LRU across all keys)
volatile-lru → only evicts keys that HAVE a TTL set
```

If every key has a TTL, `allkeys-lru` and `volatile-lru` behave identically.
In our setup, every meaningful key has a TTL — inventory keys have none
in the current design (Phase 2), which means `volatile-lru` would ignore
them. That's why we use `allkeys-lru`: even inventory keys can be evicted
under memory pressure (and rebuilt from Postgres).

[In this repo] `docker-compose.yml` Redis service section.

---

## 6. Persistence: RDB and AOF

Redis can persist to disk. Two mechanisms:

**RDB (Redis Database) — point-in-time snapshot:**

```redis
save 60 1000       -- BGSAVE if ≥1000 keys changed in last 60 s
save 300 10        -- or if ≥10 keys changed in last 5 min
save 900 1         -- or if ≥1 key changed in last 15 min
```

- Fast restore (just load the binary file)
- Compact (compressed)
- Risk: last few minutes of data if Redis crashes between saves

**AOF (Append Only File) — log of every write:**

```redis
appendonly yes
appendfsync everysec    -- flush to disk every second (default)
# appendfsync always    -- flush on every command (slow, safe)
# appendfsync no        -- let the OS decide (fast, risky)
```

- Logs every write command
- Larger files; periodic rewrite when too big
- Better durability than RDB (1-second window vs potentially minutes)

This project's Redis is configured with `--appendonly yes`. But we do not
**rely** on it:

```
Cardinal rule: Postgres never touches the hot path.
              Redis never replaces the system of record.
```

Our inventory and hold keys are in-memory truth for the hot path. They
are also durably recorded via SQS → Postgres (Phase 6 worker). If Redis
loses data (crash before AOF flush), the inventory can be rebuilt from
Postgres after the SQS drain catches up. We treat Redis as durable enough
for caching and hot-path state, never as the source of truth.

AOF rewrite under load is CPU-intensive. In production, tune
`auto-aof-rewrite-percentage` and `auto-aof-rewrite-min-size` to avoid
mid-traffic rewrites.

[In this repo] `docker-compose.yml`: `--appendonly yes`.

---

## 7. Redis vs Postgres vs Memcached


| Concern           | Redis              | Postgres           | Memcached           |
| ------------------- | -------------------- | -------------------- | --------------------- |
| Latency           | ~50 µs            | ~1-10 ms           | ~50 µs             |
| Data model        | Key-value + types  | Relational SQL     | Key-value (strings) |
| Atomicity         | Lua scripts        | ACID transactions  | CAS only            |
| Persistence       | RDB + AOF          | WAL + tables       | None                |
| Complex queries   | No                 | Yes (SQL, JOINs)   | No                  |
| Memory efficiency | Lower (data types) | Higher (compact)   | Higher (simple)     |
| Scaling           | Cluster (sharding) | Read replicas      | Consistent hash     |
| TTL               | Native, all keys   | Manual (WHERE)     | Native              |
| Pub/Sub built-in  | Yes                | No (LISTEN/NOTIFY) | No                  |

**Why Redis over Memcached for this project:**

- **Atomicity**: Memcached has CAS (compare-and-swap) but no Lua.
  Our seat-reservation logic needs conditional reads + writes in one
  atomic step. That requires Lua or a client-side retry loop.
- **Data structures**: ZSET for the waiting room queue, HASH for bucket
  state. Memcached only has strings.
- **Pub/Sub**: native fan-out for SSE position updates (Phase 4).
  Memcached has no pub/sub.

**Why Redis over Postgres for the hot path:**

- 50 µs vs 1-10 ms. The hot path must stay under 50 ms end-to-end.
- Atomic Lua. Postgres needs `SELECT FOR UPDATE` or optimistic locking
  with retry, which adds latency.
- Per-key TTL. Postgres rows don't expire on their own.

**Why we also need Postgres:**

- Durable ledger of confirmed orders (forever truth).
- Complex queries (admin tools, analytics).
- ACID transactions for payment confirmation.
- Idempotency via unique constraints (`ON CONFLICT DO NOTHING`).

[In this repo] `docs/architecture.md §1` has the three-tier diagram.
`docs/architecture.md §7` ("The Two-State Model") explains the split.

---

# Part 2 - Data Types

## 8. Strings

The workhorse. A Redis string can hold up to 512 MB of binary data —
anything: plain text, JSON, protocol buffers, image thumbnails.

Core commands:

```redis
SET key value [EX seconds | PX milliseconds] [NX | XX]
GET key                           → string or nil
SETNX key value                   -- atomic set-if-not-exists (returns 1 or 0)
SETEX key seconds value           -- set + expiry, not atomic with each other
MSET key1 val1 key2 val2          -- multi-set, NOT atomic across keys
MGET key1 key2 key3               -- multi-get
APPEND key suffix                 -- concatenate (returns new length)
STRLEN key                        -- byte length
```

The `NX` and `XX` modifiers:

```redis
SET lock:mine "owner-uuid" NX EX 30   -- only if not exists
SET session:abc "data" XX EX 3600     -- only if exists (refresh)
```

This is the classic **distributed lock** pattern:

```lua
-- Acquire (Go side):
-- SET lock:task:123 uuid NX EX 30
-- If result == OK, we own it.
--
-- Release (Lua, atomic):
local current = redis.call('GET', KEYS[1])
if current == ARGV[1] then
    return redis.call('DEL', KEYS[1])
else
    return 0
end
```

[In this repo] We do not use raw SET for locking. Our hold key uses
SET with EX to create a TTL'd entry as a side effect of reservation
(`scripts/reserve.lua` line 33).

[Go deeper] https://redis.io/docs/data-types/strings/

---

## 9. Atomic counters (INCR / DECR / INCRBY / DECRBY)

This is the most operationally important command family for our project.

```redis
INCR key              -- +1, returns new value
DECR key              -- -1
INCRBY key N          -- +N
DECRBY key N          -- -N
INCRBYFLOAT key F     -- +F (float)
```

Why INCR is special:

```
Thread A: INCR inventory:event:42  → 9   (not "get 10, add 1, set 9")
Thread B: INCR inventory:event:42  → 10
```

The entire operation — read, increment, write — is atomic at the command
level. No TOCTOU window between GET and SET.

[Gotcha] INCR/DECR operate on string values stored as decimal integers.
If you `SET inventory:event:42 "ten"` and then `INCR`, Redis returns
an error. Our Lua scripts always call `tonumber()` to convert before
comparing.

Our inventory model:

```lua
-- scripts/reserve.lua (simplified)
local available = tonumber(redis.call('GET', KEYS[1]))
-- available = 10
if available < requested then
    return {0, 'sold_out'}
end
redis.call('DECRBY', KEYS[1], requested)
-- inventory is now 9
```

For group purchases (N > 1 seats in one call):

```lua
local requested = tonumber(ARGV[2])   -- e.g., 3
if available < requested then
    return {0, 'sold_out'}
end
redis.call('DECRBY', KEYS[1], requested)
```

[In this repo] `scripts/reserve.lua` line 32: `DECRBY` with the Lua
`tonumber()` guard on the read. `internal/redis/reserve_test.go`
`TestReserve_MultiSeatSoldOut` tests the "not enough left" path.

---

## 10. Hashes

A hash is a map of field-value pairs under a single key.

```redis
HSET user:42 name "Alice" email "alice@example.com"
HGET user:42 name                   → "Alice"
HMSET user:42 name "Alice" email "alice@example.com"   -- older form
HMGET user:42 name email            → ["Alice", "alice@example.com"]
HGETALL user:42                    -- all fields (careful on large hashes)
HINCRBY user:42 login_count 1      -- atomic increment on a field
HEXISTS user:42 email              → 1
HDEL user:42 email                 -- delete field
HSCAN user:42 cursor               -- incremental scan (like SCAN)
```

Hashes use O(1) memory per field and are stored efficiently (Redis
encodes them differently depending on size).

Where hashes would appear in this project (not yet used):

- **Phase 7** reservation state:
  `HSET reservation:<id> user_id u1 event_id 42 status PENDING_PAYMENT`
- **Phase 4** per-IP bucket state:
  `HSET bucket:ip:192.168.1.1 tokens 97 last_refill_ms <timestamp>`

Our current bucket state (`scripts/token_bucket.lua`) uses a Redis HASH:

```lua
-- token_bucket.lua (simplified)
local bucket = redis.call('HMGET', KEYS[1], 'tokens', 'last_refill_ms')
-- HMGET returns [tokens_string, last_refill_ms_string] or [nil, nil]
```

[In this repo] `scripts/token_bucket.lua` lines 31-32: `HMGET` to read
bucket state. Lines 47-48: `HMSET` to write it back.

[Gotcha] `HGETALL` is O(N) where N = number of fields. Never call it on
a hash with thousands of fields in a hot path. Use `HMGET` for the
specific fields you need.

---

## 11. Lists

Redis lists are ordered sequences of strings, implemented as quicklists
(Redis 3.2+). They allow duplicates.

```redis
LPUSH  key value         -- push to head (left)
RPUSH  key value         -- push to tail (right)
LPOP   key              -- pop from head
RPOP   key              -- pop from tail
BRPOP  key timeout      -- blocking pop (waits for data)
BLPOP  key timeout      -- blocking pop from head
LRANGE key 0 -1          -- get all (0 = first, -1 = last)
LLEN   key              -- length
LTRIM  key 0 99         -- keep first 100 elements (queue bounded)
```

**The blocking variants (BRPOP / BLPOP)** are the classic way to build
a reliable worker queue in Redis:

```redis
-- Worker side:
BRPOP queue:reservations 0   -- block forever until data arrives
-- Returns [key, value]
```

**Why we do not use Lists for our waiting room:**

```
LPUSH queue:event:42 user123   -- add to queue
LPOP  queue:event:42           -- remove from queue
```

Problems:

1. **No rank query** — after enqueueing, a user cannot ask "what is my
   position?" without scanning the whole list.
2. **Lost position on consume** — once you LPOP, the item is gone. You
   cannot leave it in the queue and confirm admission later.
3. **No TTL per entry** — Lists have no per-element expiry. We would
   need a parallel ZSET or background sweeper.

Sorted Sets (ZSETs) solve all three problems.

Use Lists when you have a simple producer-consumer queue with no need
for position queries: activity feeds, batch job queues, chat message
buffers.

[In this repo] We do not use Lists anywhere in the project.

---

## 12. Sets

Sets are unordered collections of unique strings.

```redis
SADD key member [member ...]    -- add (returns number added, 0 if exists)
SREM key member [member ...]     -- remove
SMEMBERS key                     -- get all (careful: O(N))
SISMEMBER key member            -- O(1) membership test
SCARD key                        -- cardinality (count)
SPOP key [count]                 -- remove and return random member(s)
SRANDMEMBER key [count]          -- get random without removing

SINTER key1 key2                 -- intersection
SUNION key1 key2                 -- union
SDIFF  key1 key2                  -- difference
```

O(1) add, O(1) membership test, O(N) for full scan.

Where Sets would appear in this project (not yet used):

- **Unique visitor tracking**: `SADD pageviews:2026-10-02 <user_id>`
  — deduplicate visitors to an event page.
- **Tagging**: `SADD event:42:tags vip early-access`.

---

## 13. Sorted Sets (ZSETs) — THE important one for us

ZSET = every member has a score (float64), sorted by score.

```redis
ZADD key score member [score member ...]    -- add or update
ZREM  key member                            -- remove
ZRANGE key start stop [WITHSCORES]          -- by rank (0 = lowest)
ZREVRANGE key start stop [WITHSCORES]       -- highest score first
ZRANK  key member                           -- 0-based rank (nil if absent)
ZREVRANK key member                          -- highest-score rank
ZSCORE key member                            -- score
ZCARD  key                                   -- count
ZINCRBY key increment member                  -- atomic score +N
ZREMRANGEBYSCORE key min max                 -- remove by score range
ZREMRANGEBYRANK key start stop               -- remove by rank range
```

The score is a float64. Our project uses Unix milliseconds as the score
for one reason: **nanosecond-resolution timestamps give FIFO ordering**.

```lua
-- token_bucket.lua (simplified)
redis.call('ZADD', queue_key, now_ms, user_id)
-- Score = now_ms (current time in ms)
-- ZRANGEBYSCORE with -inf to cutoff removes stale entries
-- ZRANK returns 0-based position
local zero_based_rank = redis.call('ZRANK', KEYS[2], user_id)
local position = zero_based_rank + 1   -- human-friendly 1-based
```

Why ZRANK returning nil is important:

```
ZRANK queue:event:42 user-123
(nil)   ← user not in queue
```

In go-redis, this surfaces as the `redisclient.Nil` error. We handle it
explicitly in `GetPosition`:

```go
// internal/redis/waitingroom.go
rank, err := c.ZRank(ctx, queueKey(eventID), userID).Result()
if err == redisclient.Nil {
    return 0, nil   // not in queue
}
```

[In this repo] `scripts/token_bucket.lua` lines 54-63. The ZSET is the
waiting room queue. `docs/architecture.md §3` explains the drainer
goroutine that reads from this ZSET.

[Gotcha] ZRANK is O(log N) — logarithmic in the number of members. For a
queue of 10,000 users this is ~14 steps. Fine. For 1 million users, it's
still ~20 steps. Acceptable.

[Go deeper] https://redis.io/docs/data-types/sorted-sets/

---

## 14. Streams

Streams (Redis 5+) are durable, append-only logs with consumer groups.

```redis
XADD key * field value [field value ...]    -- * = auto-generate ID
XLEN key
XRANGE key start end [COUNT N]
XREAD [COUNT N] [BLOCK ms] STREAMS key id  -- read new entries
XGROUP CREATE key group $                    -- create consumer group
XREADGROUP GROUP group consumer COUNT N      -- read for a specific consumer
XACK key group id                           -- acknowledge processed
XPENDING key group                          -- list pending (unacknowledged)
XTRIM key MAXLEN N                          -- trim to N entries
```

Consumer groups give Streams a capability Lists lack: **multiple
consumers processing the same stream independently, with acknowledgments.**

This is the Redis-native alternative to SQS consumer groups. We chose
SQS because:

- SQS is a managed service (no Redis cluster ops).
- SQS Standard has unlimited throughput.
- SQS integrates with AWS Lambda, SQS triggers, dead-letter queues.

Where Streams might appear in this project (stretch goals):

- **Phase 7 expiration watcher**: `XADD expirations * event_id 42 user_id u1`
  with a consumer group processing the log and incrementing inventory.
- **Audit log**: every state transition (reserved → confirmed → expired)
  written to a Stream for replay.

[Go deeper] https://redis.io/docs/data-types/streams/

---

## 15. Bitmaps, HyperLogLog, Geo — what they exist for

These are specialized data types for specific problem shapes. We do not
use them, but knowing they exist prevents reinventing them badly.

**Bitmaps (SETBIT / GETBIT / BITCOUNT / BITOP):**

A bitmap is a string where each bit represents a position. Useful for
"user X performed action Y on day D":

```redis
SETBIT daily:2026-10-02:logins <user_offset> 1
BITCOUNT daily:2026-10-02:logins              -- unique logins today
```

Memory: 1 bit per user. 1 million users = 125 KB per day. Excellent for
high-cardinality unique counts.

**HyperLogLog (PFADD / PFCOUNT / PFMERGE):**

Estimates unique counts with 0.81% standard error in ~12 KB.

```redis
PFADD pageviews:2026-10-02 "user-abc"
PFCOUNT pageviews:2026-10-02    -- estimate of unique visitors
```

Useful for analytics where exact counts are not needed.

**Geo (GEOADD / GEODIST / GEORADIUS):**

Store lat/long and query by radius:

```redis
GEOADD venues:thailand 100.543 13.7563 "Bangkok"
GEORADIUS venues:thailand 100.5 13.7 50 km WITHDIST
```

We do not use Geo (ticket events are identified by ID, not location).

---

# Part 3 - Atomicity & Lua

## 16. TOCTOU — the problem atomicity solves

TOCTOU (Time-Of-Check to Time-Of-Use) is the class of bug that causes
overselling. It looks like this in pseudocode:

```
available = redis.GET("inventory:event:42")   -- read: 10
if available >= requested:
    sleep(10ms)                              -- other requests get in
    redis.SET("inventory:event:42", available - requested)  -- write
```

With 1,000 concurrent requests, the `sleep` window is open 1,000 times.
Multiple requests all read "10", all decide "yes", all decrement. Oversell.

The relational DB equivalent (the classic):

```
TX-A: SELECT seats FROM events WHERE id=42  → 1 seat
TX-B: SELECT seats FROM events WHERE id=42  → 1 seat  (stale!)
TX-A: UPDATE events SET seats=0 WHERE id=42
TX-B: UPDATE events SET seats=0 WHERE id=42  -- both committed → oversold
```

Relational fix: `SELECT ... FOR UPDATE` (pessimistic lock) or optimistic
locking with a version column.

Redis fix: Lua.

[In this repo] `docs/architecture.md §6` has the canonical sequence
diagrams comparing the race with Postgres vs the atomic Lua execution.

---

## 17. Transactions (MULTI/EXEC)

Redis transactions batch commands:

```redis
MULTI
GET inventory:event:42
DECRBY inventory:event:42 1
SET hold:event:42:user:abc EX 600
EXEC
```

`EXEC` returns an array of replies, one per command.

**What they do:**

- Queue commands and execute them sequentially with no interleaving from
  other clients.
- Atomic — all-or-nothing (DISCARD clears the queue).

**What they DON'T do:**

- There is no isolation between queued commands. `GET` inside a
  transaction returns the value at `EXEC` time, not at `MULTI` time.
  The queued `GET` has not "seen" prior queued commands' writes yet. 

```redis
MULTI
SET foo 1
GET foo       -- returns nil! GET hasn't executed yet, it's queued.
SET foo 2
EXEC          -- returns [OK, nil, OK]
```

This is why MULTI/EXEC alone cannot solve our problem:

```
MULTI
GET inventory:event:42    -- still returns old value at EXEC time
DECRBY inventory:event:42 1
EXEC
```

Two concurrent transactions both `GET` the same value, both `DECRBY`
by 1, and both write — without either seeing the other's write. Still
a TOCTOU race inside the transaction.

Use MULTI/EXEC when you want to batch independent commands with one
round-trip and guarantee they don't interleave with other clients'
commands. Use Lua when you need conditional logic.

---

## 18. WATCH — optimistic locking

WATCH tracks key versions and aborts the transaction if any watched key
changes before EXEC:

```redis
WATCH inventory:event:42
current = GET inventory:event:42
if current >= requested:
    MULTI
    DECRBY inventory:event:42 requested
    EXEC          -- returns null if key changed since WATCH
else:
    UNWATCH
```

The client must retry the whole loop if EXEC returns nil.

**Why we do not use WATCH in this project:**

- Retries mean multiple round-trips under contention.
- At 1,000 concurrent requests, many will retry, multiplying load.
- Lua gives us the conditional logic AND the atomicity in one round-trip.

WATCH is appropriate when:

- Contention is rare (most requests succeed on first try).
- You cannot use Lua (e.g., you're using a client that doesn't support it).

---

## 19. Lua scripts (EVAL) — the real atomicity tool

EVAL uploads and runs a Lua script on the Redis server:

```redis
EVAL "return redis.call('GET', KEYS[1])" 1 mykey
--                        ^script body^   ^num keys^ ^keys^ ^args^
```

The script runs to completion on the single command thread. No other
client command interleaves. The script is a transaction by construction.

```lua
-- Our reserve.lua (simplified)
local available = tonumber(redis.call('GET', KEYS[1]))
if available == nil then
    return {-1, 'event_not_found'}
end
if available < tonumber(ARGV[2]) then
    return {0, 'sold_out'}
end
local existing = redis.call('GET', KEYS[2])
if existing then
    return {0, 'already_holding'}
end
redis.call('DECRBY', KEYS[1], ARGV[2])
redis.call('SET', KEYS[2], ARGV[2], 'EX', ARGV[1])
return {1, 'reserved'}
```

Four operations in one atomic step: GET, compare, DECRBY, SET.
No TOCTOU window. No retries. No WATCH.

Redis 7+ scripts are also cached server-side and can be called by SHA1
(EVALSHA) on subsequent calls. See §21.

[Go deeper] https://redis.io/docs/interact/programmability/eval-intro/

[In this repo] `scripts/reserve.lua` and `scripts/token_bucket.lua`.

---

## 20. Lua KEYS vs ARGV — cluster safety

Lua scripts take two argument lists:

```lua
-- KEYS[] = keys the script reads/writes (for cluster routing)
-- ARGV[] = everything else (literal values, flags, TTLs)
local tokens = tonumber(redis.call('HMGET', KEYS[1], 'tokens', 'last_refill_ms')[1])
local now_ms = tonumber(ARGV[3])
```

**Why the split matters:**

In Redis Cluster, each key is hashed to one of 16,384 slots. A single
command (or a Lua script) can only touch keys in the **same slot** —
otherwise Redis returns a `CROSSSLOT` error.

When you pass keys in `KEYS[n]`, Redis can hash them to verify they're
in the same slot. `ARGV` values are never hashed for routing.

```go
// Go side: pass keys separately from values
keys := []string{inventoryKey, holdKey}
args := []any{holdTTLSec, seatsRequested}
reserveScript.Run(ctx, client, keys, args...)
```

**Rule:** any key your script touches must be in `KEYS[]`. Never
interpolate a key name from `ARGV` into a Redis command inside Lua.

```lua
-- WRONG (violates the rule):
local val = redis.call('GET', ARGV[1])   -- what if ARGV[1] is a key name?

-- CORRECT:
local val = redis.call('GET', KEYS[1])   -- Redis can verify slot
```

[In this repo] Every Lua script header documents KEYS and ARGV:

```lua
-- scripts/reserve.lua
-- KEYS[1] = seat_inventory_key   e.g. "inventory:event:42"
-- KEYS[2] = user_hold_key        e.g. "hold:event:42:user:abc123"
-- ARGV[1] = hold_ttl_seconds     e.g. "600"
-- ARGV[2] = seats_requested      e.g. "1"
```

---

## 21. EVALSHA — script caching

Every Lua script has a deterministic SHA1 hash derived from its source.
Redis caches the compiled bytecode server-side by SHA1.

```
First call:   EVAL "return redis.call('GET', KEYS[1])" 1 mykey
                → server compiles Lua → caches under SHA1 → executes

Subsequent:   EVALSHA sha1_of_above_script 1 mykey
                → server has bytecode → executes directly
                → faster (no upload)
```

If the server evicts the script from cache (e.g., after FLUSHDB,
FLUSHALL, or `SCRIPT FLUSH`), EVALSHA returns `NOSCRIPT` and the
client must fall back to EVAL.

**go-redis handles this automatically:**

```go
// internal/redis/reserve.go
var reserveScript = redisclient.NewScript(reserveScriptSrc)
// NewScript computes the SHA1 from src immediately.

// Every call:
raw, err := reserveScript.Run(ctx, client, keys, args...).Result()
// go-redis: tries EVALSHA first.
//           If NOSCRIPT → falls back to EVAL automatically.
```

The script source is embedded at compile time via `//go:embed`, so the
SHA1 is stable across process restarts. See §31.

[In this repo] `internal/redis/reserve.go` line 12-17. `internal/redis/ waitingroom.go` line 12-15.

---

## 22. Atomic patterns in this project

Three patterns from our two Lua scripts:

**Pattern A — check-then-decrement (with floor at zero):**

```lua
-- reserve.lua
local available = tonumber(redis.call('GET', KEYS[1]))
if available < requested then
    return {0, 'sold_out'}   -- reject, NO side effect
end
redis.call('DECRBY', KEYS[1], requested)   -- commit
```

The decrement only happens if `available >= requested`. This is the
anti-oversell guard.

**Pattern B — conditional lock acquisition:**

```lua
-- reserve.lua
local existing = redis.call('GET', KEYS[2])
if existing then
    return {0, 'already_holding'}   -- someone already has the lock
end
redis.call('SET', KEYS[2], requested, 'EX', ARGV[1])  -- take lock
```

The hold key is both a lock (one per user) and a TTL (auto-release).

**Pattern C — lazy refill with admission gate:**

```lua
-- token_bucket.lua
local elapsed = (now_ms - last_refill_ms) / 1000
tokens = math.min(capacity, tokens + elapsed * refill_rate)
-- tokens is now the current token count including refill

if tokens >= 1 then
    tokens = tokens - 1
    redis.call('HMSET', KEYS[1], 'tokens', tokens, 'last_refill_ms', now_ms)
    return {1, 'admitted'}
end

-- No token: queue with timestamp score
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', cutoff)
redis.call('ZADD', KEYS[2], now_ms, user_id)
local rank = redis.call('ZRANK', KEYS[2], user_id)
return {0, tostring(rank + 1)}
```

The queue + position in a single atomic step: no interleaving client can
insert between the "check" and the "enqueue."

[In this repo] `scripts/reserve.lua` lines 15-35. `scripts/ token_bucket.lua` lines 30-63.

---

## 23. Lua pitfalls

**Pitfall 1 — Long scripts block the server.**

A Lua script that takes 10 ms blocks all other clients for 10 ms. For
context: a normal Redis command takes 1-100 µs.

```
10 ms script × 1000 concurrent clients = 10 seconds of queued requests
```

Rule: keep scripts under 1 ms. Our reserve.lua is ~200 µs. Our
token_bucket.lua is ~300 µs.

**Pitfall 2 — Never use TIME inside Lua.**

```lua
-- WRONG: TIME is non-deterministic (wall clock, can jump)
local now = redis.call('TIME')   -- [seconds, microseconds]

-- CORRECT: pass time as ARGV
local now_ms = tonumber(ARGV[3])   -- injected by the caller
```

We pass `now_ms` as `ARGV[3]` in `token_bucket.lua` specifically so
tests can pass a fixed clock. In production, the caller passes
`time.Now().UnixMilli()`.

**Pitfall 3 — Keys must be in KEYS[], never interpolated from ARGV.**

```lua
-- WRONG:
local key = ARGV[1]
redis.call('GET', key)   -- Redis can't verify slot placement

-- CORRECT:
redis.call('GET', KEYS[1])   -- slot-verified
```

**Pitfall 4 — Redis 7+ replicates commands differently.**

In Redis 7+, Lua scripts run in `redis.replicate_commands()` mode by
default (pre-7 it was the opposite). This means side effects (calls that
modify data) are replicated to replicas. Our scripts don't need to worry
about this because we don't use replicas — but if you add replica reads
in the future, be aware of the distinction.

**Pitfall 5 — A script that crashes mid-execution aborts the client
connection.**

If your Lua script hits an error (typo, wrong type, etc.), Redis returns
an error to the client. The inventory state depends on which step the
script was at. In our design, any error from `reserveScript.Run` is
treated as a fatal error to the caller — we return an error, not an
incorrect state. The seat inventory is unchanged because the DECRBY only
happens after all guards pass.

---

# Part 4 - Building Blocks

## 24. Pub/Sub

Redis has a built-in publish/subscribe message bus.

```redis
SUBSCRIBE channel1 channel2
PSUBSCRIBE pattern*          -- glob pattern subscription
PUBLISH channel1 message
UNSUBSCRIBE [channel]
PUBSUB CHANNELS              -- list active channels
PUBSUB NUMSUB channel1       -- subscriber counts
```

The contract:

- Fire-and-forget. If no subscriber is listening, the message is dropped.
- No persistence. If a subscriber reconnects, it misses messages sent
  while it was disconnected.
- At-most-once delivery. A subscriber either gets the message or doesn't.

Redis 7+ adds **sharded Pub/Sub** (`SSUBSCRIBE`/`SPUBLISH`) which routes
by slot — relevant for Redis Cluster.

**Phase 4 design (from `docs/architecture.md §3`):**

The drainer goroutine would publish position updates to a per-event
channel:

```redis
PUBLISH queue:event:42:updates '{"user_id":"abc","position":5}'
```

API servers subscribed to that channel forward the update to the
corresponding SSE client. This scales better than each API server
polling ZRANK every second per connected client.

We are not using Pub/Sub yet. The Phase 4 SSE implementation polls ZRANK
periodically (every 1 second) from each API server. Pub/Sub would be an
optimization for higher scale.

[In this repo] `docs/architecture.md §3` "SSE position updates" describes
the current polling approach.

---

## 25. Streams vs Pub/Sub vs Lists — when to use which


| Property        | Pub/Sub      | Lists          | Streams              |
| ----------------- | -------------- | ---------------- | ---------------------- |
| Persistence     | None         | Until consumed | Until trimmed        |
| Replay          | No           | No             | Yes (XRANGE)         |
| Consumer groups | No           | Manual         | Yes (XREADGROUP)     |
| Per-entry TTL   | No           | No             | Yes (MAXLEN)         |
| At-most-once    | Yes          | Yes            | At-least-once (XACK) |
| Fan-out         | Yes (native) | Limited        | Limited              |

**Our choices:**

- **Waiting room queue** → ZSET (sorted by timestamp). We need position
  queries, per-entry TTL cleanup, and O(log N) operations.
- **SSE position updates** → currently polling (ZRANK every 1s). At scale,
  would be Pub/Sub (fan-out to many SSE listeners) or Streams (durable
  audit log).
- **Async fulfillment queue** → AWS SQS (managed service, integrates
  with Lambda, dead-letter queues, cloud-native).

---

## 26. Pipelines — batch round-trips

A pipeline sends N commands in one TCP frame and reads N replies in one
response. It is not atomic across commands (other clients' commands can
interleave), but it eliminates N-1 network round-trips.

```go
pipe := client.Pipeline()
pipe.Set(ctx, "k1", "v1", 0)
pipe.Set(ctx, "k2", "v2", 0)
pipe.Get(ctx, "k3")
cmds, err := pipe.Exec(ctx)
// cmds[0] = *StatusCmd (SET k1)
// cmds[1] = *StatusCmd (SET k2)
// cmds[2] = *StringCmd (GET k3)
```

Each command in a pipeline is individually atomic (runs on the single
command thread), but the sequence as a whole is not.

**When to use:**

- Multiple independent commands where you want minimum latency.
- Cleanup operations (DEL key1 key2 key3) in tests.

**When NOT to use:**

- Commands that depend on each other's results (use Lua).
- Atomicity across the batch matters (use Lua or MULTI/EXEC).

[In this repo] `internal/redis/waitingroom.go` `Enqueue()` uses a
pipeline for three independent operations:

```go
pipe := c.Pipeline()
pipe.ZRemRangeByScore(ctx, queueKey(eventID), "-inf", cutoff)
pipe.ZAdd(ctx, queueKey(eventID), redisclient.Z{Score: float64(nowMS), Member: userID})
pipe.Expire(ctx, queueKey(eventID), time.Duration(ttlSec)*time.Second)
if _, err := pipe.Exec(ctx); err != nil {
    return 0, fmt.Errorf("redis: enqueue pipeline failed: %w", err)
}
```

These three operations are independent: cleanup, add, and set TTL. A
pipeline is the right tool.

[Gotcha] A pipeline that returns an error on one command does NOT
automatically abort the others. You must check each `cmds[i]` individually.

---

## 27. SCAN, never KEYS

`KEYS *` walks the entire keyspace synchronously and returns every key.
On a Redis instance with 1 million keys, this blocks the server for
hundreds of milliseconds — during which no other command can run.

```redis
KEYS *                      -- O(N), BLOCKS the server
KEYS inventory:event:*      -- still O(N), still blocks
```

`SCAN` is the safe alternative:

```redis
SCAN cursor [MATCH pattern] [COUNT n] [TYPE string]
-- Returns [next_cursor, [key1, key2, ...]]
-- cursor = 0 means done
```

`SCAN` returns a small batch per call (default 10), yielding the server
thread between batches. It may return duplicates (the client must dedup).

Variants for iterating inside a specific type:

```redis
HSCAN hash_key cursor [COUNT n]
SSCAN set_key cursor [COUNT n]
ZSCAN zset_key cursor [COUNT n]
```

In our project we never scan the keyspace in production. All access is
by specific, known keys. Tests use explicit `DEL` of known keys via
`withCleanKeys` helper.

```go
// internal/redis/reserve_test.go line 29
func withCleanKeys(t *testing.T, c *redisclient.Client, keys ...string) {
    t.Cleanup(func() {
        c.Del(ctx, keys...).Err()   -- specific keys, no scan
    })
}
```

[Gotcha] `SCAN` is O(1) per call but the full iteration is O(N). Don't
use it to "find all keys matching a pattern" in a hot path.

---

## 28. Transactions vs Pipelines vs Lua — when to use which


| Need                                    | Use        | Reason                                     |
| ----------------------------------------- | ------------ | -------------------------------------------- |
| N independent commands, minimum latency | Pipeline   | One round-trip, not atomic across commands |
| N commands atomic, no conditional logic | MULTI/EXEC | All-or-nothing, but no read isolation      |
| Conditional reads + writes              | Lua        | The only tool with real atomicity + logic  |
| Simple counter                          | INCR/DECR  | O(1) atomic per-command primitive          |
| Lock acquisition                        | SET NX EX  | Atomic set-if-not-exists with TTL          |

**Our project:**

- Seat reservation → Lua (conditional: only decrement if available).
- Token bucket refill → Lua (conditional: only admit if tokens >= 1).
- Enqueue in waiting room → Pipeline (three independent ops: cleanup +
  add + set TTL).
- Connection pool setup → `redisclient.NewClient()` (one-time init).

---

## 29. Connections, blocking commands, timeouts

**Connection pool:**

go-redis manages a pool of TCP connections. Each `Run`, `Get`, `Set` etc.
acquires a connection from the pool, runs the command, and returns the
connection.

```go
// internal/redis/client.go
redisclient.NewClient(&redisclient.Options{
    Addr:         cfg.Addr,        // "localhost:6379"
    DB:           cfg.DB,          // 0
    PoolSize:     100,             // max connections
    MinIdleConns: 5,               // keep warm
    DialTimeout:  3 * time.Second,
    ReadTimeout:  2 * time.Second,
    WriteTimeout: 2 * time.Second,
})
```

PoolSize of 100 means at most 100 concurrent commands executing at once.
Our hot path uses EVAL scripts (~50-300 µs each), so the pool is more
than enough for 1,000 concurrent requests — connections are acquired and
released within milliseconds.

**Blocking commands:**

`BLPOP`, `BRPOP`, `BRPOPLPUSH`, `XREAD BLOCK` hold a connection from the
pool until data arrives. Each blocks the pool. With PoolSize=100 and 100
concurrent BLPOP callers, no connections are left for normal commands.
Size your pool accordingly.

We do not use blocking commands in this project (we use SQS long-polling
instead).

**Timeouts:**

```
DialTimeout:  connect() syscall timeout (we set 3s)
ReadTimeout: time from write complete to first byte of response (we set 2s)
WriteTimeout: time to write the request (we set 2s)
```

[Gotcha] A Lua script that takes longer than `ReadTimeout` is killed at
the client. The server still runs the script to completion. The side
effects (DECRBY, SET) are present. The client sees `read: i/o timeout`.
Handle this by treating script errors as potentially-partial-state
errors — in our project, we return the error to the caller and do not
retry automatically.

---

## 30. The Go client (go-redis) — connection pool, Script type, EVALSHA caching

We use `github.com/redis/go-redis/v9`. The import alias is `redisclient`
to avoid collision with the `redis` package name.

**Pool configuration:**

```go
// internal/redis/client.go
func NewClient(cfg Config) *redisclient.Client {
    return redisclient.NewClient(&redisclient.Options{
        Addr:         cfg.Addr,
        DB:           cfg.DB,
        PoolSize:     100,
        MinIdleConns: 5,
        DialTimeout:  3 * time.Second,
        ReadTimeout:  2 * time.Second,
        WriteTimeout: 2 * time.Second,
    })
}
```

**Script type — automatic EVALSHA caching:**

```go
// internal/redis/reserve.go
//go:embed scripts/reserve.lua
var reserveScriptSrc string

var reserveScript = redisclient.NewScript(reserveScriptSrc)
// SHA1 computed from src at NewScript() time (not at Run time).
// Package-level var = computed once, shared across all calls.

// On every ReserveSeat() call:
raw, err := reserveScript.Run(ctx, client, keys, args...).Result()
// go-redis: tries EVALSHA first.
//           If NOSCRIPT → falls back to EVAL automatically.
```

**Pipeline:**

```go
pipe := client.Pipeline()
pipe.ZRemRangeByScore(ctx, key, "-inf", cutoff)
pipe.ZAdd(ctx, key, redisclient.Z{Score: score, Member: member})
pipe.Expire(ctx, key, ttl)
_, err := pipe.Exec(ctx)
```

**Return value handling:**

go-redis returns `*redisclient.StringCmd`, `*redisclient.IntCmd`,
`*redisclient.StatusCmd`, etc. Use `.Result()` which returns `(value, error)`. `.Val()` returns the value only (panics on error).

```go
// Recommended:
val, err := client.Get(ctx, "key").Result()
if err == redisclient.Nil {
    // key not found
} else if err != nil {
    // real error
}

// Risky (panics on error):
val := client.Get(ctx, "key").Val()
```

**Our convention for Nil:**

```go
// internal/redis/waitingroom.go
rank, err := c.ZRank(ctx, queueKey(eventID), userID).Result()
if err == redisclient.Nil {
    return 0, nil   // user not in queue
}
```

[In this repo] `internal/redis/client.go`, `internal/redis/reserve.go`,
`internal/redis/waitingroom.go`.

[Go deeper] https://redis.uptrace.dev/guide/

---

# Part 5 - In This Project

## 31. Project layout

```
internal/redis/
├── client.go              -- connection pool, NewClient, Ping
├── reserve.go             -- ReserveSeat + types
├── waitingroom.go         -- TryAdmit, GetPosition, Promote, Enqueue
├── reserve_test.go        -- 5 tests including TestReserve_NoOversell
├── waitingroom_test.go    -- 4 tests
└── scripts/
    ├── reserve.lua        -- atomic seat reservation
    └── token_bucket.lua   -- token bucket + ZSET waiting room queue
```

The Lua scripts are embedded at compile time:

```go
// internal/redis/reserve.go
import _ "embed"
var reserveScriptSrc string   // populated by //go:embed below

//go:embed scripts/reserve.lua
var reserveScriptSrc string
```

`//go:embed` resolves paths relative to the **package directory** (where
the `.go` file lives), not the project root. The scripts live in
`internal/redis/scripts/reserve.lua`, and the embed directive in
`internal/redis/reserve.go` reads from `./scripts/reserve.lua` relative
to `internal/redis/`.

Why embed vs read at runtime:

- **Single binary** — no separate file to distribute or lose.
- **SHA1 stability** — the script source is baked into the binary, so the
  SHA1 is deterministic. `NewScript(src)` computes the SHA1 at program
  start. The server caches by SHA1.
- **No file I/O at startup** — `//go:embed` is a compile-time constant.

[In this repo] `internal/redis/reserve.go` lines 12-17. `internal/redis/ waitingroom.go` lines 12-15.

---

## 32. The Redis service in docker-compose.yml

```yaml
redis:
  image: redis:7-alpine
  ports:
    - "6379:6379"
  command: >
    redis-server
      --notify-keyspace-events Ex    # Phase 7: expired-key events
      --appendonly yes               # AOF (we don't rely on it)
      --maxmemory 512mb             # memory cap
      --maxmemory-policy allkeys-lru # evict LRU when full
  healthcheck:
    test: ["CMD", "redis-cli", "ping"]
    interval: 5s
    timeout: 3s
    retries: 10
    start_period: 3s
  volumes:
    - redis-data:/data
  restart: unless-stopped
```

Flags in order of importance:


| Flag                             | Why it matters                                    |
| ---------------------------------- | --------------------------------------------------- |
| `--maxmemory 512mb`              | Caps memory. Prevents runaway growth.             |
| `--maxmemory-policy allkeys-lru` | With 512mb cap, what gets evicted when full?      |
| `--notify-keyspace-events Ex`    | Publish expired-key events on keyspace 0. Phase 7 |
| `--appendonly yes`               | AOF for crash recovery (we don't rely on it).     |

The `Ex` notification flag: `E` = expired events, `x` = set/expired
events (capital X = all events). Lowercase `x` is sufficient for our
use — we subscribe to `__keyevent@0__:expired`.

[In this repo] `docker-compose.yml` lines 9-28.

---

## 33. The two-script model

This project uses two Lua scripts, not one giant script. The split is
intentional:


| Script             | When it runs         | What it does                            |
| -------------------- | ---------------------- | ----------------------------------------- |
| `reserve.lua`      | Every`/reserve` call | Check + lock + decrement inventory      |
| `token_bucket.lua` | Every`/enter` call   | Rate-limit admission or enqueue to ZSET |

**Why separate scripts:**

1. **Different access patterns.** `/enter` runs the token bucket on every
   unauthenticated request. `/reserve` only runs after JWT verification.
   Mixing them would make the reserve path slower (token bucket logic
   runs every time).
2. **Separation of concerns.** The waiting room is a rate limiter. The
   seat reservation is the core business logic. Keeping them separate
   makes each script easier to reason about and test.
3. **Cache efficiency.** `token_bucket.lua` is called more often and is
   more likely to be in Redis's script cache. `reserve.lua` is called
   less often but must be fast when it runs. Separate SHA1s mean cache
   misses for one don't affect the other.

**The coordination contract:**

```
/enter → token_bucket.lua
         ├─ admitted → issue JWT → user calls /reserve
         └─ queued  → return queue_url → SSE position stream

/reserve → reserve.lua (requires valid JWT)
           ├─ reserved → hold key set (TTL 600s) → SQS message → 200
           ├─ sold_out → 409 Conflict
           └─ already_holding → 409 Conflict (same user, same event)
```

[In this repo] `docs/architecture.md §2` has the full sequence diagram.

---

## 34. reserve.lua annotated

```lua
-- KEYS[1] = seat_inventory_key   e.g. "inventory:event:42"
-- KEYS[2] = user_hold_key        e.g. "hold:event:42:user:abc123"
-- ARGV[1] = hold_ttl_seconds     e.g. "600" (10 minutes)
-- ARGV[2] = seats_requested      e.g. "1"
--
-- Return: {status_code, reason_string}
--   1, "reserved"         → success
--   0, "sold_out"          → not enough seats
--   0, "already_holding"   → user has an active hold
--  -1, "event_not_found"   → inventory key missing

-- Step 1: read inventory counter
local available = tonumber(redis.call('GET', KEYS[1]))

-- Step 2: guard — event never seeded
if available == nil then
    return {-1, 'event_not_found'}
end

-- Step 3: guard — not enough seats
local requested = tonumber(ARGV[2])
if available < requested then
    return {0, 'sold_out'}
end

-- Step 4: guard — same user already has a hold (anti-double-book)
local existing_hold = redis.call('GET', KEYS[2])
if existing_hold then
    return {0, 'already_holding'}
end

-- Step 5: commit — decrement inventory
redis.call('DECRBY', KEYS[1], requested)

-- Step 6: commit — create hold key with TTL (auto-release in 600s)
redis.call('SET', KEYS[2], ARGV[2], 'EX', ARGV[1])

return {1, 'reserved'}
```

Key design decisions:

- **The "sold_out" guard runs before the hold check.** This means if
  seats are 0, we return `sold_out` immediately, without checking the
  hold key. A user with an existing hold still gets `sold_out` when
  seats run out.
- **The hold key is created AFTER the inventory decrement.** This order
  matters. If it were reversed, two concurrent requests from the same
  user could both pass the hold-key check before either decremented the
  inventory.
- **The Lua return is a 2-element array.** go-redis returns this as
  `[]interface{}`. We assert `[0]` is `int64` and `[1]` is `string`.

[In this repo] `scripts/reserve.lua` (35 lines, verbatim from
`docs/architecture.md §4`).

---

## 35. reserve.go — typed Result, status enum

```go
// internal/redis/reserve.go

// Status is the high-level outcome of a reserve attempt.
type Status int

const (
    StatusReserved      Status = 1  // seat allocated, hold key set
    StatusRejected      Status = 0  // see RejectionReason
    StatusEventNotFound Status = -1 // inventory key never seeded
)

// RejectionReason disambiguates StatusRejected.
type RejectionReason int

const (
    ReasonSoldOut        RejectionReason = 1 // no seats left
    ReasonAlreadyHolding RejectionReason = 2 // user already has active hold
)

// Result is the typed return from ReserveSeat.
type Result struct {
    Status Status
    Reason RejectionReason // meaningful only when Status == StatusRejected
}

func ReserveSeat(ctx context.Context, c *redisclient.Client,
    eventID int64, userID string, holdTTLSec, seatsRequested int,
) (Result, error) {
    keys := []string{InventoryKey(eventID), HoldKey(eventID, userID)}
    args := []any{holdTTLSec, seatsRequested}

    raw, err := reserveScript.Run(ctx, c, keys, args...).Result()
    if err != nil {
        return Result{}, fmt.Errorf("redis: reserve script failed: %w", err)
    }

    arr, ok := raw.([]any)
    if !ok || len(arr) != 2 {
        return Result{}, fmt.Errorf("redis: unexpected reply shape %v", raw)
    }

    code, _ := arr[0].(int64)
    reasonStr, _ := arr[1].(string)

    switch code {
    case 1:
        return Result{Status: StatusReserved}, nil
    case -1:
        return Result{Status: StatusEventNotFound}, nil
    case 0:
        switch reasonStr {
        case "sold_out":
            return Result{Status: StatusRejected, Reason: ReasonSoldOut}, nil
        case "already_holding":
            return Result{Status: StatusRejected, Reason: ReasonAlreadyHolding}, nil
        default:
            return Result{}, fmt.Errorf("%w: %q", ErrUnknownRejection, reasonStr)
        }
    default:
        return Result{}, fmt.Errorf("redis: unexpected status code %d", code)
    }
}
```

Key design decisions:

- **The Result type is the only public surface.** Callers never see the
  raw `[1, "reserved"]` array. The type system makes misuse harder.
- **RejectionReason is only meaningful when Status == StatusRejected.**
  The API handler must check Status first before reading Reason.
- **`ErrUnknownRejection` wraps unexpected Lua reason strings.** This would
  only happen if someone edited the Lua and introduced an unrecognized
  reason. It means "the Lua and Go are out of sync" — a build-time bug,
  not a runtime one.
- **`fmt.Errorf` with `%w` chains errors.** The caller's error includes
  the underlying Redis error for logging and debugging.

[In this repo] `internal/redis/reserve.go`. The `InventoryKey()` and
`HoldKey()` helpers are on lines 47-62.

---

## 36. token_bucket.lua annotated

```lua
-- KEYS[1] = bucket state key  e.g. "bucket:event:42"
-- KEYS[2] = queue key         e.g. "queue:event:42"
-- ARGV[1] = capacity           e.g. "100"  (max tokens)
-- ARGV[2] = refill_rate        e.g. "10"   (tokens per second)
-- ARGV[3] = now_ms             current time in milliseconds (ARGV, not TIME)
-- ARGV[4] = user_id            e.g. "u1"
-- ARGV[5] = queue_ttl_seconds  e.g. "300"  (auto-cleanup of stale entries)
--
-- Return: {status_code, payload_string}
--   1, "admitted"         → token consumed
--   0, "<position>"       → queued (1-based position)

-- Step 1: read bucket state (tokens + last refill timestamp)
local bucket = redis.call('HMGET', KEYS[1], 'tokens', 'last_refill_ms')
local tokens = tonumber(bucket[1])
local last_refill_ms = tonumber(bucket[2])

if tokens == nil then
    -- First caller for this event: start full, last_refill = now
    tokens = capacity
    last_refill_ms = now_ms
else
    -- Lazy refill: add tokens based on elapsed time since last refill
    local elapsed_secs = math.max(0, (now_ms - last_refill_ms) / 1000)
    tokens = math.min(capacity, tokens + elapsed_secs * refill_rate)
end

-- Step 2: try to admit (consume one token)
if tokens >= 1 then
    tokens = tokens - 1
    redis.call('HMSET', KEYS[1], 'tokens', tokens, 'last_refill_ms', now_ms)
    redis.call('EXPIRE', KEYS[1], 86400)   -- safety net: bucket state expires in 1 day
    return {1, 'admitted'}
end

-- Step 3: queue the user (bucket is empty)
local cutoff_ms = now_ms - (queue_ttl_secs * 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', cutoff_ms) -- clean stale

redis.call('ZADD', KEYS[2], now_ms, user_id)       -- score = enqueue time (FIFO)
redis.call('EXPIRE', KEYS[2], queue_ttl_secs)        -- auto-cleanup

local zero_based_rank = redis.call('ZRANK', KEYS[2], user_id)
local position = zero_based_rank + 1   -- human-friendly 1-based

return {0, tostring(position)}
```

Key design decisions:

- **Lazy refill.** Tokens are only refilled when a request arrives, not
  continuously in the background. This means the bucket may briefly
  over-refill (one big burst request that drains it, then the next request
  refills it), but the refill is capped at `capacity`. The 1-day TTL on
  the bucket state is a safety net — if no one calls for a day, the
  state is evicted and the next caller starts fresh.
- **The ZSET score is `now_ms`** (millisecond timestamp). This makes the
  queue FIFO by enqueue time. Nanosecond precision would be even fairer,
  but millisecond precision is sufficient for our scale.
- **Stale entry cleanup is inline in the queue path.** Before adding a
  new user, we remove all entries older than `queue_ttl_secs`. This
  prevents the queue from growing unbounded from abandoned sessions.
- **`now_ms` comes from ARGV[3]`, not `TIME`.** This is critical for
  deterministic tests. See §23 pitfall 2.

[In this repo] `scripts/token_bucket.lua` (63 lines).

---

## 37. waitingroom.go — TryAdmit, GetPosition, Promote, Enqueue

Four functions in `waitingroom.go`:

**TryAdmit** — the main entry point (runs the Lua script):

```go
func TryAdmit(ctx context.Context, c *redisclient.Client,
    cfg WaitRoomConfig, userID string, nowMS int64,
) (AdmissionResult, error) {
    keys := []string{bucketKey(cfg.EventID), queueKey(cfg.EventID)}
    args := []any{
        cfg.Capacity,
        cfg.RefillRate,
        nowMS,          // passed in for testability
        userID,
        cfg.QueueTTLSeconds,
    }

    raw, err := tokenBucketScript.Run(ctx, c, keys, args...).Result()
    // ... parse {code, payload} ...
    if code == 1 && payload == "admitted" {
        return AdmissionResult{Admitted: true}, nil
    }
    var pos int
    fmt.Sscanf(payload, "%d", &pos)
    return AdmissionResult{Admitted: false, Position: pos}, nil
}
```

**GetPosition** — used by SSE to report queue position:

```go
func GetPosition(ctx context.Context, c *redisclient.Client,
    eventID int64, userID string,
) (int, error) {
    rank, err := c.ZRank(ctx, queueKey(eventID), userID).Result()
    if err == redisclient.Nil {
        return 0, nil   // not in queue
    }
    // ...
    return int(rank) + 1, nil
}
```

**Promote** — called by the drainer goroutine when a token becomes
available:

```go
func Promote(ctx context.Context, c *redisclient.Client,
    eventID int64, userID string,
) error {
    return c.ZRem(ctx, queueKey(eventID), userID).Err()
}
```

**Enqueue** — explicit enqueue for admin tools or partner integrations
(uses pipeline):

```go
func Enqueue(ctx context.Context, c *redisclient.Client,
    eventID int64, userID string, nowMS int64, ttlSec int,
) (int, error) {
    cutoff := nowMS - int64(ttlSec)*1000
    pipe := c.Pipeline()
    pipe.ZRemRangeByScore(ctx, queueKey(eventID), "-inf", fmt.Sprintf("%d", cutoff))
    pipe.ZAdd(ctx, queueKey(eventID), redisclient.Z{Score: float64(nowMS), Member: userID})
    pipe.Expire(ctx, queueKey(eventID), time.Duration(ttlSec)*time.Second)
    if _, err := pipe.Exec(ctx); err != nil {
        return 0, fmt.Errorf("redis: enqueue pipeline failed: %w", err)
    }
    rank, err := c.ZRank(ctx, queueKey(eventID), userID).Result()
    // ...
    return int(rank) + 1, nil
}
```

[In this repo] `internal/redis/waitingroom.go`.

---

## 38. TestReserve_NoOversell — the headline 1000-goroutine proof

This is the most important test in Phase 2:

```go
func TestReserve_NoOversell(t *testing.T) {
    rdb := newTestClient(t)
    ctx := context.Background()
    const eventID = int64(42000)   // unique per test run

    // Seed: 10 seats available
    require.NoError(t, rdb.Set(ctx, InventoryKey(eventID), 10, 0).Err())
    withCleanKeys(t, rdb, InventoryKey(eventID))

    var reserved, soldOut, alreadyHolding int64
    var wg sync.WaitGroup

    // 1000 concurrent goroutines, each a unique user
    for i := 0; i < 1000; i++ {
        wg.Add(1)
        go func(idx int) {
            defer wg.Done()
            uid := fmt.Sprintf("user-%d", idx)
            res, err := ReserveSeat(ctx, rdb, eventID, uid, 600, 1)
            require.NoError(t, err)
            switch res.Status {
            case StatusReserved:
                atomic.AddInt64(&reserved, 1)
                withCleanKeys(t, rdb, HoldKey(eventID, uid))
            case StatusRejected:
                switch res.Reason {
                case ReasonSoldOut:
                    atomic.AddInt64(&soldOut, 1)
                case ReasonAlreadyHolding:
                    atomic.AddInt64(&alreadyHolding, 1)
                }
            }
        }(i)
    }
    wg.Wait()

    require.Equal(t, int64(10), reserved, "exactly 10 reservations succeeded")
    require.Equal(t, int64(990), soldOut, "990 hit sold_out")
    require.Equal(t, int64(0), alreadyHolding, "no double-book")
}
```

What this proves:

```
Without Lua:  1000 goroutines → 1000 × GET "10" → 1000 × SET "9" → oversell
With Lua:     1000 goroutines → GET → DECRBY → SET → exactly 10 reserved
```

Redis's single command thread serializes the 1000 `EVAL reserve.lua`
calls. They execute one after another:

```
Goroutine 1:  GET 10 → check → DECRBY → SET 9 → SET hold EX 600 → 1
Goroutine 2:  GET 9  → check → DECRBY → SET 8 → SET hold EX 600 → 1
...
Goroutine 10: GET 1  → check → DECRBY → SET 0 → SET hold EX 600 → 1
Goroutine 11: GET 0  → check → return sold_out
...
Goroutine 1000: GET 0 → return sold_out
```

The Lua atomicity guarantee makes this work without any application-level
locks or retries.

[In this repo] `internal/redis/reserve_test.go` lines 43-86.
`docs/architecture.md §4` explains why this matters.

---

## 39. Key naming conventions

This project uses colon-separated namespacing consistently:

```
inventory:event:{event_id}              -- STRING: available seat counter
hold:event:{event_id}:user:{user_id}    -- STRING: per-user hold (TTL 600s)
bucket:event:{event_id}                 -- HASH: {tokens, last_refill_ms}
queue:event:{event_id}                  -- ZSET: {member=user_id, score=now_ms}
```

**Why `event_id` is always the second segment:**

This allows per-event key scanning with `SCAN` if needed:

```redis
SCAN 0 MATCH bucket:event:42*
```

It also groups keys by event, which makes monitoring and debugging
easier (`redis-cli --scan --pattern '*:event:42*'` finds all keys for
event 42).

**Why the waiting room ZSET is FIFO by score:**

```lua
redis.call('ZADD', queue_key, now_ms, user_id)
-- score = now_ms = millisecond timestamp of enqueue
-- ZRANGEBYSCORE queue_key -inf cutoff  → all entries before cutoff
-- ZRANK queue_key user_id               → 0-based position
```

Earlier enqueue time → lower score → lower rank → earlier in ZRANGE.
This is fair FIFO: the first user to call `/enter` when the bucket is
empty is the first to receive a token when one becomes available.

**The drainer's role (Phase 4):**

```
every 100ms:
  ZRANGE queue:event:42 0 (refill_rate/10 - 1)  → top N users
  for each user:
    ZREM queue:event:42 user_id
    issue JWT
```

The drainer runs on each API server instance independently. With multiple
API servers, multiple drainers could race to promote the same user.
Mitigation: the drainer also calls `Promote` (ZREM) which is idempotent —
removing an already-removed member is a no-op. The JWT issue would be
deduplicated by the hold key check (if the user already has a hold, the
reserve path returns `already_holding`).

---

## 40. Pitfalls + future Redis usage

**Pitfall 1 — Eviction race on inventory keys.**

If `maxmemory` is hit and `inventory:event:42` is evicted, the event
appears "not found" (`StatusEventNotFound`). The event was never seeded
or was evicted. We do not distinguish between these two cases in Phase 2.
Mitigation: Phase 5 seeds inventory from Postgres on first access if
missing.

**Pitfall 2 — Hold key under TTL + allkeys-lru interaction.**

If a hold key is evicted under `allkeys-lru` before its 600s TTL fires,
the user loses their reservation without the seat being released. This
is why Phase 7 (expiration handling) is critical — the keyspace
notification only fires on natural TTL expiry, not on eviction. Mitigation:
the sweep job (Approach B in `docs/architecture.md §9`) queries Postgres
for expired reservations and increments inventory regardless of whether
the Redis hold key exists.

**Pitfall 3 — Hot event keys under extreme load.**

`inventory:event:42` receiving 10,000 req/s becomes the bottleneck.
Redis serializes EVAL calls, so throughput = 1 / script_time. At
50 µs/script, max throughput = 20,000/s. At 100 µs/script, 10,000/s.
Mitigation: Redis Cluster (shard by event_id) or read-from-replica for
the GET-heavy path.

**Pitfall 4 — Dual-write between Redis and SQS.**

The reserve flow:

1. `reserve.lua` → Redis hold key + inventory decrement
2. SQS SendMessage → durable record

If step 2 fails (SQS down), step 1 has already committed. The seat is
held for 600s but no Postgres row exists. The sweep job catches this
because the hold key expires naturally. Risk: brief window where
Postgres has no record but Redis does. Acceptable for our consistency
model.

**Pitfall 5 — Lost update (not applicable with Lua).**

GET → decide → SET is a TOCTOU race. Our project uses Lua for all
read-decide-write paths, so this is mitigated. The pitfall is if a
future developer adds a code path that does this outside Lua.

**Pitfall 6 — ABA problem in ZRANK/ZREM.**

User calls `/enter`, gets queued. While waiting, the drainer promotes
them (ZREM). If they call `/enter` again immediately, they're re-added to
the queue at the new timestamp, losing their earlier position. Mitigation:
`GetPosition` checks the hold key first — if the user already has a
reservation, they're not re-admitted to the queue.

---

**Future Redis usage (Phases 4-7):**


| Phase   | Feature                | Redis usage                                                  |
| --------- | ------------------------ | -------------------------------------------------------------- |
| 4       | Per-IP rate limit      | `bucket:ip:{ip}` HASH (separate from event bucket)           |
| 4       | SSE drainer            | Periodic ZRANK per SSE client; Pub/Sub at scale              |
| 6       | Idempotency key        | `idem:{user_id}:{event_id}` STRING with TTL                  |
| 7       | Expiration: Approach A | Keyspace notifications (`PSUBSCRIBE __keyevent@0__:expired`) |
| 7       | Expiration: Approach B | Postgres sweep → Redis INCR (belt-and-suspenders)           |
| 7       | Audit log              | `XADD reservations * ...` Stream for replay                  |
| Stretch | Redis Cluster          | Shard by`event_id` hash tag                                  |

[In this repo] `docs/implementation-plan.md` lines 425-481 (Phase 4)
and lines 595-640 (Phase 7).

---

# Appendix

## A. Reading order for a quickstart

If you only have **30 minutes**, read these in order:

1. §2 (single-threaded) — the one fact that makes everything else make
   sense.
2. §9 (atomic counters) + §13 (ZSETs) — the two data types we use most.
3. §16 (TOCTOU) + §19 (Lua EVAL) — the why and the how of atomicity.
4. §34-§35 (reserve.lua + reserve.go) — the concrete example of the
   patterns above.

That's enough to understand what `internal/redis/reserve.go` does and
why the tests prove what they prove.

If you have **2 hours**, add:

5. §1 (what Redis is) + §3 (keys/namespacing) + §4 (TTL).
6. §22 (atomic patterns) — ties the patterns to our two scripts.
7. §38 (TestReserve_NoOversell) — the 1000-goroutine proof.
8. §31-§33 (project layout + docker-compose config) — where everything
   lives.

If you have an **afternoon**, add:

9. §24-§28 (Pub/Sub, Streams, Pipelines, SCAN) — the building blocks.
10. §36-§37 (token_bucket.lua + waitingroom.go) — the second script.
11. §39 (key naming) + §40 (pitfalls) — operational wisdom.

## B. Tools

### redis-cli (essential)

```bash
# Connectivity
redis-cli ping                          → PONG
redis-cli -h localhost -p 6379 ping

# Read/write
redis-cli GET inventory:event:42
redis-cli SET inventory:event:42 10 EX 0
redis-cli DEL hold:event:42:user:abc

# TTL
redis-cli TTL hold:event:42:user:abc    → 587  (seconds remaining)
redis-cli PTTL hold:event:42:user:abc   → 587000  (ms)

# ZSET (waiting room queue)
redis-cli ZRANGE queue:event:42 0 -1 WITHSCORES
redis-cli ZRANK queue:event:42 user-5    → 4  (0-based)
redis-cli ZREM queue:event:42 user-5

# HASH (bucket state)
redis-cli HGETALL bucket:event:42
redis-cli HINCRBY bucket:event:42 tokens -1

# Server info
redis-cli INFO memory | head -5
redis-cli DBSIZE                          → total key count
redis-cli CLIENT LIST | wc -l             → open connections

# Scan (production-safe KEYS alternative)
redis-cli SCAN 0 MATCH 'inventory:event:*' COUNT 100
```

### redis-cli monitoring (don't run in prod)

```bash
redis-cli MONITOR   # streams every command (very noisy, dev only)
redis-cli --stat    # server stats every 1s (requests, connections, memory)
redis-cli INFO stats | grep -E '(keyspace_hits|keyspace_misses|evicted)'
```

### GUI

**RedisInsight** (official, free) — browse keys, run commands, visualize
data types, see memory usage. Download from redis.io/redis-enterprise/redis-insight.

### Debug (dev only)

```bash
redis-cli DEBUG SLEEP 2   # block the server for 2s (DO NOT run in prod)
redis-cli DEBUG SEGFAULT # crash the server (testing only)
```

### Flags to remember

```
--no-raw   keep binary-safe values as-is (don't decode escape sequences)
--csv      CSV output for scripting
-r N       repeat command N times (useful for load estimation: redis-cli -r 1000 INCR dummy)
```

## C. Glossary

- **AOF** — Append Only File. Redis persistence mode that logs every write
  command.
- **CAS** — Compare-And-Swap. A primitive where you set a value only if it
  matches an expected old value. Memcached has this; Redis does not need
  it because of Lua.
- **Cluster** — Redis's horizontal scaling mode. 16,384 slots distributed
  across nodes. Keys are routed by hash of the key name (or hash tag if
  present).
- **DBSIZE** — `redis-cli DBSIZE` returns the total number of keys in
  the default database. O(1) metadata operation.
- **Eviction** — automatic removal of a key when `maxmemory` is reached
  and a policy requires freeing space.
- **Hash tag** — the `{}` substring in a key like `event:{42}:hold` used
  to force two keys to the same cluster slot.
- **Hot key** — a key receiving very high traffic. In single-threaded
  Redis, a hot key serializes all requests that touch it.
- **Keyspace notification** — pub/sub channel for Redis internal events
  (key creation, expiry, eviction). Enabled with `--notify-keyspace-events`.
- **LRU** — Least Recently Used. Eviction policy that removes the key
  whose value was accessed longest ago. Redis approximates LRU by sampling.
- **LFU** — Least Frequently Used. Eviction policy that removes the
  least frequently accessed keys (Redis 4+).
- **Lua** — the scripting language embedded in Redis. Scripts run
  atomically on the single command thread.
- **MULTI/EXEC** — Redis transactions. Commands are queued and executed
  atomically, but without isolation between queued commands.
- **NOSCRIPT** — error returned by EVALSHA when the server has evicted
  the script from its cache. Client must fall back to EVAL.
- **RDB** — Redis Database. Point-in-time snapshot persistence format.
- **Replica** — Redis term for a read-replica (called "slave" in older
  versions). Not used in this project.
- **Script** — a Lua script executed by Redis via EVAL or EVALSHA.
- **SHA1** — the 40-character hexadecimal hash that uniquely identifies a
  Lua script. Used for EVALSHA caching.
- **Slot** — one of 16,384 hash slots in Redis Cluster. All keys in a
  command (or script KEYS[]) must be in the same slot.
- **TOCTOU** — Time-Of-Check to Time-Of-Use. The race condition where
  the result of a read is used to make a decision that a later write
  acts on — with a window between the two where another client can
  change the state.
- **TTL** — Time To Live. Remaining lifetime of a key in seconds (or
  milliseconds with PTTL).
- **ZSET** — Sorted Set. A set where each member has an associated
  score, sorted by score.
- **ZRANK / ZREVRANK** — 0-based rank of a member in a ZSET (lowest
  score first, or highest score first with ZREVRANK).

---

_Last touched on the day this file was added to `docs/`. Once you've
read it once, return to specific sections rather than re-reading end
to end. For a quick orientation, start with Appendix A._
