package iplimit

import (
	"context"
	"testing"

	redisclient "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// newTestClient connects to localhost:6379 and skips the test if Redis
// is unavailable, so tests gracefully degrade in CI without Redis.
func newTestClient(t *testing.T) *redisclient.Client {
	t.Helper()
	c := redisclient.NewClient(&redisclient.Options{Addr: "localhost:6379"})
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis unavailable at localhost:6379: %v", err)
	}
	return c
}

// withCleanKey registers a DEL cleanup for the given key.
func withCleanKey(t *testing.T, c *redisclient.Client, key string) {
	t.Helper()
	t.Cleanup(func() {
		_ = c.Del(context.Background(), key).Err()
	})
}

// TestCheck_AdmitThenReject: with capacity=3, first 3 calls admitted,
// 4th rejected.
func TestCheck_AdmitThenReject(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()
	const ip = "5.6.7.8"
	cfg := Config{Capacity: 3, RefillRate: 0.5}
	withCleanKey(t, rdb, bucketKey(ip))

	baseTime := int64(1_000_000)

	// First 3 admitted
	for i := 0; i < 3; i++ {
		ok, err := Check(ctx, rdb, cfg, ip, baseTime)
		require.NoError(t, err)
		require.True(t, ok, "call %d should be admitted", i+1)
	}

	// 4th rejected
	ok, err := Check(ctx, rdb, cfg, ip, baseTime)
	require.NoError(t, err)
	require.False(t, ok, "4th call should be rejected (bucket exhausted)")
}

// TestCheck_Refill: tokens refill at the configured rate.
func TestCheck_Refill(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()
	const ip = "5.6.7.9"
	cfg := Config{Capacity: 2, RefillRate: 10.0} // 10 tokens/sec
	withCleanKey(t, rdb, bucketKey(ip))

	// Drain bucket at t=0
	ok, err := Check(ctx, rdb, cfg, ip, 0)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = Check(ctx, rdb, cfg, ip, 0)
	require.NoError(t, err)
	require.True(t, ok)

	// Bucket now empty. At t=100ms → 1 token refilled (10/s × 0.1s = 1)
	ok, err = Check(ctx, rdb, cfg, ip, 100)
	require.NoError(t, err)
	require.True(t, ok, "at 100ms, 1 token should have refilled → admit")

	// Bucket empty again. At t=200ms → another 1 token refilled → admit
	ok, err = Check(ctx, rdb, cfg, ip, 200)
	require.NoError(t, err)
	require.True(t, ok)

	// At t=200ms still: bucket exhausted, no refill happened yet → reject
	ok, err = Check(ctx, rdb, cfg, ip, 200)
	require.NoError(t, err)
	require.False(t, ok, "bucket should be empty at same timestamp")
}

// TestCheck_CapacityCap: long waits never accumulate beyond capacity.
func TestCheck_CapacityCap(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()
	const ip = "5.6.7.10"
	cfg := Config{Capacity: 2, RefillRate: 100.0} // very fast refill
	withCleanKey(t, rdb, bucketKey(ip))

	// Drain at t=0
	require.True(t, mustCheck(ctx, rdb, cfg, ip, 0))
	require.True(t, mustCheck(ctx, rdb, cfg, ip, 0))

	// Wait a long time (t=10s → 1000 tokens, capped at capacity=2)
	ok, err := Check(ctx, rdb, cfg, ip, 10_000)
	require.NoError(t, err)
	require.True(t, ok, "after 10s, bucket should be at capacity=2 → admit")

	ok, err = Check(ctx, rdb, cfg, ip, 10_000)
	require.NoError(t, err)
	require.True(t, ok, "second token in same bucket → also admit")

	// Same instant: bucket drained again → reject
	ok, err = Check(ctx, rdb, cfg, ip, 10_000)
	require.NoError(t, err)
	require.False(t, ok, "drained at same instant → reject (no refill yet)")
}

// TestCheck_DifferentIPsIndependent: two IPs share nothing.
func TestCheck_DifferentIPsIndependent(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()
	cfg := Config{Capacity: 1, RefillRate: 0.1}
	withCleanKey(t, rdb, bucketKey("ip-a"))
	withCleanKey(t, rdb, bucketKey("ip-b"))

	require.True(t, mustCheck(ctx, rdb, cfg, "ip-a", 0))
	require.False(t, mustCheck(ctx, rdb, cfg, "ip-a", 0), "ip-a exhausted")

	// ip-b is completely independent
	require.True(t, mustCheck(ctx, rdb, cfg, "ip-b", 0))
	require.False(t, mustCheck(ctx, rdb, cfg, "ip-b", 0))
}

// mustCheck is a test helper that fatalizes on error.
func mustCheck(ctx context.Context, rdb *redisclient.Client, cfg Config, ip string, nowMS int64) bool {
	ok, err := Check(ctx, rdb, cfg, ip, nowMS)
	if err != nil {
		panic("Check error: " + err.Error())
	}
	return ok
}
