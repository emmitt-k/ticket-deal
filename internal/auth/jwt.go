// Package auth implements the lite auth model: HS256 JWT carrying
// exactly three claims — sub, event_id, exp. The token is the only
// proof of identity between /enter and /reserve, so its correctness
// gates the entire ticket-purchase flow.
//
// Design rationale (claims shape, expiry, signing-method enforcement,
// threat model): see docs/implementation-plan.md §Auth Design (lite).
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is the payload we sign.
//
// EventID scopes the token to a single drop: a token minted for
// event A cannot be used to reserve on event B. This catches the most
// common token-replay attempt (waiting for one drop, then racing
// against a hotter one) at the middleware layer with zero state.
type Claims struct {
	EventID int64 `json:"event_id"`
	jwt.RegisteredClaims
}

// Public, stable errors. Callers (middleware, tests) inspect these
// with errors.Is rather than poking at the underlying jwt library, so
// switching implementations later requires no caller changes.
var (
	ErrTokenMalformed      = errors.New("auth: token malformed")
	ErrTokenSignature      = errors.New("auth: token signature invalid")
	ErrTokenExpired        = errors.New("auth: token expired")
	ErrTokenNotYetValid    = errors.New("auth: token not yet valid")
	ErrTokenAlgUnexpected  = errors.New("auth: unexpected signing method")
	ErrClaimsMissing       = errors.New("auth: required claim missing")
)

// Issue signs a new JWT for the given user and event, valid for ttl.
//
// The caller is responsible for ensuring secret is at least 32 bytes;
// config.Load already enforces this at startup, so app code can rely
// on the check at the edge. We re-check here because Issue is exported.
func Issue(userID string, eventID int64, secret []byte, ttl time.Duration) (string, error) {
	if len(secret) < 32 {
		return "", errors.New("auth: secret must be at least 32 bytes")
	}
	if userID == "" {
		return "", errors.New("auth: userID is required")
	}
	if eventID <= 0 {
		return "", errors.New("auth: eventID must be positive")
	}
	if ttl <= 0 {
		return "", errors.New("auth: ttl must be positive")
	}

	now := time.Now()
	claims := Claims{
		EventID: eventID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(secret)
}

// Verify parses and validates the token. The signing method is
// restricted to HS256 via jwt.WithValidMethods — this is the single
// most important line in the file: it explicitly rejects alg=none
// and alg=RS256 swaps, which are the classic JWT vulnerabilities.
func Verify(tokenString string, secret []byte) (*Claims, error) {
	if tokenString == "" {
		return nil, ErrTokenMalformed
	}

	const expectedAlg = "HS256"
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{expectedAlg}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	)

	var claims Claims
	tok, err := parser.ParseWithClaims(tokenString, &claims, func(_ *jwt.Token) (any, error) {
		return secret, nil
	})
	if err != nil {
		// jwt v5 collapses "alg not in allowed list" into ErrTokenSignatureInvalid
		// — the only way to tell the two apart is to inspect the half-parsed
		// tok.Method.Alg(). When it isn't HS256, that's our ErrTokenAlgUnexpected
		// case (forged alg=none or alg=RS256 swap).
		if tok != nil && tok.Method != nil && tok.Method.Alg() != expectedAlg {
			return nil, ErrTokenAlgUnexpected
		}
		return nil, classifyError(err)
	}
	if !tok.Valid {
		return nil, ErrTokenMalformed
	}

	// Defence in depth: parser already validated exp/nbf via RegisteredClaims,
	// but sub and event_id are custom and not auto-checked.
	if claims.Subject == "" {
		return nil, fmt.Errorf("%w: sub", ErrClaimsMissing)
	}
	if claims.EventID <= 0 {
		return nil, fmt.Errorf("%w: event_id", ErrClaimsMissing)
	}

	return &claims, nil
}

// classifyError maps jwt library errors to our stable set so callers
// don't need to keep up with jwt's internal taxonomy.
//
// Order matters: the "alg not allowed" branch chains both
// ErrTokenUnverifiable AND ErrTokenSignatureInvalid; we must catch
// the more specific one first.
func classifyError(err error) error {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return ErrTokenExpired
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return ErrTokenNotYetValid
	case errors.Is(err, jwt.ErrTokenUnverifiable):
		// Covers unexpected alg (e.g. RS256, none) when the parser
		// refuses to even attempt verification. Chained together with
		// ErrTokenSignatureInvalid, so checked first.
		return ErrTokenAlgUnexpected
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return ErrTokenSignature
	case errors.Is(err, jwt.ErrTokenMalformed):
		return ErrTokenMalformed
	default:
		return fmt.Errorf("auth: %w", err)
	}
}
