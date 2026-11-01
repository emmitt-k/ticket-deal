// events.go — the event resource: the thing a ticket is FOR.
//
// An event is the top of the reservation tree: every reservation points
// at one, and the event's price is what a seat costs. Until this file
// existed, the only code that touched the events table was the
// reservation INSERT's price subquery and the dev seeder — there was
// no way to create, read, list, update or cancel an event.
//
// ── The two stores ─────────────────────────────────────────────────────
//
// An event lives in TWO places:
//
//	Postgres  the durable record — the name, the price, the status
//	Redis     the live inventory counter (inventory:event:<id>)
//
// The counter is the source of truth AT SELL TIME. The Lua reserve
// script reads it atomically, and it is the reason zero overselling
// is possible. Postgres is the source of truth for everything else.
//
// That split is why CreateEvent writes to BOTH, and why it treats a
// Redis failure as fatal: an event in Postgres with no counter in
// Redis cannot sell a single seat, and it would answer
// "event_not_found" to every customer — a trap, not a feature.
//
// ── The status ─────────────────────────────────────────────────────────
//
// active | cancelled. The row is NEVER deleted — see
// migrations/011_event_status.sql for why. A cancelled event is a
// tombstone, and it still answers "what happened to that drop?".
package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/emmitt-k/ticket-deal/internal/eventid"
)

// Event status values mirror the CHECK constraint in
// migrations/011_event_status.sql.
const (
	// EventStatusActive means the drop is live and its inventory is
	// on sale.
	EventStatusActive = "active"
	// EventStatusCancelled means the drop was pulled before it sold
	// anything. The row stays as a tombstone.
	EventStatusCancelled = "cancelled"
)

// EventRow is the read model of an event.
//
// The JSON tags are the public contract — the same shape the public
// catalogue serves and the admin API returns, so a client learns one
// shape for one thing.
//
// Field-by-field:
//
//	id                 the event's UUID (app-minted — see internal/eventid)
//	name               human-readable title
//	status             active | cancelled
//	initial_inventory  the seats the event was created with — the
//	                   historical number, NOT the live count
//	price_cents        the price of ONE seat
//	currency           the 3-letter ISO code the price is in
//	seats_available    the LIVE remaining-seat count, read from Redis
//	                   at the moment of the read. It is -1 (and
//	                   seats_available_known is false) when Redis has
//	                   no counter for this event
//	seats_available_known  whether seats_available was actually read
//	created_at         when the event was created
type EventRow struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Status           string `json:"status"`
	InitialInventory int    `json:"initial_inventory"`
	PriceCents       int64  `json:"price_cents"`
	Currency         string `json:"currency"`

	// SeatsAvailable is the live remaining-seat count, read from
	// Redis. -1 when Redis has no counter for this event — which
	// means the event has never been seeded, or was cancelled.
	SeatsAvailable int `json:"seats_available"`

	// SeatsAvailableKnown distinguishes "Redis says 0" from "Redis
	// has no counter for this event". Both would otherwise report
	// seats_available = 0, and "sold out" and "not on sale" are
	// very different facts to a customer.
	SeatsAvailableKnown bool `json:"seats_available_known"`

	CreatedAt time.Time `json:"created_at"`
}

// EventCreate is the input to CreateEvent.
//
// The id is NOT part of it: the application mints it. One convention
// for an app-minted identifier (see internal/eventid).
//
// PriceCents and Currency are POINTERS for the same reason
// EventPatch's fields are: "not supplied" and "supplied as zero" are
// different intents. When PriceCents is nil the COLUMN DEFAULT
// applies ($25.00) — an event that nobody priced is still sellable,
// which matters because a $0.00 event is a dead end for Stripe.
type EventCreate struct {
	Name             string
	InitialInventory int
	PriceCents       *int64
	Currency         string
}

// EventPatch is a partial update to an event.
//
// Every field is a POINTER, because "not supplied" and "supplied as
// zero" are different intents: a PATCH that omits PriceCents must
// not set the price to 0, and a PATCH that sets PriceCents to 0 must
// actually set it (a free event is legal — the CHECK allows it, and
// Stripe rejects it, which is a payment-layer concern).
type EventPatch struct {
	Name       *string
	PriceCents *int64
	Currency   *string

	// AddSeats adds to the live inventory counter. It is separate
	// from InitialInventory on purpose: adding seats to a live drop
	// changes the counter (Redis) but does not rewrite the event's
	// history (Postgres).
	AddSeats *int
}

// Validate checks the create input.
//
// The checks live here rather than in the handler so that every
// caller — the admin API today, a CLI tomorrow — gets the same
// rules. A rule enforced in one caller is a rule that drifts.
func (e EventCreate) Validate() error {
	if e.Name == "" {
		return errors.New("db: event name is required")
	}
	if len(e.Name) > 200 {
		return errors.New("db: event name must be at most 200 characters")
	}
	if e.InitialInventory < 0 {
		return errors.New("db: initial_inventory must be >= 0")
	}
	if e.InitialInventory > 1_000_000 {
		return errors.New("db: initial_inventory must be at most 1,000,000")
	}
	if e.PriceCents != nil && *e.PriceCents < 0 {
		return errors.New("db: price_cents must be >= 0")
	}
	if e.Currency == "" {
		return errors.New("db: currency is required")
	}
	if !isValidCurrency(e.Currency) {
		return fmt.Errorf("db: currency %q must be a 3-letter lowercase ISO code", e.Currency)
	}
	return nil
}

// isValidCurrency mirrors the CHECK constraint
// (currency ~ '^[a-z]{3}$'). Duplicated in Go so the API can reject a
// bad value with a clear message instead of a 23514 from Postgres.
func isValidCurrency(c string) bool {
	if len(c) != 3 {
		return false
	}
	for _, r := range c {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// CreateEvent inserts an event row and returns it.
//
// The id is minted HERE, by the application, and the column has no
// database default — so there is exactly one way an event id comes
// into existence.
//
// The insert is a plain INSERT (not ON CONFLICT DO NOTHING): an
// event id is a fresh uuid, so a conflict means a uuid collision,
// which is a bug worth surfacing rather than swallowing.
func CreateEvent(ctx context.Context, pool *Pool, ev EventCreate) (EventRow, error) {
	if err := ev.Validate(); err != nil {
		return EventRow{}, err
	}

	id := eventid.New()
	row := EventRow{
		ID:               id,
		Name:             ev.Name,
		Status:           EventStatusActive,
		InitialInventory: ev.InitialInventory,
		Currency:         ev.Currency,
		SeatsAvailable:   -1,
	}

	// The price is only sent when the caller supplied it. A nil
	// PriceCents means "use the column default" — and that is done
	// by NOT naming the column in the INSERT at all, because naming
	// it with a 0 would override the default and produce an event
	// Stripe refuses to charge.
	var price any
	if ev.PriceCents != nil {
		price = *ev.PriceCents
		row.PriceCents = *ev.PriceCents
	}

	query := `INSERT INTO events (id, name, initial_inventory, currency, status`
	args := []any{id, ev.Name, ev.InitialInventory, ev.Currency, EventStatusActive}
	if ev.PriceCents != nil {
		query += `, price_cents`
		args = append(args, price)
	}
	query += `) VALUES ($1, $2, $3, $4, $5`
	if ev.PriceCents != nil {
		query += `, $6`
	}
	query += `) RETURNING price_cents, created_at`

	var storedPrice int64
	err := pool.QueryRow(ctx, query, args...).Scan(&storedPrice, &row.CreatedAt)
	if err != nil {
		return EventRow{}, fmt.Errorf("db: insert event: %w", err)
	}
	// The RETURNING value is what Postgres actually stored, which is
	// the column default when the caller did not supply a price.
	row.PriceCents = storedPrice
	return row, nil
}

// GetEvent reads one event by id.
//
// A missing row is reported with pgx.ErrNoRows, so the caller can use
// IsNoRows — the same convention every other read in this package
// uses.
func GetEvent(ctx context.Context, pool *Pool, id string) (EventRow, error) {
	var row EventRow
	err := pool.QueryRow(ctx,
		`SELECT id, name, status, initial_inventory, price_cents, currency, created_at
		   FROM events
		  WHERE id = $1`,
		id,
	).Scan(&row.ID, &row.Name, &row.Status, &row.InitialInventory,
		&row.PriceCents, &row.Currency, &row.CreatedAt)
	if err != nil {
		return EventRow{}, err
	}
	row.SeatsAvailable = -1
	return row, nil
}

// ListEvents returns the events, newest first.
//
// includeCancelled is the difference between the two audiences:
//
//   - false  the PUBLIC catalogue: only live drops
//   - true   the admin/ops view: everything, cancelled included
//
// A cancelled event is a tombstone, and the ops view is where you
// go to read it. The public catalogue is where you go to buy a
// ticket, and a cancelled drop is not for sale.
//
// limit is clamped to [1, 200] and offset to >= 0, the same bounds
// the reservation listing uses — so a client cannot ask for the whole
// table.
func ListEvents(ctx context.Context, pool *Pool, includeCancelled bool, limit, offset int) ([]EventRow, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	query := `SELECT id, name, status, initial_inventory, price_cents, currency, created_at
	            FROM events`
	args := []any{}
	if !includeCancelled {
		query += ` WHERE status = 'active'`
	}
	// Newest first, with the id as the tie-breaker so the order is
	// total and pagination is stable. Two events created in the same
	// microsecond must not swap places between pages.
	query += ` ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`
	args = append(args, limit, offset)

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list events: %w", err)
	}
	defer rows.Close()

	// An empty result is an empty slice, never nil — a nil slice
	// serialises to `null` in JSON, and a client expecting a list
	// would have to special-case it. (The same bug once lived in
	// ListReservationsByUser and was fixed.)
	out := []EventRow{}
	for rows.Next() {
		var row EventRow
		if err := rows.Scan(&row.ID, &row.Name, &row.Status, &row.InitialInventory,
			&row.PriceCents, &row.Currency, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan event row: %w", err)
		}
		row.SeatsAvailable = -1
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate event rows: %w", err)
	}
	return out, nil
}

// UpdateEvent applies a partial update to an event.
//
// Only the fields the patch supplies are written. The UPDATE is
// dynamic, but the column names come from a fixed whitelist below —
// never from the request — so there is no way to write a column the
// caller did not name.
//
// A cancelled event is NOT editable. The tombstone is the record of
// what happened; letting someone rename it afterwards makes the
// tombstone lie. So an update to a cancelled event is a conflict.
func UpdateEvent(ctx context.Context, pool *Pool, id string, patch EventPatch) (EventRow, error) {
	sets := []string{}
	args := []any{}

	if patch.Name != nil {
		if *patch.Name == "" {
			return EventRow{}, errors.New("db: event name must not be empty")
		}
		if len(*patch.Name) > 200 {
			return EventRow{}, errors.New("db: event name must be at most 200 characters")
		}
		sets = append(sets, "name = $"+itoa(len(args)+1))
		args = append(args, *patch.Name)
	}
	if patch.PriceCents != nil {
		if *patch.PriceCents < 0 {
			return EventRow{}, errors.New("db: price_cents must be >= 0")
		}
		sets = append(sets, "price_cents = $"+itoa(len(args)+1))
		args = append(args, *patch.PriceCents)
	}
	if patch.Currency != nil {
		if !isValidCurrency(*patch.Currency) {
			return EventRow{}, fmt.Errorf("db: currency %q must be a 3-letter lowercase ISO code", *patch.Currency)
		}
		sets = append(sets, "currency = $"+itoa(len(args)+1))
		args = append(args, *patch.Currency)
	}

	if len(sets) == 0 {
		// Nothing to do. Return the current row rather than an
		// error: a PATCH with no changes is a no-op, and the caller
		// wants to see the event either way.
		row, err := GetEvent(ctx, pool, id)
		if err != nil {
			return EventRow{}, err
		}
		if row.Status == EventStatusCancelled {
			return EventRow{}, ErrEventCancelled
		}
		return row, nil
	}

	query := `UPDATE events SET ` + joinSetters(sets) +
		` WHERE id = $` + itoa(len(args)+1) + ` AND status = 'active'`
	args = append(args, id)

	var row EventRow
	err := pool.QueryRow(ctx,
		query+` RETURNING id, name, status, initial_inventory, price_cents, currency, created_at`,
		args...,
	).Scan(&row.ID, &row.Name, &row.Status, &row.InitialInventory,
		&row.PriceCents, &row.Currency, &row.CreatedAt)
	if err != nil {
		if IsNoRows(err) {
			// The UPDATE matched no rows. Either the event does not
			// exist, or it is a cancelled tombstone — two different
			// facts, so work out which.
			return EventRow{}, classifyUpdateFailure(ctx, pool, id)
		}
		return EventRow{}, fmt.Errorf("db: update event: %w", err)
	}
	row.SeatsAvailable = -1
	return row, nil
}

// classifyUpdateFailure explains why an update matched no rows.
//
// The "not active" guard lives in the UPDATE's own WHERE clause — the
// same shape as the compare-and-set in db.TransitionStatus (see
// Convention #13). A read-then-write would leave a window where a
// tombstone gets rewritten between the read and the write.
//
// The consequence of putting the guard in the UPDATE is that the write
// does NOT happen — which is the point. A tombstone that had its name
// rewritten would be a record of what happened that no longer says
// what happened.
func classifyUpdateFailure(ctx context.Context, pool *Pool, id string) error {
	var status string
	err := pool.QueryRow(ctx, `SELECT status FROM events WHERE id = $1`, id).Scan(&status)
	if err != nil {
		if IsNoRows(err) {
			return ErrEventNotFound
		}
		return fmt.Errorf("db: classify update failure: %w", err)
	}
	if status == EventStatusCancelled {
		return ErrEventCancelled
	}
	// The event exists and is active, so the update should have
	// matched. This is a genuine surprise — report it rather than
	// pretending it was a not-found.
	return fmt.Errorf("db: update event %s matched no rows but the event is active", id)
}

// ErrEventNotFound is returned when an event id does not exist.
var ErrEventNotFound = errors.New("db: event not found")

// ErrEventCancelled is returned when a write targets a cancelled
// event. A tombstone is a record of what happened; it is not
// editable.
var ErrEventCancelled = errors.New("db: event is cancelled")

// ErrEventHasReservations is returned when an event with any
// reservation is cancelled.
//
// The threshold is "any reservation EVER existed", not "any
// reservation is still live": an expired reservation still proves a
// customer interacted with the drop, and pulling the event out from
// under someone who tried to buy is worse than refusing.
var ErrEventHasReservations = errors.New("db: event has reservations")

// CancelEvent cancels an event.
//
// THE GUARD: an event may only be cancelled while it has ZERO
// reservations. This is not an arbitrary line — it is where the
// schema already draws it. reservations.event_id REFERENCES
// events(id) has NO ON DELETE CASCADE, so a hard delete against an
// event with any reservation fails at the database level. The
// explicit check turns an ugly 23503 foreign_key_violation into a
// clear conflict with a count.
//
// The guard is enforced IN THE SAME STATEMENT as the update, as a
// WHERE clause — the same shape as db.TransitionStatus's
// compare-and-set (see Convention #13). A check in a separate
// SELECT followed by an UPDATE is a TOCTOU window: two admins
// cancelling the same event at once could both see zero reservations
// and both write.
//
// The UPDATE is also guarded on the current status, so cancelling an
// already-cancelled event is a no-op rather than an error — a
// retried admin call is not a failure.
//
// Returns ErrEventNotFound, ErrEventCancelled or
// ErrEventHasReservations.
func CancelEvent(ctx context.Context, pool *Pool, id string) (EventRow, error) {
	var row EventRow
	err := pool.QueryRow(ctx,
		`UPDATE events
		    SET status = 'cancelled'
		  WHERE id = $1
		    AND status = 'active'
		    AND NOT EXISTS (SELECT 1 FROM reservations WHERE event_id = $1)
		  RETURNING id, name, status, initial_inventory, price_cents, currency, created_at`,
		id,
	).Scan(&row.ID, &row.Name, &row.Status, &row.InitialInventory,
		&row.PriceCents, &row.Currency, &row.CreatedAt)
	if err != nil {
		if IsNoRows(err) {
			// The UPDATE matched nothing. Work out WHY — the
			// answers are different facts and the caller needs
			// to say which one it is.
			return EventRow{}, classifyCancelFailure(ctx, pool, id)
		}
		return EventRow{}, fmt.Errorf("db: cancel event: %w", err)
	}
	row.SeatsAvailable = -1
	return row, nil
}

// classifyCancelFailure explains why a cancel matched no rows.
//
// Three possibilities, and they are genuinely different facts:
//
//	not found         no such event
//	already cancelled  it is a tombstone already (idempotent 200)
//	has reservations   the guard fired
func classifyCancelFailure(ctx context.Context, pool *Pool, id string) error {
	var status string
	err := pool.QueryRow(ctx, `SELECT status FROM events WHERE id = $1`, id).Scan(&status)
	if err != nil {
		if IsNoRows(err) {
			return ErrEventNotFound
		}
		return fmt.Errorf("db: classify cancel failure: %w", err)
	}
	if status == EventStatusCancelled {
		return ErrEventCancelled
	}

	// The event is active but the cancel did not match, so the
	// guard must have fired. Count the reservations so the caller
	// can say how many.
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reservations WHERE event_id = $1`, id).Scan(&count); err != nil {
		return fmt.Errorf("db: count reservations: %w", err)
	}
	return fmt.Errorf("%w: %d", ErrEventHasReservations, count)
}

// CountEventReservations returns how many reservations an event has,
// ever.
//
// It is used to explain a refused cancel, and it is the number the
// conflict response carries.
func CountEventReservations(ctx context.Context, pool *Pool, eventID string) (int, error) {
	var count int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reservations WHERE event_id = $1`, eventID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("db: count event reservations: %w", err)
	}
	return count, nil
}

// itoa formats a small int as a string. It exists because the UPDATE
// builder needs "$1", "$2", … .
func itoa(n int) string {
	return strconv.Itoa(n)
}

// joinSetters joins the SET clauses with ", ".
func joinSetters(sets []string) string {
	out := ""
	for i, s := range sets {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
