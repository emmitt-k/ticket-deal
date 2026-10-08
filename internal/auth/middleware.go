package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
)

// ctxKey is a private named type. Using an unexported named type for
// context keys is the standard Go pattern — it prevents accidental
// collisions with keys defined by other packages.
type ctxKey int

const claimsKey ctxKey = 1

// ClaimsFromContext returns the verified claims placed in the request
// context by Middleware, or nil when called on a context that didn't
// pass through Middleware (e.g. an unauthenticated public route).
func ClaimsFromContext(ctx context.Context) *Claims {
	c, _ := ctx.Value(claimsKey).(*Claims)
	return c
}

// Middleware returns a chi middleware that verifies the
// "Authorization: Bearer <token>" header, then injects the verified
// *Claims into the request context.
//
// Any failure short-circuits with a 401 JSON response. The response
// uses an opaque error code ("missing_bearer" / "invalid_token" /
// "token_expired") so clients can branch on cause without us leaking
// any cryptographic detail.
func Middleware(secret []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := r.Header.Get("Authorization")
			if raw == "" || !strings.HasPrefix(raw, "Bearer ") {
				apiutil.WriteError(w, http.StatusUnauthorized,
					"missing_bearer", "Authorization: Bearer <token> header required")
				return
			}

			tok := strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
			if tok == "" {
				apiutil.WriteError(w, http.StatusUnauthorized,
					"missing_bearer", "token is empty")
				return
			}

			claims, err := Verify(tok, secret)
			if err != nil {
				code := classifyAuthError(err)
				apiutil.WriteError(w, http.StatusUnauthorized, code, "token rejected")
				return
			}

			ctx := context.WithValue(r.Context(), claimsKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// classifyAuthError narrows the verify error into a client-safe code.
// Critically it does NOT echo the underlying reason ("signature
// invalid", "wrong algorithm") back to the user — attackers don't
// deserve that signal.
func classifyAuthError(err error) string {
	switch {
	case errors.Is(err, ErrTokenExpired):
		return "token_expired"
	case errors.Is(err, ErrTokenSignature),
		errors.Is(err, ErrTokenAlgUnexpected):
		return "invalid_signature"
	default:
		return "invalid_token"
	}
}
