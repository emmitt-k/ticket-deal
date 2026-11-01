package redis

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	redisclient "github.com/redis/go-redis/v9"
)

//go:embed scripts/reserve.lua
var reserveScriptSrc string

// reserveScript is loaded once at package init. go-redis automatically
// uses EVALSHA on subsequent calls (with EVAL fallback on NOSCRIPT).
var reserveScript = redisclient.NewScript(reserveScriptSrc)

// Status is the high-level outcome of a reserve attempt.
type Status int

const (
	StatusReserved      Status = 1  // seat allocated, hold key set
	StatusRejected      Status = 0  // see RejectionReason (sold_out | already_holding)
	StatusEventNotFound Status = -1 // inventory key never seeded for this event_id
)

// RejectionReason disambiguates StatusRejected.
type RejectionReason int

const (
	ReasonSoldOut        RejectionReason = 1 // no seats left
	ReasonAlreadyHolding RejectionReason = 2 // user already has an active hold
)

// ErrUnknownRejection is returned if the script replies with an
// unexpected reason string. Should never happen — indicates the
// Lua was edited incompatibly.
var ErrUnknownRejection = errors.New("redis: unknown rejection reason")

// Result is the typed return from ReserveSeat.
type Result struct {
	Status Status
	Reason RejectionReason // meaningful only when Status == StatusRejected
}

// InventoryKey is the canonical Redis key for an event's available-seat counter.
//
//	"inventory:event:9f8c1e2a-...-4b3a"
//
// The event id is a UUID (see internal/eventid), so the key carries it
// verbatim. Every layer that builds this key MUST use this function —
// a hand-built key is how the hold-key parser ends up disagreeing with
// the writer (Convention #10).
func InventoryKey(eventID string) string {
	return fmt.Sprintf("inventory:event:%s", eventID)
}

// AvailableSeats returns the current available-seat count for an event
// by reading the inventory key. Returns 0 if the key is missing (no
// inventory seeded yet). Used by the metrics state updater — the hot
// path uses the Lua reserve script which is atomic.
func AvailableSeats(ctx context.Context, c *redisclient.Client, eventID string) (int, error) {
	v, err := c.Get(ctx, InventoryKey(eventID)).Int()
	if err == redisclient.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("redis: get inventory failed: %w", err)
	}
	return v, nil
}

// AddSeats puts more seats on sale for an event and returns the NEW
// total.
//
// It is the admin's "we opened the back of the house" operation: an
// organizer adds capacity to a live drop.
//
// It uses INCRBY, so it ADDS to the current count rather than
// replacing it — adding seats to an event that has already sold some
// must not resurrect the sold ones.
//
// A negative amount is refused: seats can be added, not removed. The
// removal path is the expiry/release path, which is guarded by the
// hold key (see release_seat.lua) — an admin INCRBY of a negative
// number would have no such guard and could drive the counter
// negative, which is a phantom seat waiting to happen.
//
// A missing inventory key is an error, not a zero: the event has never
// been seeded, and adding seats to a counter that does not exist would
// create one with the wrong number (the added seats, not the event's
// inventory).
func AddSeats(ctx context.Context, c *redisclient.Client, eventID string, seats int) (int, error) {
	if seats <= 0 {
		return 0, fmt.Errorf("redis: seats must be positive, got %d", seats)
	}

	key := InventoryKey(eventID)

	// The key must already exist. INCRBY would happily create it,
	// and a counter holding only the added seats would be wrong.
	exists, err := c.Exists(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("redis: check inventory existence failed: %w", err)
	}
	if exists == 0 {
		return 0, fmt.Errorf("redis: event %s has no inventory counter", eventID)
	}

	total, err := c.IncrBy(ctx, key, int64(seats)).Result()
	if err != nil {
		return 0, fmt.Errorf("redis: add seats failed: %w", err)
	}
	return int(total), nil
}

// HoldKey is the canonical Redis key for a per-user hold lock.
//
//	"hold:event:9f8c1e2a-...-4b3a:user:u1"
//
// Acts as: (1) anti-double-book, (2) auto-release on abandonment, (3) cleanup-free TTL.
// See docs/architecture.md §4 "The hold key".
//
// The event id is a UUID, so the key format is unambiguous to parse
// back: a UUID contains no colons, so the FIRST ":user:" in the key
// is the separator. See expire.ParseHoldKey.
func HoldKey(eventID string, userID string) string {
	return fmt.Sprintf("hold:event:%s:user:%s", eventID, userID)
}

// ReserveSeat runs the atomic Lua reserve script.
//
// Lua KEYS:
//   - KEYS[1] = inventory key (InventoryKey)
//   - KEYS[2] = hold key      (HoldKey)
//
// Lua ARGV:
//   - ARGV[1] = hold TTL seconds  (e.g. 600 for a 10-minute hold)
//   - ARGV[2] = seats requested   (typically 1; >1 for group purchases)
//
// Returns:
//   - {Status: StatusReserved}                                     on success
//   - {Status: StatusEventNotFound}                                if event_id is unknown
//   - {Status: StatusRejected, Reason: ReasonSoldOut}              if no seats remain
//   - {Status: StatusRejected, Reason: ReasonAlreadyHolding}       if user holds one
//   - non-nil error                                               on connection / script issues
func ReserveSeat(ctx context.Context, c *redisclient.Client,
	eventID string, userID string, holdTTLSec, seatsRequested int,
) (Result, error) {
	keys := []string{InventoryKey(eventID), HoldKey(eventID, userID)}
	args := []any{holdTTLSec, seatsRequested}

	raw, err := reserveScript.Run(ctx, c, keys, args...).Result()
	if err != nil {
		return Result{}, fmt.Errorf("redis: reserve script failed: %w", err)
	}

	arr, ok := raw.([]any)
	if !ok || len(arr) != 2 {
		return Result{}, fmt.Errorf("redis: unexpected reply shape %v", raw)
	}

	code, _ := arr[0].(int64)
	reasonStr, _ := arr[1].(string)

	switch code {
	case 1:
		return Result{Status: StatusReserved}, nil
	case -1:
		return Result{Status: StatusEventNotFound}, nil
	case 0:
		switch reasonStr {
		case "sold_out":
			return Result{Status: StatusRejected, Reason: ReasonSoldOut}, nil
		case "already_holding":
			return Result{Status: StatusRejected, Reason: ReasonAlreadyHolding}, nil
		default:
			return Result{}, fmt.Errorf("%w: %q", ErrUnknownRejection, reasonStr)
		}
	default:
		return Result{}, fmt.Errorf("redis: unexpected status code %d", code)
	}
}
