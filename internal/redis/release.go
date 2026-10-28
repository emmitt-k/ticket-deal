package redis

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	redisclient "github.com/redis/go-redis/v9"
)

//go:embed scripts/release_seat.lua
var releaseSeatScriptSrc string

// releaseSeatScript is loaded once at package init. go-redis uses
// EVALSHA on subsequent calls, with an EVAL fallback on NOSCRIPT.
var releaseSeatScript = redisclient.NewScript(releaseSeatScriptSrc)

// ReleaseOutcome is the result of a seat release.
type ReleaseOutcome int

const (
	// ReleaseSeatDone — the hold existed and the seat is back on sale.
	ReleaseSeatDone ReleaseOutcome = 1
	// ReleaseSeatNotHeld — there was no hold to release. The seat is
	// already back on sale (the hold expired and the expiration
	// watcher already released it), so releasing it again would
	// oversell.
	ReleaseSeatNotHeld ReleaseOutcome = 0
	// ReleaseSeatNoInventory — the event was never seeded, so there
	// is no inventory counter to put the seat back into.
	ReleaseSeatNoInventory ReleaseOutcome = -1
)

// ErrUnknownReleaseOutcome is returned if the script replies with an
// unexpected reason string. Should never happen — it means the Lua
// was edited incompatibly.
var ErrUnknownReleaseOutcome = errors.New("redis: unknown release-seat reason")

// ReleaseSeat puts a held seat back on sale.
//
// It is the admin/ops counterpart of the seat release in the cancel
// path, and it is atomic where that one is not: the "is it still
// held?" check and the release are one Redis operation, so two
// concurrent callers can never release the same hold twice.
//
// The user-facing cancel does not need this — its compare-and-set on
// the reservation status arbitrates between the cancel and the
// expiry. The admin release-seat path changes no status, so nothing
// else arbitrates and the hold key IS the guard.
//
// userID is the owner of the hold; the hold key is
// hold:event:<eventID>:user:<userID>.
func ReleaseSeat(ctx context.Context, c *redisclient.Client, eventID int64, userID string, seats int) (ReleaseOutcome, error) {
	if seats <= 0 {
		return ReleaseSeatDone, nil // nothing to release
	}

	raw, err := releaseSeatScript.Run(ctx, c,
		[]string{HoldKey(eventID, userID), InventoryKey(eventID)},
		seats,
	).Result()
	if err != nil {
		return ReleaseSeatDone, fmt.Errorf("redis: release seat: %w", err)
	}

	// The script replies {code, reason} — an integer and a string, so
	// it is parsed the same way the reserve script's reply is (and
	// NOT with Int64Slice, which cannot read the string half).
	arr, ok := raw.([]any)
	if !ok || len(arr) != 2 {
		return ReleaseSeatDone, fmt.Errorf("redis: release seat: unexpected reply shape %v", raw)
	}
	code, _ := arr[0].(int64)
	reason, _ := arr[1].(string)

	switch code {
	case int64(ReleaseSeatDone):
		return ReleaseSeatDone, nil
	case int64(ReleaseSeatNotHeld):
		return ReleaseSeatNotHeld, nil
	case int64(ReleaseSeatNoInventory):
		return ReleaseSeatNoInventory, nil
	default:
		return ReleaseSeatDone, fmt.Errorf("%w: %d (%q)", ErrUnknownReleaseOutcome, code, reason)
	}
}
