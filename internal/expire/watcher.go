package expire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/emmitt-k/ticket-deal/internal/db"
	"github.com/emmitt-k/ticket-deal/internal/eventid"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// The two ways a hold key can fail to parse. They are separate
// sentinels because the watcher treats them differently: one is
// background noise, the other is a bug worth a loud log line.
var (
	// ErrNotHoldKey means the expired key is not a hold key at all.
	// The keyspace channel fires for every expired key in the
	// database, so this is the common, expected case.
	ErrNotHoldKey = errors.New("expire: not a hold key")

	// ErrMalformedHoldKey means the key LOOKS like a hold key but
	// its contents are wrong — a bad event id, or a missing user id.
	// A seat may be held against an event we cannot identify, so this
	// is logged at ERROR rather than skipped.
	ErrMalformedHoldKey = errors.New("expire: malformed hold key")
)

// ParseHoldKey extracts (eventID, userID) from a hold key like
//
//	"hold:event:9f8c1e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b:user:abc-123"
//
// Returns an error if the key doesn't match the expected shape.
// The keyspace channel fires for EVERY expired key, not just hold
// keys, so callers must filter — we do that here at the parse step
// rather than dispatching first and failing later.
//
// The two failure modes are DISTINCT and the distinction matters:
//
//   - ErrNotHoldKey    the key is not ours (some other key expired).
//     The caller skips it silently. This is the common case and it is
//     not an error at all.
//   - ErrMalformedHoldKey the key IS shaped like a hold key but the
//     event id inside it is not a well-formed UUID. That is a real
//     bug — a key written by a different version of the key format,
//     or a corrupted one — and it must NOT be silently skipped. A
//     hold key whose event id cannot be parsed means a seat is being
//     held against an event we cannot identify, and the compensation
//     never runs.
//
// The event id used to be an int64, parsed with strconv.ParseInt. It
// is now a UUID (see internal/eventid), so the parse is a UUID
// validation. The split on ":user:" is unchanged and still exact: a
// UUID contains no colons, so the first ":user:" in the key is the
// separator.
func ParseHoldKey(key string) (eventID string, userID string, err error) {
	const prefix = "hold:event:"
	if !strings.HasPrefix(key, prefix) {
		return "", "", fmt.Errorf("%w: %q", ErrNotHoldKey, key)
	}
	rest := key[len(prefix):]
	const userSep = ":user:"
	idx := strings.Index(rest, userSep)
	if idx < 0 {
		return "", "", fmt.Errorf("%w: %q (no :user:)", ErrMalformedHoldKey, key)
	}
	eventIDStr := rest[:idx]
	userID = rest[idx+len(userSep):]

	if _, parseErr := eventid.Parse(eventIDStr); parseErr != nil {
		return "", "", fmt.Errorf("%w: %q has an invalid event id: %v",
			ErrMalformedHoldKey, key, parseErr)
	}
	if userID == "" {
		return "", "", fmt.Errorf("%w: %q (empty user_id)", ErrMalformedHoldKey, key)
	}
	return eventIDStr, userID, nil
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

		slog.WarnContext(ctx, "watcher error (reconnecting)",
			"backoff", backoff,
			"error", err,
		)
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
	slog.InfoContext(ctx, "watcher subscribed", "channel", channel)

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
				// Two different situations, and they must not be
				// treated the same:
				//
				//   ErrNotHoldKey       the expired key is some
				//                       other key entirely (the
				//                       waiting-room ZSET members,
				//                       for one). Skip it
				//                       silently — this is the
				//                       expected case.
				//
				//   ErrMalformedHoldKey it IS a hold key but we
				//                       cannot read the event id
				//                       out of it. A seat may be
				//                       held against an event we
				//                       cannot identify, so the
				//                       compensation would never
				//                       run. Log it loudly —
				//                       silently skipping it would
				//                       leak a seat.
				if errors.Is(parseErr, ErrNotHoldKey) {
					continue
				}
				slog.ErrorContext(ctx, "malformed hold key expired",
					"key", msg.Payload,
					"error", parseErr,
				)
				continue
			}
			if compErr := Compensate(ctx, pool, rdb, eventID, userID); compErr != nil {
				// Transient — log and skip. The sweep will catch
				// the row within 60 s if compensation failed.
				slog.ErrorContext(ctx, "compensate failed",
					"event", eventID,
					"user", userID,
					"error", compErr,
				)
			}
		}
	}
}
