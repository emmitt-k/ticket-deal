package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// The SQS-facing half of the queue package.
//
// These tests talk to a REAL broker (ElasticMQ), because the things
// worth checking here are broker behaviours: that a published message
// actually arrives, that a handled message is deleted, that a failed
// handler leaves the message visible so it comes back, and that the
// traceparent survives the roundtrip.
//
// Per the repo convention, a missing broker means SKIP, not fail — so
// `go test ./...` still passes with no services running. In CI the
// broker IS a service container, so these run there.

// elasticMQEndpoint is where the local broker lives. Overridable so
// CI (and any other environment) can point the tests at a different
// address — and so the "broker is down -> skip" path can be exercised.
func elasticMQEndpoint() string {
	if v := os.Getenv("SQS_ENDPOINT_URL"); v != "" {
		return v
	}
	return "http://localhost:9324"
}

// newTestSQSClient returns a client pointed at the local ElasticMQ, or
// skips the test when no broker is running.
func newTestSQSClient(t *testing.T) *sqs.Client {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := NewSQSClient(ctx, AWSConfig{
		Region:      "us-east-1",
		EndpointURL: elasticMQEndpoint(),
		QueueURL:    elasticMQEndpoint() + "/000000000000/probe",
	})
	if err != nil {
		t.Skipf("could not build an SQS client: %v", err)
	}

	// Probe the broker. ElasticMQ answers ListQueues with an empty
	// list when it is up, and the connection is refused when it is not.
	if _, err := client.ListQueues(ctx, &sqs.ListQueuesInput{}); err != nil {
		t.Skipf("ElasticMQ unavailable at %s: %v", elasticMQEndpoint(), err)
	}
	return client
}

// newTestQueue creates a queue with a unique name, so tests never see
// each other's messages.
func newTestQueue(t *testing.T, client *sqs.Client) string {
	t.Helper()

	name := "td-test-" + uuid.NewString()
	out, err := client.CreateQueue(context.Background(), &sqs.CreateQueueInput{
		QueueName: aws.String(name),
	})
	require.NoError(t, err)
	return aws.ToString(out.QueueUrl)
}

// ── NewSQSClient — the pure error paths ───────────────────────────────

// TestNewSQSClient_MissingConfig — the two settings the SDK cannot
// default sensibly are startup errors, not silent defaults.
func TestNewSQSClient_MissingConfig(t *testing.T) {
	t.Run("no region", func(t *testing.T) {
		_, err := NewSQSClient(context.Background(), AWSConfig{
			QueueURL: "http://localhost:9324/000000000000/x",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "region")
	})

	t.Run("no queue URL", func(t *testing.T) {
		_, err := NewSQSClient(context.Background(), AWSConfig{
			Region: "us-east-1",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "queue URL")
	})
}

// TestReservationAPIPublisher_MalformedJSON — the adapter validates
// the wire data BEFORE it hits the broker. A malformed body is a
// caller bug and must be reported as one, not as an SQS error.
func TestReservationAPIPublisher_MalformedJSON(t *testing.T) {
	adapter := NewReservationAPIPublisher(&SQSPublisher{}) // no client needed
	err := adapter.Publish(context.Background(), []byte("not json"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unmarshal")
}

// ── The publisher, against a real broker ──────────────────────────────

// TestSQSPublisher_SendArrives — the message published is the message
// received, byte for byte.
func TestSQSPublisher_SendArrives(t *testing.T) {
	client := newTestSQSClient(t)
	queueURL := newTestQueue(t, client)

	pub := NewSQSPublisher(client, queueURL)
	body := []byte(`{"reservation_id":"abc-123","user_id":"alice","event_id":7,"seats":2}`)
	require.NoError(t, pub.Send(context.Background(), body))

	out, err := client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(queueURL),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     5,
	})
	require.NoError(t, err)
	require.Len(t, out.Messages, 1)
	require.JSONEq(t, string(body), aws.ToString(out.Messages[0].Body))
}

// TestSQSPublisher_SendReservation — the typed wrapper round-trips a
// Reservation through the broker unchanged.
func TestSQSPublisher_SendReservation(t *testing.T) {
	client := newTestSQSClient(t)
	queueURL := newTestQueue(t, client)

	pub := NewSQSPublisher(client, queueURL)
	original := Reservation{
		ReservationID: "res-abc",
		UserID:        "alice",
		EventID:       42,
		Seats:         3,
		CreatedAt:     "2026-10-27T10:00:00Z",
		ExpiresAt:     "2026-10-27T10:10:00Z",
	}
	require.NoError(t, pub.SendReservation(context.Background(), original))

	out, err := client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(queueURL),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     5,
	})
	require.NoError(t, err)
	require.Len(t, out.Messages, 1)

	var decoded Reservation
	require.NoError(t, json.Unmarshal([]byte(aws.ToString(out.Messages[0].Body)), &decoded))
	require.Equal(t, original, decoded)
}

// TestSQSPublisher_SendEmbedsTheTraceparent — the publisher splices the
// trace context into the message BODY as `_traceparent`. That is the
// fallback that survives ElasticMQ stripping MessageAttributes, and it
// is the reason a stripe-worker's span can be a child of the API's
// webhook span.
func TestSQSPublisher_SendEmbedsTheTraceparent(t *testing.T) {
	client := newTestSQSClient(t)
	queueURL := newTestQueue(t, client)

	// A trace context only exists when there is an active span — which
	// is exactly the case in production (the webhook handler and the
	// reserve handler both run inside a span). With no span there is
	// nothing to propagate, so the publisher correctly embeds nothing.
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	ctx, span := tp.Tracer("test").Start(context.Background(), "webhook-span")
	defer span.End()

	pub := NewSQSPublisher(client, queueURL)
	require.NoError(t, pub.Send(ctx, []byte(`{"event_id":1}`)))

	out, err := client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(queueURL),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     5,
	})
	require.NoError(t, err)
	require.Len(t, out.Messages, 1)

	body := aws.ToString(out.Messages[0].Body)
	require.Contains(t, body, "_traceparent",
		"the publisher must embed the trace context in the body")

	// And the embedded value must be a real traceparent, not a stub.
	embedded := extractTraceparentFromBody(body)
	require.Regexp(t, `^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`, embedded,
		"the embedded traceparent must be well-formed")
}

// TestSQSPublisher_Ping — Ping is what /readyz calls. It must succeed
// for a live queue and fail for a queue that does not exist.
func TestSQSPublisher_Ping(t *testing.T) {
	client := newTestSQSClient(t)
	queueURL := newTestQueue(t, client)

	pub := NewSQSPublisher(client, queueURL)
	require.NoError(t, pub.Ping(context.Background()))

	missing := NewSQSPublisher(client, queueURL+"-does-not-exist")
	require.Error(t, missing.Ping(context.Background()),
		"pinging a queue that does not exist must fail")
}

// TestEventAPIPublisher_Publish — the Stripe event publisher. It is
// the path the webhook handler uses, so it must deliver a payload
// unchanged.
func TestEventAPIPublisher_Publish(t *testing.T) {
	client := newTestSQSClient(t)
	queueURL := newTestQueue(t, client)

	pub := NewEventAPIPublisher(NewSQSPublisher(client, queueURL))
	body := []byte(`{"id":"evt_1","type":"payment_intent.succeeded"}`)
	require.NoError(t, pub.Publish(context.Background(), body))

	out, err := client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(queueURL),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     5,
	})
	require.NoError(t, err)
	require.Len(t, out.Messages, 1)
	require.JSONEq(t, string(body), aws.ToString(out.Messages[0].Body))
}

// TestReservationAPIPublisher_Ping — the /readyz seam that lets the
// API probe SQS without holding the raw client.
func TestReservationAPIPublisher_Ping(t *testing.T) {
	client := newTestSQSClient(t)
	queueURL := newTestQueue(t, client)

	pub := NewReservationAPIPublisher(NewSQSPublisher(client, queueURL))
	require.NoError(t, pub.Ping(context.Background()))
}

// ── The consumer, against a real broker ───────────────────────────────

// TestPoll_DeliversThenDeletes — the core at-least-once contract:
// a message the handler accepts is deleted from the queue.
func TestPoll_DeliversThenDeletes(t *testing.T) {
	client := newTestSQSClient(t)
	queueURL := newTestQueue(t, client)

	require.NoError(t, NewSQSPublisher(client, queueURL).
		Send(context.Background(), []byte(`{"n":1}`)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handled := make(chan string, 4)
	go func() {
		_ = Poll(ctx, ConsumerConfig{
			Client:                   client,
			QueueURL:                 queueURL,
			MaxMessages:              1,
			WaitTimeSeconds:          1,
			VisibilityTimeoutSeconds: 5,
		}, MessageHandlerFunc(func(_ context.Context, body []byte) error {
			handled <- string(body)
			return nil
		}))
	}()

	select {
	case body := <-handled:
		require.JSONEq(t, `{"n":1}`, body)
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for Poll to deliver the message")
	}

	// The handler succeeded, so Poll must have deleted the message.
	require.Eventually(t, func() bool {
		out, err := client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(queueURL),
			MaxNumberOfMessages: 1,
			WaitTimeSeconds:     0,
		})
		return err == nil && len(out.Messages) == 0
	}, 10*time.Second, 250*time.Millisecond,
		"a handled message must be deleted from the queue")
}

// TestPoll_HandlerErrorLeavesTheMessage — the other half of the
// contract: a handler that fails must NOT delete the message, so SQS
// redelivers it. The proof is the SECOND delivery: the message comes
// back after its visibility timeout.
func TestPoll_HandlerErrorLeavesTheMessage(t *testing.T) {
	client := newTestSQSClient(t)
	queueURL := newTestQueue(t, client)

	require.NoError(t, NewSQSPublisher(client, queueURL).
		Send(context.Background(), []byte(`{"n":2}`)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := make(chan struct{}, 8)
	go func() {
		_ = Poll(ctx, ConsumerConfig{
			Client:                   client,
			QueueURL:                 queueURL,
			MaxMessages:              1,
			WaitTimeSeconds:          1,
			VisibilityTimeoutSeconds: 1, // short, so the redelivery is quick
		}, MessageHandlerFunc(func(_ context.Context, _ []byte) error {
			calls <- struct{}{}
			return errors.New("boom")
		}))
	}()

	select {
	case <-calls:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the first delivery")
	}

	select {
	case <-calls:
		// The second delivery is the proof: the message was not
		// deleted, so the broker made it visible again.
	case <-time.After(20 * time.Second):
		t.Fatal("the message was never redelivered — it was deleted despite the handler failing")
	}
}

// TestPoll_ContextCancel — Poll blocks until its context is cancelled,
// and then returns the cancellation rather than looping forever.
func TestPoll_ContextCancel(t *testing.T) {
	client := newTestSQSClient(t)
	queueURL := newTestQueue(t, client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	err := Poll(ctx, ConsumerConfig{
		Client:   client,
		QueueURL: queueURL,
	}, MessageHandlerFunc(func(context.Context, []byte) error { return nil }))

	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled),
		"Poll must return the context error, got %v", err)
}

// TestMessageHandlerFunc — the function-shaped adapter.
func TestMessageHandlerFunc(t *testing.T) {
	called := false
	h := MessageHandlerFunc(func(context.Context, []byte) error {
		called = true
		return nil
	})
	require.NoError(t, h.HandleMessage(context.Background(), nil))
	require.True(t, called)

	h = MessageHandlerFunc(func(context.Context, []byte) error {
		return fmt.Errorf("nope")
	})
	require.Error(t, h.HandleMessage(context.Background(), nil))
}
