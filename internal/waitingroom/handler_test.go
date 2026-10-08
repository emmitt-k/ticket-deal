package waitingroom

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emmitt-k/ticket-deal/internal/config"
	"github.com/stretchr/testify/require"
)

// bucketKeyForTest and queueKeyForTest mirror the private functions in
// internal/redis/waitingroom.go so tests can clean up.
func bucketKeyForTest(eventID int64) string { return fmt.Sprintf("bucket:event:%d", eventID) }
func queueKeyForTest(eventID int64) string { return fmt.Sprintf("queue:event:%d", eventID) }

func TestEnterHandler_IPReject(t *testing.T) {
	rdb := testClient(t)
	cfg := config.Config{
		IPLimit: config.IPLimitConfig{
			Capacity:   0, // everyone rejected
			RefillRate: 1.0,
		},
		WaitRoom: config.WaitRoomConfig{
			Capacity:        100,
			RefillRate:     10.0,
			QueueTTLSeconds: 60,
		},
	}

	h := EnterHandler(cfg, rdb)

	body := strings.NewReader(`{"user_id":"u1","event_id":42}`)
	req := httptest.NewRequest(http.MethodPost, "/api/tickets/enter", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, "ip_rate_limited", resp["error"])
}

func TestEnterHandler_AdmitAndQueue(t *testing.T) {
	rdb := testClient(t)
	const eventID = int64(51000)

	cleanupKeys := []string{
		bucketKeyForTest(eventID),
		queueKeyForTest(eventID),
		"ip:1.2.3.4",
	}
	t.Cleanup(func() {
		for _, k := range cleanupKeys {
			rdb.Del(context.Background(), k)
		}
	})

	cfg := config.Config{
		IPLimit: config.IPLimitConfig{
			Capacity:   100,
			RefillRate: 100.0,
		},
		WaitRoom: config.WaitRoomConfig{
			Capacity:        1, // 1 token → first user admitted, rest queued
			RefillRate:     0.001,
			QueueTTLSeconds: 60,
		},
		JWTSecret: make([]byte, 32), // dummy 32-byte secret
	}

	h := EnterHandler(cfg, rdb)

	makeReq := func(userID string) (int, map[string]any) {
		body := strings.NewReader(fmt.Sprintf(
			`{"user_id":"%s","event_id":%d}`, userID, eventID))
		req := httptest.NewRequest(http.MethodPost, "/api/tickets/enter", body)
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "1.2.3.4:12345"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var resp map[string]any
		_ = json.NewDecoder(rec.Body).Decode(&resp)
		return rec.Code, resp
	}

	// First user → admitted (200)
	code, body := makeReq("u1")
	require.Equal(t, http.StatusOK, code, "first user should be admitted")
	require.Contains(t, body, "token", "admitted response should contain token")
	require.Equal(t, 120, int(body["expires_in"].(float64)), "token expires in 2 min")

	// Second user → queued (429)
	code, body = makeReq("u2")
	require.Equal(t, http.StatusTooManyRequests, code, "second user should be queued")
	require.Contains(t, body, "queue_url")
	require.Equal(t, float64(1), body["position"], "u2 should be 1st in queue")
}

func TestETA(t *testing.T) {
	require.Equal(t, 0, etaSeconds(0, 10.0))
	require.Equal(t, 0, etaSeconds(5, 0))
	require.Equal(t, 10, etaSeconds(100, 10.0))
	require.Equal(t, 3, etaSeconds(30, 10.0))
}

func TestQueueSSEHandler_NotInQueue(t *testing.T) {
	rdb := testClient(t)
	h := QueueSSEHandler(rdb)

	req := httptest.NewRequest(http.MethodGet,
		"/api/tickets/queue?event_id=99999&user_id=nonexistent", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	require.Equal(t, "not_in_queue", body["error"])
}

func TestQueueSSEHandler_BadParams(t *testing.T) {
	rdb := testClient(t)
	h := QueueSSEHandler(rdb)

	tests := []struct {
		query string
	}{
		{"/api/tickets/queue"},                       // missing both
		{"/api/tickets/queue?event_id=42"},           // missing user_id
		{"/api/tickets/queue?user_id=u1"},           // missing event_id
		{"/api/tickets/queue?event_id=abc&user_id=u1"}, // bad event_id
		{"/api/tickets/queue?event_id=-1&user_id=u1"}, // negative event_id
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, tc.query, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, "query: %s", tc.query)
	}
}

func TestEnterHandler_QueueURLFormat(t *testing.T) {
	rdb := testClient(t)
	const eventID = int64(51001)

	// Pre-seed inventory so TryAdmit sees the event
	rdb.Set(context.Background(), fmt.Sprintf("inventory:event:%d", eventID), 1, 0)

	t.Cleanup(func() {
		rdb.Del(context.Background(),
			fmt.Sprintf("inventory:event:%d", eventID),
			bucketKeyForTest(eventID),
			queueKeyForTest(eventID),
			"ip:5.6.7.8",
		)
	})

	cfg := config.Config{
		IPLimit: config.IPLimitConfig{Capacity: 100, RefillRate: 100.0},
		WaitRoom: config.WaitRoomConfig{
			Capacity:        0, // queue everyone
			RefillRate:     0.001,
			QueueTTLSeconds: 60,
		},
		JWTSecret: make([]byte, 32),
	}

	h := EnterHandler(cfg, rdb)
	body := strings.NewReader(fmt.Sprintf(`{"user_id":"u1","event_id":%d}`, eventID))
	req := httptest.NewRequest(http.MethodPost, "/api/tickets/enter", body)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "5.6.7.8:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Contains(t, resp["queue_url"], "/api/tickets/queue")
	require.Contains(t, resp["queue_url"], "event_id=51001")
	require.Contains(t, resp["queue_url"], "user_id=u1")
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		header string
		addr   string
		want   string
	}{
		{"", "1.2.3.4:12345", "1.2.3.4"},
		{"9.8.7.6", "1.2.3.4:12345", "9.8.7.6"},
		{"9.8.7.6, 5.5.5.5", "1.2.3.4:12345", "9.8.7.6"},
	}
	for _, tc := range tests {
		r := &http.Request{RemoteAddr: tc.addr, Header: make(http.Header)}
		if tc.header != "" {
			r.Header.Set("X-Forwarded-For", tc.header)
		}
		got := clientIP(r)
		require.Equal(t, tc.want, got, "header=%q addr=%q", tc.header, tc.addr)
	}
}

// verifyHTTPResponse is a test helper that reads the body and checks status.
func verifyHTTPResponse(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) map[string]any {
	t.Helper()
	require.Equal(t, wantStatus, rec.Code)
	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	return body
}
