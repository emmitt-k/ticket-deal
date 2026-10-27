package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// doReadyZ invokes the readyz handler with the given checks. Returns
// the recorder + decoded JSON body. No auth, no routing scaffolding
// — the handler is the system under test, full stop.
//
// NB: this helper drains rec.Body via the json.Decoder — callers
// that need raw bytes must use doReadyZRaw below (the ResponseRecorder
// is a *bytes.Buffer; reading from it via Decoder empties it).
func doReadyZ(checks ReadinessChecks) (*httptest.ResponseRecorder, map[string]any) {
	rec, raw := doReadyZRaw(checks)

	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return rec, body
}

// doReadyZRaw is the bytes-preserving variant of doReadyZ. Returns
// the recorder + raw response body bytes (so the caller can re-parse
// or assert on fields the helper would otherwise have destructured).
func doReadyZRaw(checks ReadinessChecks) (*httptest.ResponseRecorder, []byte) {
	handler := ReadyZHandler(checks)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec, rec.Body.Bytes()
}

// waitForParallelism boundary: if checks run in parallel, the total
// handler latency should be roughly max(perCheck) not sum(perCheck).
// Used by TestReadyZ_RunsInParallel below.
const testCheckDuration = 200 * time.Millisecond

// slowOK returns a check that sleeps `d` and then succeeds. Used to
// verify the handler runs the three checks in parallel.
func slowOK(d time.Duration) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// slowErr returns a check that sleeps `d` then returns err. Used to
// verify per-check timeouts surface as "timeout after 500ms".
func slowErr(d time.Duration, err error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-time.After(d):
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ── Pure-unit tests — no external deps ─────────────────────────────────

// TestReadyZ_AllOK — three passing checks → 200 + ready:true.
func TestReadyZ_AllOK(t *testing.T) {
	rec, body := doReadyZ(ReadinessChecks{
		Redis:    func(ctx context.Context) error { return nil },
		Postgres: func(ctx context.Context) error { return nil },
		SQS:      func(ctx context.Context) error { return nil },
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok", body["redis"])
	require.Equal(t, "ok", body["postgres"])
	require.Equal(t, "ok", body["sqs"])
	require.Equal(t, true, body["ready"])
}

// TestReadyZ_NotConfiguredDevMode — SQS is nil. Should report
// "not_configured" and still count as healthy (K8s/dev workflows
// stay green without an SQS queue configured).
func TestReadyZ_NotConfiguredDevMode(t *testing.T) {
	rec, body := doReadyZ(ReadinessChecks{
		Redis:    func(ctx context.Context) error { return nil },
		Postgres: func(ctx context.Context) error { return nil },
		SQS:      nil, // dev mode — LogPublisher wires no SQS
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok", body["redis"])
	require.Equal(t, "ok", body["postgres"])
	require.Equal(t, "not_configured", body["sqs"])
	require.Equal(t, true, body["ready"])
}

// TestReadyZ_RedisFailure — Redis returns an error. Other two pass.
// Expected: 503, redis: "error: ...", postgres: "ok", sqs: "ok",
// ready: false.
func TestReadyZ_RedisFailure(t *testing.T) {
	rec, body := doReadyZ(ReadinessChecks{
		Redis:    func(ctx context.Context) error { return errors.New("connection refused") },
		Postgres: func(ctx context.Context) error { return nil },
		SQS:      func(ctx context.Context) error { return nil },
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "error: connection refused", body["redis"])
	require.Equal(t, "ok", body["postgres"])
	require.Equal(t, "ok", body["sqs"])
	require.Equal(t, false, body["ready"])
}

// TestReadyZ_PostgresTimesOut — Postgres check returns
// context.DeadlineExceeded. Expected: 503, postgres: "timeout after 500ms".
func TestReadyZ_PostgresTimesOut(t *testing.T) {
	rec, body := doReadyZ(ReadinessChecks{
		Redis:    func(ctx context.Context) error { return nil },
		Postgres: func(ctx context.Context) error { return context.DeadlineExceeded },
		SQS:      func(ctx context.Context) error { return nil },
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "ok", body["redis"])
	require.Equal(t, "timeout after 500ms", body["postgres"])
	require.Equal(t, "ok", body["sqs"])
	require.Equal(t, false, body["ready"])
}

// TestReadyZ_SQSFailure — SQS ping returns an SDK-style error.
// Expected: 503, sqs: "error: ...".
func TestReadyZ_SQSFailure(t *testing.T) {
	rec, body := doReadyZ(ReadinessChecks{
		Redis:    func(ctx context.Context) error { return nil },
		Postgres: func(ctx context.Context) error { return nil },
		SQS: func(ctx context.Context) error {
			return errors.New("operation error SQS: GetQueueAttributes: 500 Internal Server Error")
		},
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "ok", body["redis"])
	require.Equal(t, "ok", body["postgres"])
	require.Equal(t, "error: operation error SQS: GetQueueAttributes: 500 Internal Server Error", body["sqs"])
	require.Equal(t, false, body["ready"])
}

// TestReadyZ_AllThreeFail — every check errors. Verifies each gets its
// own status string and the aggregate is false (not "first failure wins").
func TestReadyZ_AllThreeFail(t *testing.T) {
	rec, body := doReadyZ(ReadinessChecks{
		Redis:    func(ctx context.Context) error { return errors.New("redis down") },
		Postgres: func(ctx context.Context) error { return context.DeadlineExceeded },
		SQS:      func(ctx context.Context) error { return errors.New("sqs down") },
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "error: redis down", body["redis"])
	require.Equal(t, "timeout after 500ms", body["postgres"])
	require.Equal(t, "error: sqs down", body["sqs"])
	require.Equal(t, false, body["ready"])
}

// TestReadyZ_RunsInParallel — three slow checks (each ~200ms) must
// total roughly 200ms, not 600ms. Validates the goroutine fan-out.
func TestReadyZ_RunsInParallel(t *testing.T) {
	start := time.Now()
	rec, body := doReadyZ(ReadinessChecks{
		Redis:    slowOK(testCheckDuration),
		Postgres: slowOK(testCheckDuration),
		SQS:      slowOK(testCheckDuration),
	})
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, true, body["ready"])

	// Generous upper bound: parallel = 200ms, sequential would be 600ms.
	// Allow 450ms to absorb scheduler noise + the 500ms per-check timeout
	// overlap on slow CI machines.
	require.Less(t, elapsed, 450*time.Millisecond,
		"three 200ms checks should complete in roughly 200ms (parallel), got %v", elapsed)
}

// TestReadyZ_PerCheckTimeoutEnforced — check that sleeps longer than
// the 500ms per-check timeout should be cut short and the ctx.Err()
// surfaced as "timeout after 500ms". Combined with the parallelism
// test, this proves the bounded context is wired correctly.
func TestReadyZ_PerCheckTimeoutEnforced(t *testing.T) {
	rec, body := doReadyZ(ReadinessChecks{
		Redis:    func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		Postgres: func(ctx context.Context) error { return nil },
		SQS:      func(ctx context.Context) error { return nil },
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "timeout after 500ms", body["redis"])
}

// TestReadyZ_RealRedisAndPostgres_OK — integration: against a real
// Redis + Postgres (skips cleanly when either is unreachable).
// Validates the wiring of `redis.Ping(ctx, rdb)` + `pool.Ping(ctx)`
// doesn't crash, panic, or hang.
func TestReadyZ_RealRedisAndPostgres_OK(t *testing.T) {
	rdb := newTestClient(t)   // skips when Redis unavailable
	pool := apiNewTestPool(t) // skips when Postgres unavailable

	rec, body := doReadyZ(ReadinessChecks{
		Redis:    func(ctx context.Context) error { return redis.Ping(ctx, rdb) },
		Postgres: func(ctx context.Context) error { return pool.Ping(ctx) },
		SQS:      nil, // dev mode — LogPublisher in test environment
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%v", body)
	require.Equal(t, "ok", body["redis"])
	require.Equal(t, "ok", body["postgres"])
	require.Equal(t, "not_configured", body["sqs"])
	require.Equal(t, true, body["ready"])
}

// TestReadyZ_ResponseShape — JSON keys must match the spec exactly
// so K8s probe matchers / external clients don't break on a field
// rename. (The struct tags already enforce this; this test exists
// as a guardrail for future refactors.)
func TestReadyZ_ResponseShape(t *testing.T) {
	rec, rawBytes := doReadyZRaw(ReadinessChecks{
		Redis:    func(ctx context.Context) error { return nil },
		Postgres: func(ctx context.Context) error { return nil },
		SQS:      func(ctx context.Context) error { return nil },
	})
	require.Equal(t, http.StatusOK, rec.Code)

	// Re-parse raw bytes into the RawMessage map so we can ask
	// whether the top-level shape matches the spec example.
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rawBytes, &raw))
	require.Contains(t, raw, "redis")
	require.Contains(t, raw, "postgres")
	require.Contains(t, raw, "sqs")
	require.Contains(t, raw, "ready")
	require.Len(t, raw, 4, "no extra top-level keys allowed")
}
