package queue

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestReservation_RoundTrip — JSON Marshal → Unmarshal produces the
// same struct (same field values). Catches drift in the wire format.
func TestReservation_RoundTrip(t *testing.T) {
	original := Reservation{
		ReservationID: "abc-123-def",
		UserID:        "alice",
		EventID:       42,
		Seats:         3,
		CreatedAt:     "2026-10-06T10:00:00Z",
		ExpiresAt:     "2026-10-06T10:10:00Z",
	}

	body, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded Reservation
	require.NoError(t, json.Unmarshal(body, &decoded))
	require.Equal(t, original, decoded)
}

// TestReservation_JSONShape — the field names are the public wire
// contract. If someone renames `ReservationID` to `ID` without
// updating the JSON tag, the worker would silently see a missing
// field. This test pins the JSON keys.
func TestReservation_JSONShape(t *testing.T) {
	r := Reservation{
		ReservationID: "x",
		UserID:        "x",
		EventID:       42,
		Seats:         1,
		CreatedAt:     "x",
		ExpiresAt:     "x",
	}
	body, err := json.Marshal(r)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))

	require.Contains(t, raw, "reservation_id")
	require.Contains(t, raw, "user_id")
	require.Contains(t, raw, "event_id")
	require.Contains(t, raw, "seats")
	require.Contains(t, raw, "created_at")
	require.Contains(t, raw, "expires_at")
	require.Len(t, raw, 6, "no extra fields")
}

// TestReservation_Parse_HappyPath — both timestamps parse cleanly.
func TestReservation_Parse_HappyPath(t *testing.T) {
	r := Reservation{
		ReservationID: "x",
		CreatedAt:     "2026-10-06T10:00:00Z",
		ExpiresAt:     "2026-10-06T10:10:00Z",
	}
	parsed, err := r.Parse()
	require.NoError(t, err)
	require.Equal(t, 2026, parsed.CreatedAtT.Year())
	require.Equal(t, time.October, parsed.CreatedAtT.Month())
	require.Equal(t, 6, parsed.CreatedAtT.Day())
	require.Equal(t, 10, parsed.ExpiresAtT.Hour())

	// 10 minutes apart
	require.Equal(t, 10*time.Minute, parsed.ExpiresAtT.Sub(parsed.CreatedAtT))
}

// TestReservation_Parse_BadCreatedAt — returns an error so the worker
// can log and skip (delete) the malformed message.
func TestReservation_Parse_BadCreatedAt(t *testing.T) {
	r := Reservation{
		CreatedAt: "not-a-timestamp",
		ExpiresAt: "2026-10-06T10:10:00Z",
	}
	_, err := r.Parse()
	require.Error(t, err)
}

// TestReservation_Parse_BadExpiresAt — same, but the other field.
func TestReservation_Parse_BadExpiresAt(t *testing.T) {
	r := Reservation{
		CreatedAt: "2026-10-06T10:00:00Z",
		ExpiresAt: "garbage",
	}
	_, err := r.Parse()
	require.Error(t, err)
}

// TestReservation_Parse_AcceptsOffset — RFC 3339 with offset is valid
// (UTC 'Z' or '±HH:MM'). The parser must accept both.
func TestReservation_Parse_AcceptsOffset(t *testing.T) {
	r := Reservation{
		CreatedAt: "2026-10-06T10:00:00+07:00",
		ExpiresAt: "2026-10-06T10:10:00-05:00",
	}
	parsed, err := r.Parse()
	require.NoError(t, err)
	_, offset := parsed.CreatedAtT.Zone()
	// +07:00 → 7*3600 = 25200 seconds east of UTC
	require.Equal(t, 25200, offset)
}

// TestMessageHandlerFunc — the function-shaped adapter must satisfy
// the MessageHandler interface. Compile-time check via assignment.
func TestMessageHandlerFunc(t *testing.T) {
	var h MessageHandler = MessageHandlerFunc(func(_ context.Context, _ []byte) error {
		return nil
	})
	require.NotNil(t, h)
}