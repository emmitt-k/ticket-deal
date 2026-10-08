package queue

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// MessageHandler is the per-message callback the consumer dispatches
// to. Returning nil means "I handled this successfully, delete it".
// Returning an error means "I couldn't handle this, leave it visible
// so SQS redelivers after the visibility timeout".
//
// This is the core of at-least-once delivery:
//   - handler returns nil  → message deleted
//   - handler returns err  → message left visible, SQS will redeliver
//   - worker crashes mid-handler → message visibility expires, SQS
//     will redeliver to another instance
//
// Combined with INSERT ... ON CONFLICT DO NOTHING in the DB layer,
// a redelivered message creates zero extra rows. The Idempotency test
// in the implementation plan proves this.
type MessageHandler interface {
	HandleMessage(ctx context.Context, body []byte) error
}

// MessageHandlerFunc is the function-shaped adapter that lets callers
// pass a plain `func(ctx, body) error` instead of declaring a struct.
type MessageHandlerFunc func(ctx context.Context, body []byte) error

// HandleMessage implements MessageHandler.
func (f MessageHandlerFunc) HandleMessage(ctx context.Context, body []byte) error {
	return f(ctx, body)
}

// ConsumerConfig groups the SQS client and queue URL into one value
// the Poll function can carry around without huge signatures.
type ConsumerConfig struct {
	Client   *sqs.Client
	QueueURL string
	// MaxMessages is the max number of messages per ReceiveMessage
	// call. SQS caps this at 10 (1 or 10 in practice — values like
	// 5 also work but SQS prefers 10 for batch throughput).
	MaxMessages int32
	// WaitTimeSeconds is the long-poll duration. SQS caps at 20.
	// 0 disables long polling (don't do that, it hammers the API).
	// Production: 20. Tests: shorter (e.g. 1) so they don't hang.
	WaitTimeSeconds int32
	// VisibilityTimeoutSeconds is how long SQS hides a delivered
	// message before assuming the handler crashed. Should be ≥ the
	// handler's worst-case runtime. Production: 30.
	VisibilityTimeoutSeconds int32
}

// Poll long-polls SQS in a loop, dispatching each message to handler.
//
// Exit conditions:
//   - ctx.Done() returns true      → returns ctx.Err()
//   - handler returns nil          → message deleted, loop continues
//   - handler returns non-nil err  → message left visible, loop
//                                    continues (logs at the call site
//                                    so we don't spam the same error)
//   - ReceiveMessage itself errors → logs, sleeps 1 s, retries
//
// Poll blocks until ctx is cancelled. The cmd/worker binary runs it
// in its main goroutine, watching a separate signal channel for
// graceful shutdown.
func Poll(ctx context.Context, cfg ConsumerConfig, handler MessageHandler) error {
	if cfg.MaxMessages == 0 {
		cfg.MaxMessages = 10
	}
	if cfg.WaitTimeSeconds == 0 {
		cfg.WaitTimeSeconds = 20
	}
	if cfg.VisibilityTimeoutSeconds == 0 {
		cfg.VisibilityTimeoutSeconds = 30
	}

	for {
		// Check ctx before the blocking call so we don't waste a
		// long-poll round-trip if we're already shutting down.
		if err := ctx.Err(); err != nil {
			return err
		}

		out, err := cfg.Client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(cfg.QueueURL),
			MaxNumberOfMessages: cfg.MaxMessages,
			WaitTimeSeconds:     cfg.WaitTimeSeconds,
			VisibilityTimeout:   cfg.VisibilityTimeoutSeconds,
		})
		if err != nil {
			// If ctx was cancelled mid-call, that's a clean exit.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			log.Printf("worker: ReceiveMessage failed (will retry): %v", err)
			// Brief pause so we don't 100%-CPU a sick broker.
			// ctx-aware sleep so shutdown isn't blocked for the full second.
			t := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
				// fall through to retry
			}
			continue
		}

		for _, msg := range out.Messages {
			body := aws.ToString(msg.Body)
			receipt := aws.ToString(msg.ReceiptHandle)

			herr := handler.HandleMessage(ctx, []byte(body))
			if herr != nil {
				// Don't delete. SQS will redeliver after visibility
				// timeout. Log so an operator can see which message
				// type is failing.
				log.Printf("worker: handler error (will redeliver) id=%s: %v",
					aws.ToString(msg.MessageId), herr)
				continue
			}

			// Delete AFTER successful handling. If we crash between
			// commit and delete, the next worker to receive the
			// redelivered message will see ON CONFLICT DO NOTHING
			// and treat it as a no-op — exactly the at-least-once
			// guarantee.
			if _, derr := cfg.Client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(cfg.QueueURL),
				ReceiptHandle: aws.String(receipt),
			}); derr != nil {
				// Already-handled but not-deleted is the worst case:
				// SQS will redeliver, and the DB layer will idempotently
				// skip the duplicate. Log and move on.
				log.Printf("worker: DeleteMessage failed (handler succeeded; DB idempotency will absorb redelivery) id=%s: %v",
					aws.ToString(msg.MessageId), derr)
			}
		}
	}
}