package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/metrics"
	"github.com/emmitt-k/ticket-deal/internal/queue"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// tracerName is the OTel "instrumentation library" identifier. Group
// your spans under a stable name so Jaeger can filter by who created
// them.
const tracerName = "github.com/emmitt-k/ticket-deal/internal/api"

var tracer = otel.Tracer(tracerName)

// idempotencyReplayHeader is set on the response when a request hits
// the idempotency cache. Clients can use this header to distinguish
// "fresh reservation just minted" from "this is the cached answer
// from your retry" — useful for tests / observability. Not part of
// the public contract; just a courtesy signal.
const idempotencyReplayHeader = "Idempotency-Replayed"

// maxIdempotencyKeyLen caps the size of an Idempotency-Key header
// so a malicious client can't OOM our Redis keyspace with multi-MB
// keys. 255 chars matches Stripe's documented maximum and is far
// more than any reasonable UUID / ULID / txn id.
const maxIdempotencyKeyLen = 255

// ReserveHandler limits POST /api/tickets/reserve.
//
// Auth:        middleware has already verified the JWT and injected
//              *auth.Claims into the request context. We trust those
//              claims as the single source of identity — no body field
//              for user_id or event_id (they'd be attack vectors: a stale
//              token for event 1 plus a body saying event 2 would be
//              a privilege escalation if we cross-checked loosely).
//
// Body:        { "seats_requested": N }    (optional, default 1, max 10)
//
// Idempotency-Key (optional):
//              Clients MAY send `Idempotency-Key: <token>` to make
//              retries safe. Layer 1 item 5 of docs/product-roadmap.md.
//              Behaviour:
//                - First call with a given key: fresh reserve, then
//                  cache the 200 response under the key for 24 h.
//                - Retry with same key + same body: cached 200 returned
//                  with `Idempotency-Replayed: true` header.
//                - Same key + DIFFERENT body: 409 idempotency_key_mismatch.
//                - Two parallel retries: only one runs the reserve;
//                  the other gets 409 in_flight.
//              idempotency may be nil; in that case the header is
//              ignored (the handler still works). main.go wires a
//              real one; tests pass nil for the simple cases.
//
// Status codes:
//   200 { reservation_id, expires_in }                seat reserved
//   200 Idempotency-Replayed: true                    cached replay
//   400 invalid_request                               bad seats_requested
//   400 invalid_idempotency_key                       key format / length
//   401 missing_claims                                middleware not run (defensive)
//   404 event_not_found                               inventory key missing
//   409 sold_out                                      no seats remain
//   409 already_holding                               user already has one
//   409 in_flight                                     concurrent retry on same key
//   409 idempotency_key_mismatch                      same key, different body
//   500 internal_error                                Redis or marshal failure
//
// Phase 5 publishes the reservation event via ReservationPublisher
// (currently a log-only LogPublisher). Phase 6 swaps the publisher
// for a real SQS implementation without touching this handler.
func ReserveHandler(rdb *redis.Client, publisher ReservationPublisher, idempotency *redis.Idempotency, holdTTL time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ── 0. Open the handler-level span ────────────────────────
		ctx, span := tracer.Start(r.Context(), "reserve.handle",
			trace.WithAttributes(
				attribute.String("reservation.user_id", ""), // filled in below
			),
		)
		defer span.End()
		r = r.WithContext(ctx)

		// ── 1. Pull verified claims from context ──────────────────
		claims := auth.ClaimsFromContext(r.Context())
		if claims == nil {
			apiutil.WriteError(w, http.StatusUnauthorized,
				"missing_claims", "auth middleware did not run")
			return
		}
		span.SetAttributes(
			attribute.Int64("reservation.event_id", int64(claims.EventID)),
			attribute.String("reservation.user_id", claims.Subject),
		)

		// ── 2. Read body upfront (so we can HASH it later) ────────
		bodyBytes, err := readBody(r)
		if err != nil {
			apiutil.WriteError(w, http.StatusBadRequest,
				"invalid_request", "could not read request body")
			return
		}

		var req struct {
			SeatsRequested int `json:"seats_requested"`
		}
		if len(bodyBytes) > 0 {
			if err := json.Unmarshal(bodyBytes, &req); err != nil {
				apiutil.WriteError(w, http.StatusBadRequest,
					"invalid_request", "body must be JSON with optional seats_requested")
				return
			}
		}
		seats := 1
		if req.SeatsRequested != 0 {
			if req.SeatsRequested < 1 || req.SeatsRequested > 10 {
				apiutil.WriteError(w, http.StatusBadRequest,
					"invalid_request", "seats_requested must be between 1 and 10")
				return
			}
			seats = req.SeatsRequested
		}
		span.SetAttributes(attribute.Int("reservation.seats_requested", seats))

		// Body hash is computed from the raw bytes (even empty — its
		// sha256 is a well-known constant), so the idempotency cache
		// entry is consistent across same-body retries.
		bodyHash := redis.HashBody(bodyBytes)

		// ── 3. Idempotency pre-flight (cache lookup + lock) ───────
		idemKey := r.Header.Get("Idempotency-Key")
		idempotent := idempotency != nil && idemKey != ""

		// Set span attribute unconditionally (even when no key is present)
		// so Jaeger can group "idempotent retries" vs "fresh reserves".
		span.SetAttributes(
			attribute.Bool("idempotency.enabled", idempotent),
			attribute.String("idempotency.key_hash", redis.HashBody([]byte(idemKey))), // sha256 hex; '' case is fine
		)

		if idempotent {
			if verr := validateIdempotencyKey(idemKey); verr != nil {
				apiutil.WriteError(w, http.StatusBadRequest,
					"invalid_idempotency_key", verr.Error())
				return
			}

			// 3a. Cache lookup — fast path: same key + same body →
			// replay the cached response byte-for-byte.
			cachedStatus, cachedBody, hit, lerr := idempotency.Lookup(ctx, idemKey, bodyHash)
			switch {
			case lerr != nil && errors.Is(lerr, redis.ErrIdempotencyBodyMismatch):
				span.SetAttributes(attribute.String("idempotency.outcome", "body_mismatch"))
				metrics.IdempotencyBodyMismatch.Inc()
				apiutil.WriteError(w, http.StatusConflict,
					"idempotency_key_mismatch",
					"this Idempotency-Key was previously used with a different request body")
				return
			case lerr != nil && errors.Is(lerr, redis.ErrIdempotencyCacheCorrupt):
				// Malformed cache entry (manual tampering, schema drift).
				// Treat as a miss + log. The bad entry will TTL out.
				slog.WarnContext(ctx, "idempotency: cache entry corrupt; treating as miss",
					"key", idemKey, "error", lerr)
				hit = false
			case lerr != nil:
				slog.ErrorContext(ctx, "idempotency: redis lookup failed",
					"key", idemKey, "error", lerr)
				apiutil.WriteError(w, http.StatusInternalServerError,
					"internal_error", "idempotency lookup failed")
				return
			}
			if hit {
				// Replay: status + body from cache, identity preserved.
				span.SetAttributes(attribute.String("idempotency.outcome", "hit"))
				metrics.IdempotencyReplays.Inc()
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set(idempotencyReplayHeader, "true")
				w.WriteHeader(cachedStatus)
				_, _ = w.Write(cachedBody)
				return
			}

			// 3b. Lock acquisition — protects against two parallel
			// retries creating two reservations. Short TTL covers
			// the underlying operation; if we crash, retry can proceed.
			acquired, aerr := idempotency.AcquireLock(ctx, idemKey)
			if aerr != nil {
				slog.ErrorContext(ctx, "idempotency: redis SETNX failed",
					"key", idemKey, "error", aerr)
				apiutil.WriteError(w, http.StatusInternalServerError,
					"internal_error", "idempotency lock failed")
				return
			}
			if !acquired {
				span.SetAttributes(attribute.String("idempotency.outcome", "in_flight"))
				apiutil.WriteError(w, http.StatusConflict,
					"in_flight", "another retry with this Idempotency-Key is already being processed")
				return
			}
		}

		// ── 4. Wrap writer so we can capture the response (status +
		//      body) for caching after a successful fresh reserve. ──
		var cw *cachingResponseWriter
		if idempotent {
			cw = &cachingResponseWriter{ResponseWriter: w}
			w = cw
		}

		// ── 5. Atomic seat lock (Lua reserve script) ─────────────
		_, holdSpan := tracer.Start(r.Context(), "redis.acquire-hold",
			trace.WithAttributes(
				attribute.Int64("hold.event_id", int64(claims.EventID)),
				attribute.Int64("hold.user_id_hash", hashUserID(claims.Subject)),
				attribute.Int("hold.ttl_seconds", int(holdTTL.Seconds())),
			),
		)
		result, err := redis.ReserveSeat(r.Context(), rdb,
			claims.EventID, claims.Subject,
			int(holdTTL.Seconds()), seats)
		if err != nil {
			holdSpan.RecordError(err)
			holdSpan.SetStatus(codes.Error, "lua reserve script failed")
			holdSpan.End()
			slog.ErrorContext(r.Context(), "reserve script failed",
				"event", claims.EventID,
				"user", claims.Subject,
				"error", err,
			)
			releaseLockIfHeld(idempotency, ctx, idemKey, span)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "reservation failed, try again")
			return
		}
		holdSpan.SetAttributes(
			attribute.Int("hold.status_code", int(result.Status)),
			attribute.Int("hold.rejection_reason", int(result.Reason)),
		)
		holdSpan.End()

		// ── 6. Map Lua outcome to HTTP response ───────────────────
		switch result.Status {
		case redis.StatusEventNotFound:
			releaseLockIfHeld(idempotency, ctx, idemKey, span)
			apiutil.WriteError(w, http.StatusNotFound,
				"event_not_found", "no inventory has been seeded for this event")
			return
		case redis.StatusRejected:
			releaseLockIfHeld(idempotency, ctx, idemKey, span)
			switch result.Reason {
			case redis.ReasonSoldOut:
				metrics.ReservationsSoldOut.Inc()
				apiutil.WriteError(w, http.StatusConflict,
					"sold_out", "this event is sold out")
			case redis.ReasonAlreadyHolding:
				metrics.ReservationsAlreadyHolding.Inc()
				apiutil.WriteError(w, http.StatusConflict,
					"already_holding",
					"you already have an active hold for this event")
			default:
				apiutil.WriteError(w, http.StatusInternalServerError,
					"internal_error", "unexpected rejection reason")
			}
			return
		}
		if result.Status != redis.StatusReserved {
			releaseLockIfHeld(idempotency, ctx, idemKey, span)
			slog.ErrorContext(r.Context(), "reserve returned unknown status",
				"status", result.Status,
				"event", claims.EventID,
				"user", claims.Subject,
			)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "unexpected reservation outcome")
			return
		}

		// ── 7. Success: mint reservation_id, publish, respond ──────
		metrics.ReservationsHeld.Inc()

		reservationID := uuid.NewString()
		now := time.Now()
		expiresAt := now.Add(holdTTL)
		span.SetAttributes(attribute.String("reservation.id", reservationID))

		payload, err := json.Marshal(queue.Reservation{
			ReservationID: reservationID,
			UserID:        claims.Subject,
			EventID:       claims.EventID,
			Seats:         seats,
			CreatedAt:     now.UTC().Format(time.RFC3339),
			ExpiresAt:     expiresAt.UTC().Format(time.RFC3339),
		})
		if err != nil {
			releaseLockIfHeld(idempotency, ctx, idemKey, span)
			slog.ErrorContext(r.Context(), "marshal publish payload failed", "error", err)
			apiutil.WriteError(w, http.StatusInternalServerError,
				"internal_error", "failed to build event message")
			return
		}

		_, publishSpan := tracer.Start(r.Context(), "sqs.publish",
			trace.WithAttributes(
				attribute.String("messaging.system", "aws.sqs"),
				attribute.String("messaging.destination.name", "ticket-reservations"),
				attribute.Int("messaging.message.body.size", len(payload)),
			),
		)
		if err := publisher.Publish(r.Context(), payload); err != nil {
			publishSpan.RecordError(err)
			publishSpan.SetStatus(codes.Error, "sqs publish failed")
			slog.ErrorContext(r.Context(), "publish failed (reservation still valid)",
				"reservation_id", reservationID,
				"error", err,
			)
		} else {
			metrics.ReservationsCompleted.Inc()
		}
		publishSpan.End()

		apiutil.WriteJSON(w, http.StatusOK, map[string]any{
			"reservation_id": reservationID,
			"seats":          seats,
			"expires_in":     int(holdTTL.Seconds()),
		})

		// ── 8. Post-success: cache the response + release the lock ─
		// Spec is "on success, SETEX the response". We only cache
		// the success path (200). Error responses are NOT cached so a
		// retry against a transient error gets a fresh attempt.
		if idempotent && cw != nil && cw.statusCode == http.StatusOK {
			span.SetAttributes(attribute.String("idempotency.outcome", "stored"))
			if serr := idempotency.Store(ctx, idemKey, bodyHash, cw.statusCode, cw.body.Bytes()); serr != nil {
				// Cache failure is non-fatal — the reservation succeeded,
				// the user gets their seat. Future retries won't have a
				// cached response and will mint a new reservation, but
				// that's an at-most-once fallback, not data loss.
				slog.WarnContext(ctx, "idempotency: cache store failed; future retries won't replay",
					"key", idemKey, "reservation_id", reservationID, "error", serr)
			}
		}
		releaseLockIfHeld(idempotency, ctx, idemKey, span)
	}
}

// readBody consumes the request body fully. Returns the raw bytes
// (could be nil/empty if the client sent no body) and an error on
// I/O failure. The handler passes the bytes both to json.Unmarshal
// (for seats_requested parsing) and to redis.HashBody (for the
// idempotency body-hash).
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	const maxBodyBytes = 1 << 14 // 16 KiB — comfortably larger than any reasonable reserve body
	return io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
}

// validateIdempotencyKey enforces the spec's "UUID-shaped (lenient),
// max 255 chars" format. Lenient UUID means: only [A-Za-z0-9-], no
// other characters; length cap at 255. Common UUID/ULID/txn-id
// formats all fit. Rejects empty (handler treats empty as "no key"
// before this is called — defensive double-check).
func validateIdempotencyKey(s string) error {
	if s == "" {
		return fmt.Errorf("Idempotency-Key must not be empty")
	}
	if len(s) > maxIdempotencyKeyLen {
		return fmt.Errorf("Idempotency-Key too long (max %d chars)", maxIdempotencyKeyLen)
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-':
			// ok
		default:
			return fmt.Errorf("Idempotency-Key contains invalid character %q", c)
		}
	}
	return nil
}

// releaseLockIfHeld best-effort releases the in-flight lock. Failure
// is logged but not bubbled (the lock has a short TTL and will auto-
// clear anyway). Safe to call with idempotency == nil (no-op).
func releaseLockIfHeld(idempotency *redis.Idempotency, ctx context.Context, key string, span trace.Span) {
	if idempotency == nil || key == "" {
		return
	}
	if err := idempotency.ReleaseLock(ctx, key); err != nil {
		slog.WarnContext(ctx, "idempotency: lock release failed", "key", key, "error", err)
	}
	span.SetAttributes(attribute.Bool("idempotency.lock_released", true))
}

// cachingResponseWriter wraps http.ResponseWriter to capture the
// status code and body bytes for caching. Used by the idempotency
// path so the handler can store the full response under the key
// after a successful fresh reserve.
//
// stdlib http semantics:
//   - WriteHeader is called once (handlers MUST set the status via
//     WriteHeader or indirectly via http.Error / WriteContentType
//     helpers). We capture it.
//   - Write may be called multiple times; we collect all bytes and
//     forward to the underlying writer (so the client still receives
//     the response).
//   - If a handler calls Write before WriteHeader (allowed but rare),
//     net/http auto-inserts a 200; we mirror that here for the
//     captured status code.
type cachingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	body       bytes.Buffer
	wroteHead  bool
}

// WriteHeader records the status code and forwards.
func (c *cachingResponseWriter) WriteHeader(code int) {
	if !c.wroteHead {
		c.statusCode = code
		c.wroteHead = true
	}
	c.ResponseWriter.WriteHeader(code)
}

// Write records the bytes and forwards. Auto-emits WriteHeader(200)
// if the handler skipped it (mirrors net/http behaviour).
func (c *cachingResponseWriter) Write(b []byte) (int, error) {
	if !c.wroteHead {
		c.WriteHeader(http.StatusOK)
	}
	c.body.Write(b)
	return c.ResponseWriter.Write(b)
}

// hashUserID returns a stable, short hash of the user ID for use as a
// span attribute. The raw user_id (which may be PII) is intentionally
// NOT set as a span attribute — only this hash is. Lets you correlate
// traces for the same user across requests without leaking identity.
func hashUserID(s string) int64 {
	var h int64
	for _, b := range []byte(s) {
		h = h*31 + int64(b)
	}
	if h < 0 {
		h = -h
	}
	return h
}
