// Package eventid owns the identity of an event.
//
// The event id is a UUID, minted by the application. It started life
// as a BIGINT running counter, which was wrong for two reasons:
//
//   - It was GUESSABLE. A customer could walk id=1, 2, 3… and
//     enumerate the whole catalogue — every drop's name, price and
//     inventory with it.
//   - It was allocated BY HAND. Every id had to be invented by
//     whatever wrote the row, so an event could not be created without
//     someone deciding its id first.
//
// The id now travels as a plain `string` — the same representation
// `reservation_id` already uses — so there is exactly one convention
// in the codebase for an app-minted identifier.
//
// This package is the ONE place that knows what a valid event id
// looks like. The id crosses a lot of boundaries — a JWT claim, a
// Redis key, a JSON payload, a SQL parameter, a URL query — and a
// mismatch between two of those boundaries is the kind of bug that
// fails silently. Validate at the edges, here, once.
package eventid

import (
	"fmt"

	"github.com/google/uuid"
)

// ErrInvalid is returned when a string is not a well-formed event id.
//
// It is a sentinel so callers can use errors.Is rather than comparing
// strings — the same style the auth package uses.
var ErrInvalid = fmt.Errorf("eventid: invalid event id")

// Valid reports whether s is a well-formed event id.
//
// A well-formed id is the canonical 36-character hyphenated form:
// 8-4-4-4-12 lowercase hex digits. The check is deliberately stricter
// than "parses as a uuid": it rejects the urn: and braced forms that
// uuid.Parse accepts, because those would let two spellings of the
// SAME id reach two different boundaries — and a Redis key built from
// one spelling would not match a key built from the other.
func Valid(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHexLower(c) {
				return false
			}
		}
	}
	return true
}

// Parse validates s and returns it, or an error wrapping ErrInvalid.
//
// The error message names the offending value, because the value
// usually came from a request and the operator reading the log needs
// to see what was actually sent.
func Parse(s string) (string, error) {
	if !Valid(s) {
		return "", fmt.Errorf("%w: %q", ErrInvalid, s)
	}
	return s, nil
}

// New mints a fresh event id.
//
// The application always supplies the id — the `events.id` column has
// no DEFAULT, deliberately, so there is exactly one way an event id
// comes into existence. See the package doc.
func New() string {
	return uuid.NewString()
}

func isHexLower(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}
