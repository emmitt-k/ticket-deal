package expire

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// ParseHoldKey extracts (eventID, userID) from a hold key like
//
//	"hold:event:42:user:abc-123"
//
// Returns an error if the key doesn't match the expected shape.
// The keyspace channel fires for EVERY expired key, not just hold
// keys, so callers must filter — we do that here at the parse step
// rather than dispatching first and failing later.
func ParseHoldKey(key string) (eventID int64, userID string, err error) {
	const prefix = "hold:event:"
	if !strings.HasPrefix(key, prefix) {
		return 0, "", fmt.Errorf("expire: key %q is not a hold key", key)
	}
	rest := key[len(prefix):]
	const userSep = ":user:"
	idx := strings.Index(rest, userSep)
	if idx < 0 {
		return 0, "", fmt.Errorf("expire: malformed hold key %q (no :user:)", key)
	}
	eventIDStr := rest[:idx]
	userID = rest[idx+len(userSep):]

	n, parseErr := strconv.ParseInt(eventIDStr, 10, 64)
	if parseErr != nil {
		return 0, "", fmt.Errorf("expire: parse event_id %q: %w", eventIDStr, parseErr)
	}
	if userID == "" {
		return 0, "", fmt.Errorf("expire: empty user_id in key %q", key)
	}
	return n, userID, nil
}

// RunWatcher subscribes to Redis keyspace notifications and compensates
// each expired hold key. Blocks until ctx is canceled.
//
// The channel name embeds the Redis DB number:
// __keyevent@<DB>__:expired fires once for every expired key in that DB.
// We use DB 0 (the only DB this app uses — see redis.NewClient).
//
// Reconnect: go-redis PubSub does not auto-reconnect on connection drop,
// so we wrap the subscribe loop in a reconnect-with-backoff. Each
// iteration: subscribe, read messages; on error log + sleep + retry.
// Backoff is exponential, capped at 30 s.
//
// Redis must be started with --notify-keyspace-events Ex (see
// docker-compose.yml) for any of this to work — without that flag the
// channel stays silent.
func RunWatcher(ctx context.Context, rdb *redis.Client, pool *db.Pool) error {
	const channel = "__keyevent@0__:expired"

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := watchOnce(ctx, rdb, pool, channel)
		if err == nil {
			return nil
		}
		// Graceful shutdown: any ctx-related error means we're done.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		log.Printf("expire: watcher error (reconnecting in %s): %v", backoff, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		// Exponential backoff capped at 30 s. Backoff resets after a
		// clean run (the next loop iteration starts from 1 s).
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// watchOnce runs a single subscribe/compensate loop. Returns when the
// context is canceled (nil/ctx.Err) or the pubsub channel closes (non-nil
// error so the outer loop reconnects).
func watchOnce(ctx context.Context, rdb *redis.Client, pool *db.Pool, channel string) error {
	pubsub := rdb.PSubscribe(ctx, channel)
	defer pubsub.Close()

	// Block until the SUBSCRIBE is confirmed by the server. Without
	// this, Channel() could miss the first round of messages.
	if _, err := pubsub.Receive(ctx); err != nil {
		return fmt.Errorf("expire: subscribe: %w", err)
	}
	log.Printf("expire: watcher subscribed to %s", channel)

	// Reset backoff on a successful subscribe — we're back online.
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				return errors.New("expire: pubsub channel closed")
			}
			eventID, userID, parseErr := ParseHoldKey(msg.Payload)
			if parseErr != nil {
				// Not a hold key — ignore silently. The expired
				// channel fires for EVERY expired key in DB 0
				// (waiting-room ZSET members, etc.).
				continue
			}
			if compErr := Compensate(ctx, pool, rdb, eventID, userID); compErr != nil {
				// Transient — log and skip. The sweep will catch
				// the row within 60 s if compensation failed.
				log.Printf("expire: compensate failed event=%d user=%s err=%v",
					eventID, userID, compErr)
			}
		}
	}
}