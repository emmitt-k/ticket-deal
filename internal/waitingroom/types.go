// Package waitingroom wires the per-event token bucket (from internal/redis)
// into the HTTP layer: the /enter handler, the /queue SSE stream, and the
// background drainer that promotes users as tokens refill.
//
// Architecture:
//
//	POST /enter
//	  → IP limit check (iplimit.Check)
//	  → Event bucket check (redis.TryAdmit)
//	    admitted → Issue JWT → 200 {token}
//	    queued  → Enqueue   → 429 {queue_url}
//
//	GET /queue (SSE)
//	  → GetPosition (ZRANK)
//	    in queue → stream position events until admitted
//	    not in queue → 404
//
//	Drainer goroutine (background)
//	  → Every 100ms: read queue, promote users, deliver JWTs via SSE channels
package waitingroom

// SSEEvent is the payload sent over a user's SSE stream.
type SSEEvent struct {
	// Type is "position" or "admitted".
	Type string `json:"type"`
	// Position is 1-based queue position (meaningful for type="position").
	Position int `json:"position,omitempty"`
	// ETASeconds is estimated wait (position / refillRate).
	ETASeconds int `json:"eta_seconds,omitempty"`
	// Token is the JWT delivered when type="admitted".
	Token string `json:"token,omitempty"`
}
