package redis

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWaitRoom_AdmitsThenQueues: with capacity=3, first 3 users are admitted,
// next 2 are queued at positions 1, 2.
func TestWaitRoom_AdmitsThenQueues(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()
	const eventID = int64(50000)
	cfg := WaitRoomConfig{
		EventID:         eventID,
		Capacity:        3,
		RefillRate:      0.5, // 1 token per 2 seconds
		QueueTTLSeconds: 60,
	}
	withCleanKeys(t, rdb, bucketKey(eventID), queueKey(eventID))

	baseTime := int64(1_000_000) // arbitrary fixed start time for determinism

	// First 3 admitted immediately
	for i := 0; i < 3; i++ {
		res, err := TryAdmit(ctx, rdb, cfg, fmt.Sprintf("u%d", i), baseTime)
		require.NoError(t, err)
		require.True(t, res.Admitted, "user %d should be admitted (capacity not exhausted)", i)
	}

	// Next 2 queued at 1, 2
	for i := 3; i < 5; i++ {
		res, err := TryAdmit(ctx, rdb, cfg, fmt.Sprintf("u%d", i), baseTime)
		require.NoError(t, err)
		require.False(t, res.Admitted, "user %d should be queued", i)
		require.Equal(t, i-2, res.Position, "user %d position should be %d", i, i-2)
	}
}

// TestWaitRoom_Refill: tokens refill at the configured rate, capped at capacity.
func TestWaitRoom_Refill(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()
	const eventID = int64(50001)
	cfg := WaitRoomConfig{
		EventID:         eventID,
		Capacity:        2,
		RefillRate:      1.0, // 1 token per second
		QueueTTLSeconds: 60,
	}
	withCleanKeys(t, rdb, bucketKey(eventID), queueKey(eventID))

	// Drain bucket at t=0
	res, err := TryAdmit(ctx, rdb, cfg, "drain-1", 0)
	require.NoError(t, err)
	require.True(t, res.Admitted)

	res, err = TryAdmit(ctx, rdb, cfg, "drain-2", 0)
	require.NoError(t, err)
	require.True(t, res.Admitted)

	// At t=0.5s: bucket has refilled 0.5 tokens → still < 1, so queued
	res, err = TryAdmit(ctx, rdb, cfg, "queued-1", 500)
	require.NoError(t, err)
	require.False(t, res.Admitted, "bucket not yet full enough at 0.5s")

	// At t=2.5s: bucket has refilled 2.5 tokens, capped at capacity (2) → admitted
	res, err = TryAdmit(ctx, rdb, cfg, "refilled-1", 2500)
	require.NoError(t, err)
	require.True(t, res.Admitted, "should have refilled 1 token by 2.5s")

	res, err = TryAdmit(ctx, rdb, cfg, "refilled-2", 2600)
	require.NoError(t, err)
	require.True(t, res.Admitted, "second refill should also work")

	// Capacity cap: bucket refills LAZILY up to capacity; long waits never
	// accumulate more than `capacity` tokens. Verify by waiting a long time
	// then draining twice (back to 0); a third admission at the same instant
	// should fail because the bucket is drained.
	res, err = TryAdmit(ctx, rdb, cfg, "cap-1", 100000) // t=100s; refill capped at 2
	require.NoError(t, err)
	require.True(t, res.Admitted, "after 100s, bucket should be at capacity = 2 → admit")

	res, err = TryAdmit(ctx, rdb, cfg, "cap-2", 100000)
	require.NoError(t, err)
	require.True(t, res.Admitted, "second token in same bucket → also admit")

	// Immediately after draining both: bucket is empty (refill rate is 1/sec).
	res, err = TryAdmit(ctx, rdb, cfg, "over-cap", 100000)
	require.NoError(t, err)
	require.False(t, res.Admitted,
		"drained bucket at the same moment should queue (refill hasn't happened yet)")
}

// TestWaitRoom_GetPosition: position 0 means "not in queue".
func TestWaitRoom_GetPosition(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()
	const eventID = int64(50002)
	cfg := WaitRoomConfig{
		EventID:         eventID,
		Capacity:        1,
		RefillRate:      0.1, // tiny refill → easy to queue
		QueueTTLSeconds: 60,
	}
	withCleanKeys(t, rdb, bucketKey(eventID), queueKey(eventID))

	// User not in queue → 0
	pos, err := GetPosition(ctx, rdb, eventID, "ghost-user")
	require.NoError(t, err)
	require.Equal(t, 0, pos)

	// Take the only token
	res, err := TryAdmit(ctx, rdb, cfg, "first", 0)
	require.NoError(t, err)
	require.True(t, res.Admitted)

	// Queue three users
	for i := 0; i < 3; i++ {
		res, err := TryAdmit(ctx, rdb, cfg, fmt.Sprintf("user-%d", i), int64(100+i))
		require.NoError(t, err)
		require.False(t, res.Admitted)
	}

	// Position 1, 2, 3 by enqueue order
	for i, want := range []int{1, 2, 3} {
		pos, err := GetPosition(ctx, rdb, eventID, fmt.Sprintf("user-%d", i))
		require.NoError(t, err)
		require.Equal(t, want, pos)
	}

	// Promote the head user → position 0 (not in queue)
	require.NoError(t, Promote(ctx, rdb, eventID, "user-0"))
	pos, err = GetPosition(ctx, rdb, eventID, "user-0")
	require.NoError(t, err)
	require.Equal(t, 0, pos, "promoted user should be removed from queue")
}

// TestWaitRoom_Promote also tests explicit Enqueue as the helper surface.
func TestWaitRoom_Enqueue(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()
	const eventID = int64(50003)
	withCleanKeys(t, rdb, bucketKey(eventID), queueKey(eventID))

	// Enqueue user-1 and user-2 directly
	pos, err := Enqueue(ctx, rdb, eventID, "user-1", 1000, 60)
	require.NoError(t, err)
	require.Equal(t, 1, pos)

	pos, err = Enqueue(ctx, rdb, eventID, "user-2", 2000, 60)
	require.NoError(t, err)
	require.Equal(t, 2, pos)

	// user-1 head-of-queue
	p, err := GetPosition(ctx, rdb, eventID, "user-1")
	require.NoError(t, err)
	require.Equal(t, 1, p)
}
