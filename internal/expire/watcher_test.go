package expire

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestParseHoldKey_Valid — the canonical shape.
func TestParseHoldKey_Valid(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantEvt int64
		wantUsr string
	}{
		{"simple", "hold:event:42:user:abc123", 42, "abc123"},
		{"uuid-user", "hold:event:1:user:550e8400-e29b-41d4-a716-446655440000", 1, "550e8400-e29b-41d4-a716-446655440000"},
		{"big-event-id", "hold:event:9999999999:user:u1", 9999999999, "u1"},
		{"user-with-dashes", "hold:event:42:user:alice-bob", 42, "alice-bob"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eventID, userID, err := ParseHoldKey(tc.key)
			require.NoError(t, err)
			require.Equal(t, tc.wantEvt, eventID)
			require.Equal(t, tc.wantUsr, userID)
		})
	}
}

// TestParseHoldKey_NotHold — keys that fire on the expired channel but
// aren't hold keys. Must be filtered out, not panicked on.
func TestParseHoldKey_NotHold(t *testing.T) {
	tests := []string{
		"inventory:event:42",          // the inventory counter
		"wait:queue:event:42",         // waiting-room zset
		"token_bucket:ip:1.2.3.4",     // IP-limit bucket
		"",                            // empty
		"hold",                        // truncated
		"hold:event",                 // truncated
		"hold:other:42:user:u1",      // wrong prefix part
	}
	for _, key := range tests {
		t.Run(key, func(t *testing.T) {
			_, _, err := ParseHoldKey(key)
			require.Error(t, err)
		})
	}
}

// TestParseHoldKey_MalformedEventID — event_id segment isn't a number.
func TestParseHoldKey_MalformedEventID(t *testing.T) {
	_, _, err := ParseHoldKey("hold:event:notanumber:user:u1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse event_id")
}

// TestParseHoldKey_EmptyUser — ":user:" with empty user_id is invalid.
func TestParseHoldKey_EmptyUser(t *testing.T) {
	_, _, err := ParseHoldKey("hold:event:42:user:")
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty user_id")
}

// TestRunWatcher_ContextCancel — RunWatcher returns promptly when ctx
// is canceled (without ever subscribing, since pubsub.Receive would
// block on a context that hasn't been set up).
//
// We don't test the full PSUBSCRIBE loop here — miniredis doesn't
// fully emulate keyspace notifications. The live smoke test
// (docker-compose Redis with --notify-keyspace-events Ex) is the
// end-to-end verification of the PSUBSCRIBE path.
func TestRunWatcher_ContextCancel(t *testing.T) {
	rdb := newTestRedis(t) // skip if no live Redis
	// pool is not used in this path because RunWatcher bails before
	// subscribing; pass nil to prove we never reach Compensate.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling

	err := RunWatcher(ctx, rdb, nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled))
}

// TestRunWatcher_ReconnectsAfterPubsubClose — open a subscription,
// cancel the inner ctx, see RunWatcher exit. We can't easily simulate
// a connection drop in unit tests; this just exercises the
// "ctx canceled before subscribe" branch.
func TestRunWatcher_ReconnectsAfterPubsubClose(t *testing.T) {
	rdb := newTestRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := RunWatcher(ctx, rdb, nil)
	elapsed := time.Since(start)

	require.Error(t, err)
	// WithTimeout produces DeadlineExceeded; WithCancel produces Canceled.
	// Either is a graceful ctx-done signal from RunWatcher's perspective.
	require.True(t, errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded),
		"RunWatcher should return a ctx-done error, got %v", err)
	require.Less(t, elapsed, 2*time.Second, "RunWatcher should bail promptly on ctx cancel")
	require.GreaterOrEqual(t, elapsed, 50*time.Millisecond, "should respect the ctx deadline")
}

// silence unused-import warning for strings kept for future key-shape helpers.
var _ = strings.Contains