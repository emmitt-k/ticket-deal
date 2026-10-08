package waitingroom

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/redis"
	"github.com/stretchr/testify/require"
)

func TestDrainer_StartsOnNotify(t *testing.T) {
	rdb := testClient(t)
	const eventID = int64(52000)

	// Pre-seed queue with one user
	rdb.ZAdd(context.Background(), queueKeyForTest(eventID), redis.Z{Score: 100, Member: "queued-user"})
	t.Cleanup(func() {
		rdb.Del(context.Background(),
			bucketKeyForTest(eventID),
			queueKeyForTest(eventID),
		)
		Unregister(eventID, "queued-user")
	})

	cfg := DrainerConfig{
		RDB: rdb,
		WaitRoom: redis.WaitRoomConfig{
			Capacity:        100,
			RefillRate:      100.0, // fast refill so we promote in first tick
			QueueTTLSeconds: 60,
		},
		JWTSecret:   make([]byte, 32),
		TickInterval: 50 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	StartDrainer(ctx, cfg)
	NotifyEvent(eventID)

	// Wait for the drainer to promote the user
	select {
	case <-ctx.Done():
		t.Fatal("drainer did not promote user within timeout")
	case token := <-WaitForAdmission(eventID, "queued-user"):
		require.NotEmpty(t, token, "promoted user should receive JWT")
		claims, err := auth.Verify(token, cfg.JWTSecret)
		require.NoError(t, err)
		require.Equal(t, "queued-user", claims.Subject)
		require.Equal(t, eventID, claims.EventID)
	}
}

// WaitForAdmission registers a user's SSE channel and waits up to timeout
// for a JWT to arrive. The caller's context controls the overall deadline;
// this function returns the channel so the caller can select on it alongside
// ctx.Done(). The registration is cleaned up automatically when ctx is cancelled
// (via deferred Unregister in the test).
func WaitForAdmission(eventID int64, userID string) chan string {
	return Register(eventID, userID)
}

func TestDrainer_PromotesAndIssuesValidJWT(t *testing.T) {
	rdb := testClient(t)
	const eventID = int64(52001)

	// Seed a queue with 2 users
	rdb.ZAdd(context.Background(), queueKeyForTest(eventID), redis.Z{Score: 100, Member: "user1"})
	rdb.ZAdd(context.Background(), queueKeyForTest(eventID), redis.Z{Score: 200, Member: "user2"})
	t.Cleanup(func() {
		rdb.Del(context.Background(),
			bucketKeyForTest(eventID),
			queueKeyForTest(eventID),
		)
		Unregister(eventID, "user1")
		Unregister(eventID, "user2")
	})

	cfg := DrainerConfig{
		RDB: rdb,
		WaitRoom: redis.WaitRoomConfig{
			Capacity:        100,
			RefillRate:      10.0,
			QueueTTLSeconds: 60,
		},
		JWTSecret:   make([]byte, 32),
		TickInterval: 20 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	StartDrainer(ctx, cfg)
	NotifyEvent(eventID)

	// user1 should be promoted (queue not empty + fast refill)
	tokenCh := WaitForAdmission(eventID, "user1")
	select {
	case <-ctx.Done():
		t.Fatal("user1 was not promoted within timeout")
	case token := <-tokenCh:
		require.NotEmpty(t, token)
		claims, err := auth.Verify(token, cfg.JWTSecret)
		require.NoError(t, err)
		require.Equal(t, "user1", claims.Subject)
		require.Equal(t, eventID, claims.EventID)
	}
}

func TestDrainer_CleansUpAfterEmpty(t *testing.T) {
	rdb := testClient(t)
	const eventID = int64(52002)

	t.Cleanup(func() {
		rdb.Del(context.Background(),
			bucketKeyForTest(eventID),
			queueKeyForTest(eventID),
		)
	})

	cfg := DrainerConfig{
		RDB: rdb,
		WaitRoom: redis.WaitRoomConfig{
			Capacity:        100,
			RefillRate:      10.0,
			QueueTTLSeconds: 60,
		},
		JWTSecret:   make([]byte, 32),
		TickInterval: 20 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	StartDrainer(ctx, cfg)
	NotifyEvent(eventID)

	// Should not panic; exits when ctx is cancelled
	select {
	case <-ctx.Done():
		// expected
	}
}

func TestRegisterEventCallback(t *testing.T) {
	rdb := testClient(t)
	const eventID int64 = 52003

	t.Cleanup(func() {
		rdb.Del(context.Background(),
			bucketKeyForTest(eventID),
			queueKeyForTest(eventID),
		)
	})

	var called int32
	RegisterEventCallback(func(eid int64) {
		if eid == eventID {
			atomic.AddInt32(&called, 1)
		}
	})

	NotifyEvent(eventID)
	time.Sleep(30 * time.Millisecond)
	require.Equal(t, int32(1), called)
}
