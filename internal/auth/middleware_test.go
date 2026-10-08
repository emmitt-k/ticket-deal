package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/auth"
)

// Tests use the external test package (`auth_test`) so they exercise
// only the public API. Internal helpers stay unexported.

// TestMiddleware_NoHeader: every unauthenticated request must fail
// before any handler logic runs.
func TestMiddleware_NoHeader(t *testing.T) {
	rec := doAuthReq(t, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	body := decodeErrBody(t, rec)
	if body.Error != "missing_bearer" {
		t.Errorf("error = %q, want missing_bearer", body.Error)
	}
}

// TestMiddleware_WrongScheme: only "Bearer " is accepted; "Token "
// "Basic ", etc. all rejected.
func TestMiddleware_WrongScheme(t *testing.T) {
	for _, h := range []string{"Token abc", "Basic abc", "abc"} {
		rec := doAuthReq(t, h)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q: status = %d, want 401", h, rec.Code)
		}
	}
}

// TestMiddleware_EmptyToken: "Bearer " with nothing after means we
// can't even attempt to verify it.
func TestMiddleware_EmptyToken(t *testing.T) {
	rec := doAuthReq(t, "Bearer ")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestMiddleware_Tampered: payload or signature flipped ⇒ 401.
func TestMiddleware_Tampered(t *testing.T) {
	tok, _ := auth.Issue("alice", 42, testSecret, time.Minute)
	tampered := tok[:len(tok)-2] + "AA"
	rec := doAuthReq(t, "Bearer "+tampered)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestMiddleware_Expired: past-exp token rejected, error code
// "token_expired" so client can show "session expired, please /enter
// again".
func TestMiddleware_Expired(t *testing.T) {
	tok := mintExpiredHS256Token(t)
	rec := doAuthReq(t, "Bearer "+tok)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	body := decodeErrBody(t, rec)
	if body.Error != "token_expired" {
		t.Errorf("error = %q, want token_expired", body.Error)
	}
}

// TestMiddleware_ValidPassesAndClaimsInjected is the happy-path
// integration test: middleware must hand verified claims to the
// downstream handler.
func TestMiddleware_ValidPassesAndClaimsInjected(t *testing.T) {
	tok, _ := auth.Issue("alice", 42, testSecret, time.Minute)

	var seenSub string
	var seenEvent int64
	handler := auth.Middleware(testSecret)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			c := auth.ClaimsFromContext(r.Context())
			if c == nil {
				t.Error("ClaimsFromContext returned nil")
				http.Error(w, "no claims", http.StatusInternalServerError)
				return
			}
			seenSub = c.Subject
			seenEvent = c.EventID
			w.WriteHeader(http.StatusOK)
		},
	))

	req := httptest.NewRequest("GET", "/api/tickets/reserve", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seenSub != "alice" {
		t.Errorf("sub = %q, want alice", seenSub)
	}
	if seenEvent != 42 {
		t.Errorf("event_id = %d, want 42", seenEvent)
	}
}

// TestClaimsFromContext_Empty: nil claims is the right answer when
// no middleware ran — not a panic.
func TestClaimsFromContext_Empty(t *testing.T) {
	if c := auth.ClaimsFromContext(httptest.NewRequest("GET", "/", nil).Context()); c != nil {
		t.Errorf("expected nil on bare context, got %+v", c)
	}
}

// ─────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────

func doAuthReq(t *testing.T, authHeader string) *httptest.ResponseRecorder {
	t.Helper()

	hit := false
	handler := auth.Middleware(testSecret)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			hit = true
			w.WriteHeader(http.StatusOK)
		},
	))

	req := httptest.NewRequest("GET", "/api/tickets/reserve", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if hit {
		t.Fatal("downstream handler ran on a request the middleware should have rejected")
	}
	return rec
}

func decodeErrBody(t *testing.T, rec *httptest.ResponseRecorder) apiutil.ErrorBody {
	t.Helper()
	var b apiutil.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode error body: %v (raw=%s)", err, rec.Body.String())
	}
	return b
}
