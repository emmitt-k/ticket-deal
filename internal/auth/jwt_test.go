package auth_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/golang-jwt/jwt/v5"
)

// testSecret is exactly 32 bytes — the minimum the HS256 algorithm
// considers safe. Reused across every test in the package.
var testSecret = []byte("0123456789abcdef0123456789abcdef") // 32 bytes

// ─────────────────────────────────────────────────────────────────────
// Issue + Verify roundtrip
// ─────────────────────────────────────────────────────────────────────

func TestIssueVerifyRoundtrip(t *testing.T) {
	tok, err := auth.Issue("alice", 42, testSecret, 2*time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	c, err := auth.Verify(tok, testSecret)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Subject != "alice" {
		t.Errorf("sub = %q, want alice", c.Subject)
	}
	if c.EventID != 42 {
		t.Errorf("event_id = %d, want 42", c.EventID)
	}
	if c.ExpiresAt == nil {
		t.Error("exp claim missing")
	}
}

func TestIssueRejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		user    string
		event   int64
		secret  []byte
		ttl     time.Duration
		wantSub string
	}{
		{"empty_user", "", 1, testSecret, time.Minute, "userID"},
		{"zero_event", "alice", 0, testSecret, time.Minute, "eventID"},
		{"negative_event", "alice", -1, testSecret, time.Minute, "eventID"},
		{"short_secret", "alice", 1, []byte("too-short"), time.Minute, "secret"},
		{"zero_ttl", "alice", 1, testSecret, 0, "ttl"},
		{"negative_ttl", "alice", 1, testSecret, -time.Second, "ttl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := auth.Issue(tc.user, tc.event, tc.secret, tc.ttl)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q should contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// Verify rejects tampered, wrong-secret, expired, alg=none, empty/garbage
// ─────────────────────────────────────────────────────────────────────

func TestVerifyRejectsTampered(t *testing.T) {
	tok, _ := auth.Issue("alice", 42, testSecret, time.Minute)
	tampered := tok[:len(tok)-2] + "AA" // flip last byte of signature
	_, err := auth.Verify(tampered, testSecret)
	if !errors.Is(err, auth.ErrTokenSignature) {
		t.Errorf("tampered → ErrTokenSignature, got %v", err)
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	tok, _ := auth.Issue("alice", 42, testSecret, time.Minute)
	otherSecret := []byte("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz") // 32 bytes, different
	_, err := auth.Verify(tok, otherSecret)
	if !errors.Is(err, auth.ErrTokenSignature) {
		t.Errorf("wrong secret → ErrTokenSignature, got %v", err)
	}
}

// mintExpiredHS256Token forges a well-formed but already-expired
// token, bypassing Issue (which now rejects non-positive TTL).
// Used by the expired-token tests.
func mintExpiredHS256Token(t *testing.T) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":      "alice",
		"event_id": 42,
		"iat":      time.Now().Add(-time.Hour).Unix(),
		"exp":      time.Now().Add(-time.Second).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(testSecret)
	if err != nil {
		t.Fatalf("forge expired token: %v", err)
	}
	return signed
}

func TestVerifyRejectsExpired(t *testing.T) {
	tok := mintExpiredHS256Token(t)
	_, err := auth.Verify(tok, testSecret)
	if !errors.Is(err, auth.ErrTokenExpired) {
		t.Errorf("expired → ErrTokenExpired, got %v", err)
	}
}

// TestVerifyRejectsAlgNone is the critical-security test. An
// attacker who can pass `alg: none` and an empty signature can mint
// valid tokens. WithValidMethods in our Verify() must prevent this.
func TestVerifyRejectsAlgNone(t *testing.T) {
	claims := auth.Claims{
		EventID: 42,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "alice",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("forge alg=none token: %v", err)
	}

	_, err = auth.Verify(signed, testSecret)
	if !errors.Is(err, auth.ErrTokenAlgUnexpected) {
		t.Errorf("alg=none → ErrTokenAlgUnexpected, got %v", err)
	}
}

func TestVerifyRejectsEmptyAndGarbage(t *testing.T) {
	if _, err := auth.Verify("", testSecret); !errors.Is(err, auth.ErrTokenMalformed) {
		t.Errorf("empty → ErrTokenMalformed, got %v", err)
	}
	if _, err := auth.Verify("not.a.jwt", testSecret); !errors.Is(err, auth.ErrTokenMalformed) {
		t.Errorf("garbage → ErrTokenMalformed, got %v", err)
	}
	if _, err := auth.Verify("a.b.c", testSecret); !errors.Is(err, auth.ErrTokenMalformed) {
		t.Errorf("three-segment garbage → ErrTokenMalformed, got %v", err)
	}
}

// TestVerifyRejectsMissingClaims guards the integrity of the auth
// model: a token without a sub or without a positive event_id cannot
// safely reach a handler.
func TestVerifyRejectsMissingEventID(t *testing.T) {
	// Forge a token with HS256 but event_id=0.
	claims := jwt.MapClaims{
		"sub": "alice",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, _ := tok.SignedString(testSecret)

	_, err := auth.Verify(signed, testSecret)
	if !errors.Is(err, auth.ErrClaimsMissing) {
		t.Errorf("event_id=0 → ErrClaimsMissing, got %v", err)
	}
}
