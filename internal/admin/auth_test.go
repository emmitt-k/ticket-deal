package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	testSecret  = "admin-secret-one"
	testSecret2 = "admin-secret-two"
)

// sign builds the headers for a request signed with secret.
func sign(t *testing.T, secret, method, uri string, body []byte, at time.Time) (string, string) {
	t.Helper()
	ts := at.Unix()
	return Sign(secret, method, uri, body, ts), strconv.FormatInt(ts, 10)
}

// ── The happy path ────────────────────────────────────────────────────

// TestVerify_ValidSignature — the actor id is the FINGERPRINT of the
// key that signed, so the audit trail records WHICH key acted without
// the secret ever being stored.
func TestVerify_ValidSignature(t *testing.T) {
	v := NewVerifier([]string{testSecret})
	now := time.Now()
	sig, ts := sign(t, testSecret, http.MethodPost, "/admin/reservations/x/cancel", nil, now)

	actor, err := v.Verify(http.MethodPost, "/admin/reservations/x/cancel", nil, sig, ts)
	require.NoError(t, err)
	require.NotEmpty(t, actor)
	require.Len(t, actor, 8, "the fingerprint is 8 hex characters")
	require.NotContains(t, actor, testSecret, "the fingerprint must not leak the secret")
}

// TestVerify_KeyRotation — more than one key may be configured, and
// each has its OWN fingerprint. That is what makes rotation possible:
// the audit trail can tell the two keys apart.
func TestVerify_KeyRotation(t *testing.T) {
	v := NewVerifier([]string{testSecret, testSecret2})
	now := time.Now()

	sig1, ts1 := sign(t, testSecret, http.MethodGet, "/admin/reservations/lookup", nil, now)
	sig2, ts2 := sign(t, testSecret2, http.MethodGet, "/admin/reservations/lookup", nil, now)

	actor1, err := v.Verify(http.MethodGet, "/admin/reservations/lookup", nil, sig1, ts1)
	require.NoError(t, err)
	actor2, err := v.Verify(http.MethodGet, "/admin/reservations/lookup", nil, sig2, ts2)
	require.NoError(t, err)

	require.NotEqual(t, actor1, actor2, "the two keys must have different fingerprints")
	require.Len(t, v.Fingerprints(), 2)
}

// ── The rejections ────────────────────────────────────────────────────

// TestVerify_WrongKey — a signature made with a key that is not
// configured must not verify.
func TestVerify_WrongKey(t *testing.T) {
	v := NewVerifier([]string{testSecret})
	now := time.Now()
	sig, ts := sign(t, "some-other-secret", http.MethodGet, "/admin/reservations/lookup", nil, now)

	_, err := v.Verify(http.MethodGet, "/admin/reservations/lookup", nil, sig, ts)
	require.ErrorIs(t, err, ErrInvalidSignature)
}

// TestVerify_MissingCredentials — no signature or no timestamp.
func TestVerify_MissingCredentials(t *testing.T) {
	v := NewVerifier([]string{testSecret})
	now := time.Now()
	sig, ts := sign(t, testSecret, http.MethodGet, "/admin/reservations/lookup", nil, now)

	_, err := v.Verify(http.MethodGet, "/admin/reservations/lookup", nil, "", ts)
	require.ErrorIs(t, err, ErrMissingCredentials)

	_, err = v.Verify(http.MethodGet, "/admin/reservations/lookup", nil, sig, "")
	require.ErrorIs(t, err, ErrMissingCredentials)
}

// TestVerify_ReplayWindow — the timestamp must be within the window
// in BOTH directions. This is the replay protection: a captured
// signature is only good for five minutes.
func TestVerify_ReplayWindow(t *testing.T) {
	v := NewVerifier([]string{testSecret})
	now := time.Now()

	t.Run("too old", func(t *testing.T) {
		old := now.Add(-(ReplayWindowSeconds + 60) * time.Second)
		sig, ts := sign(t, testSecret, http.MethodGet, "/admin/reservations/lookup", nil, old)
		_, err := v.Verify(http.MethodGet, "/admin/reservations/lookup", nil, sig, ts)
		require.ErrorIs(t, err, ErrStaleTimestamp)
	})

	t.Run("too far in the future", func(t *testing.T) {
		future := now.Add((ReplayWindowSeconds + 60) * time.Second)
		sig, ts := sign(t, testSecret, http.MethodGet, "/admin/reservations/lookup", nil, future)
		_, err := v.Verify(http.MethodGet, "/admin/reservations/lookup", nil, sig, ts)
		require.ErrorIs(t, err, ErrStaleTimestamp)
	})

	t.Run("just inside the window", func(t *testing.T) {
		inside := now.Add(-(ReplayWindowSeconds - 30) * time.Second)
		sig, ts := sign(t, testSecret, http.MethodGet, "/admin/reservations/lookup", nil, inside)
		_, err := v.Verify(http.MethodGet, "/admin/reservations/lookup", nil, sig, ts)
		require.NoError(t, err)
	})

	t.Run("not a number", func(t *testing.T) {
		sig, _ := sign(t, testSecret, http.MethodGet, "/admin/reservations/lookup", nil, now)
		_, err := v.Verify(http.MethodGet, "/admin/reservations/lookup", nil, sig, "yesterday")
		require.ErrorIs(t, err, ErrStaleTimestamp)
	})
}

// TestVerify_TamperedRequest — the signature covers the method, the
// URI and the body, so changing ANY of them invalidates it.
//
// The query string is the interesting one: a signature that did not
// cover it could be replayed against a different user_id, which would
// turn a support lookup into a data-exfiltration tool.
func TestVerify_TamperedRequest(t *testing.T) {
	v := NewVerifier([]string{testSecret})
	now := time.Now()
	body := []byte(`{"reason":"customer asked"}`)

	t.Run("body changed", func(t *testing.T) {
		sig, ts := sign(t, testSecret, http.MethodPost, "/admin/reservations/x/cancel", body, now)
		_, err := v.Verify(http.MethodPost, "/admin/reservations/x/cancel",
			[]byte(`{"reason":"someone else asked"}`), sig, ts)
		require.ErrorIs(t, err, ErrInvalidSignature)
	})

	t.Run("path changed", func(t *testing.T) {
		sig, ts := sign(t, testSecret, http.MethodPost, "/admin/reservations/x/cancel", body, now)
		_, err := v.Verify(http.MethodPost, "/admin/reservations/y/cancel", body, sig, ts)
		require.ErrorIs(t, err, ErrInvalidSignature)
	})

	t.Run("method changed", func(t *testing.T) {
		sig, ts := sign(t, testSecret, http.MethodPost, "/admin/reservations/x/cancel", body, now)
		_, err := v.Verify(http.MethodGet, "/admin/reservations/x/cancel", body, sig, ts)
		require.ErrorIs(t, err, ErrInvalidSignature)
	})

	t.Run("query string changed", func(t *testing.T) {
		sig, ts := sign(t, testSecret, http.MethodGet, "/admin/reservations/lookup?user_id=alice", nil, now)
		_, err := v.Verify(http.MethodGet, "/admin/reservations/lookup?user_id=bob", nil, sig, ts)
		require.ErrorIs(t, err, ErrInvalidSignature)
	})
}

// TestVerify_NoKeysConfigured — a verifier with no keys rejects
// everything. There is no "open" mode.
func TestVerify_NoKeysConfigured(t *testing.T) {
	v := NewVerifier(nil)
	require.False(t, v.Enabled())
	require.Empty(t, v.Fingerprints())

	now := time.Now()
	sig, ts := sign(t, testSecret, http.MethodGet, "/admin/reservations/lookup", nil, now)
	_, err := v.Verify(http.MethodGet, "/admin/reservations/lookup", nil, sig, ts)
	require.ErrorIs(t, err, ErrInvalidSignature)
}

// TestNewVerifier_IgnoresEmptySecrets — a stray comma must not
// produce an empty key that everything would match.
func TestNewVerifier_IgnoresEmptySecrets(t *testing.T) {
	v := NewVerifier([]string{"", testSecret, ""})
	require.True(t, v.Enabled())
	require.Len(t, v.Fingerprints(), 1)
}

// ── The middleware ────────────────────────────────────────────────────

// TestMiddleware — the request only reaches the handler when the
// signature verifies, the body survives the verification, and the
// actor lands in the context.
func TestMiddleware(t *testing.T) {
	v := NewVerifier([]string{testSecret})
	now := time.Now()

	var gotActor string
	var gotBody string
	handler := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotActor = ActorFromContext(r.Context())
		raw := make([]byte, 64)
		n, _ := r.Body.Read(raw)
		gotBody = string(raw[:n])
		w.WriteHeader(http.StatusOK)
	}))

	t.Run("a valid request reaches the handler", func(t *testing.T) {
		body := []byte(`{"reason":"customer asked"}`)
		uri := "/admin/reservations/x/cancel"
		sig, ts := sign(t, testSecret, http.MethodPost, uri, body, now)

		req := httptest.NewRequest(http.MethodPost, uri, strings.NewReader(string(body)))
		req.Header.Set(HeaderKey, sig)
		req.Header.Set(HeaderTimestamp, ts)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.NotEmpty(t, gotActor, "the actor must reach the handler")
		require.Equal(t, string(body), gotBody, "the body must survive the verification")
	})

	t.Run("a forged request is rejected", func(t *testing.T) {
		uri := "/admin/reservations/x/cancel"
		sig, ts := sign(t, "not-the-key", http.MethodPost, uri, nil, now)

		req := httptest.NewRequest(http.MethodPost, uri, strings.NewReader(""))
		req.Header.Set(HeaderKey, sig)
		req.Header.Set(HeaderTimestamp, ts)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		require.Equal(t, http.StatusUnauthorized, rec.Code)
		require.Contains(t, rec.Body.String(), "invalid_signature")
	})

	t.Run("a stale request is rejected with its own code", func(t *testing.T) {
		uri := "/admin/reservations/x/cancel"
		old := now.Add(-(ReplayWindowSeconds + 60) * time.Second)
		sig, ts := sign(t, testSecret, http.MethodPost, uri, nil, old)

		req := httptest.NewRequest(http.MethodPost, uri, strings.NewReader(""))
		req.Header.Set(HeaderKey, sig)
		req.Header.Set(HeaderTimestamp, ts)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		require.Equal(t, http.StatusUnauthorized, rec.Code)
		require.Contains(t, rec.Body.String(), "stale_timestamp")
	})

	t.Run("missing credentials are rejected with their own code", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/admin/reservations/lookup", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		require.Equal(t, http.StatusUnauthorized, rec.Code)
		require.Contains(t, rec.Body.String(), "missing_credentials")
	})
}

// TestActorFromContext_Empty — a context that never went through the
// verifier has no actor.
func TestActorFromContext_Empty(t *testing.T) {
	require.Empty(t, ActorFromContext(context.Background()))
}

// TestSigningPayload_IsStable — the payload format is a contract
// between the server and a support tool. If it changes, every tool
// breaks at once, so it is pinned here.
func TestSigningPayload_IsStable(t *testing.T) {
	got := string(SigningPayload("post", "/admin/reservations/x/cancel", []byte("b"), 1700000000))
	require.Equal(t, "POST\n/admin/reservations/x/cancel\nb\n1700000000", got)
}
