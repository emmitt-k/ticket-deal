// Package iplimit implements per-IP rate limiting via a Redis Lua token
// bucket. It is the first gate in /enter: an exhausted IP receives a
// hard 429 before it ever touches the per-event waiting room bucket.
package iplimit

import (
	"context"
	_ "embed"
	"fmt"

	redisclient "github.com/redis/go-redis/v9"
)

//go:embed scripts/ip_bucket.lua
var ipBucketScriptSrc string

var ipBucketScript = redisclient.NewScript(ipBucketScriptSrc)

// Config holds the per-IP token bucket parameters.
type Config struct {
	// Capacity is the max tokens (burst size) per IP.
	// E.g. 10 means an IP can fire 10 requests "instantly" before being limited.
	Capacity int
	// RefillRate is tokens per second per IP.
	// E.g. 2 means each IP earns 2 tokens every second after the bucket drains.
	RefillRate float64
}

// bucketKey returns the Redis key for an IP's bucket state.
//
//	key format: "ip:1.2.3.4"
func bucketKey(ip string) string {
	return fmt.Sprintf("ip:%s", ip)
}

// Check atomically checks-and-consumes one IP token using the Lua script.
// Returns true if admitted, false if the IP is exhausted (caller should 429).
//
// nowMS is passed by the caller (not computed inside Lua) to keep tests deterministic.
// In production pass time.Now().UnixMilli().
func Check(ctx context.Context, rdb *redisclient.Client,
	cfg Config, ip string, nowMS int64,
) (bool, error) {
	keys := []string{bucketKey(ip)}
	args := []any{cfg.Capacity, cfg.RefillRate, nowMS}

	raw, err := ipBucketScript.Run(ctx, rdb, keys, args...).Result()
	if err != nil {
		return false, fmt.Errorf("redis: ip bucket script failed: %w", err)
	}

	arr, ok := raw.([]any)
	if !ok || len(arr) != 2 {
		return false, fmt.Errorf("redis: unexpected reply shape %v", raw)
	}

	code, _ := arr[0].(int64)
	payload, _ := arr[1].(string)

	if code == 1 && payload == "admitted" {
		return true, nil
	}
	// code == 0 && payload == "rejected"
	return false, nil
}
