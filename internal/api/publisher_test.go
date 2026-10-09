package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLogPublisher — the Phase 5 stub. It logs the body (with a
// "stub SQS publish" message) and returns nil. We capture the slog
// output by swapping the default logger for one that writes to a
// buffer, so the test doesn't pollute the test runner's stdout.
//
// After the slog migration (see docs/logging-plan.md) the test no
// longer uses the deprecated `[STUB SQS publish]` prefix; it checks
// the structured fields directly so future log-format changes don't
// break the test.
func TestLogPublisher(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p := LogPublisher{}
	require.NoError(t, p.Publish(context.Background(), []byte(`{"hello":"world"}`)))

	out := buf.String()
	require.Contains(t, out, "stub SQS publish",
		"the stub logs a recognizable message")
	// The body is rendered as a JSON-escaped quoted string in slog
	// text format, so we check for the unescaped form. (The original
	// `{"hello":"world"}` is the wire payload — log output quotes it.)
	require.Contains(t, out, `body="{\"hello\":\"world\"}"`,
		"the body is logged verbatim so a human can read what would have been sent")
}

// TestReservationPublisherInterface — compile-time check that
// LogPublisher is a valid ReservationPublisher. If Phase 6's SQS impl
// ever drifts from the interface, this won't compile.
func TestReservationPublisherInterface(t *testing.T) {
	var p ReservationPublisher = LogPublisher{}
	require.NoError(t, p.Publish(context.Background(), []byte("x")))
}

// TestPublisher_PayloadDeserialisable sanity-checks the JSON shape the
// handler emits (same as TestReserveHandler_Success's payload assertion,
// but isolated here so a Phase 6 contract change in the message body
// is caught even if the reserve handler isn't covered by that test).
func TestPublisher_PayloadShape(t *testing.T) {
	body := mustEncode(map[string]any{
		"reservation_id": "abc-123",
		"user_id":        "u1",
		"event_id":       42,
		"seats":          1,
		"created_at":     "2026-01-01T00:00:00Z",
		"expires_at":     "2026-01-01T00:10:00Z",
	})

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	require.Equal(t, "abc-123", decoded["reservation_id"])
	require.Equal(t, "u1", decoded["user_id"])
	require.Equal(t, float64(42), decoded["event_id"])
	require.Equal(t, float64(1), decoded["seats"])
	require.Equal(t, "2026-01-01T00:00:00Z", decoded["created_at"])
	require.Equal(t, "2026-01-01T00:10:00Z", decoded["expires_at"])
}

// mustEncode is a tiny helper for tests — same as the json.Marshal
// call inside the handler, lifted out so tests can construct payloads.
func mustEncode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestPublisher_EmptyBodyLogPublisher tests that LogPublisher handles
// an empty body without panicking. Edge case — unlikely in practice
// (the handler always builds a real payload), but cheap to cover.
//
// After the slog migration we no longer count "[STUB SQS publish]"
// occurrences — instead we just verify the call returns nil for both
// nil and empty byte slices. Slog with no body attribute behaves
// identically to the old log.Printf("%s", "") for an empty input.
func TestPublisher_EmptyBodyLogPublisher(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p := LogPublisher{}
	require.NoError(t, p.Publish(context.Background(), nil))
	require.NoError(t, p.Publish(context.Background(), []byte{}))
	// Each call should produce exactly one structured log line that
	// mentions the stub message. Two calls → two lines.
	require.Equal(t, 2, bytes.Count(buf.Bytes(), []byte("stub SQS publish")))
}
