package redis

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// The release-seat script is the guard against a phantom seat. These
// tests pin the three outcomes and — more importantly — that a second
// release of the same hold does NOT count the seat twice.

func newReleaseTestRedis(t *testing.T) *Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	c := NewClient(Config{Addr: addr})
	if err := Ping(context.Background(), c); err != nil {
		t.Skipf("Redis unavailable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestReleaseSeat_ReleasesAHeldSeat — the happy path: the hold
// existed, so the seat goes back on sale and the hold is gone.
func TestReleaseSeat_ReleasesAHeldSeat(t *testing.T) {
	c := newReleaseTestRedis(t)
	ctx := context.Background()

	const eventID = int64(96001)
	const user = "release-user-1"
	require.NoError(t, c.Set(ctx, InventoryKey(eventID), 100, 0).Err())
	t.Cleanup(func() { _ = c.Del(ctx, InventoryKey(eventID)).Err() })

	res, err := ReserveSeat(ctx, c, eventID, user, 600, 2)
	require.NoError(t, err)
	require.Equal(t, StatusReserved, res.Status)

	outcome, err := ReleaseSeat(ctx, c, eventID, user, 2)
	require.NoError(t, err)
	require.Equal(t, ReleaseSeatDone, outcome)

	avail, err := c.Get(ctx, InventoryKey(eventID)).Int()
	require.NoError(t, err)
	require.Equal(t, 100, avail, "the seats must be back on sale")

	held, err := c.Exists(ctx, HoldKey(eventID, user)).Result()
	require.NoError(t, err)
	require.Zero(t, held, "the hold must be gone")
}

// TestReleaseSeat_TwiceDoesNotDoubleCount — THE guard.
//
// A second release of the same hold must NOT increment the inventory
// again. If it did, the seat count would be one too high — a phantom
// seat, which is an oversell.
func TestReleaseSeat_TwiceDoesNotDoubleCount(t *testing.T) {
	c := newReleaseTestRedis(t)
	ctx := context.Background()

	const eventID = int64(96002)
	const user = "release-user-2"
	require.NoError(t, c.Set(ctx, InventoryKey(eventID), 100, 0).Err())
	t.Cleanup(func() {
		_ = c.Del(ctx, InventoryKey(eventID)).Err()
		_ = c.Del(ctx, HoldKey(eventID, user)).Err()
	})

	_, err := ReserveSeat(ctx, c, eventID, user, 600, 1)
	require.NoError(t, err)

	outcome, err := ReleaseSeat(ctx, c, eventID, user, 1)
	require.NoError(t, err)
	require.Equal(t, ReleaseSeatDone, outcome)

	outcome, err = ReleaseSeat(ctx, c, eventID, user, 1)
	require.NoError(t, err)
	require.Equal(t, ReleaseSeatNotHeld, outcome,
		"the second release must find no hold")

	avail, err := c.Get(ctx, InventoryKey(eventID)).Int()
	require.NoError(t, err)
	require.Equal(t, 100, avail, "the inventory must be incremented exactly once")
}

// TestReleaseSeat_NotHeld — a hold that has already expired (the
// expiration watcher put the seat back) must not be released again.
func TestReleaseSeat_NotHeld(t *testing.T) {
	c := newReleaseTestRedis(t)
	ctx := context.Background()

	const eventID = int64(96003)
	const user = "release-user-3"
	require.NoError(t, c.Set(ctx, InventoryKey(eventID), 100, 0).Err())
	t.Cleanup(func() { _ = c.Del(ctx, InventoryKey(eventID)).Err() })

	outcome, err := ReleaseSeat(ctx, c, eventID, user, 1)
	require.NoError(t, err)
	require.Equal(t, ReleaseSeatNotHeld, outcome)

	avail, err := c.Get(ctx, InventoryKey(eventID)).Int()
	require.NoError(t, err)
	require.Equal(t, 100, avail, "the inventory must not move")
}

// TestReleaseSeat_NoInventory — an event that was never seeded has no
// counter to put the seat back into. Reporting that is better than
// creating a counter with the wrong value.
func TestReleaseSeat_NoInventory(t *testing.T) {
	c := newReleaseTestRedis(t)
	ctx := context.Background()

	const eventID = int64(96004)
	outcome, err := ReleaseSeat(ctx, c, eventID, "release-user-4", 1)
	require.NoError(t, err)
	require.Equal(t, ReleaseSeatNoInventory, outcome)
}

// TestReleaseSeat_ZeroSeats — a no-op is a no-op.
func TestReleaseSeat_ZeroSeats(t *testing.T) {
	c := newReleaseTestRedis(t)
	ctx := context.Background()

	outcome, err := ReleaseSeat(ctx, c, 96005, "release-user-5", 0)
	require.NoError(t, err)
	require.Equal(t, ReleaseSeatDone, outcome)
}
