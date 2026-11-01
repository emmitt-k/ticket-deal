package expire

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// TestParseHoldKey_Valid — the canonical shape.
//
// The event id is a UUID (see internal/eventid). The key format is
// "hold:event:<uuid>:user:<user>" — and the split is unambiguous
// because a UUID contains no colons.
func TestParseHoldKey_Valid(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantEvt string
		wantUsr string
	}{
		{
			"uuid-event-id",
			"hold:event:9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b:user:abc123",
			"9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b", "abc123",
		},
		{
			"uuid-user",
			"hold:event:9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b:user:550e8400-e29b-41d4-a716-446655440000",
			"9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b", "550e8400-e29b-41d4-a716-446655440000",
		},
		{
			"user-with-dashes",
			"hold:event:9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b:user:alice-bob",
			"9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b", "alice-bob",
		},
		{
			"user-with-colons",
			"hold:event:9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b:user:a:b:c",
			"9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b", "a:b:c",
		},
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

// TestParseHoldKey_RoundTripsHoldKey is THE test for this change.
//
// The parser and the writer are two halves of one contract: the
// watcher recognises a hold key and reads the event id back out of
// it. If the key format and the parser ever disagree, the failure is
// SILENT — the watcher skips the key, the compensation never runs, and
// the seat leaks. Nothing crashes; the seat just quietly disappears.
//
// The event id changed from an int64 to a UUID, and the parse changed
// from strconv.ParseInt to a UUID validation. This test asserts the
// two halves still agree, using the SAME constructor production uses
// to write the key.
func TestParseHoldKey_RoundTripsHoldKey(t *testing.T) {
	eventIDs := []string{
		"9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b",
		"00000000-0000-4000-8000-00000000cb23",
		"ffffffff-ffff-4fff-bfff-ffffffffffff",
	}
	userIDs := []string{"u1", "alice-bob", "550e8400-e29b-41d4-a716-446655440000"}

	for _, eventID := range eventIDs {
		for _, userID := range userIDs {
			t.Run(eventID+"/"+userID, func(t *testing.T) {
				key := redis.HoldKey(eventID, userID)

				gotEvent, gotUser, err := ParseHoldKey(key)
				require.NoError(t, err, "the parser must understand the key the writer produced")
				require.Equal(t, eventID, gotEvent)
				require.Equal(t, userID, gotUser)
			})
		}
	}
}

// TestParseHoldKey_NotHold — keys that fire on the expired channel but
// aren't hold keys. Must be filtered out, not panicked on.
//
// These are ErrNotHoldKey — background noise, not a bug.
func TestParseHoldKey_NotHold(t *testing.T) {
	tests := []string{
		"inventory:event:9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b", // the inventory counter
		"wait:queue:event:42",     // waiting-room zset
		"token_bucket:ip:1.2.3.4", // IP-limit bucket
		"",                        // empty
		"hold",                    // truncated
		"hold:event",              // truncated
		"hold:other:9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b:user:u1", // wrong prefix part
	}
	for _, key := range tests {
		t.Run(key, func(t *testing.T) {
			_, _, err := ParseHoldKey(key)
			require.Error(t, err)
			// A key that is not ours is NOT an error condition — the
			// watcher skips it silently. It must be distinguishable
			// from a key that IS ours but is broken.
			require.ErrorIs(t, err, ErrNotHoldKey)
			require.NotErrorIs(t, err, ErrMalformedHoldKey)
		})
	}
}

// TestParseHoldKey_MalformedEventID — the key IS a hold key, but the
// event id inside it is not a well-formed UUID.
//
// This is ErrMalformedHoldKey, and it is a REAL bug: a seat may be
// held against an event we cannot identify, so the compensation would
// never run and the seat leaks. The watcher logs it at ERROR rather
// than skipping it silently.
func TestParseHoldKey_MalformedEventID(t *testing.T) {
	for _, bad := range []string{
		"notanumber",                            // the old int64 shape — no longer valid
		"42",                                    // the old int64 shape — no longer valid
		"9f8c1e2a",                              // too short
		"9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5",   // one char short
		"9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5bb", // one char long
		"9F8C1E2A-3B4D-4E5F-8A9B-0C1D2E3F4A5B",  // uppercase — not canonical
		"9f8c1e2a_3b4d_4e5f_8a9b_0c1d2e3f4a5b",  // underscores
	} {
		t.Run(bad, func(t *testing.T) {
			_, _, err := ParseHoldKey("hold:event:" + bad + ":user:u1")
			require.Error(t, err)
			require.ErrorIs(t, err, ErrMalformedHoldKey)
			require.NotErrorIs(t, err, ErrNotHoldKey)
		})
	}
}

// TestParseHoldKey_EmptyUser — ":user:" with empty user_id is invalid.
func TestParseHoldKey_EmptyUser(t *testing.T) {
	_, _, err := ParseHoldKey("hold:event:9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b:user:")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrMalformedHoldKey)
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
