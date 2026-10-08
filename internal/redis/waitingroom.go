package redis

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	redisclient "github.com/redis/go-redis/v9"
)

// ErrQueueEmpty is returned by ZPopMin when the queue has no members.
var ErrQueueEmpty = errors.New("redis: queue is empty")

//go:embed scripts/token_bucket.lua
var tokenBucketScriptSrc string

var tokenBucketScript = redisclient.NewScript(tokenBucketScriptSrc)

// AdmissionResult is the typed return from TryAdmit.
type AdmissionResult struct {
	Admitted bool // true = got a token; false = queued
	Position int  // 1-based queue position; meaningful only when !Admitted
}

// WaitRoomConfig holds the bucket parameters.
//
// Capacity: max tokens (burst size). E.g. 100 means up to 100 users
//
//	can be admitted "instantly" if the bucket is full.
//
// RefillRate: tokens per second (steady-state admission rate).
//
//	E.g. 10 = 10 users/sec once the bucket is drained.
//
// QueueTTLSeconds: auto-cleanup window for abandoned queue entries.
//   Prevents unbounded growth from disconnected users.
type WaitRoomConfig struct {
	EventID         int64
	Capacity        int
	RefillRate      float64
	QueueTTLSeconds int
}

// bucketKey returns the bucket state key for an event.
//
//   "bucket:event:42"
func bucketKey(eventID int64) string {
	return fmt.Sprintf("bucket:event:%d", eventID)
}

// queueKey returns the queue key for an event.
//
//   "queue:event:42"
func queueKey(eventID int64) string {
	return fmt.Sprintf("queue:event:%d", eventID)
}

// TryAdmit runs the token bucket atomically: refills tokens if time has
// passed, then either consumes one (Admitted=true) or queues the user
// (Admitted=false with 1-based Position).
//
// nowMS is passed in by the caller (instead of using Lua's `TIME`) to
// keep tests deterministic. In production, pass time.Now().UnixMilli().
//
// Lua KEYS:
//   - KEYS[1] = bucket state key (bucketKey)
//   - KEYS[2] = queue key        (queueKey)
//
// Lua ARGV:
//   - ARGV[1] = capacity
//   - ARGV[2] = refill_rate (tokens/sec)
//   - ARGV[3] = now_ms (current time in ms)
//   - ARGV[4] = user_id
//   - ARGV[5] = queue TTL seconds
func TryAdmit(ctx context.Context, c *redisclient.Client,
	cfg WaitRoomConfig, userID string, nowMS int64,
) (AdmissionResult, error) {
	keys := []string{bucketKey(cfg.EventID), queueKey(cfg.EventID)}
	args := []any{
		cfg.Capacity,
		cfg.RefillRate,
		nowMS,
		userID,
		cfg.QueueTTLSeconds,
	}

	raw, err := tokenBucketScript.Run(ctx, c, keys, args...).Result()
	if err != nil {
		return AdmissionResult{}, fmt.Errorf("redis: token bucket script failed: %w", err)
	}

	arr, ok := raw.([]any)
	if !ok || len(arr) != 2 {
		return AdmissionResult{}, fmt.Errorf("redis: unexpected reply shape %v", raw)
	}

	code, _ := arr[0].(int64)
	payload, _ := arr[1].(string)

	if code == 1 && payload == "admitted" {
		return AdmissionResult{Admitted: true}, nil
	}
	if code == 0 {
		var pos int
		if _, err := fmt.Sscanf(payload, "%d", &pos); err != nil {
			return AdmissionResult{}, fmt.Errorf("redis: bad position payload %q", payload)
		}
		return AdmissionResult{Admitted: false, Position: pos}, nil
	}
	return AdmissionResult{}, fmt.Errorf("redis: unexpected code/payload %d/%q", code, payload)
}

// GetPosition returns a user's 1-based queue position.
// Returns 0 if the user is not in the queue.
func GetPosition(ctx context.Context, c *redisclient.Client,
	eventID int64, userID string,
) (int, error) {
	rank, err := c.ZRank(ctx, queueKey(eventID), userID).Result()
	if err == redisclient.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("redis: ZRANK failed: %w", err)
	}
	return int(rank) + 1, nil
}

// Promote removes a user from the queue. The drainer goroutine (Phase 4)
// calls this when a token becomes available for a queued user.
func Promote(ctx context.Context, c *redisclient.Client,
	eventID int64, userID string,
) error {
	if err := c.ZRem(ctx, queueKey(eventID), userID).Err(); err != nil {
		return fmt.Errorf("redis: ZREM failed: %w", err)
	}
	return nil
}

// Enqueue puts a user at the end of the queue explicitly. Most callers don't
// need this — TryAdmit queues atomically when it can't admit. Exposed for
// programmatic enqueuing (e.g. admin tools, partner integrations).
//
// Returns the user's 1-based queue position.
func Enqueue(ctx context.Context, c *redisclient.Client,
	eventID int64, userID string, nowMS int64, ttlSec int,
) (int, error) {
	cutoff := nowMS - int64(ttlSec)*1000
	pipe := c.Pipeline()
	pipe.ZRemRangeByScore(ctx, queueKey(eventID), "-inf", fmt.Sprintf("%d", cutoff))
	pipe.ZAdd(ctx, queueKey(eventID), redisclient.Z{
		Score:  float64(nowMS),
		Member: userID,
	})
	pipe.Expire(ctx, queueKey(eventID), time.Duration(ttlSec)*time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("redis: enqueue pipeline failed: %w", err)
	}
	rank, err := c.ZRank(ctx, queueKey(eventID), userID).Result()
	if err != nil {
		return 0, fmt.Errorf("redis: ZRANK after enqueue failed: %w", err)
	}
	return int(rank) + 1, nil
}

// ZPopMin removes and returns the userID with the lowest score (earliest
// enqueue time) from the event's queue. Used by the drainer to promote
// the head of the queue when a token refills.
//
// Returns ErrQueueEmpty if the queue is empty.
func ZPopMin(ctx context.Context, c *redisclient.Client, eventID int64) (string, error) {
	result, err := c.ZPopMin(ctx, queueKey(eventID), 1).Result()
	if err == redisclient.Nil {
		return "", ErrQueueEmpty
	}
	if err != nil {
		return "", fmt.Errorf("redis: ZPopMin failed: %w", err)
	}
	if len(result) == 0 {
		return "", ErrQueueEmpty
	}
	return result[0].Member.(string), nil
}

// QueueSize returns the number of users currently waiting in the event's queue.
func QueueSize(ctx context.Context, c *redisclient.Client, eventID int64) (int, error) {
	n, err := c.ZCard(ctx, queueKey(eventID)).Result()
	if err != nil {
		return 0, fmt.Errorf("redis: ZCard failed: %w", err)
	}
	return int(n), nil
}
