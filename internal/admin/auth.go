// Package admin is the support/ops API — the "support override" every
// real product has, and usually glosses over.
//
// It is deliberately a small, bounded set of operations:
//
//	GET  /admin/reservations/lookup?user_id=…   support lookup
//	GET  /admin/reservations/{id}                support lookup
//	POST /admin/reservations/{id}/cancel         cancel a held reservation
//	POST /admin/reservations/{id}/release-seat   put a seat back on sale
//
// Every write is audited with actor_type='admin', so "who did this"
// has an answer. (The audit trail from Layer 2 item 6 already
// anticipated 'admin' as an actor type.)
//
// # Why an HMAC and not a bearer token
//
// A bearer token is a secret that travels with every request, and a
// captured one is replayable forever. An HMAC over the REQUEST is
// different: the signature covers the method, the path, the body and
// a timestamp, so a captured signature is only good for one request,
// for one body, for five minutes.
//
// The trade-off is that the caller has to sign, which rules out a
// browser session. That is the right trade here: this is a machine
// API for an internal support tool, not something a human clicks in a
// browser.
//
// # The replay window is the replay protection
//
// A request whose timestamp is more than ReplayWindowSeconds away from
// the server's clock is rejected. That bounds how long a captured
// signature stays useful. It does not prevent a replay WITHIN the
// window — for that you would need a nonce store, which is a
// different (stateful) trade. Five minutes is the window the project
// roadmap specifies.
package admin

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// HeaderKey carries the HMAC signature, hex-encoded.
	HeaderKey = "X-Admin-Key"
	// HeaderTimestamp carries the unix-seconds timestamp the signature
	// was computed over.
	HeaderTimestamp = "X-Admin-Timestamp"
)

// ReplayWindowSeconds is how far a request's timestamp may be from the
// server's clock, in either direction.
const ReplayWindowSeconds = 5 * 60

// maxBodyBytes bounds how much of a request body is read for
// signature verification. The admin API takes no bodies, but the
// middleware reads them to sign, and an unbounded read is a memory
// denial-of-service.
const maxBodyBytes = 1 << 20 // 1 MiB

// The verification failures. Each maps to its own error code, because
// "your request was rejected" is not a useful thing to tell an
// operator trying to work out what their tool is doing wrong.
var (
	// ErrMissingCredentials — no signature or no timestamp header.
	ErrMissingCredentials = errors.New("admin: missing credentials")
	// ErrStaleTimestamp — the timestamp is outside the replay window.
	ErrStaleTimestamp = errors.New("admin: timestamp outside the replay window")
	// ErrInvalidSignature — the signature did not match.
	ErrInvalidSignature = errors.New("admin: invalid signature")
)

// key is one configured admin secret and its fingerprint.
type key struct {
	secret      []byte
	fingerprint string
}

// Verifier checks admin request signatures.
//
// The zero value is not usable — build one with NewVerifier.
type Verifier struct {
	keys   []key
	window time.Duration
	now    func() time.Time
}

// NewVerifier builds a Verifier from a list of admin secrets.
//
// More than one secret is supported so keys can be ROTATED: add the
// new one, deploy, remove the old one. The audit trail records the
// FINGERPRINT of the key that acted, not the key itself, so an
// operator can tell the two apart without the secret ever being
// stored.
//
// A Verifier with no keys rejects everything.
func NewVerifier(secrets []string) *Verifier {
	v := &Verifier{
		window: ReplayWindowSeconds * time.Second,
		now:    time.Now,
	}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		sum := sha256.Sum256([]byte(s))
		v.keys = append(v.keys, key{
			secret:      []byte(s),
			fingerprint: hex.EncodeToString(sum[:])[:8],
		})
	}
	return v
}

// Enabled reports whether any admin key is configured. The API uses
// this to answer 503 instead of mounting routes that would always
// reject.
func (v *Verifier) Enabled() bool { return v != nil && len(v.keys) > 0 }

// Fingerprints returns the fingerprints of the configured keys. Used by
// the health endpoint and by the tests.
func (v *Verifier) Fingerprints() []string {
	if v == nil {
		return nil
	}
	out := make([]string, 0, len(v.keys))
	for _, k := range v.keys {
		out = append(out, k.fingerprint)
	}
	return out
}

// SigningPayload returns the exact bytes a client must sign.
//
// It is exported so a support tool and the tests build the SAME
// payload — the signing scheme is a contract between two programs,
// and duplicating it in two places is how it drifts.
//
//	method + "\n" + uri + "\n" + body + "\n" + timestamp
//
// The uri is the FULL request URI including the query string: a
// signature that did not cover the query could be replayed against a
// different user_id.
func SigningPayload(method, uri string, body []byte, timestamp int64) []byte {
	var b bytes.Buffer
	b.WriteString(strings.ToUpper(method))
	b.WriteByte('\n')
	b.WriteString(uri)
	b.WriteByte('\n')
	b.Write(body)
	b.WriteByte('\n')
	b.WriteString(strconv.FormatInt(timestamp, 10))
	return b.Bytes()
}

// Sign computes the signature for a request. This is what a support
// tool calls.
func Sign(secret, method, uri string, body []byte, timestamp int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(SigningPayload(method, uri, body, timestamp))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a request's signature and returns the FINGERPRINT of
// the key that signed it — that is the actor id written to the audit
// trail.
func (v *Verifier) Verify(method, uri string, body []byte, sig, timestamp string) (string, error) {
	if v == nil || len(v.keys) == 0 {
		return "", ErrInvalidSignature
	}
	if sig == "" || timestamp == "" {
		return "", ErrMissingCredentials
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return "", ErrStaleTimestamp
	}
	// The window is checked BEFORE the signature: a stale request is
	// rejected whether or not it is signed correctly, and it saves the
	// HMAC work.
	skew := v.now().Sub(time.Unix(ts, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > v.window {
		return "", ErrStaleTimestamp
	}

	payload := SigningPayload(method, uri, body, ts)
	for _, k := range v.keys {
		mac := hmac.New(sha256.New, k.secret)
		mac.Write(payload)
		// hmac.Equal is constant-time — a byte-by-byte comparison
		// leaks how much of a guessed signature was right.
		if hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(sig)) {
			return k.fingerprint, nil
		}
	}
	return "", ErrInvalidSignature
}

// actorKey is the context key the verified actor id is stored under.
type actorKey struct{}

// ActorFromContext returns the verified admin actor id, or "" if the
// request did not pass through the verifier.
func ActorFromContext(ctx context.Context) string {
	id, _ := ctx.Value(actorKey{}).(string)
	return id
}

// Middleware verifies the admin signature on every request.
//
// On success it stores the actor id in the request context and calls
// next. On failure it answers 401 with a code that says WHICH check
// failed, because "your request was rejected" is not a useful thing
// to tell an operator working out what their tool is doing wrong.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The body has to be read to verify the signature, and then
		// restored for the handler.
		var body []byte
		if r.Body != nil {
			limited := http.MaxBytesReader(w, r.Body, maxBodyBytes)
			raw, err := io.ReadAll(limited)
			if err != nil {
				writeError(w, http.StatusRequestEntityTooLarge, "body_too_large",
					"the request body could not be read")
				return
			}
			body = raw
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}

		actor, err := v.Verify(r.Method, r.URL.RequestURI(), body,
			r.Header.Get(HeaderKey), r.Header.Get(HeaderTimestamp))
		if err != nil {
			switch {
			case errors.Is(err, ErrMissingCredentials):
				writeError(w, http.StatusUnauthorized, "missing_credentials",
					fmt.Sprintf("%s and %s headers are required", HeaderKey, HeaderTimestamp))
			case errors.Is(err, ErrStaleTimestamp):
				writeError(w, http.StatusUnauthorized, "stale_timestamp",
					fmt.Sprintf("the timestamp must be within %d seconds of the server's clock", ReplayWindowSeconds))
			default:
				writeError(w, http.StatusUnauthorized, "invalid_signature",
					"the signature did not match")
			}
			return
		}

		ctx := context.WithValue(r.Context(), actorKey{}, actor)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
