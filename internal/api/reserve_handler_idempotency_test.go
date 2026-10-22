package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// Helper to set up the idempotency wrapper with short TTLs so tests
// don't have to wait the full 24 h / 5 s.
func newTestIdempotency(t *testing.T, rdb *redis.Client) *redis.Idempotency {
	t.Helper()
	return redis.NewIdempotency(rdb, redis.IdempotencyConfig{
		CacheTTL: 1 * time.Hour,
		LockTTL:  2 * time.Second,
	})
}

// freshIdemKey returns a UUID-formatted string + t.Cleanup that
// wipes the cache + lock entries. Each test that uses one gets
// its own key.
func freshIdemKey(t *testing.T, c *redis.Client) string {
	t.Helper()
	key := uuid.NewString()
	t.Cleanup(func() {
		_ = c.Del(context.Background(),
			redis.CacheKey(key), redis.LockKey(key)).Err()
	})
	return key
}

// ── Happy path: cached replay ──────────────────────────────────────────

// TestReserveHandler_Idempotency_Replay — first call with a key
// returns 200 and caches the response; second call with the same key
// + same body returns the cached 200 with the Idempotency-Replayed
// header set, NO new reservation minted (verified by inventory).
func TestReserveHandler_Idempotency_Replay(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	idem := newTestIdempotency(t, rdb)

	const eventID = int64(77001)
	const owner = "u-idem-replay-1"

	// Seed inventory 10→9 on first reserve; second call must not change it.
	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 10, 0).Err())
	cleanupReserveKeys(t, rdb, eventID, owner)

	key := freshIdemKey(t, rdb)

	// First call — fresh reserve, caches the response.
	rec1, body1 := doReserveFull(t, rdb, pub, idem, 600*time.Second,
		owner, eventID, `{"seats_requested":1}`, key)
	require.Equal(t, http.StatusOK, rec1.Code)
	require.Equal(t, "", rec1.Header().Get(idempotencyReplayHeader),
		"first call should not be a replay")
	require.NotEmpty(t, body1["reservation_id"])

	// Second call with same key + same body — cached replay.
	rec2, body2 := doReserveFull(t, rdb, pub, idem, 600*time.Second,
		owner, eventID, `{"seats_requested":1}`, key)
	require.Equal(t, http.StatusOK, rec2.Code)
	require.Equal(t, "true", rec2.Header().Get(idempotencyReplayHeader),
		"second call should set Idempotency-Replayed: true")
	require.Equal(t, body1["reservation_id"], body2["reservation_id"],
		"replay must return the ORIGINAL reservation_id, not a new one")

	// Inventory should only have been decremented once (by the first call).
	inv, err := rdb.Get(context.Background(), redis.InventoryKey(eventID)).Int()
	require.NoError(t, err)
	require.Equal(t, 9, inv, "second call must NOT decrement inventory again")

	// And exactly one publish should have happened.
	require.Equal(t, 1, pub.Calls(), "second call must NOT publish a second SQS message")
}

// ── Body mismatch ───────────────────────────────────────────────────────

// TestReserveHandler_Idempotency_BodyMismatch — same key, different
// body → 409 idempotency_key_mismatch. Catches a class of client bugs
// where the user thinks they're retrying the same request but they
// actually changed it.
func TestReserveHandler_Idempotency_BodyMismatch(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	idem := newTestIdempotency(t, rdb)

	const eventID = int64(77002)
	const owner = "u-idem-mismatch-1"
	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 10, 0).Err())
	cleanupReserveKeys(t, rdb, eventID, owner)

	key := freshIdemKey(t, rdb)

	// First call: seats=1.
	rec1, _ := doReserveFull(t, rdb, pub, idem, 600*time.Second,
		owner, eventID, `{"seats_requested":1}`, key)
	require.Equal(t, http.StatusOK, rec1.Code)

	// Second call: same key, different body (seats=2).
	rec2, body2 := doReserveFull(t, rdb, pub, idem, 600*time.Second,
		owner, eventID, `{"seats_requested":2}`, key)
	require.Equal(t, http.StatusConflict, rec2.Code)
	require.Equal(t, "idempotency_key_mismatch", body2["error"])
}

// ── Invalid key format ─────────────────────────────────────────────────

// TestReserveHandler_Idempotency_InvalidKey — header value contains
// disallowed characters (anything outside [A-Za-z0-9-]) or is too
// long. Both → 400 invalid_idempotency_key.
func TestReserveHandler_Idempotency_InvalidKey(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	idem := newTestIdempotency(t, rdb)

	const eventID = int64(77003)
	const owner = "u-idem-badkey-1"
	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 10, 0).Err())
	cleanupReserveKeys(t, rdb, eventID, owner)

	cases := []struct {
		name string
		key  string
	}{
		{"contains space", "abc 123"},
		{"contains slash", "abc/def"},
		{"contains unicode", "💀key"},
		{"too long", strings.Repeat("a", 256)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := doReserveFull(t, rdb, pub, idem, 600*time.Second,
				owner, eventID, `{"seats_requested":1}`, tc.key)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "invalid_idempotency_key", body["error"])
		})
	}
}

// ── In-flight lock: two parallel retries ────────────────────────────────

// TestReserveHandler_Idempotency_ParallelRetries — two concurrent
// requests with the same key. One wins, the other gets 409 in_flight.
// The "winner" path runs the reserve; the "loser" path returns
// immediately without consuming a seat.
func TestReserveHandler_Idempotency_ParallelRetries(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	idem := newTestIdempotency(t, rdb)

	const eventID = int64(77004)
	const owner = "u-idem-parallel-1"
	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 10, 0).Err())
	cleanupReserveKeys(t, rdb, eventID, owner)

	key := freshIdemKey(t, rdb)

	// Two concurrent HTTP requests through the handler. They race
	// for the in-flight lock; one wins, one loses. We don't care
	// which wins — we just check the SET of outcomes.
	type result struct {
		code int
		body map[string]any
	}
	results := make(chan result, 2)

	for i := 0; i < 2; i++ {
		go func() {
			handler := newAuthedReserveHandler(t, rdb, pub, idem, owner, eventID)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/tickets/reserve",
				strings.NewReader(`{"seats_requested":1}`))
			req.Header.Set("Authorization", "Bearer "+mustAuth(t, owner, eventID))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", key)
			handler.ServeHTTP(rec, req)
			results <- result{code: rec.Code}
		}()
	}

	var sawOK, sawInFlight bool
	for i := 0; i < 2; i++ {
		r := <-results
		switch r.code {
		case http.StatusOK:
			sawOK = true
		case http.StatusConflict:
			// 409 is also a valid conflict for the held-status code path;
			// but for this test, the in-flight one matters. The other
			// possible 409 here would be sold_out (impossible, we have
			// seats) or already_holding (impossible, same key/middleware
			// is fresh per-request). So 409 == in_flight here.
			sawInFlight = true
		default:
			t.Fatalf("unexpected status %d", r.code)
		}
	}

	require.True(t, sawOK, "one of the parallel calls must succeed")
	require.True(t, sawInFlight, "the other must get 409 in_flight")

	// Only one publish happened.
	require.Equal(t, 1, pub.Calls(), "only the lock winner publishes to SQS")
}

// ── Error responses are NOT cached ─────────────────────────────────────

// TestReserveHandler_Idempotency_ErrorsNotCached — first call returns
// a deterministic business error (sold_out). The second call must NOT
// receive the cached 409; it should re-attempt the reserve. (If a hold
// was added between calls the second could even succeed.)
func TestReserveHandler_Idempotency_ErrorsNotCached(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	idem := newTestIdempotency(t, rdb)

	const eventID = int64(77005)
	const owner = "u-idem-err-1"

	// Inventory 0 → sold_out.
	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 0, 0).Err())
	cleanupReserveKeys(t, rdb, eventID, owner)

	key := freshIdemKey(t, rdb)

	// First call: 409 sold_out.
	rec1, _ := doReserveFull(t, rdb, pub, idem, 600*time.Second,
		owner, eventID, `{"seats_requested":1}`, key)
	require.Equal(t, http.StatusConflict, rec1.Code)

	// Second call: should re-attempt (NOT cached replay). Expect 409
	// sold_out again, but for a fresh reason. Either way the
	// Idempotency-Replayed header should NOT be set.
	rec2, body2 := doReserveFull(t, rdb, pub, idem, 600*time.Second,
		owner, eventID, `{"seats_requested":1}`, key)
	require.Equal(t, http.StatusConflict, rec2.Code)
	require.Equal(t, "sold_out", body2["error"])
	require.Equal(t, "", rec2.Header().Get(idempotencyReplayHeader),
		"error responses should not be cached as replays")
	require.Equal(t, 0, pub.Calls(),
		"error responses should not have triggered a publish either")
}

// ── Header absent: behaves as before ──────────────────────────────────

// TestReserveHandler_Idempotency_HeaderAbsent — no Idempotency-Key
// → behaves identically to the pre-feature handler. Backward-compat
// guard; covers existing clients that haven't opted in.
func TestReserveHandler_Idempotency_HeaderAbsent(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	idem := newTestIdempotency(t, rdb)

	const eventID = int64(77006)
	const owner = "u-idem-nokey-1"
	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 10, 0).Err())
	cleanupReserveKeys(t, rdb, eventID, owner)

	rec, body := doReserveFull(t, rdb, pub, idem, 600*time.Second,
		owner, eventID, `{"seats_requested":1}`, "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotEmpty(t, body["reservation_id"])
	require.Equal(t, "", rec.Header().Get(idempotencyReplayHeader))
}

// ── Test helpers ────────────────────────────────────────────────────────

// newAuthedReserveHandler builds the authed wrapper for parallel-retry
// tests where we need to issue a request without going through
// doReserveFull (which doesn't expose the wiring needed for parallel
// httptest.NewRecorder instances).
func newAuthedReserveHandler(t *testing.T, rdb *redis.Client, pub *testPublisher,
	idem *redis.Idempotency, owner string, eventID int64,
) http.Handler {
	t.Helper()
	token, err := mustAuthErr(t, owner, eventID)
	require.NoError(t, err)
	t.Logf("token=%s", token)
	return auth.Middleware(testSecret)(ReserveHandler(rdb, pub, idem, 600*time.Second))
}

// mustAuth mints a JWT and returns the token string. Panics on
// failure — test setup.
func mustAuth(t *testing.T, owner string, eventID int64) string {
	t.Helper()
	tok, err := mustAuthErr(t, owner, eventID)
	require.NoError(t, err)
	return tok
}

// mustAuthErr is the (token, error) variant. Kept separate so the
// parallel-retry helper can do its own error handling.
func mustAuthErr(t *testing.T, owner string, eventID int64) (string, error) {
	t.Helper()
	return auth.Issue(owner, eventID, testSecret, 2*time.Minute)
}
