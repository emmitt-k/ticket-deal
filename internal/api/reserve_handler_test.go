package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// testSecret is a fixed 32-byte secret used across all handler tests.
// The real value doesn't matter (auth.Issue just signs bytes), it just
// has to be ≥ 32 bytes to pass auth.Issue's defensive check.
var testSecret = []byte("test-secret-not-a-real-secret-1234567890")

// testPublisher captures every Publish call into an in-memory buffer so
// tests can assert both the call count and the payload contents.
// Thread-safe — the handler may run concurrently under -race.
type testPublisher struct {
	mu       sync.Mutex
	calls    int
	lastBody []byte
	failNext bool // when true, the next Publish returns an error
	err      error
}

func (p *testPublisher) Publish(_ context.Context, body []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.lastBody = make([]byte, len(body))
	copy(p.lastBody, body)
	if p.failNext {
		p.failNext = false
		return p.err
	}
	return nil
}

func (p *testPublisher) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// doReserve wraps the handler in the auth middleware, mints a fresh
// JWT, and POSTs to the test server with the given body. Returns the
// recorded response. The middleware is included so claims-injection
// uses the real production code path — handler tests then verify only
// the reserve logic.
func doReserve(t *testing.T, rdb *redis.Client, publisher ReservationPublisher,
	holdTTL time.Duration, userID string, eventID int64, body string,
) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	token, err := auth.Issue(userID, eventID, testSecret, 2*time.Minute)
	require.NoError(t, err, "Issue test JWT")

	middleware := auth.Middleware(testSecret)
	handler := middleware(ReserveHandler(rdb, publisher, holdTTL))

	var reqBody *strings.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	} else {
		reqBody = strings.NewReader("")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tickets/reserve", reqBody)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var parsed map[string]any
	if rec.Body.Len() > 0 {
		_ = json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&parsed)
	}
	return rec, parsed
}

// newTestClient returns the package-level redis helper. Skips the test
// when Redis is unavailable so CI without Redis can still pass other
// packages (mirrors the pattern in internal/redis/reserve_test.go).
func newTestClient(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(redis.Config{Addr: "localhost:6379"})
	if err := redis.Ping(context.Background(), c); err != nil {
		t.Skipf("Redis unavailable at localhost:6379: %v", err)
	}
	return c
}

// cleanupReserveKeys wipes the inventory + hold keys for the given
// event so each test starts from a clean slate.
func cleanupReserveKeys(t *testing.T, rdb *redis.Client, eventID int64, userIDs ...string) {
	t.Helper()
	keys := []string{redis.InventoryKey(eventID)}
	for _, u := range userIDs {
		keys = append(keys, redis.HoldKey(eventID, u))
	}
	t.Cleanup(func() {
		if err := rdb.Del(context.Background(), keys...).Err(); err != nil {
			t.Logf("warning: cleanup of %v failed: %v", keys, err)
		}
	})
}

// TestReserveHandler_Success — the headline test. A valid JWT against
// a seeded event reserves exactly one seat, returns a UUID-shaped
// reservation_id, decrements inventory, sets a hold key with TTL ≈
// holdTTL, and publishes a JSON body that the worker (Phase 6) will
// consume.
func TestReserveHandler_Success(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	const eventID = int64(51001)
	const userID = "u-success"
	cleanupReserveKeys(t, rdb, eventID, userID)

	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 10, 0).Err())

	rec, body := doReserve(t, rdb, pub, 600*time.Second,
		userID, eventID, `{"seats_requested":1}`)

	require.Equal(t, http.StatusOK, rec.Code, "happy path should be 200")
	require.Contains(t, body, "reservation_id")
	require.Equal(t, float64(600), body["expires_in"], "expires_in matches holdTTL")

	// reservation_id must be a valid UUID.
	resID, ok := body["reservation_id"].(string)
	require.True(t, ok, "reservation_id is a string")
	require.NoError(t, uuidValidate(resID), "reservation_id is a valid UUID")

	// Inventory decremented by 1.
	inv, err := rdb.Get(context.Background(), redis.InventoryKey(eventID)).Int()
	require.NoError(t, err)
	require.Equal(t, 9, inv, "inventory 10 → 9")

	// Hold key exists with TTL ≈ holdTTL.
	ttl, err := rdb.TTL(context.Background(), redis.HoldKey(eventID, userID)).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 590*time.Second, "TTL > 590s")
	require.LessOrEqual(t, ttl, 600*time.Second, "TTL ≤ 600s")

	// Publisher was called exactly once with the right shape.
	require.Equal(t, 1, pub.Calls())
	var msg map[string]any
	require.NoError(t, json.Unmarshal(pub.lastBody, &msg))
	require.Equal(t, resID, msg["reservation_id"])
	require.Equal(t, userID, msg["user_id"])
	require.Equal(t, float64(eventID), msg["event_id"])
	require.Equal(t, float64(1), msg["seats"])
	require.NotEmpty(t, msg["created_at"])
	require.NotEmpty(t, msg["expires_at"])
}

// TestReserveHandler_AlreadyHolding — same user reserves twice within
// hold window. Lua atomically sees the existing key → 409.
func TestReserveHandler_AlreadyHolding(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	const eventID = int64(51002)
	const userID = "u-already"
	cleanupReserveKeys(t, rdb, eventID, userID)

	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 5, 0).Err())

	// First call succeeds.
	rec, _ := doReserve(t, rdb, pub, 600*time.Second,
		userID, eventID, ``)
	require.Equal(t, http.StatusOK, rec.Code)

	// Second call same event+user → already_holding.
	rec, body := doReserve(t, rdb, pub, 600*time.Second,
		userID, eventID, ``)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "already_holding", body["error"])

	// Inventory decremented by exactly 1 (the second call didn't touch it).
	inv, err := rdb.Get(context.Background(), redis.InventoryKey(eventID)).Int()
	require.NoError(t, err)
	require.Equal(t, 4, inv)
}

// TestReserveHandler_SoldOut — inventory = 0, all calls rejected.
func TestReserveHandler_SoldOut(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	const eventID = int64(51003)
	const userID = "u-soldout"
	cleanupReserveKeys(t, rdb, eventID, userID)

	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 0, 0).Err())

	rec, body := doReserve(t, rdb, pub, 600*time.Second,
		userID, eventID, ``)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "sold_out", body["error"])
	require.Equal(t, 0, pub.Calls(), "publisher not called on rejection")
}

// TestReserveHandler_EventNotFound — inventory key was never seeded.
// (In practice, an event should always be seeded before /reserve is
// hit; this is a defensive 404.)
func TestReserveHandler_EventNotFound(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	const eventID = int64(51004) // intentionally never seeded

	rec, body := doReserve(t, rdb, pub, 600*time.Second,
		"u1", eventID, ``)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "event_not_found", body["error"])
	require.Equal(t, 0, pub.Calls(), "publisher not called on rejection")
}

// TestReserveHandler_MultiSeat — seats_requested > 1 decrements by N.
func TestReserveHandler_MultiSeat(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	const eventID = int64(51005)
	const userID = "u-group"
	cleanupReserveKeys(t, rdb, eventID, userID)

	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 10, 0).Err())

	rec, body := doReserve(t, rdb, pub, 600*time.Second,
		userID, eventID, `{"seats_requested":4}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, float64(4), body["seats"], "response echoes requested seats")

	inv, err := rdb.Get(context.Background(), redis.InventoryKey(eventID)).Int()
	require.NoError(t, err)
	require.Equal(t, 6, inv, "10 - 4 = 6")

	// Publisher payload includes seats=4.
	var msg map[string]any
	require.NoError(t, json.Unmarshal(pub.lastBody, &msg))
	require.Equal(t, float64(4), msg["seats"])
}

// TestReserveHandler_DefaultSeatCount — absent / null / zero all default
// to 1 seat. JSON semantics: an omitted field, an explicit null, and
// 0 are indistinguishable from Go's int decoding, so we accept all
// three as "use default".
func TestReserveHandler_DefaultSeatCount(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	const eventID = int64(51006)
	// Distinct users so the second/third don't hit already_holding.
	cleanupReserveKeys(t, rdb, eventID, "empty", "null", "zero")

	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 100, 0).Err())

	cases := []struct {
		name string
		user string
	}{
		{"empty-body", "empty"},
		{"null", "null"},
		{"zero", "zero"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rec, body := doReserve(t, rdb, pub, 600*time.Second,
				tc.user, eventID, `{"seats_requested":0}`)
			require.Equal(t, http.StatusOK, rec.Code,
				"missing/null/0 should fall back to default 1")
			require.Equal(t, float64(1), body["seats"])
		})
	}
}

// TestReserveHandler_InvalidSeatsRequested — negative / > 10 are
// hard errors (400). Each subtest uses a distinct user so they
// don't interfere via already_holding.
func TestReserveHandler_InvalidSeatsRequested(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	const eventID = int64(51006)
	cleanupReserveKeys(t, rdb, eventID, "neg", "toomany")

	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 100, 0).Err())

	cases := []struct {
		name string
		user string
		body string
	}{
		{"negative", "neg", `{"seats_requested":-1}`},
		{"too-many", "toomany", `{"seats_requested":11}`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rec, body := doReserve(t, rdb, pub, 600*time.Second,
				tc.user, eventID, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "invalid_request", body["error"])
		})
	}
	require.Equal(t, 0, pub.Calls(), "publisher not called on invalid input")
}

// TestReserveHandler_BadJSON body — malformed body → 400.
func TestReserveHandler_BadJSONBody(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	const eventID = int64(51007)
	cleanupReserveKeys(t, rdb, eventID, "u1")

	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 5, 0).Err())

	rec, body := doReserve(t, rdb, pub, 600*time.Second,
		"u1", eventID, `{this is not json`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request", body["error"])
	require.Equal(t, 0, pub.Calls())
}

// TestReserveHandler_PublisherError — publish fails but reservation
// stays valid (worker will replay via ON CONFLICT DO NOTHING).
func TestReserveHandler_PublisherError(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{failNext: true, err: fmt.Errorf("sqs unavailable")}
	const eventID = int64(51008)
	const userID = "u-pubfail"
	cleanupReserveKeys(t, rdb, eventID, userID)

	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 5, 0).Err())

	rec, body := doReserve(t, rdb, pub, 600*time.Second,
		userID, eventID, ``)
	// Reservation succeeded (Redis hold was set), 200 is returned even
	// though the publish failed — Phase 6's worker can replay this.
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, body, "reservation_id")

	inv, err := rdb.Get(context.Background(), redis.InventoryKey(eventID)).Int()
	require.NoError(t, err)
	require.Equal(t, 4, inv, "inventory still decremented")
}

// TestReserveHandler_NoClaims — handler invoked without auth middleware.
// Should be impossible in production, but if someone wires the route
// wrong, fail loud with 401 (NOT 500 — that would hide the bug).
func TestReserveHandler_NoClaims(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	handler := ReserveHandler(rdb, pub, 600*time.Second)

	req := httptest.NewRequest(http.MethodPost, "/api/tickets/reserve", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "missing_claims")
}

// TestReserveHandler_DifferentUsersDifferentHolds — two users each get
// their own hold; both 200. Inventory decremented by 2.
func TestReserveHandler_DifferentUsersDifferentHolds(t *testing.T) {
	rdb := newTestClient(t)
	pub := &testPublisher{}
	const eventID = int64(51009)
	cleanupReserveKeys(t, rdb, eventID, "alice", "bob")

	require.NoError(t, rdb.Set(context.Background(),
		redis.InventoryKey(eventID), 10, 0).Err())

	recA, _ := doReserve(t, rdb, pub, 600*time.Second, "alice", eventID, ``)
	require.Equal(t, http.StatusOK, recA.Code)

	recB, _ := doReserve(t, rdb, pub, 600*time.Second, "bob", eventID, ``)
	require.Equal(t, http.StatusOK, recB.Code)

	inv, err := rdb.Get(context.Background(), redis.InventoryKey(eventID)).Int()
	require.NoError(t, err)
	require.Equal(t, 8, inv)
	require.Equal(t, 2, pub.Calls())
}

// uuidValidate wraps google/uuid.Parse — tiny helper so the test body
// stays clean (just `require.NoError(uuidValidate(s))`).
func uuidValidate(s string) error {
	_, err := uuid.Parse(s)
	return err
}