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

// queue.go manages the in-memory registry that connects drainer promotion
// events to the correct SSE goroutine for each user.
//
// This is the simplest possible pub/sub: a map from "eventID:userID" to
// a chan string that carries the JWT when a user is promoted from the queue.
// No Redis pub/sub, no external broker — at Phase 4 scale (thousands, not
// millions) this is fine. The map is small (only currently-connected SSE
// clients), short-lived (SSE connections close when users navigate away),
// and the per-user channels are closed immediately after promotion.

import (
	"fmt"
	"sync"
)

// admissionChannels is the registry of live SSE goroutines waiting for promotion.
// Key: "eventID:userID", Value: chan string (delivers JWT on admission).
var admissionChannels = struct {
	mu  sync.RWMutex
	ch  map[string]chan string
}{
	ch: make(map[string]chan string),
}

// eventCallback is the drainer's hook to learn about new event queues.
// Set once by StartDrainer via RegisterEventCallback.
var eventCallback func(eventID int64)

// RegisterEventCallback registers a function the queue package calls
// whenever a user enters a new event's queue. Used by the drainer to
// lazily start a drain goroutine for each event.
func RegisterEventCallback(fn func(eventID int64)) {
	eventCallback = fn
}

// NotifyEvent tells the drainer that a user has entered the queue for
// eventID. No-op if no callback is registered.
func NotifyEvent(eventID int64) {
	if eventCallback != nil {
		eventCallback(eventID)
	}
}

// queueKey builds the registry key for a user's SSE channel.
func queueKey(eventID int64, userID string) string {
	return fmt.Sprintf("%d:%s", eventID, userID)
}

// Register opens a new admission channel for a user who is connecting to
// the SSE stream. Returns the channel that will receive the JWT when
// the drainer promotes this user.
//
// If a channel already exists for this user (e.g. user opened multiple
// SSE tabs), the existing channel is returned — only one SSE stream per
// event per user is supported.
func Register(eventID int64, userID string) chan string {
	key := queueKey(eventID, userID)
	admissionChannels.mu.Lock()
	defer admissionChannels.mu.Unlock()

	if ch, exists := admissionChannels.ch[key]; exists {
		return ch
	}
	ch := make(chan string, 1) // buffered: drainer sends once, handler reads once
	admissionChannels.ch[key] = ch
	return ch
}

// Unregister removes a user's admission channel. Called when the SSE
// connection closes (client disconnects, context cancelled, timeout).
// The channel is closed to signal to any pending handler goroutine to exit.
func Unregister(eventID int64, userID string) {
	key := queueKey(eventID, userID)
	admissionChannels.mu.Lock()
	defer admissionChannels.mu.Unlock()

	if ch, exists := admissionChannels.ch[key]; exists {
		delete(admissionChannels.ch, key)
		close(ch)
	}
}

// Deliver delivers a JWT to the named user's SSE goroutine.
// If the user has no active SSE connection (e.g. they closed the tab),
// the send does nothing — the channel is already closed or empty.
// Returns true if the user was found and notified, false otherwise.
func Deliver(eventID int64, userID string, token string) bool {
	key := queueKey(eventID, userID)
	admissionChannels.mu.RLock()
	ch, exists := admissionChannels.ch[key]
	admissionChannels.mu.RUnlock()

	if !exists {
		return false
	}

	// Non-blocking send: if the handler is fast enough to have already
	// received and closed the channel, the default case fires and we
	// silently discard. That's fine — the user got their JWT via the
	// SSE stream already.
	select {
	case ch <- token:
		return true
	default:
		return false
	}
}
