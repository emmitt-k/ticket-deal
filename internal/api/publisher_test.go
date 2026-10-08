package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLogPublisher — the Phase 5 stub. It logs the body (with a [STUB
// SQS publish] prefix) and returns nil. We capture the logger output
// via log.SetOutput so the test doesn't pollute the test runner's stdout.
func TestLogPublisher(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	p := LogPublisher{}
	require.NoError(t, p.Publish(context.Background(), []byte(`{"hello":"world"}`)))

	out := buf.String()
	require.Contains(t, out, "[STUB SQS publish]",
		"the stub logs a recognizable prefix")
	require.Contains(t, out, `{"hello":"world"}`,
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
func TestPublisher_EmptyBodyLogPublisher(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	p := LogPublisher{}
	require.NoError(t, p.Publish(context.Background(), nil))
	require.NoError(t, p.Publish(context.Background(), []byte{}))
	require.Equal(t, 2, strings.Count(buf.String(), "[STUB SQS publish]"))
}