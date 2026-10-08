package redis

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	redisclient "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// newTestClient connects to the live Redis at localhost:6379 (docker compose).
// Skips the test gracefully if Redis is unreachable so CI without Redis
// can still pass other packages.
func newTestClient(t *testing.T) *redisclient.Client {
	t.Helper()
	c := NewClient(Config{Addr: "localhost:6379"})
	if err := Ping(context.Background(), c); err != nil {
		t.Skipf("Redis unavailable at localhost:6379: %v", err)
	}
	return c
}

// withCleanKeys registers DEL cleanup for each given key.
func withCleanKeys(t *testing.T, c *redisclient.Client, keys ...string) {
	t.Helper()
	t.Cleanup(func() {
		if err := c.Del(context.Background(), keys...).Err(); err != nil {
			t.Logf("warning: cleanup of %v failed: %v", keys, err)
		}
	})
}

// TestReserve_NoOversell is the HEADLINE test of Phase 2.
// 1000 concurrent goroutines fight for 10 seats — exactly 10 win.
//
// Without Lua atomicity, this fails catastrophically (100 users could
// each see "9 seats available" and decrement to oversell).
// With Lua, Redis serializes the scripts: 1, 2, 3, ..., 10 → reserved.
// The 11th sees 0 → sold_out. The 12th sees 0 → sold_out. Etc.
func TestReserve_NoOversell(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()
	const eventID = int64(42000)

	require.NoError(t, rdb.Set(ctx, InventoryKey(eventID), 10, 0).Err())
	withCleanKeys(t, rdb, InventoryKey(eventID))

	var reserved, soldOut, alreadyHolding int64
	var wg sync.WaitGroup

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
				// Register hold-key cleanup on the reserved goroutine's behalf
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
	require.Equal(t, int64(0), alreadyHolding, "no double-book (each user unique)")

	// Redis returns GET as a string; cast for comparison.
	inv, err := rdb.Get(ctx, InventoryKey(eventID)).Int()
	require.NoError(t, err)
	require.Equal(t, 0, inv, "inventory fully drained")
}

// TestReserve_AlreadyHolding: same user reserves twice → second is rejected.
func TestReserve_AlreadyHolding(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()
	const eventID = int64(42001)

	require.NoError(t, rdb.Set(ctx, InventoryKey(eventID), 5, 0).Err())
	withCleanKeys(t, rdb, InventoryKey(eventID), HoldKey(eventID, "u1"))

	// First call: reserved
	res, err := ReserveSeat(ctx, rdb, eventID, "u1", 600, 1)
	require.NoError(t, err)
	require.Equal(t, StatusReserved, res.Status)

	// Second call same user: already_holding (anti-double-book guard)
	res, err = ReserveSeat(ctx, rdb, eventID, "u1", 600, 1)
	require.NoError(t, err)
	require.Equal(t, StatusRejected, res.Status)
	require.Equal(t, ReasonAlreadyHolding, res.Reason)
}

// TestReserve_EventNotFound: inventory was never seeded.
func TestReserve_EventNotFound(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()
	const eventID = int64(42002) // not seeded

	res, err := ReserveSeat(ctx, rdb, eventID, "u1", 600, 1)
	require.NoError(t, err)
	require.Equal(t, StatusEventNotFound, res.Status)
}

// TestReserve_MultiSeat: request N>1 seats in one call.
func TestReserve_MultiSeat(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()
	const eventID = int64(42003)

	require.NoError(t, rdb.Set(ctx, InventoryKey(eventID), 5, 0).Err())
	withCleanKeys(t, rdb, InventoryKey(eventID), HoldKey(eventID, "group-buyer"))

	res, err := ReserveSeat(ctx, rdb, eventID, "group-buyer", 600, 3)
	require.NoError(t, err)
	require.Equal(t, StatusReserved, res.Status)

	inv, err := rdb.Get(ctx, InventoryKey(eventID)).Int()
	require.NoError(t, err)
	require.Equal(t, 2, inv, "inventory decremented by 3 (5 → 2)")
}

// TestReserve_MultiSeatSoldOut: ask for more than remain.
func TestReserve_MultiSeatSoldOut(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()
	const eventID = int64(42004)

	require.NoError(t, rdb.Set(ctx, InventoryKey(eventID), 2, 0).Err())
	withCleanKeys(t, rdb, InventoryKey(eventID), HoldKey(eventID, "group-buyer"))

	res, err := ReserveSeat(ctx, rdb, eventID, "group-buyer", 600, 5)
	require.NoError(t, err)
	require.Equal(t, StatusRejected, res.Status)
	require.Equal(t, ReasonSoldOut, res.Reason)

	inv, err := rdb.Get(ctx, InventoryKey(eventID)).Int()
	require.NoError(t, err)
	require.Equal(t, 2, inv, "inventory unchanged on rejection")
}
