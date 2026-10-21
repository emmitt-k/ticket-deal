// Package api holds the HTTP handlers and supporting types for the
// ticket-reservation endpoints. It sits between cmd/api (which wires
// the router) and internal/redis (which owns the Lua primitives) —
// the handler is a thin shell that does input validation, claims
// extraction, and result mapping, then delegates the actual atomic
// work to internal/redis.
package api

import (
	"context"
	"log/slog"
)

// ReservationPublisher is the seam between the API handler and the
// message queue that ships reservation events to the worker.
//
// Phase 5 only has a logging stub — the implementation is swapped for
// a real SQS publisher in Phase 6 (cmd/api/main.go is the only file
// that needs to change, the handler is unaware of the underlying
// transport). Defining the interface at the consumer site (here) is
// the standard Go idiom: "accept interfaces, return structs."
//
// Contract:
//   - Publish must be at-least-once. If the worker fails to process
//     the message, it must be redelivered, not lost.
//   - Publish must not block longer than the request context allows.
//     Returning ctx.Err() early is acceptable; the handler logs and
//     proceeds (the Redis hold is already created — the worker can
//     be replayed from another instance).
type ReservationPublisher interface {
	Publish(ctx context.Context, body []byte) error
}

// LogPublisher is the Phase 5 stub. It pretends to publish by writing
// the message body to the standard logger with a [STUB] prefix, so a
// human reading the API logs can see exactly what would have gone to
// SQS in production.
//
// Phase 6 replaces this with an SQS-backed implementation that wraps
// the aws-sdk-go-v2 SQS client.
type LogPublisher struct{}

// Publish logs the body and returns nil. Always succeeds.
func (LogPublisher) Publish(ctx context.Context, body []byte) error {
	slog.InfoContext(ctx, "stub SQS publish", "body", string(body))
	return nil
}