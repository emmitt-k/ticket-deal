package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// Publisher is the worker-facing interface for sending messages to SQS.
// The API handler depends on the smaller api.ReservationPublisher
// interface; this one is what the queue package itself implements.
//
// The two are kept distinct because the worker might in Phase 7+ want
// to publish status updates ("reservation confirmed", "expired") that
// don't fit the Reservation payload — those would take a different
// method (PublishStatus or similar) and not pollute the API contract.
type Publisher interface {
	// SendReservation publishes a reservation event. It blocks until
	// the broker acknowledges receipt (or returns an error).
	SendReservation(ctx context.Context, r Reservation) error
}

// SQSPublisher implements Publisher using the AWS SQS SDK. The same
// code talks to real AWS SQS in production and ElasticMQ locally —
// the SDK uses the AWS_ENDPOINT_URL_SQS environment variable (set via
// aws config.WithEndpointResolver) to point at http://localhost:9324
// during dev/test.
type SQSPublisher struct {
	client   *sqs.Client
	queueURL string
}

// NewSQSPublisher constructs an SQS publisher bound to one queue.
// The caller owns the underlying sqs.Client lifetime.
func NewSQSPublisher(client *sqs.Client, queueURL string) *SQSPublisher {
	return &SQSPublisher{client: client, queueURL: queueURL}
}

// SendReservation marshals the payload as JSON and sends it.
//
// On error from the SDK (network, auth, queue not found), we return
// the wrapped error. The caller (API handler) logs but does NOT roll
// back the reservation, because Redis already holds the seat and the
// worker can recover by polling the DB or replaying from another
// instance — Postgres's ON CONFLICT clause makes INSERT idempotent.
func (p *SQSPublisher) SendReservation(ctx context.Context, r Reservation) error {
	body, err := json.Marshal(r)
	if err != nil {
		// json.Marshal on a struct with only primitive fields cannot
		// fail at runtime — but defensively, return a wrapped error
		// instead of panicking.
		return fmt.Errorf("queue: marshal reservation: %w", err)
	}

	_, err = p.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(p.queueURL),
		MessageBody: aws.String(string(body)),
	})
	if err != nil {
		return fmt.Errorf("queue: SQS SendMessage: %w", err)
	}
	return nil
}

// ReservationAPIPublisher adapts an SQSPublisher to the smaller
// api.ReservationPublisher interface (which takes a raw []byte).
//
// We keep this thin adapter (rather than making SQSPublisher itself
// implement both) so the queue package doesn't have to import the
// api package — that would create a circular dependency since the
// api package's reserve_handler.go will import queue for the Reservation
// type.
type ReservationAPIPublisher struct {
	inner *SQSPublisher
}

// NewReservationAPIPublisher wraps an SQSPublisher so it can be
// passed to api.ReserveHandler (which expects the byte-slice interface).
func NewReservationAPIPublisher(inner *SQSPublisher) *ReservationAPIPublisher {
	return &ReservationAPIPublisher{inner: inner}
}

// Publish unmarshals the byte slice (which the API handler builds
// with json.Marshal) and forwards to SendReservation.
//
// We could skip unmarshal+marshal entirely (the handler builds JSON, and
// SendMessage takes a string anyway), but doing it round-trip lets the
// adapter detect malformed wire data before it hits SQS. The cost is a
// few microseconds per message, which is negligible.
func (a *ReservationAPIPublisher) Publish(ctx context.Context, body []byte) error {
	var r Reservation
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("queue: API publisher unmarshal: %w", err)
	}
	return a.inner.SendReservation(ctx, r)
}