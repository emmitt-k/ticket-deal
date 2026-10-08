// handler.go implements the two HTTP endpoints for the waiting room:
//   - POST /enter      — IP check → event bucket → JWT or queue
//   - GET  /queue SSE — stream position events to queued users
//
// Handlers are stdlib net/http (not chi.Mux) so they stay portable and
// easier to test with httptest.

package waitingroom

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/emmitt-k/ticket-deal/internal/apiutil"
	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/config"
	"github.com/emmitt-k/ticket-deal/internal/iplimit"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// EnterHandler handles POST /api/tickets/enter.
// It runs the two-tier gate: IP bucket → event token bucket.
// Returns a JWT on admission, a queue_url on overflow.
func EnterHandler(cfg config.Config, rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UserID  string `json:"user_id"`
			EventID int64  `json:"event_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" || req.EventID <= 0 {
			apiutil.WriteError(w, http.StatusBadRequest, "invalid_request",
				"body must be {user_id: string, event_id: positive integer}")
			return
		}

		// ── Tier 1: IP bucket ────────────────────────────────────────
		ip := clientIP(r)
		admitted, err := iplimit.Check(r.Context(), rdb,
			iplimit.Config{
				Capacity:   cfg.IPLimit.Capacity,
				RefillRate: cfg.IPLimit.RefillRate,
			},
			ip, time.Now().UnixMilli(),
		)
		if err != nil {
			log.Printf("wr: ip limit check failed ip=%s: %v", ip, err)
			apiutil.WriteError(w, http.StatusInternalServerError, "internal_error",
				"rate limit check failed, try again")
			return
		}
		if !admitted {
			apiutil.WriteError(w, http.StatusTooManyRequests, "ip_rate_limited",
				"too many requests from this IP, slow down")
			return
		}

		// ── Tier 2: Event token bucket ───────────────────────────────
		waitCfg := redis.WaitRoomConfig{
			EventID:         req.EventID,
			Capacity:        cfg.WaitRoom.Capacity,
			RefillRate:      cfg.WaitRoom.RefillRate,
			QueueTTLSeconds: cfg.WaitRoom.QueueTTLSeconds,
		}
		result, err := redis.TryAdmit(r.Context(), rdb, waitCfg, req.UserID, time.Now().UnixMilli())
		if err != nil {
			log.Printf("wr: TryAdmit failed event=%d user=%s: %v", req.EventID, req.UserID, err)
			apiutil.WriteError(w, http.StatusInternalServerError, "internal_error",
				"waiting room unavailable, try again")
			return
		}

		if result.Admitted {
			// ── Admitted: mint JWT ───────────────────────────────────
			token, err := auth.Issue(req.UserID, req.EventID, cfg.JWTSecret, 2*time.Minute)
			if err != nil {
				log.Printf("wr: Issue failed user=%s event=%d: %v", req.UserID, req.EventID, err)
				apiutil.WriteError(w, http.StatusInternalServerError, "internal_error",
					"failed to issue token, try again")
				return
			}
			apiutil.WriteJSON(w, http.StatusOK, map[string]any{
				"token":      token,
				"expires_in": 120, // seconds
			})
			return
		}

		// ── Queued: enqueue explicitly and return position ─────────
		pos, err := redis.Enqueue(r.Context(), rdb, req.EventID, req.UserID,
			time.Now().UnixMilli(), cfg.WaitRoom.QueueTTLSeconds)
		if err != nil {
			log.Printf("wr: Enqueue failed event=%d user=%s: %v", req.EventID, req.UserID, err)
			apiutil.WriteError(w, http.StatusInternalServerError, "internal_error",
				"failed to enqueue, try again")
			return
		}

		// Notify the drainer that this event now has a queue (starts a drainer
		// goroutine if one isn't already running for this event).
		NotifyEvent(req.EventID)

		eta := etaSeconds(pos, cfg.WaitRoom.RefillRate)
		apiutil.WriteJSON(w, http.StatusTooManyRequests, map[string]any{
			"queue_url":    fmt.Sprintf("/api/tickets/queue?event_id=%d&user_id=%s", req.EventID, req.UserID),
			"position":     pos,
			"eta_seconds":  eta,
			"message":     "event is at capacity, you are in the queue",
		})
	}
}

// QueueSSEHandler handles GET /api/tickets/queue (SSE).
// Streams position updates until the user is promoted from the queue.
func QueueSSEHandler(rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		eventIDStr := r.URL.Query().Get("event_id")
		userID := r.URL.Query().Get("user_id")
		eventID, err := strconv.ParseInt(eventIDStr, 10, 64)
		if err != nil || eventID <= 0 || userID == "" {
			apiutil.WriteError(w, http.StatusBadRequest, "invalid_request",
				"event_id (positive integer) and user_id (string) are required")
			return
		}

		// Check if user is actually in the queue
		pos, err := redis.GetPosition(r.Context(), rdb, eventID, userID)
		if err != nil {
			log.Printf("wr: GetPosition failed event=%d user=%s: %v", eventID, userID, err)
			apiutil.WriteError(w, http.StatusInternalServerError, "internal_error",
				"failed to check queue position")
			return
		}
		if pos == 0 {
			apiutil.WriteError(w, http.StatusNotFound, "not_in_queue",
				"you are not in the queue for this event")
			return
		}

		// Register this SSE stream for admission notifications
		admissionCh := Register(eventID, userID)
		defer Unregister(eventID, userID)

		// Set SSE headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
		flusher, ok := w.(http.Flusher)
		if !ok {
			apiutil.WriteError(w, http.StatusInternalServerError, "internal_error",
				"streaming not supported")
			return
		}

		// Goroutine: streams events, exits when context is cancelled (client gone)
		streamCtx, cancel := r.Context(), func() {}
		// Override cancel if the request context has one
		if cn, ok := w.(http.CloseNotifier); ok {
			closeNotify := cn.CloseNotify()
			streamCtx, cancel = context.WithCancel(r.Context())
			defer cancel()
			go func() {
				<-closeNotify
				cancel()
			}()
		}

		go streamSSE(streamCtx, w, flusher, admissionCh, rdb, eventID, userID)

		// Keep the handler alive (the goroutine owns the response)
		<-streamCtx.Done()
	}
}

// streamSSE runs in a goroutine and streams SSE events to the client.
// It exits when ctx is cancelled (client disconnected) or the
// admission channel fires (user was promoted).
func streamSSE(ctx context.Context, w http.ResponseWriter, flusher http.Flusher,
	admissionCh chan string, rdb *redis.Client, eventID int64, userID string,
) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("wr: streamSSE recovered panic: %v", r)
		}
	}()

	// 15-second heartbeat ticker to refresh position even if queue hasn't moved
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	// Send initial position immediately
	if err := sendSSEEvent(w, flusher, SSEEvent{
		Type:       "position",
		Position:   0, // will be filled below
		ETASeconds: 0,
	}); err != nil {
		return // client gone
	}

	for {
		select {
		case <-ctx.Done():
			return // client disconnected

		case token := <-admissionCh:
			// User was promoted by the drainer — send final event with JWT
			_ = sendSSEEvent(w, flusher, SSEEvent{
				Type:  "admitted",
				Token: token,
			})
			return

		case <-ticker.C:
			// Refresh position from Redis (queue may have moved)
			pos, err := redis.GetPosition(ctx, rdb, eventID, userID)
			if err != nil || pos == 0 {
				// User was removed from queue (promoted or TTL expired)
				// Don't fail silently — send a final position=0
				_ = sendSSEEvent(w, flusher, SSEEvent{
					Type:     "position",
					Position: 0,
				})
				return
			}
			_ = sendSSEEvent(w, flusher, SSEEvent{
				Type:       "position",
				Position:   pos,
				ETASeconds: etaSeconds(pos, 10.0), // refill rate is an approximation here
			})
		}
	}
}

// sendSSEEvent writes one SSE chunk and flushes.
// Returns error if the client is gone (write failed).
func sendSSEEvent(w http.ResponseWriter, flusher http.Flusher, ev SSEEvent) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
	flusher.Flush()
	return err
}

// clientIP extracts the client IP from the request, checking X-Forwarded-For
// first (for proxies), then falling back to RemoteAddr.
// Strips the port from RemoteAddr so tests are deterministic.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		// First IP in the chain is the original client
		if idx := strings.Index(fwd, ","); idx != -1 {
			fwd = fwd[:idx]
		}
		return strings.TrimSpace(fwd)
	}
	// RemoteAddr is "IP:port" or "[IPv6]:port" — use net.SplitHostPort to strip port
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// etaSeconds approximates wait time from queue position.
// Not exact (queue drains in bursts) but good enough for UX.
func etaSeconds(position int, refillRate float64) int {
	if refillRate <= 0 || position <= 0 {
		return 0
	}
	return int(float64(position) / refillRate)
}
