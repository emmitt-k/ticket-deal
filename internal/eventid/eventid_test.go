package eventid_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/emmitt-k/ticket-deal/internal/eventid"
)

// TestNew mints a distinct, well-formed id every time.
//
// The uniqueness is not a nice-to-have: `events.id` is the primary
// key, and two events sharing an id is a duplicate-key error at
// insert time.
func TestNew(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := eventid.New()
		require.True(t, eventid.Valid(id), "New() must return a valid id, got %q", id)
		require.False(t, seen[id], "New() returned a duplicate id: %q", id)
		seen[id] = true
	}
}

// TestValid covers the accepted shape and, more importantly, the
// rejected ones.
//
// The check is deliberately STRICTER than "parses as a uuid". The
// google/uuid library accepts the urn:uuid: prefix, the braced form
// and the 32-character unhyphenated form. Those all name the SAME
// event — but they spell it differently, and a Redis key built from
// one spelling would not match a key built from another. A hold key
// that does not match its hold is a seat that leaks.
//
// So exactly ONE spelling is accepted: the canonical 36-character
// hyphenated lowercase form.
func TestValid(t *testing.T) {
	valid := []string{
		"9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b",
		"00000000-0000-4000-8000-000000000000",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
		"00000000-0000-0000-0000-000000000001",
	}
	for _, s := range valid {
		t.Run("valid/"+s, func(t *testing.T) {
			require.True(t, eventid.Valid(s), "%q should be valid", s)
		})
	}

	invalid := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"uppercase", "9F8C1E2A-3B4D-4E5F-8A9B-0C1D2E3F4A5B"},
		{"no_hyphens", "9f8c1e2a3b4d4e5f8a9b0c1d2e3f4a5b"},
		{"urn_prefix", "urn:uuid:9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b"},
		{"braced", "{9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b}"},
		{"too_short", "9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5"},
		{"too_long", "9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5bb"},
		{"wrong_hyphen_positions", "9f8c1e2a3-b4d-4e5f-8a9b-0c1d2e3f4a5b"},
		{"non_hex", "9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5z"},
		{"the_old_int64", "42"},
		{"just_a_number", "12345678901234567890123456789012"},
		{"has_a_colon", "9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b:x"},
		{"has_a_space", "9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b "},
	}
	for _, tc := range invalid {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			require.False(t, eventid.Valid(tc.id), "%q should be invalid", tc.id)
		})
	}
}

// TestParse returns the id unchanged when it is valid.
func TestParse(t *testing.T) {
	const id = "9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b"

	got, err := eventid.Parse(id)
	require.NoError(t, err)
	require.Equal(t, id, got)
}

// TestParseRejectsInvalid — the error wraps ErrInvalid so callers can
// use errors.Is rather than matching on the message.
func TestParseRejectsInvalid(t *testing.T) {
	for _, bad := range []string{"", "42", "not-a-uuid", "9F8C1E2A-3B4D-4E5F-8A9B-0C1D2E3F4A5B"} {
		_, err := eventid.Parse(bad)
		require.Error(t, err, "%q should be rejected", bad)
		require.True(t, errors.Is(err, eventid.ErrInvalid),
			"the error for %q should wrap ErrInvalid, got %v", bad, err)
	}
}
