// Package redis — idempotency.go: thin wrapper over Redis for the
// Idempotency-Key pattern (Layer 1 item 5 of docs/product-roadmap.md).
//
// Per the spec:
//
//   1. On request, GET idempotency:<key>. If the cached body_hash
//      matches the current request body, return the cached response
//      with its recorded HTTP status.
//   2. On the first call, run the underlying operation (reserve, etc.).
//      On success, SETEX the response under the key with a long TTL
//      (24h by default — long enough for retries, short enough to
//      self-clean).
//   3. Same key + DIFFERENT body → 409 idempotency_key_mismatch. The
//      safe Stripe behaviour: catches client bugs that retry with a
//      different request without bumping the key.
//
// Concurrent retries against the same key (network blip + impatient
// client) are handled via a short-TTL SETNX "in-flight" lock, so a
// slow first call doesn't get its response overwritten by a racing
// retry.
//
// Storage shape (a single Redis STRING per key — one round-trip):
//
//   {
//     "body_hash": "<sha256 hex of request body>",
//     "status":    <http status code>,
//     "response":  <original response body as JSON>
//   }
//
// Body-hash co-storage prevents the "user retries with a different
// body under the same key" footgun; status-co-storage lets the API
// preserve the original HTTP status on cache hits.
package redis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	redisclient "github.com/redis/go-redis/v9"
)

// ErrIdempotencyBodyMismatch is the sentinel returned by Lookup when
// the cached entry's body_hash differs from the current request body.
// Callers translate this into 409 idempotency_key_mismatch.
// Detected via errors.Is — the wrapped error carries the two hashes
// for ops debugging (without leaking the request body itself).
var ErrIdempotencyBodyMismatch = errors.New("redis: idempotency body mismatch")

// ErrIdempotencyCacheCorrupt is returned when the Redis value under
// the idempotency key exists but can't be unmarshalled. Treated by
// the caller as a cache miss + warning logged, not an error to the user.
var ErrIdempotencyCacheCorrupt = errors.New("redis: idempotency cache corrupt")

// cachedRecord is the on-the-wire JSON shape stored under each key.
// Defined here (not exported) so callers don't depend on storage.
type cachedRecord struct {
	BodyHash string          `json:"body_hash"` // sha256 hex of request body
	Status   int             `json:"status"`    // HTTP status code of original response
	Response json.RawMessage `json:"response"`  // original JSON response body
}

// IdempotencyConfig tunes the wrapper's two TTLs:
//
//   CacheTTL:  spec-mandated 24 h (long enough to span retries across
//              user-network blips, short enough to self-clean the keyspace).
//   LockTTL:   short — only needs to outlast the underlying operation
//              (Lua reserve + SQS publish = ~30 ms typical). 5 s is
//              generous; covers a paused API process (e.g. SIGSTOP)
//              without permanently blocking retries.
type IdempotencyConfig struct {
	CacheTTL time.Duration
	LockTTL  time.Duration
}

// Default Idempotency values match docs/product-roadmap.md §5.
const (
	DefaultIdempotencyCacheTTL = 24 * time.Hour
	DefaultIdempotencyLockTTL  = 5 * time.Second
)

// Idempotency is the wrapper. One per process (no per-key state;
// everything is in Redis under stable key prefixes).
type Idempotency struct {
	rdb      *Client
	cacheTTL time.Duration
	lockTTL  time.Duration
}

// NewIdempotency constructs a wrapper with the given TTLs.
// Zero values fall back to the spec defaults.
func NewIdempotency(rdb *Client, cfg IdempotencyConfig) *Idempotency {
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = DefaultIdempotencyCacheTTL
	}
	if cfg.LockTTL == 0 {
		cfg.LockTTL = DefaultIdempotencyLockTTL
	}
	return &Idempotency{
		rdb:      rdb,
		cacheTTL: cfg.CacheTTL,
		lockTTL:  cfg.LockTTL,
	}
}

// CacheKey is the canonical Redis key for a cached response.
// Exported so tests / observability can name it (e.g. span attributes,
// logs that mention the key, debug-shell queries).
func CacheKey(key string) string {
	return "idempotency:" + key
}

// LockKey is the canonical Redis key for the in-flight lock.
// Distinct suffix so the lock can't collide with the cache key.
func LockKey(key string) string {
	return "idempotency:" + key + ":inflight"
}

// HashBody returns the sha256-hex digest of the request body.
// Empty body is a valid hash (sha256("") is a stable well-known value),
// so callers don't need to special-case "no body".
func HashBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Look up a cached response. Three outcomes (sentinel error + bool):
//
//   hit=true,  err=nil        — cached, body matched. Status + Response
//                               populated from the cached entry.
//   hit=false, err=nil        — no cached entry (Redis nil), OR
//                               cache-corrupt (treat the same as miss).
//   hit=false, err wraps ErrIdempotencyBodyMismatch
//                             — cached but body differs → caller
//                               returns 409 idempotency_key_mismatch.
//
// Body-mismatch detection is built-in; saves the caller from having to
// compare hashes after a hit.
func (i *Idempotency) Lookup(ctx context.Context, key, bodyHash string) (status int, body []byte, hit bool, err error) {
	raw, gerr := i.rdb.Get(ctx, CacheKey(key)).Bytes()
	if errors.Is(gerr, redisclient.Nil) {
		return 0, nil, false, nil
	}
	if gerr != nil {
		return 0, nil, false, fmt.Errorf("idempotency: redis GET: %w", gerr)
	}

	var rec cachedRecord
	if uerr := json.Unmarshal(raw, &rec); uerr != nil {
		// Cache entry exists but is malformed (manual tampering, schema
		// drift after a deploy, etc.). Treat as a miss + log. Returning
		// an error to the user would be wrong — the bad cache entry
		// will TTL out.
		return 0, nil, false, fmt.Errorf("%w: %v", ErrIdempotencyCacheCorrupt, uerr)
	}

	if rec.BodyHash != bodyHash {
		return 0, nil, false, fmt.Errorf("%w: cached=%s request=%s",
			ErrIdempotencyBodyMismatch, rec.BodyHash, bodyHash)
	}

	return rec.Status, []byte(rec.Response), true, nil
}

// AcquireLock attempts to claim the in-flight lock for `key`.
// Returns true if acquired, false if another concurrent retry holds it.
// Lock has the configured short TTL so a crashed holder doesn't block
// future retries permanently.
func (i *Idempotency) AcquireLock(ctx context.Context, key string) (bool, error) {
	ok, err := i.rdb.SetNX(ctx, LockKey(key), "1", i.lockTTL).Result()
	if err != nil {
		return false, fmt.Errorf("idempotency: redis SETNX lock: %w", err)
	}
	return ok, nil
}

// ReleaseLock removes the in-flight lock. Called on both success and
// failure of the underlying operation so retries aren't permanently
// blocked. Failure here is logged by the caller — not bubbled up —
// because the lock has a short TTL and will auto-clear.
func (i *Idempotency) ReleaseLock(ctx context.Context, key string) error {
	if err := i.rdb.Del(ctx, LockKey(key)).Err(); err != nil {
		return fmt.Errorf("idempotency: redis DEL lock: %w", err)
	}
	return nil
}

// Store writes the response under (key, bodyHash) with the long cache
// TTL. Replaces any prior entry (a previous successful call under
// the same key, which is impossible once the lock is held — defensive
// against future use cases where the same key might be reused).
func (i *Idempotency) Store(ctx context.Context, key, bodyHash string, status int, body []byte) error {
	rec := cachedRecord{
		BodyHash: bodyHash,
		Status:   status,
		Response: json.RawMessage(body),
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("idempotency: marshal cached record: %w", err)
	}
	if err := i.rdb.Set(ctx, CacheKey(key), encoded, i.cacheTTL).Err(); err != nil {
		return fmt.Errorf("idempotency: redis SET cache: %w", err)
	}
	return nil
}
