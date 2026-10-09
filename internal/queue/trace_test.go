package queue

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestInjectExtractRoundtrip verifies the trace context set on ctx
// survives an SQS roundtrip:
//
//   ctx → InjectTraceContext → SQS input → SQS msg → ExtractTraceContext → ctx
//
// The trace_id MUST be the same on both sides. If it isn't, the worker's
// span will be a disconnected root and the end-to-end waterfall breaks.
func TestInjectExtractRoundtrip(t *testing.T) {
	// In-memory SDK so we can create real spans.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	tracer := tp.Tracer("test")
	ctx, span1 := tracer.Start(context.Background(), "publisher-span")
	// We deliberately do NOT call span1.End() yet — the SDK needs the
	// span to be "recording" to populate its context. End after we
	// capture the trace_id.
	originalTraceID := span1.SpanContext().TraceID()
	originalSpanID := span1.SpanContext().SpanID()
	span1.End()

	require.True(t, originalTraceID.IsValid(), "publisher span should have a valid trace_id")

	// Publisher path: take a fresh SendMessageInput and inject.
	input := &sqs.SendMessageInput{
		QueueUrl:    aws.String("https://sqs.example/q"),
		MessageBody: aws.String("{}"),
	}
	InjectTraceContext(ctx, input)

	// Sanity: the carrier should have populated MessageAttributes.
	require.NotEmpty(t, input.MessageAttributes, "inject should add traceparent attribute")
	require.Contains(t, input.MessageAttributes, "traceparent")

	// Consumer path: simulate receiving the message. We don't have a
	// real SQS client in the test, so we just turn the input back into
	// a types.Message (which is what ReceiveMessage returns).
	msg := types.Message{
		MessageId:     aws.String("test-msg-id"),
		Body:          input.MessageBody,
		MessageAttributes: input.MessageAttributes,
	}
	extractedCtx := ExtractTraceContext(context.Background(), msg)

	// The extracted context should now carry a valid SpanContext with
	// the SAME trace_id (so it's a child of the publisher's span).
	gotSC := trace.SpanContextFromContext(extractedCtx)
	require.True(t, gotSC.IsValid(),
		"extracted context should have a valid SpanContext")
	require.Equal(t, originalTraceID, gotSC.TraceID(),
		"trace_id should survive the SQS roundtrip")
	require.Equal(t, originalSpanID, gotSC.SpanID(),
		"publisher's span_id should become the consumer's parent")
}

// TestInjectEmpty verifies that InjectTraceContext on a context with
// no active span doesn't panic and produces no attributes.
func TestInjectEmpty(t *testing.T) {
	input := &sqs.SendMessageInput{
		QueueUrl:    aws.String("https://sqs.example/q"),
		MessageBody: aws.String("{}"),
	}
	InjectTraceContext(context.Background(), input)
	require.Empty(t, input.MessageAttributes,
		"inject with no active span should leave attributes empty")
}

// TestExtractNoAttributes verifies that ExtractTraceContext on a
// message with no attributes returns the input context unchanged
// (the resulting span will be a fresh root, not a propagated child).
func TestExtractNoAttributes(t *testing.T) {
	ctx := context.Background()
	got := ExtractTraceContext(ctx, types.Message{})
	require.False(t, trace.SpanContextFromContext(got).IsValid(),
		"context with no extracted attributes should not have a valid SpanContext")
}
